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
	"math"
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

// Predictor defines the interface required by the Director for latency prediction and training.
// The real *latencypredictor.Predictor satisfies this interface.
type Predictor interface {
	Predict(req latencypredictor.PredictionRequest) (*latencypredictor.PredictionResponse, error)
	AddTrainingDataBulk(entry []latencypredictor.TrainingEntry) error
}

// NewDirectorWithConfig creates a new Director instance with all dependencies.
// It accepts a pre-initialized latency predictor. The caller is responsible for creating
// and managing the lifecycle (Start/Stop) of the predictor.
func NewDirectorWithConfig(datastore datastore.Datastore, scheduler Scheduler, saturationDetector SaturationDetector, config *Config, predictor Predictor) *Director {
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
	latencyPredictor    Predictor
	preRequestPlugins   []PreRequest
	postResponsePlugins []PostResponse
}

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

// prepareRequest populates the RequestContext and predicts only the initial TTFT.
func (d *Director) prepareRequest(ctx context.Context, reqCtx *handlers.RequestContext, result *schedulingtypes.SchedulingResult) (*handlers.RequestContext, error) {
	logger := log.FromContext(ctx)
	if result == nil || len(result.ProfileResults) == 0 {
		return reqCtx, errutil.Error{Code: errutil.Internal, Msg: "results must be greater than zero"}
	}
	targetPod := result.ProfileResults[result.PrimaryProfileName].TargetPod.GetPod()
	// ... (endpoint creation logic is unchanged) ...
	pool, err := d.datastore.PoolGet()
	if err != nil {
		return reqCtx, err
	}
	targetPort := int(pool.Spec.TargetPortNumber)

	endpoint := net.JoinHostPort(targetPod.Address, strconv.Itoa(targetPort))
	logger.V(logutil.DEFAULT).Info("Request handled", "model", reqCtx.Model, "targetModel", reqCtx.ResolvedTargetModel, "endpoint", targetPod)

	reqCtx.TargetPod = targetPod
	reqCtx.TargetEndpoint = endpoint

	reqCtx.LastSeenMetrics = result.ProfileResults[result.PrimaryProfileName].TargetPod.GetMetrics()
	reqCtx.SchedulingResult = result

	// ===================================================================
	// == Latency Predictor Integration: Predict Initial TTFT
	// ===================================================================
	if d.latencyPredictor != nil {
		predictionReq := latencypredictor.PredictionRequest{
			KVCachePercentage: reqCtx.LastSeenMetrics.KVCacheUsagePercent,
			InputTokenLength:  len(splitWords(reqCtx.Prompt)),
			NumRequestWaiting: reqCtx.LastSeenMetrics.WaitingQueueSize,
			NumRequestRunning: reqCtx.LastSeenMetrics.RunningQueueSize,
			NumTokensGenerated: 0, // Initial prediction, no tokens generated yet		
		}

		prediction, err := d.latencyPredictor.Predict(predictionReq)
		if err != nil {
			logger.V(logutil.DEBUG).Error(err, "Latency prediction failed")
		} else if prediction != nil {
			// Only store the initial TTFT prediction. TPOT will be predicted per-chunk.
			reqCtx.PredictedTTFT = prediction.TTFT
			logger.V(logutil.TRACE).Info("Updated context with initial TTFT prediction",
				"predicted_ttft_ms", prediction.TTFT)
		}
	}
	// ===================================================================

	d.runPreRequestPlugins(ctx, reqCtx.SchedulingRequest, result, targetPort)
	return reqCtx, nil
}


// HandleResponseHeaders is called when the first chunk of the response arrives.
func (d *Director) HandleResponseHeaders(ctx context.Context, reqCtx *handlers.RequestContext) (*handlers.RequestContext, error){
	response := &Response{
		RequestId: reqCtx.Request.Headers[requtil.RequestIdHeaderKey],
		Headers:   reqCtx.Response.Headers,
	}

	d.runPostResponsePlugins(ctx, reqCtx.SchedulingRequest, response, reqCtx.TargetPod)


	if d.latencyPredictor == nil{
		return reqCtx, nil
	}
	
	now := time.Now()
	// This is our one-time measurement for Time To First Token.
	reqCtx.TTFT = float64(now.Sub(reqCtx.RequestReceivedTimestamp).Milliseconds())
	reqCtx.LastTokenTimestamp = now // Set the baseline for the first inter-token latency measurement.

	// Create a training entry specifically for the TTFT model.
	entry := latencypredictor.TrainingEntry{
		KVCachePercentage: reqCtx.LastSeenMetrics.KVCacheUsagePercent,
		InputTokenLength:  len(splitWords(reqCtx.Prompt)),
		ActualTTFT:        reqCtx.TTFT,
		Timestamp:         now,
		NumRequestWaiting: reqCtx.LastSeenMetrics.WaitingQueueSize,
		NumRequestRunning: reqCtx.LastSeenMetrics.RunningQueueSize,
		ActualTPOT:        0, // TPOT is not known yet, set
		NumTokensGenerated: 0, // No tokens generated yet, set to 0
	}

	if err := d.latencyPredictor.AddTrainingDataBulk([]latencypredictor.TrainingEntry{entry}); err != nil {
		log.FromContext(ctx).V(logutil.DEBUG).Error(err, "Failed to add TTFT training sample")
	}
	return reqCtx, nil
}

// HandleResponseBodyChunk is called for each streaming chunk. It now predicts and trains for each token.
func (d *Director) HandleResponseBodyChunk(ctx context.Context, reqCtx *handlers.RequestContext) error {
	if d.latencyPredictor == nil || reqCtx.TargetPod == nil {
		return nil
	}
	now := time.Now()
	interTokenLatency := float64(now.Sub(reqCtx.LastTokenTimestamp).Milliseconds())
	reqCtx.TPOTObservations = append(reqCtx.TPOTObservations, interTokenLatency)

	// --- Per-Chunk Prediction and Training ---
	// Create the prediction request using the initial state.
	predictionReq := latencypredictor.PredictionRequest{
		KVCachePercentage: reqCtx.LastSeenMetrics.KVCacheUsagePercent,
		InputTokenLength:  len(splitWords(reqCtx.Prompt)),
		NumRequestWaiting: reqCtx.LastSeenMetrics.WaitingQueueSize,
		NumRequestRunning: reqCtx.LastSeenMetrics.RunningQueueSize,
		NumTokensGenerated: len(reqCtx.TPOTObservations), // Use the current number of tokens generated
	}

	// Predict the latency for this specific upcoming token.
	prediction, err := d.latencyPredictor.Predict(predictionReq)
	if err == nil && prediction != nil {
		reqCtx.PredictedTPOTObservations = append(reqCtx.PredictedTPOTObservations, prediction.TPOT)
	} else {
		// Append a zero or placeholder if prediction fails, to keep lists in sync.
		reqCtx.PredictedTPOTObservations = append(reqCtx.PredictedTPOTObservations, 0)
	}

	// Create a training entry for this single token latency.
	entry := latencypredictor.TrainingEntry{
		KVCachePercentage: reqCtx.LastSeenMetrics.KVCacheUsagePercent,
		NumRequestWaiting: reqCtx.LastSeenMetrics.WaitingQueueSize,
		NumRequestRunning: reqCtx.LastSeenMetrics.RunningQueueSize,
		InputTokenLength:  len(splitWords(reqCtx.Prompt)),
		ActualTPOT:        interTokenLatency,
		ActualTTFT: 0,
		Timestamp:         now,
		NumTokensGenerated: len(reqCtx.TPOTObservations), // +1 for the current token
	}

	if err := d.latencyPredictor.AddTrainingDataBulk([]latencypredictor.TrainingEntry{entry}); err != nil {
		log.FromContext(ctx).V(logutil.DEBUG).Error(err, "Failed to add TPOT training sample")
	}

	reqCtx.LastTokenTimestamp = now
	return nil
}

// HandleResponseTrailers calculates final aggregate metrics and adds them to response trailers.
func (d *Director) HandleResponseTrailers(ctx context.Context, reqCtx *handlers.RequestContext) (*handlers.RequestContext, error) {
	if d.latencyPredictor != nil && len(reqCtx.TPOTObservations) > 0 {
		// --- Aggregate and Compare ---
		var sumActualTPOT, sumPredictedTPOT float64
		for _, tpot := range reqCtx.TPOTObservations {
			sumActualTPOT += tpot
		}
		for _, tpot := range reqCtx.PredictedTPOTObservations {
			sumPredictedTPOT += tpot
		}
		averageActualTPOT := sumActualTPOT / float64(len(reqCtx.TPOTObservations))
		averagePredictedTPOT := sumPredictedTPOT / float64(len(reqCtx.PredictedTPOTObservations))

		// --- Calculate MAPE ---
		mapeTTFT := 0.0
		if reqCtx.TTFT > 0 {
			mapeTTFT = math.Abs((reqCtx.TTFT-reqCtx.PredictedTTFT)/reqCtx.TTFT) * 100
		}
		
		// Element-wise MAPE for TPOT for higher accuracy
		var sumPercentageErrorTPOT float64
		errorCountTPOT := 0
		for i, actual := range reqCtx.TPOTObservations {
			if actual > 0 { // Avoid division by zero
				predicted := reqCtx.PredictedTPOTObservations[i]
				sumPercentageErrorTPOT += math.Abs((actual - predicted) / actual)
				errorCountTPOT++
			}
		}
		mapeTPOT := 0.0
		if errorCountTPOT > 0 {
			mapeTPOT = (sumPercentageErrorTPOT / float64(errorCountTPOT)) * 100
		}
		
		// --- Add Final Metrics to Response Trailers ---
		if reqCtx.Response.Headers == nil {
			reqCtx.Response.Headers = make(map[string]string)
		}
		reqCtx.Response.Headers["X-Actual-TTFT-Ms"] = fmt.Sprintf("%.2f", reqCtx.TTFT)
		reqCtx.Response.Headers["X-Predicted-TTFT-Ms"] = fmt.Sprintf("%.2f", reqCtx.PredictedTTFT)
		reqCtx.Response.Headers["X-MAPE-TTFT-Percent"] = fmt.Sprintf("%.2f", mapeTTFT)
		reqCtx.Response.Headers["X-Actual-Avg-TPOT-Ms"] = fmt.Sprintf("%.2f", averageActualTPOT)
		reqCtx.Response.Headers["X-Predicted-Avg-TPOT-Ms"] = fmt.Sprintf("%.2f", averagePredictedTPOT)
		reqCtx.Response.Headers["X-MAPE-TPOT-Percent"] = fmt.Sprintf("%.2f", mapeTPOT)

		log.FromContext(ctx).V(logutil.TRACE).Info("Final metrics calculated", "MAPE_TTFT", mapeTTFT, "MAPE_TPOT", mapeTPOT)
	}

	response := &Response{
		RequestId: reqCtx.Request.Headers[requtil.RequestIdHeaderKey],
		Headers:   reqCtx.Response.Headers,
	}
	d.runPostResponsePlugins(ctx, reqCtx.SchedulingRequest, response, reqCtx.TargetPod)

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
	logger.V(logutil.TRACE).Info("Weights for model computed", "model", model.Name, "weights", weights)
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