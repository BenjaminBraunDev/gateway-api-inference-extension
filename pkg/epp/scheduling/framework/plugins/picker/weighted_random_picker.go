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

package picker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

const (
	WeightedRandomPickerType = "weighted-random-picker"
)

// compile-time type validation
var _ framework.Picker = &WeightedRandomPicker{}

// WeightedRandomPickerFactory is the factory function for the WeightedRandomPicker.
func WeightedRandomPickerFactory(name string, rawParameters json.RawMessage, _ plugins.Handle) (plugins.Plugin, error) {
	parameters := pickerParameters{MaxNumOfEndpoints: DefaultMaxNumOfEndpoints}
	if rawParameters != nil {
		if err := json.Unmarshal(rawParameters, &parameters); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of the '%s' picker - %w", WeightedRandomPickerType, err)
		}
	}

	return NewWeightedRandomPicker(parameters.MaxNumOfEndpoints).WithName(name), nil
}

// NewWeightedRandomPicker initializes a new WeightedRandomPicker and returns its pointer.
func NewWeightedRandomPicker(maxNumOfEndpoints int) *WeightedRandomPicker {
	if maxNumOfEndpoints <= 0 {
		maxNumOfEndpoints = DefaultMaxNumOfEndpoints // on invalid configuration value, fallback to default value
	}

	return &WeightedRandomPicker{
		typedName:         plugins.TypedName{Type: WeightedRandomPickerType, Name: WeightedRandomPickerType},
		maxNumOfEndpoints: maxNumOfEndpoints,
	}
}

// WeightedRandomPicker selects pods using a weighted random algorithm.
// Pods with higher scores have a proportionally higher chance of being selected first.
type WeightedRandomPicker struct {
	typedName         plugins.TypedName
	maxNumOfEndpoints int
}

// WithName sets the name of the picker.
func (p *WeightedRandomPicker) WithName(name string) *WeightedRandomPicker {
	p.typedName.Name = name
	return p
}

// TypedName returns the type and name tuple of this plugin instance.
func (p *WeightedRandomPicker) TypedName() plugins.TypedName {
	return p.typedName
}

// podWithRandScore is an intermediate struct to hold a pod and its temporary
// randomized score, which is used only for sorting.
type podWithRandScore struct {
	pod       *types.ScoredPod
	randScore float64
}

// Pick selects pods using a weighted random algorithm (Efraim's method).
// Pods with higher scores have a proportionally higher chance of being selected first.
func (p *WeightedRandomPicker) Pick(ctx context.Context, _ *types.CycleState, scoredPods []*types.ScoredPod) *types.ProfileRunResult {
	log.FromContext(ctx).V(logutil.DEBUG).Info(fmt.Sprintf("Selecting maximum '%d' pods from %d candidates using weighted random priority: %+v", p.maxNumOfEndpoints,
		len(scoredPods), scoredPods))

	// Handle edge case where there are no pods to choose from.
	if len(scoredPods) == 0 {
		return &types.ProfileRunResult{TargetPods: []types.Pod{}}
	}

	// Rand package is not safe for concurrent use, so we create a new instance for each pick.
	randomGenerator := rand.New(rand.NewSource(time.Now().UnixNano()))

	podsForSorting := make([]podWithRandScore, 0, len(scoredPods))

	for _, pod := range scoredPods {
		if pod.Score <= 0 {
			podsForSorting = append(podsForSorting, podWithRandScore{pod: pod, randScore: 0.0})
			continue
		}

		r := randomGenerator.Float64()
		randomizedScore := math.Pow(r, 1.0/pod.Score)
		podsForSorting = append(podsForSorting, podWithRandScore{pod: pod, randScore: randomizedScore})
	}

	// Sort the pods in DESCENDING order based on their new randomized score.
	slices.SortFunc(podsForSorting, func(a, b podWithRandScore) int {
		if a.randScore > b.randScore {
			return -1
		}
		if a.randScore < b.randScore {
			return 1
		}
		return 0
	})

	if p.maxNumOfEndpoints < len(podsForSorting) {
		podsForSorting = podsForSorting[:p.maxNumOfEndpoints]
	}

	targetPods := make([]types.Pod, len(podsForSorting))
	for i, pwr := range podsForSorting {
		targetPods[i] = pwr.pod
	}

	return &types.ProfileRunResult{TargetPods: targetPods}
}
