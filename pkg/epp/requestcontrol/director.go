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

// Package requestcontrol defines the Director component responsible for orchestrating request processing after initial
// parsing.
package requestcontrol

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/api/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/handlers"

	// Assuming the predictor is located here. Adjust the import path if necessary.
	latencypredictor "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/latencypredictorasync"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics"
	schedulingtypes "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
	errutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/error"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
	requtil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/request"
)

/*
NOTE: To support this refined logic, the `handlers.RequestContext` struct
(defined in a different package) would need to be updated as follows:

type RequestContext struct {
    // ... existing fields ...
	RequestReceivedTimestamp time.Time
	FirstTokenTimestamp      time.Time
	ResponseCompleteTimestamp time.Time
	IsModelServerStreaming   func() bool
	ResponseComplete         bool
	Prompt                   string
	LastSeenMetrics           *backend.Metrics
    // ... etc ...

    // -- New fields for latency predictor --
    PredictedTTFT float64 // The predicted TTFT in milliseconds.
    PredictedTPOT float64 // The predicted TPOT in milliseconds.
}

*/
// splitWords splits a string into words based on whitespace and returns the resulting slice.
func splitWords(input string) []string {
	return strings.Fields(input)
}

// Scheduler defines the interface required by the Director for scheduling.
type Scheduler interface {
	Schedule(ctx context.Context, b *schedulingtypes.LLMRequest) (result *schedulingtypes.SchedulingResult, err error)
}

// SaturationDetector provides a signal indicating whether the backends are considered saturated.
type SaturationDetector interface {
	IsSaturated(ctx context.Context) bool
}




func NewDirectorWithConfig(datastore datastore.Datastore, scheduler Scheduler, saturationDetector SaturationDetector, config *Config, predictor latencypredictor.PredictorInterface) *Director {
	log.Log.Info("Director created", 
		"predictor", predictor, 
		"predictorIsNil", predictor == nil,
		"predictorType", fmt.Sprintf("%T", predictor))
	return &Director{
		datastore:           datastore,
		scheduler:           scheduler,
		saturationDetector:  saturationDetector,
		latencyPredictor:    predictor, // Use the passed-in predictor instance.
		preRequestPlugins:   config.preRequestPlugins,
		postResponsePlugins: config.postResponsePlugins,
	}
}

// Director orchestrates the request handling flow, including scheduling.
type Director struct {
	datastore           datastore.Datastore
	scheduler           Scheduler
	saturationDetector  SaturationDetector
	latencyPredictor    latencypredictor.PredictorInterface
	preRequestPlugins   []PreRequest
	postResponsePlugins []PostResponse
}

const (
	// Maximum number of TPOT observations to retain per request
	maxTPOTObservations = 4096
)


// HandleRequest orchestrates the request lifecycle.
func (d *Director) HandleRequest(ctx context.Context, reqCtx *handlers.RequestContext) (*handlers.RequestContext, error) {
	logger := log.FromContext(ctx)

	// --- 1. Parse Request Details ---
	var ok bool
	requestBodyMap := reqCtx.Request.Body
	reqCtx.Model, ok = requestBodyMap["model"].(string)
	if !ok {
		return reqCtx, errutil.Error{Code: errutil.BadRequest, Msg: "model not found in request body"}
	}
	prompt, err := requtil.ExtractPromptFromRequestBody(requestBodyMap)
	if err != nil {
		return reqCtx, err
	} else {
		reqCtx.Prompt = prompt
	}

	modelObj := d.datastore.ModelGet(reqCtx.Model)
	if modelObj == nil {
		logger.Info("No associated inferenceModel found, using default", "model", reqCtx.Model)
		sheddable := v1alpha2.Sheddable
		modelObj = &v1alpha2.InferenceModel{
			Spec: v1alpha2.InferenceModelSpec{
				ModelName:   reqCtx.Model,
				Criticality: &sheddable,
			},
		}
	}

	reqCtx.ResolvedTargetModel = reqCtx.Model
	if len(modelObj.Spec.TargetModels) > 0 {
		reqCtx.ResolvedTargetModel = RandomWeightedDraw(logger, modelObj, 0)
		if reqCtx.ResolvedTargetModel == "" {
			return reqCtx, errutil.Error{Code: errutil.BadConfiguration, Msg: fmt.Sprintf("error getting target model name for model %v", modelObj.Name)}
		}
		reqCtx.Request.Body["model"] = reqCtx.ResolvedTargetModel // Update target model in the body.
	}

	requestCriticality := v1alpha2.Standard
	if modelObj.Spec.Criticality != nil {
		requestCriticality = *modelObj.Spec.Criticality
	}

	// Prepare LLMRequest (needed for both saturation detection and Scheduler)
	reqCtx.SchedulingRequest = &schedulingtypes.LLMRequest{
		RequestId:   reqCtx.Request.Headers[requtil.RequestIdHeaderKey],
		TargetModel: reqCtx.ResolvedTargetModel,
		Prompt:      prompt,
		Headers:     reqCtx.Request.Headers,
	}

	logger = logger.WithValues("model", reqCtx.Model, "resolvedTargetModel", reqCtx.ResolvedTargetModel, "criticality", requestCriticality)
	ctx = log.IntoContext(ctx, logger)
	logger.V(logutil.DEBUG).Info("LLM request assembled")

	// --- 2. Admission Control check --
	if err := d.admitRequest(ctx, requestCriticality); err != nil {
		return reqCtx, err
	}

	// --- 3. Call Scheduler ---
	results, err := d.scheduler.Schedule(ctx, reqCtx.SchedulingRequest)
	if err != nil {
		return reqCtx, errutil.Error{Code: errutil.InferencePoolResourceExhausted, Msg: fmt.Errorf("failed to find target pod: %w", err).Error()}
	}

	// --- 4. Prepare Request, which now includes making a latency prediction ---
	reqCtx, err = d.prepareRequest(ctx, reqCtx, results)
	if err != nil {
		return reqCtx, err
	}

	return reqCtx, nil
}

// admitRequest handles admission control to decide whether or not to accept the request
// based on the request criticality and system saturation state.
func (d *Director) admitRequest(ctx context.Context, requestCriticality v1alpha2.Criticality) error {
	logger := log.FromContext(ctx)

	if requestCriticality == v1alpha2.Critical {
		logger.V(logutil.DEBUG).Info("Critical request bypassing saturation check.")
		return nil
	}

	logger.V(logutil.DEBUG).Info("Performing saturation check for non-critical request.")
	if d.saturationDetector.IsSaturated(ctx) { // Assuming non-nil Saturation Detector
		return errutil.Error{
			Code: errutil.InferencePoolResourceExhausted,
			Msg:  "system saturated, non-critical request dropped",
		}
	}

	return nil
}

// prepareRequest sets endpoint & optionally initializes LastSeenMetrics.
func (d *Director) prepareRequest(ctx context.Context, reqCtx *handlers.RequestContext, result *schedulingtypes.SchedulingResult) (*handlers.RequestContext, error) {
	if result == nil || len(result.ProfileResults) == 0 {
		return reqCtx, errutil.Error{Code: errutil.Internal, Msg: "empty scheduling results"}
	}
	pr, ok := result.ProfileResults[result.PrimaryProfileName]
	if ok && pr.TargetPod != nil {
		orig := pr.TargetPod.GetMetrics()
		copyMetrics := *orig
		reqCtx.LastSeenMetrics = &copyMetrics
	}

	// Always set endpoint even if metrics missing
	pod := pr.TargetPod.GetPod()
	pool, err := d.datastore.PoolGet()
	if err != nil {
		return reqCtx, err
	}
	reqCtx.TargetPod = pod
	reqCtx.TargetEndpoint = net.JoinHostPort(pod.Address, strconv.Itoa(int(pool.Spec.TargetPortNumber)))
	reqCtx.SchedulingResult = result
	d.runPreRequestPlugins(ctx, reqCtx.SchedulingRequest, result, int(pool.Spec.TargetPortNumber))
	return reqCtx, nil
}


// HandleResponseHeaders is called when the first chunk of the response arrives.
func (d *Director) HandleResponseHeaders(ctx context.Context, reqCtx *handlers.RequestContext) (*handlers.RequestContext, error) {
    logger := log.FromContext(ctx).WithValues("stage", "headers")
    logger.V(logutil.DEBUG).Info("Entering HandleResponseHeaders")

    response := &Response{
        RequestId: reqCtx.Request.Headers[requtil.RequestIdHeaderKey],
        Headers:   reqCtx.Response.Headers,
    }
    d.runPostResponsePlugins(ctx, reqCtx.SchedulingRequest, response, reqCtx.TargetPod)

    if d.latencyPredictor == nil {
        logger.V(logutil.DEBUG).Info("No latency predictor configured; skipping header prediction")
        return reqCtx, nil
    }
    if reqCtx.SchedulingResult == nil {
        logger.V(logutil.DEBUG).Info("No scheduling result; skipping header prediction")
        return reqCtx, nil
    }

    pr, ok := reqCtx.SchedulingResult.ProfileResults[reqCtx.SchedulingResult.PrimaryProfileName]
    if !ok || pr.TargetPod == nil {
        logger.V(logutil.DEBUG).Info("No target pod metrics; skipping header prediction", "primaryProfile", reqCtx.SchedulingResult.PrimaryProfileName)
        return reqCtx, nil
    }

    // Refresh metrics
    orig := pr.TargetPod.GetMetrics()
    copyMetrics := *orig
    reqCtx.LastSeenMetrics = &copyMetrics
    logger.V(logutil.DEBUG).Info("Refreshed LastSeenMetrics at header", 
        "KVCache%", reqCtx.LastSeenMetrics.KVCacheUsagePercent,
        "Waiting", reqCtx.LastSeenMetrics.WaitingQueueSize,
        "Running", reqCtx.LastSeenMetrics.RunningQueueSize,
    )

    // Build prediction request
    predictionReq := latencypredictor.PredictionRequest{
        KVCachePercentage:  reqCtx.LastSeenMetrics.KVCacheUsagePercent,
        InputTokenLength:   len(splitWords(reqCtx.Prompt)),
        NumRequestWaiting:  reqCtx.LastSeenMetrics.WaitingQueueSize,
        NumRequestRunning:  reqCtx.LastSeenMetrics.RunningQueueSize,
        NumTokensGenerated: 0,
    }
    logger.V(logutil.DEBUG).Info("Header prediction request built", "req", predictionReq)

    // Predict TTFT
    if prediction, err := d.latencyPredictor.Predict(ctx, predictionReq); err != nil {
		reqCtx.PredictedTTFT = 0 // Append 0 if prediction fails
        logger.V(logutil.DEBUG).Error(err, "Latency prediction failed at header stage")
    } else if prediction != nil {
        reqCtx.PredictedTTFT = prediction.TTFT
        logger.V(logutil.DEBUG).Info("Predicted TTFT at header stage", 
            "predicted_ttft_ms", prediction.TTFT,
        )
    }

    logger.V(logutil.DEBUG).Info("Exiting HandleResponseHeaders")
    return reqCtx, nil
}

// HandleResponseBodyChunk is called for each streaming chunk.
func (d *Director) HandleResponseBodyChunk(ctx context.Context, reqCtx *handlers.RequestContext) error {
    logger := log.FromContext(ctx).WithValues("stage", "bodyChunk")
    logger.V(logutil.DEBUG).Info("Entering HandleResponseBodyChunk")

    if d.latencyPredictor == nil || reqCtx.SchedulingResult == nil {
        logger.V(logutil.DEBUG).Info("Skipping body-chunk logic; predictor or scheduling missing")
        return nil
    }
    pr, ok := reqCtx.SchedulingResult.ProfileResults[reqCtx.SchedulingResult.PrimaryProfileName]
    if !ok || pr.TargetPod == nil {
        logger.V(logutil.DEBUG).Info("Skipping body-chunk logic; no valid target pod")
        return nil
    }

    now := time.Now()


    // Refresh metrics
    orig := pr.TargetPod.GetMetrics()
    copyMetrics := *orig
    reqCtx.LastSeenMetrics = &copyMetrics
    logger.V(logutil.DEBUG).Info("Refreshed LastSeenMetrics at body chunk", 
        "KVCache%", reqCtx.LastSeenMetrics.KVCacheUsagePercent,
        "Waiting", reqCtx.LastSeenMetrics.WaitingQueueSize,
        "Running", reqCtx.LastSeenMetrics.RunningQueueSize,
    )

    // Cap observations
    if len(reqCtx.TPOTObservations) >= maxTPOTObservations {
        reqCtx.TPOTObservations = reqCtx.TPOTObservations[1:]
        reqCtx.PredictedTPOTObservations = reqCtx.PredictedTPOTObservations[1:]
        logger.V(logutil.DEBUG).Info("Capped TPOT observations to max", "max", maxTPOTObservations)
    }

    // Append actual inter-token latency
    isFirst := reqCtx.TTFT == 0
    

    // Build prediction request for TPOT
    predictionReq := latencypredictor.PredictionRequest{
        KVCachePercentage:  reqCtx.LastSeenMetrics.KVCacheUsagePercent,
        InputTokenLength:   len(splitWords(reqCtx.Prompt)),
        NumRequestWaiting:  reqCtx.LastSeenMetrics.WaitingQueueSize,
        NumRequestRunning:  reqCtx.LastSeenMetrics.RunningQueueSize,
        NumTokensGenerated: len(reqCtx.TPOTObservations),
    }
    logger.V(logutil.DEBUG).Info("Body-chunk prediction request built", "req", predictionReq)

    // Predict TPOT
    if prediction, err := d.latencyPredictor.Predict(ctx, predictionReq); err != nil {
		reqCtx.PredictedTPOTObservations = append(reqCtx.PredictedTPOTObservations, 0) // Append 0 if prediction fails
        logger.V(logutil.DEBUG).Error(err, "Latency prediction failed at body chunk stage")
    } else if prediction != nil {
        reqCtx.PredictedTPOTObservations = append(reqCtx.PredictedTPOTObservations, prediction.TPOT)
        logger.V(logutil.DEBUG).Info("Predicted TPOT at body chunk stage", "predicted_tpot_ms", prediction.TPOT)
    }

    // Add training data
    if isFirst {
        // TTFT sample
        reqCtx.TTFT = float64(now.Sub(reqCtx.RequestReceivedTimestamp).Milliseconds())
        reqCtx.LastTokenTimestamp = now
        entry := latencypredictor.TrainingEntry{
            KVCachePercentage:  reqCtx.LastSeenMetrics.KVCacheUsagePercent,
            InputTokenLength:   len(splitWords(reqCtx.Prompt)),
            ActualTTFT:         reqCtx.TTFT,
            ActualTPOT:         0,
            Timestamp:          now,
            NumRequestWaiting:  reqCtx.LastSeenMetrics.WaitingQueueSize,
            NumRequestRunning:  reqCtx.LastSeenMetrics.RunningQueueSize,
            NumTokensGenerated: 0,
        }
        logger.V(logutil.DEBUG).Info("Adding TTFT training entry", "entry", entry)
        if err := d.latencyPredictor.AddTrainingDataBulk([]latencypredictor.TrainingEntry{entry}); err != nil {
            logger.V(logutil.DEBUG).Error(err, "Failed to add TTFT training sample")
        } else {
            logger.V(logutil.DEBUG).Info("Successfully added TTFT training sample")
        }
    } else {
        // TPOT sample
		interTokenLatency := float64(now.Sub(reqCtx.LastTokenTimestamp).Milliseconds())
		logger.V(logutil.DEBUG).Info("Measured inter-token latency", "latency_ms", interTokenLatency)
		reqCtx.TPOTObservations = append(reqCtx.TPOTObservations, interTokenLatency)
		logger.V(logutil.DEBUG).Info("Appended actual TPOT observation", "value", interTokenLatency, "count", len(reqCtx.TPOTObservations))

        entry := latencypredictor.TrainingEntry{
            KVCachePercentage:  reqCtx.LastSeenMetrics.KVCacheUsagePercent,
            InputTokenLength:   len(splitWords(reqCtx.Prompt)),
            ActualTPOT:         interTokenLatency,
            ActualTTFT:         0,
            Timestamp:          now,
            NumRequestWaiting:  reqCtx.LastSeenMetrics.WaitingQueueSize,
            NumRequestRunning:  reqCtx.LastSeenMetrics.RunningQueueSize,
            NumTokensGenerated: len(reqCtx.TPOTObservations),
        }
        logger.V(logutil.DEBUG).Info("Adding TPOT training entry", "entry", entry)
        if err := d.latencyPredictor.AddTrainingDataBulk([]latencypredictor.TrainingEntry{entry}); err != nil {
            logger.V(logutil.DEBUG).Error(err, "Failed to add TPOT training sample")
        } else {
            logger.V(logutil.DEBUG).Info("Successfully added TPOT training sample")
        }
        reqCtx.LastTokenTimestamp = now
    }

    logger.V(logutil.DEBUG).Info("Exiting HandleResponseBodyChunk")
    return nil
}

// HandleResponseTrailers calculates final aggregate metrics and adds them to response trailers.
func (d *Director) HandleResponseTrailers(ctx context.Context, reqCtx *handlers.RequestContext) (*handlers.RequestContext, error) {
    logger := log.FromContext(ctx).WithValues("stage", "trailers")
    logger.V(logutil.DEBUG).Info("Entering HandleResponseTrailers")
    return reqCtx, nil
}


func (d *Director) GetRandomPod() *backend.Pod {
	pods := d.datastore.PodGetAll()
	if len(pods) == 0 {
		return nil
	}
	number := rand.Intn(len(pods))
	pod := pods[number]
	return pod.GetPod()
}

func RandomWeightedDraw(logger logr.Logger, model *v1alpha2.InferenceModel, seed int64) string {
	// TODO: after we are down to 1 server implementation, make these methods a part of the struct
	// and handle random seeding on the struct.
	source := rand.NewSource(rand.Int63())
	if seed > 0 {
		source = rand.NewSource(seed)
	}
	r := rand.New(source)

	// all the weight values are nil, then we should return random model name
	if model.Spec.TargetModels[0].Weight == nil {
		index := r.Int31n(int32(len(model.Spec.TargetModels)))
		return model.Spec.TargetModels[index].Name
	}

	var weights int32
	for _, model := range model.Spec.TargetModels {
		weights += *model.Weight
	}
	logger.V(logutil.DEBUG).Info("Weights for model computed", "model", model.Name, "weights", weights)
	randomVal := r.Int31n(weights)
	// TODO: optimize this without using loop
	for _, model := range model.Spec.TargetModels {
		if randomVal < *model.Weight {
			return model.Name
		}
		randomVal -= *model.Weight
	}
	return ""
}

func (d *Director) runPreRequestPlugins(ctx context.Context, request *schedulingtypes.LLMRequest, schedulingResult *schedulingtypes.SchedulingResult,
	targetPort int) {
	for _, plugin := range d.preRequestPlugins {
		log.FromContext(ctx).V(logutil.DEBUG).Info("Running pre-request plugin", "plugin", plugin.Name())
		before := time.Now()
		plugin.PreRequest(ctx, request, schedulingResult, targetPort)
		metrics.RecordRequestControlPluginProcessingLatency(PreRequestPluginType, plugin.Name(), time.Since(before))
	}
}

func (d *Director) runPostResponsePlugins(ctx context.Context, request *schedulingtypes.LLMRequest, response *Response, targetPod *backend.Pod) {
	for _, plugin := range d.postResponsePlugins {
		log.FromContext(ctx).V(logutil.DEBUG).Info("Running post-response plugin", "plugin", plugin.Name())
		before := time.Now()
		plugin.PostResponse(ctx, request, response, targetPod)
		metrics.RecordRequestControlPluginProcessingLatency(PostResponsePluginType, plugin.Name(), time.Since(before))
	}
}

func (d *Director) IsPredictorAvailable() bool {
    return d.latencyPredictor != nil
}