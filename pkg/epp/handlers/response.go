/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics"
	schedulingtypes "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

const (
	streamingRespPrefix = "data: "
	streamingEndMsg     = "data: [DONE]"
)

// HandleResponseBody always returns the requestContext even in the error case, as the request context is used in error handling.
func (s *StreamingServer) HandleResponseBody(ctx context.Context, reqCtx *RequestContext, response map[string]any) (*RequestContext, error) {
	logger := log.FromContext(ctx)
	responseBytes, err := json.Marshal(response)
	if err != nil {
		logger.V(logutil.DEFAULT).Error(err, "error marshalling responseBody")
		return reqCtx, err
	}
	if response["usage"] != nil {
		usg := response["usage"].(map[string]any)
		usage := Usage{
			PromptTokens:     int(usg["prompt_tokens"].(float64)),
			CompletionTokens: int(usg["completion_tokens"].(float64)),
			TotalTokens:      int(usg["total_tokens"].(float64)),
		}
		reqCtx.Usage = usage
		logger.V(logutil.VERBOSE).Info("Response generated", "usage", reqCtx.Usage)
	}
	reqCtx.ResponseSize = len(responseBytes)
	reqCtx.ResponseComplete = true

	reqCtx.respBodyResp = generateResponseBodyResponses(responseBytes, true, reqCtx, logger)
	return reqCtx, nil
}

// The function is to handle streaming response if the modelServer is streaming.
func (s *StreamingServer) HandleResponseBodyModelStreaming(ctx context.Context, reqCtx *RequestContext, responseText string) {
	if strings.Contains(responseText, streamingEndMsg) {
		reqCtx.ResponseComplete = true
		resp := parseRespForUsage(ctx, responseText)
		reqCtx.Usage = resp.Usage
		metrics.RecordInputTokens(reqCtx.Model, reqCtx.ResolvedTargetModel, resp.Usage.PromptTokens)
		metrics.RecordOutputTokens(reqCtx.Model, reqCtx.ResolvedTargetModel, resp.Usage.CompletionTokens)
		s.director.HandleResponseBodyComplete(ctx, reqCtx)
	}
	if s.director != nil && s.director.IsPredictorAvailable() {
		s.director.HandleResponseBodyChunk(ctx, reqCtx)
	}
}

func (s *StreamingServer) HandleResponseHeaders(ctx context.Context, reqCtx *RequestContext, resp *extProcPb.ProcessingRequest_ResponseHeaders) (*RequestContext, error) {
	for _, header := range resp.ResponseHeaders.Headers.Headers {
		if header.RawValue != nil {
			reqCtx.Response.Headers[header.Key] = string(header.RawValue)
		} else {
			reqCtx.Response.Headers[header.Key] = header.Value
		}
	}

	reqCtx, err := s.director.HandleResponseHeaders(ctx, reqCtx)

	return reqCtx, err
}

func (s *StreamingServer) generateResponseHeaderResponse(reqCtx *RequestContext) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: s.generateResponseHeaders(reqCtx),
					},
				},
			},
		},
	}
}

func generateResponseBodyResponses(
	responseBodyBytes []byte,
	setEoS bool,
	reqCtx *RequestContext,
	logger logr.Logger,
) []*extProcPb.ProcessingResponse {
	if reqCtx != nil && reqCtx.ModelServerStreaming {

		raw := string(responseBodyBytes)
		events := strings.Split(raw, "\n\n")

		var rebuilt strings.Builder
		for _, ev := range events {
			if !strings.HasPrefix(ev, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(ev, "data: ")
			if payload == "[DONE]" {
				rebuilt.WriteString("data: [DONE]\n\n")
				continue
			}

			// Try to unmarshal only the JSON
			var obj map[string]interface{}
			if err := json.Unmarshal([]byte(payload), &obj); err != nil {
				logger.Error(err, "failed to unmarshal SSE payload", "payload", payload)
			} else {
				if usage, ok := obj["usage"].(map[string]interface{}); ok && usage != nil {
					usage["ttft_ms"] = reqCtx.TTFT
					usage["predicted_ttft_ms"] = reqCtx.PredictedTTFT
					usage["tpot_observations_ms"] = reqCtx.TPOTObservations
					usage["predicted_tpot_observations_ms"] = reqCtx.PredictedTPOTObservations
					usage["avg_tpot_ms"] = reqCtx.AvgTPOT
					usage["avg_predicted_tpot_ms"] = reqCtx.AvgPredictedTPOT
				}
				if mod, err := json.Marshal(obj); err != nil {
					logger.Error(err, "failed to re-marshal modified JSON", "obj", obj)
				} else {
					payload = string(mod)
				}
			}

			// Re-attach SSE prefix
			rebuilt.WriteString("data: ")
			rebuilt.WriteString(payload)
			rebuilt.WriteString("\n\n")
		}

		// Feed into your existing chunker
		modified := []byte(rebuilt.String())
		commonResponses := buildCommonResponses(modified, bodyByteLimit, setEoS)

		// Wrap as ProcessingResponses
		out := make([]*extProcPb.ProcessingResponse, 0, len(commonResponses))
		for _, cr := range commonResponses {
			out = append(out, &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseBody{
					ResponseBody: &extProcPb.BodyResponse{
						Response: cr,
					},
				},
			})
		}
		return out
	} else {
		commonResponses := buildCommonResponses(responseBodyBytes, bodyByteLimit, setEoS)
		responses := []*extProcPb.ProcessingResponse{}
		for _, commonResp := range commonResponses {
			resp := &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseBody{
					ResponseBody: &extProcPb.BodyResponse{
						Response: commonResp,
					},
				},
			}
			responses = append(responses, resp)
		}
		return responses
	}

}

func (s *StreamingServer) generateResponseHeaders(reqCtx *RequestContext) []*configPb.HeaderValueOption {
	headers := []*configPb.HeaderValueOption{
		{
			Header: &configPb.HeaderValue{
				Key:      "x-went-into-resp-headers",
				RawValue: []byte("true"),
			},
		},
	}

	for key, value := range reqCtx.Response.Headers {
		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      key,
				RawValue: []byte(value),
			},
		})
	}
	return headers
}

func parseRespForUsage(ctx context.Context, responseText string) ResponseBody {
	response := ResponseBody{}
	logger := log.FromContext(ctx)

	lines := strings.Split(responseText, "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, streamingRespPrefix) {
			continue
		}
		content := strings.TrimPrefix(line, streamingRespPrefix)
		if content == "[DONE]" {
			continue
		}

		byteSlice := []byte(content)
		if err := json.Unmarshal(byteSlice, &response); err != nil {
			logger.Error(err, "unmarshaling response body")
			continue
		}
	}

	return response
}

type ResponseBody struct {
	Usage Usage `json:"usage"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func GetTargetPod(
	ctx context.Context,
	schedulingResult *schedulingtypes.SchedulingResult,
) schedulingtypes.Pod {
	logger := log.FromContext(ctx)

	if schedulingResult == nil || schedulingResult.ProfileResults == nil {
		logger.V(logutil.DEBUG).Info("No scheduling result available for target pod lookup")
		return nil
	}

	targetProfile := schedulingResult.PrimaryProfileName

	profileResult, exists := schedulingResult.ProfileResults[targetProfile]
	if !exists || profileResult == nil {
		logger.V(logutil.DEBUG).Info("Profile not found, using primary profile",
			"requested_profile", targetProfile,
			"primary_profile", schedulingResult.PrimaryProfileName)
		targetProfile = schedulingResult.PrimaryProfileName
		profileResult, exists = schedulingResult.ProfileResults[targetProfile]
		if !exists || profileResult == nil {
			logger.V(logutil.DEBUG).Info("Primary profile also not found",
				"primary_profile", targetProfile)
			return nil
		}
	}

	if len(profileResult.TargetPods) == 0 {
		logger.V(logutil.DEBUG).Info("No target pods found for profile",
			"profile", targetProfile)
		return nil
	}

	targetPod := profileResult.TargetPods[0]
	podInfo := targetPod.GetPod()

	logger.V(logutil.DEBUG).Info("Found target pod for profile",
		"pod", fmt.Sprintf("%s/%s", podInfo.NamespacedName.Name, podInfo.NamespacedName.Namespace),
		"profile", targetProfile,
		"requested_profile", targetProfile)

	return targetPod
}
