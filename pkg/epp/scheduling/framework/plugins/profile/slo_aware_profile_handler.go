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

package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
)

const (
	SLOAwareProfileHandlerType = "slo-aware-profile-handler"
	DefaultProfileName         = "default"
	SLOProfileName             = "slo"

	// Boolean header string for whether to use predictor based scheduling
	PreictionBasedSchedulingHeaderKey = "x-prediction-based-scheduling"
)

// compile-time type assertion
var _ framework.ProfileHandler = &SLOAwareProfileHandler{}

// SLOAwareProfileHandlerFactory defines the factory function for SLOAwareProfileHandler.
func SLOAwareProfileHandlerFactory(name string, _ json.RawMessage, _ plugins.Handle) (plugins.Plugin, error) {
	return NewSLOAwareProfileHandler().WithName(name), nil
}

// NewSLOAwareProfileHandler initializes a new SLOAwareProfileHandler and returns its pointer.
func NewSLOAwareProfileHandler() *SLOAwareProfileHandler {
	return &SLOAwareProfileHandler{
		typedName: plugins.TypedName{Type: SLOAwareProfileHandlerType, Name: SLOAwareProfileHandlerType},
	}
}

// SLOAwareProfileHandler handles two profiles: the default profile and the SLO profile.
// When the request has PredictorBasedScheduling=true, it uses the SLO profile result to select
// the destination pod. Otherwise, it uses the default profile result.
type SLOAwareProfileHandler struct {
	typedName plugins.TypedName
}

// TypedName returns the type and name tuple of this plugin instance.
func (h *SLOAwareProfileHandler) TypedName() plugins.TypedName {
	return h.typedName
}

// WithName sets the name of the profile handler.
func (h *SLOAwareProfileHandler) WithName(name string) *SLOAwareProfileHandler {
	h.typedName.Name = name
	return h
}

// Pick selects the SchedulingProfiles to run from the list of candidate profiles, while taking into consideration the request properties and the
// previously executed cycles along with their results.
func (h *SLOAwareProfileHandler) Pick(_ context.Context, _ *types.CycleState, request *types.LLMRequest, profiles map[string]*framework.SchedulerProfile,
	profileResults map[string]*types.ProfileRunResult) map[string]*framework.SchedulerProfile {
	if len(profiles) == len(profileResults) { // all profiles have been executed already in previous call
		return map[string]*framework.SchedulerProfile{}
	}
	// return all profiles
	return profiles
}

// ProcessResults handles the outcome of the profile runs after all profiles ran.
// It may aggregate results, log test profile outputs, or apply custom logic. It specifies in the SchedulingResult the
// key of the primary profile that should be used to get the request selected destination.
// When a profile run fails, its result in the profileResults map is nil.
func (h *SLOAwareProfileHandler) ProcessResults(ctx context.Context, _ *types.CycleState, request *types.LLMRequest, profileResults map[string]*types.ProfileRunResult) (*types.SchedulingResult, error) {

	if len(profileResults) < 2 {
		return nil, errors.New("SLOAwareProfileHandler requires at least two profiles to operate")
	}

	predictorBasedScheduling, err := parseBoolHeader(*request, PreictionBasedSchedulingHeaderKey)
	if err != nil {
		return nil, fmt.Errorf("error parsing predictorBasedScheduling from header failed to choose scheduling profile: x-prediction-based-scheduling must be a bool: %v", err)
	}

	if predictorBasedScheduling { // TODO grab header directly from request.Headers instead of request field
		if profileResults[SLOProfileName] == nil { // there was an error while running the SLO profile
			return nil, fmt.Errorf("failed to run scheduler profile '%s'", SLOProfileName)
		}
		return &types.SchedulingResult{
			ProfileResults:     profileResults,
			PrimaryProfileName: SLOProfileName,
		}, nil
	}

	if profileResults[DefaultProfileName] == nil { // there was an error while running the default profile
		return nil, fmt.Errorf("failed to run scheduler profile '%s'", DefaultProfileName)
	}

	return &types.SchedulingResult{
		ProfileResults:     profileResults,
		PrimaryProfileName: DefaultProfileName,
	}, nil
}

// parseFloatHeader retrieves a header by name, parses it as a bool,
// and returns the value or an error if the header is missing or invalid.
func parseBoolHeader(request types.LLMRequest, headerName string) (bool, error) {
	// 1. Get header value from the map
	headerValue, ok := request.Headers[headerName]
	if !ok {
		return false, nil // Header not found, return 0 and false
	}

	// 2. Parse the header value to a bool
	parsedBool, err := strconv.ParseBool(headerValue)
	if err != nil {
		return false, fmt.Errorf("must be a bool: %v", headerName)
	}

	// 3. Return the successfully parsed value
	return parsedBool, nil
}
