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

package requestcontrol

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client" // Added
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/gateway-api-inference-extension/api/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	modelsubsetsconfig "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/config" // Added
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/handlers"
	schedulingtypes "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
	errutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/error"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
	requtil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/request"
	testutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"
)

// --- Mocks ---

type mockSaturationDetector struct {
	isSaturated bool
}

func (m *mockSaturationDetector) IsSaturated(_ context.Context) bool {
	return m.isSaturated
}

type mockScheduler struct {
	scheduleResults *schedulingtypes.SchedulingResult
	scheduleErr     error
	CalledWithPods  []schedulingtypes.Pod // New field to store pods passed to Schedule
}

func (m *mockScheduler) Schedule(_ context.Context, _ *schedulingtypes.LLMRequest, candidatePods []schedulingtypes.Pod) (*schedulingtypes.SchedulingResult, error) {
	m.CalledWithPods = candidatePods // Store the pods
	return m.scheduleResults, m.scheduleErr
}

func TestDirector_HandleRequest(t *testing.T) {
	ctx := logutil.NewTestLoggerIntoContext(context.Background())

	// --- Setup common objects ---
	model := "food-review"
	modelSheddable := "food-review-sheddable"
	modelWithResolvedTarget := "food-review-resolve"

	// InferenceModel definitions
	imFoodReview := testutil.MakeInferenceModel("imFoodReview").
		CreationTimestamp(metav1.Unix(1000, 0)).
		ModelName(model).
		Criticality(v1alpha2.Critical).
		ObjRef()
	imFoodReviewSheddable := testutil.MakeInferenceModel("imFoodReviewSheddable").
		CreationTimestamp(metav1.Unix(1000, 0)).
		ModelName(modelSheddable).
		Criticality(v1alpha2.Sheddable).
		ObjRef()
	imFoodReviewResolve := testutil.MakeInferenceModel("imFoodReviewResolve").
		CreationTimestamp(metav1.Unix(1000, 0)).
		ModelName(modelWithResolvedTarget).
		Criticality(v1alpha2.Standard).
		TargetModel("resolved-target-model-A").
		ObjRef()

	// Datastore setup
	pmf := backendmetrics.NewPodMetricsFactory(&backendmetrics.FakePodMetricsClient{}, time.Second)
	ds := datastore.NewDatastore(t.Context(), pmf, nil) // Added nil
	ds.ModelSetIfOlder(imFoodReview)
	ds.ModelSetIfOlder(imFoodReviewResolve)
	ds.ModelSetIfOlder(imFoodReviewSheddable)

	pool := &v1alpha2.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
		Spec: v1alpha2.InferencePoolSpec{
			TargetPortNumber: int32(8000),
			Selector: map[v1alpha2.LabelKey]v1alpha2.LabelValue{
				"app": "inference",
			},
		},
	}

	// Pod setup
	testPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			Labels:    map[string]string{"app": "inference"},
		},
		Status: corev1.PodStatus{
			PodIP:      "192.168.1.100",
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	if err := ds.PoolSet(ctx, fakeClient, pool); err != nil {
		t.Fatalf("Error while setting inference pool: %v", err)
	}
	ds.PodUpdateOrAddIfNotExist(testPod)

	defaultSuccessfulScheduleResults := &schedulingtypes.SchedulingResult{
		ProfileResults: map[string]*schedulingtypes.ProfileRunResult{
			"testProfile": {
				TargetPod: &schedulingtypes.ScoredPod{
					Pod: &schedulingtypes.PodMetrics{
						Pod: &backend.Pod{
							Address:        "192.168.1.100",
							NamespacedName: k8stypes.NamespacedName{Name: "pod1", Namespace: "default"},
						},
					},
				},
			},
		},
		PrimaryProfileName: "testProfile",
	}

	tests := []struct {
		name                   string
		reqBodyMap             map[string]interface{}
		mockSaturationDetector *mockSaturationDetector
		schedulerMockSetup     func(m *mockScheduler)
		wantErrCode            string                   // Expected errutil code string
		wantReqCtx             *handlers.RequestContext // Fields to check in the returned RequestContext
		wantMutatedBodyModel   string                   // Expected model in reqCtx.Request.Body after PostDispatch
	}{
		{
			name: "successful completions request (critical, saturation ignored)",
			reqBodyMap: map[string]interface{}{
				"model":  model,
				"prompt": "critical prompt",
			},
			mockSaturationDetector: &mockSaturationDetector{isSaturated: true},
			schedulerMockSetup: func(m *mockScheduler) {
				m.scheduleResults = defaultSuccessfulScheduleResults
			},
			wantReqCtx: &handlers.RequestContext{
				Model:               model,
				ResolvedTargetModel: model,
				TargetPod: &backend.Pod{
					NamespacedName: types.NamespacedName{Namespace: "default", Name: "pod1"},
					Address:        "192.168.1.100",
				},
				TargetEndpoint: "192.168.1.100:8000",
			},
			wantMutatedBodyModel: model,
		},
		{
			name: "successful chat completions request (critical, saturation ignored)",
			reqBodyMap: map[string]interface{}{
				"model": model,
				"messages": []interface{}{
					map[string]interface{}{
						"role":    "user",
						"content": "critical prompt",
					},
				},
			},
			schedulerMockSetup: func(m *mockScheduler) {
				m.scheduleResults = defaultSuccessfulScheduleResults
			},
			wantReqCtx: &handlers.RequestContext{
				Model:               model,
				ResolvedTargetModel: model,
				TargetPod: &backend.Pod{
					NamespacedName: types.NamespacedName{Namespace: "default", Name: "pod1"},
					Address:        "192.168.1.100",
				},
				TargetEndpoint: "192.168.1.100:8000",
			},
			wantMutatedBodyModel: model,
		},
		{
			name: "successful chat completions request with multiple messages (critical, saturation ignored)",
			reqBodyMap: map[string]interface{}{
				"model": model,
				"messages": []interface{}{
					map[string]interface{}{
						"role":    "developer",
						"content": "You are a helpful assistant.",
					},
					map[string]interface{}{
						"role":    "user",
						"content": "Hello!",
					},
				},
			},
			schedulerMockSetup: func(m *mockScheduler) {
				m.scheduleResults = defaultSuccessfulScheduleResults
			},
			wantReqCtx: &handlers.RequestContext{
				Model:               model,
				ResolvedTargetModel: model,
				TargetPod: &backend.Pod{
					NamespacedName: types.NamespacedName{Namespace: "default", Name: "pod1"},
					Address:        "192.168.1.100",
				},
				TargetEndpoint: "192.168.1.100:8000",
			},
			wantMutatedBodyModel: model,
		},
		{
			name: "successful completions request (sheddable, not saturated)",
			reqBodyMap: map[string]interface{}{
				"model":  modelSheddable,
				"prompt": "sheddable prompt",
			},
			mockSaturationDetector: &mockSaturationDetector{isSaturated: false},
			schedulerMockSetup: func(m *mockScheduler) {
				m.scheduleResults = defaultSuccessfulScheduleResults
			},
			wantReqCtx: &handlers.RequestContext{
				Model:               modelSheddable,
				ResolvedTargetModel: modelSheddable,
				TargetPod: &backend.Pod{
					NamespacedName: types.NamespacedName{Namespace: "default", Name: "pod1"},
					Address:        "192.168.1.100",
				},
				TargetEndpoint: "192.168.1.100:8000",
			},
			wantMutatedBodyModel: modelSheddable,
		},
		{
			name: "successful request with target model resolution",
			reqBodyMap: map[string]interface{}{
				"model":  modelWithResolvedTarget,
				"prompt": "prompt for target resolution",
			},
			mockSaturationDetector: &mockSaturationDetector{isSaturated: false},
			schedulerMockSetup: func(m *mockScheduler) {
				m.scheduleResults = defaultSuccessfulScheduleResults
			},
			wantReqCtx: &handlers.RequestContext{
				Model:               modelWithResolvedTarget,
				ResolvedTargetModel: "resolved-target-model-A",
				TargetPod: &backend.Pod{
					NamespacedName: types.NamespacedName{Namespace: "default", Name: "pod1"},
					Address:        "192.168.1.100",
				},
				TargetEndpoint: "192.168.1.100:8000",
			},
			wantMutatedBodyModel: "resolved-target-model-A",
		},
		{
			name: "nonexistent target defined, use default inference model",
			schedulerMockSetup: func(m *mockScheduler) {
				m.scheduleResults = defaultSuccessfulScheduleResults
			},
			wantReqCtx: &handlers.RequestContext{
				Model:               "food-review-1",
				ResolvedTargetModel: "food-review-1",
				TargetPod: &backend.Pod{
					NamespacedName: types.NamespacedName{Namespace: "default", Name: "pod1"},
					Address:        "192.168.1.100",
				},
				TargetEndpoint: "192.168.1.100:8000",
			},
			wantMutatedBodyModel: "food-review-1",
			reqBodyMap: map[string]interface{}{
				"model":  "food-review-1",
				"prompt": "test prompt",
			},
			mockSaturationDetector: &mockSaturationDetector{isSaturated: false},
		},
		{

			name: "request dropped (sheddable, saturated)",
			reqBodyMap: map[string]interface{}{
				"model":  modelSheddable,
				"prompt": "sheddable prompt",
			},
			mockSaturationDetector: &mockSaturationDetector{isSaturated: true},
			wantErrCode:            errutil.InferencePoolResourceExhausted,
		},
		{
			name:                   "model not found, expect err",
			reqBodyMap:             map[string]interface{}{"prompt": "p"},
			mockSaturationDetector: &mockSaturationDetector{isSaturated: false},
			wantErrCode:            errutil.BadRequest,
		},

		{
			name:        "prompt or messages not found, expect err",
			reqBodyMap:  map[string]interface{}{"model": model},
			wantErrCode: errutil.BadRequest,
		},
		{
			name: "empty messages, expect err",
			reqBodyMap: map[string]interface{}{
				"model":    model,
				"messages": []interface{}{},
			},
			wantErrCode: errutil.BadRequest,
		},
		{
			name: "scheduler returns error",
			reqBodyMap: map[string]interface{}{
				"model":  model,
				"prompt": "prompt that causes scheduler error",
			},
			schedulerMockSetup: func(m *mockScheduler) {
				m.scheduleErr = errors.New("simulated scheduler failure")
			},
			wantErrCode: errutil.InferencePoolResourceExhausted,
		},
		{
			name: "scheduler returns nil result and nil error",
			reqBodyMap: map[string]interface{}{
				"model":  model,
				"prompt": "prompt for nil,nil scheduler return",
			},
			schedulerMockSetup: func(m *mockScheduler) {
				m.scheduleResults = nil
				m.scheduleErr = nil
			},
			wantErrCode: errutil.Internal,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mockSched := &mockScheduler{}
			if test.schedulerMockSetup != nil {
				test.schedulerMockSetup(mockSched)
			}
			director := NewDirectorWithConfig(ds, mockSched, test.mockSaturationDetector, NewConfig())

			reqCtx := &handlers.RequestContext{
				Request: &handlers.Request{
					// Create a copy of the map for each test run to avoid mutation issues.
					Body: make(map[string]interface{}),
					Headers: map[string]string{
						requtil.RequestIdHeaderKey: "test-req-id-" + test.name, // Ensure a default request ID
					},
				},
			}
			// Deep copy the body map.
			for k, v := range test.reqBodyMap {
				reqCtx.Request.Body[k] = v
			}

			returnedReqCtx, err := director.HandleRequest(ctx, reqCtx)

			if test.wantErrCode != "" {
				assert.Error(t, err, "HandleRequest() should have returned an error")
				var e errutil.Error
				if assert.ErrorAs(t, err, &e, "Error should be of type errutil.Error") {
					assert.Equal(t, test.wantErrCode, e.Code, "Error code mismatch")
				}
				return
			}

			assert.NoError(t, err, "HandleRequest() returned unexpected error")

			if test.wantReqCtx != nil {
				assert.Equal(t, test.wantReqCtx.Model, returnedReqCtx.Model, "reqCtx.Model mismatch")
				assert.Equal(t, test.wantReqCtx.ResolvedTargetModel, returnedReqCtx.ResolvedTargetModel,
					"reqCtx.ResolvedTargetModel mismatch")
				assert.Equal(t, test.wantReqCtx.TargetPod, returnedReqCtx.TargetPod, "reqCtx.TargetPod mismatch")
				assert.Equal(t, test.wantReqCtx.TargetEndpoint, returnedReqCtx.TargetEndpoint, "reqCtx.TargetEndpoint mismatch")
			}

			if test.wantMutatedBodyModel != "" {
				assert.NotNil(t, returnedReqCtx.Request.Body, "Expected mutated body, but reqCtx.Request.Body is nil")
				assert.Equal(t, test.wantMutatedBodyModel, returnedReqCtx.Request.Body["model"],
					"Mutated reqCtx.Request.Body model mismatch")
			}
		})
	}
}

func TestRandomWeightedDraw(t *testing.T) {
	logger := logutil.NewTestLogger()
	// Note: These tests verify deterministic outcomes for a fixed seed (420).
	// They do not test the statistical properties of the random draw.
	tests := []struct {
		name  string
		model *v1alpha2.InferenceModel
		want  string
	}{
		{
			name: "deterministic draw: 50/50 weights, seed 420",
			model: &v1alpha2.InferenceModel{
				Spec: v1alpha2.InferenceModelSpec{
					TargetModels: []v1alpha2.TargetModel{
						{Name: "canary", Weight: pointer(50)},
						{Name: "v1", Weight: pointer(50)},
					},
				},
			},
			want: "canary",
		},
		{
			name: "deterministic draw: 25/55/50 weights, seed 420",
			model: &v1alpha2.InferenceModel{
				Spec: v1alpha2.InferenceModelSpec{
					TargetModels: []v1alpha2.TargetModel{
						{Name: "canary", Weight: pointer(25)},
						{Name: "v1.1", Weight: pointer(55)},
						{Name: "v1", Weight: pointer(50)},
					},
				},
			},
			want: "v1",
		},
		{
			name: "deterministic draw: 20/20/10 weights, seed 420",
			model: &v1alpha2.InferenceModel{
				Spec: v1alpha2.InferenceModelSpec{
					TargetModels: []v1alpha2.TargetModel{
						{Name: "canary", Weight: pointer(20)},
						{Name: "v1.1", Weight: pointer(20)},
						{Name: "v1", Weight: pointer(10)},
					},
				},
			},
			want: "v1.1",
		},
		{
			name: "deterministic draw: nil weights (uniform), seed 420",
			model: &v1alpha2.InferenceModel{
				Spec: v1alpha2.InferenceModelSpec{
					TargetModels: []v1alpha2.TargetModel{
						{Name: "canary"},
						{Name: "v1.1"},
						{Name: "v1"},
					},
				},
			},
			want: "canary",
		},
	}
	var seedVal int64 = 420
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := RandomWeightedDraw(logger, test.model, seedVal)
			assert.Equal(t, test.want, model, "RandomWeightedDraw() with seed %d should produce expected model", seedVal)
		})
	}
}

func TestGetRandomPod(t *testing.T) {
	tests := []struct {
		name      string
		storePods []*corev1.Pod
		expectNil bool
	}{
		{
			name:      "No pods available",
			storePods: []*corev1.Pod{},
			expectNil: true,
		},
		{
			name: "Single pod available",
			storePods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
			},
			expectNil: false,
		},
		{
			name: "Multiple pods available",
			storePods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3"}},
			},
			expectNil: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pmf := backendmetrics.NewPodMetricsFactory(&backendmetrics.FakePodMetricsClient{}, time.Millisecond)
			ds := datastore.NewDatastore(t.Context(), pmf, nil) // Added nil
			for _, pod := range test.storePods {
				ds.PodUpdateOrAddIfNotExist(pod)
			}
			d := &Director{datastore: ds}
			gotPod := d.GetRandomPod()

			if test.expectNil && gotPod != nil {
				t.Errorf("expected nil pod, got: %v", gotPod)
			}
			if !test.expectNil && gotPod == nil {
				t.Errorf("expected non-nil pod, got nil")
			}
		})
	}
}

func pointer(v int32) *int32 {
	return &v
}

func TestDirector_HandleResponse(t *testing.T) {
	pr1 := &testPostResponse{
		TypeRes: "pr1",
	}

	ctx := logutil.NewTestLoggerIntoContext(context.Background())
	// For pmf, using nil is okay if the datastore methods used by HandleResponse don't rely on it.
	// Or, initialize a real one if specific datastore interactions are tested here.
	// Given HandleResponse's current scope, a simple datastore might be enough.
	ds := datastore.NewDatastore(t.Context(), nil, nil) // Added nil for modelSubsetsConfig
	mockSched := &mockScheduler{}
	director := NewDirectorWithConfig(ds, mockSched, nil, NewConfig().WithPostResponsePlugins(pr1))

	reqCtx := &handlers.RequestContext{
		Request: &handlers.Request{
			Headers: map[string]string{
				requtil.RequestIdHeaderKey: "test-req-id-for-response",
			},
		},
		Response: &handlers.Response{ // Simulate some response headers
			Headers: map[string]string{"X-Test-Response-Header": "TestValue"},
		},

		TargetPod: &backend.Pod{NamespacedName: types.NamespacedName{Namespace: "namespace1", Name: "test-pod-name"}},
	}

	_, err := director.HandleResponse(ctx, reqCtx)
	if err != nil {
		t.Fatalf("HandleResponse() returned unexpected error: %v", err)
	}

	if diff := cmp.Diff("test-req-id-for-response", pr1.lastRespOnResponse.RequestId); diff != "" {
		t.Errorf("Scheduler.OnResponse RequestId mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(reqCtx.Response.Headers, pr1.lastRespOnResponse.Headers); diff != "" {
		t.Errorf("Scheduler.OnResponse Headers mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("namespace1/test-pod-name", pr1.lastTargetPodOnResponse); diff != "" {
		t.Errorf("Scheduler.OnResponse TargetPodName mismatch (-want +got):\n%s", diff)
	}
}

type testPostResponse struct {
	TypeRes                 string
	lastRespOnResponse      *Response
	lastTargetPodOnResponse string
}

func (p *testPostResponse) Type() string { return p.TypeRes }
func (p *testPostResponse) Name() string { return "test-post-response" }

func (p *testPostResponse) PostResponse(_ context.Context, _ *schedulingtypes.LLMRequest, response *Response, targetPod *backend.Pod) {
	p.lastRespOnResponse = response
	p.lastTargetPodOnResponse = targetPod.NamespacedName.String()
}

// --- Start of tests for applyModelSubsetFilter ---
func TestApplyModelSubsetFilter(t *testing.T) {
	logger := logutil.NewTestLogger()
	podMetrics := func(name string, labels map[string]string) backendmetrics.PodMetrics {
		// Simulate what happens during PodMetrics creation: corev1.Pod -> backend.Pod
		// The backend.Pod struct directly holds the labels.
		bePod := &backend.Pod{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
			Address:        "1.2.3.4", // Dummy address, not used by this filter
			Labels:         labels,
		}
		return &backendmetrics.FakePodMetrics{ // Use the fake from backendmetrics
			Pod:     bePod,
			Metrics: &backendmetrics.MetricsState{},
		}
	}

	allPods := []backendmetrics.PodMetrics{
		podMetrics("pod1-group-a", map[string]string{"app": "nginx", "group": "a"}),
		podMetrics("pod2-group-a", map[string]string{"app": "nginx", "group": "a", "version": "v1"}),
		podMetrics("pod3-group-b", map[string]string{"app": "nginx", "group": "b"}),
		podMetrics("pod4-no-group", map[string]string{"app": "nginx"}),
		podMetrics("pod5-other-app", map[string]string{"app": "apache", "group": "a"}),
	}

	tests := []struct {
		name          string
		allPods       []backendmetrics.PodMetrics
		selector      *modelsubsetsconfig.PodSelector
		expectedNames []string // names of pods expected to be in the result
	}{
		{
			name:    "select group a",
			allPods: allPods,
			selector: &modelsubsetsconfig.PodSelector{
				MatchLabels: map[string]string{"group": "a"},
			},
			expectedNames: []string{"pod1-group-a", "pod2-group-a", "pod5-other-app"},
		},
		{
			name:    "select group a and app nginx",
			allPods: allPods,
			selector: &modelsubsetsconfig.PodSelector{
				MatchLabels: map[string]string{"group": "a", "app": "nginx"},
			},
			expectedNames: []string{"pod1-group-a", "pod2-group-a"},
		},
		{
			name:    "select group b",
			allPods: allPods,
			selector: &modelsubsetsconfig.PodSelector{
				MatchLabels: map[string]string{"group": "b"},
			},
			expectedNames: []string{"pod3-group-b"},
		},
		{
			name:    "select by version v1 (only one pod)",
			allPods: allPods,
			selector: &modelsubsetsconfig.PodSelector{
				MatchLabels: map[string]string{"version": "v1"},
			},
			expectedNames: []string{"pod2-group-a"},
		},
		{
			name:    "selector matches nothing (non-existent label value)",
			allPods: allPods,
			selector: &modelsubsetsconfig.PodSelector{
				MatchLabels: map[string]string{"group": "c"},
			},
			expectedNames: []string{},
		},
		{
			name:    "selector matches nothing (non-existent label key)",
			allPods: allPods,
			selector: &modelsubsetsconfig.PodSelector{
				MatchLabels: map[string]string{"tier": "backend"},
			},
			expectedNames: []string{},
		},
		{
			name:    "empty matchLabels selector",
			allPods: allPods,
			selector: &modelsubsetsconfig.PodSelector{
				MatchLabels: map[string]string{},
			},
			expectedNames: []string{"pod1-group-a", "pod2-group-a", "pod3-group-b", "pod4-no-group", "pod5-other-app"}, // returns all
		},
		{
			name:          "nil matchLabels selector",
			allPods:       allPods,
			selector:      &modelsubsetsconfig.PodSelector{MatchLabels: nil},
			expectedNames: []string{"pod1-group-a", "pod2-group-a", "pod3-group-b", "pod4-no-group", "pod5-other-app"}, // returns all
		},
		{
			name:          "nil selector",
			allPods:       allPods,
			selector:      nil,
			expectedNames: []string{"pod1-group-a", "pod2-group-a", "pod3-group-b", "pod4-no-group", "pod5-other-app"}, // returns all
		},
		{
			name:          "empty input pods list",
			allPods:       []backendmetrics.PodMetrics{},
			selector:      &modelsubsetsconfig.PodSelector{MatchLabels: map[string]string{"group": "a"}},
			expectedNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := applyModelSubsetFilter(tt.allPods, tt.selector, logger)
			var gotNames []string
			for _, p := range filtered {
				gotNames = append(gotNames, p.GetPod().NamespacedName.Name)
			}
			assert.ElementsMatch(t, tt.expectedNames, gotNames, "Filtered pod names do not match expected names")
		})
	}
}

// --- End of tests for applyModelSubsetFilter ---

// --- Mock Datastore for Director tests ---
type mockDirectorDatastore struct {
	// Explicitly list fields needed for mocking specific behaviors for these tests
	podsToReturn        []backendmetrics.PodMetrics
	modelSubsetToReturn *modelsubsetsconfig.ModelSubset
	hasModelSubset      bool
	poolToReturn        *v1alpha2.InferencePool
	poolError           error
	modelToReturn       *v1alpha2.InferenceModel
}

// Ensure mockDirectorDatastore implements datastore.Datastore
var _ datastore.Datastore = &mockDirectorDatastore{}

func (m *mockDirectorDatastore) PodGetAll() []backendmetrics.PodMetrics {
	return m.podsToReturn
}

func (m *mockDirectorDatastore) GetModelSubsetConfig(modelName string) (*modelsubsetsconfig.ModelSubset, bool) {
	if m.modelSubsetToReturn != nil && m.modelSubsetToReturn.ModelName == modelName {
		return m.modelSubsetToReturn, m.hasModelSubset
	}
	// Allow a wildcard modelName in the mock setup for tests that don't care about specific model matching
	if m.modelSubsetToReturn != nil && m.modelSubsetToReturn.ModelName == "" && m.hasModelSubset {
		return m.modelSubsetToReturn, m.hasModelSubset
	}
	return nil, false
}

func (m *mockDirectorDatastore) PoolGet() (*v1alpha2.InferencePool, error) {
	return m.poolToReturn, m.poolError
}

func (m *mockDirectorDatastore) ModelGet(modelName string) *v1alpha2.InferenceModel {
	if m.modelToReturn != nil && m.modelToReturn.Spec.ModelName == modelName {
		return m.modelToReturn
	}
	// Default behavior for tests that might call ModelGet but don't care about a specific pre-set model
	return &v1alpha2.InferenceModel{Spec: v1alpha2.InferenceModelSpec{ModelName: modelName, Criticality: func(c v1alpha2.Criticality) *v1alpha2.Criticality { return &c }(v1alpha2.Standard)}}
}

// Implement other Datastore interface methods with minimal logic (panic or return defaults)
func (m *mockDirectorDatastore) PoolSet(ctx context.Context, client k8sclient.Client, pool *v1alpha2.InferencePool) error {
	panic("PoolSet not implemented on mockDirectorDatastore")
}
func (m *mockDirectorDatastore) PoolHasSynced() bool {
	return m.poolToReturn != nil // Simple mock: synced if pool is set
}
func (m *mockDirectorDatastore) PoolLabelsMatch(podLabels map[string]string) bool {
	panic("PoolLabelsMatch not implemented on mockDirectorDatastore")
}
func (m *mockDirectorDatastore) ModelSetIfOlder(infModel *v1alpha2.InferenceModel) bool {
	panic("ModelSetIfOlder not implemented on mockDirectorDatastore")
}
func (m *mockDirectorDatastore) ModelDelete(namespacedName types.NamespacedName) *v1alpha2.InferenceModel {
	panic("ModelDelete not implemented on mockDirectorDatastore")
}
func (m *mockDirectorDatastore) ModelResync(ctx context.Context, client k8sclient.Client, modelName string) (bool, error) {
	panic("ModelResync not implemented on mockDirectorDatastore")
}
func (m *mockDirectorDatastore) ModelGetAll() []*v1alpha2.InferenceModel {
	if m.modelToReturn != nil {
		return []*v1alpha2.InferenceModel{m.modelToReturn}
	}
	return []*v1alpha2.InferenceModel{}
}
func (m *mockDirectorDatastore) PodList(predicate func(backendmetrics.PodMetrics) bool) []backendmetrics.PodMetrics {
	panic("PodList not implemented on mockDirectorDatastore")
}
func (m *mockDirectorDatastore) PodUpdateOrAddIfNotExist(pod *corev1.Pod) bool {
	panic("PodUpdateOrAddIfNotExist not implemented on mockDirectorDatastore")
}
func (m *mockDirectorDatastore) PodDelete(namespacedName types.NamespacedName) {
	panic("PodDelete not implemented on mockDirectorDatastore")
}
func (m *mockDirectorDatastore) Clear() {
	panic("Clear not implemented on mockDirectorDatastore")
}

// --- Start of TestDirector_HandleRequest_ModelSubsetting ---
func TestDirector_HandleRequest_ModelSubsetting(t *testing.T) {
	ctx := logutil.NewTestLoggerIntoContext(context.Background())

	// Helper to create PodMetrics with specific names and labels
	makePodMetrics := func(name string, labels map[string]string) backendmetrics.PodMetrics {
		// Simulate what happens during PodMetrics creation: corev1.Pod -> backend.Pod
		// The backend.Pod struct directly holds the labels.
		bePod := &backend.Pod{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
			Address:        "1.2.3.4", // Dummy address
			Labels:         labels,
		}
		return &backendmetrics.FakePodMetrics{ // Use the fake from backendmetrics
			Pod:     bePod,
			Metrics: &backendmetrics.MetricsState{},
		}
	}

	allMockPods := []backendmetrics.PodMetrics{
		makePodMetrics("pod-A1", map[string]string{"group": "A"}),
		makePodMetrics("pod-A2", map[string]string{"group": "A"}),
		makePodMetrics("pod-B1", map[string]string{"group": "B"}),
	}

	defaultPool := &v1alpha2.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
		Spec: v1alpha2.InferencePoolSpec{TargetPortNumber: 8000},
	}

	successfulScheduleResult := &schedulingtypes.SchedulingResult{
		ProfileResults: map[string]*schedulingtypes.ProfileRunResult{"default": {
			TargetPod: &schedulingtypes.ScoredPod{Pod: schedulingtypes.ToSchedulerPodMetrics(allMockPods)[0]}, // Just pick the first for mock
		}},
		PrimaryProfileName: "default",
	}


	tests := []struct {
		name                    string
		modelName               string
		mockDatastore           *mockDirectorDatastore
		mockScheduler           *mockScheduler
		expectedSchedulerPodNames []string // Names of pods expected to be passed to scheduler
		wantErrMsgContains      string   // Substring of expected error message
	}{
		{
			name:      "model with subset A, scheduler sees only group A pods",
			modelName: "modelX",
			mockDatastore: &mockDirectorDatastore{
				podsToReturn: allMockPods,
				modelSubsetToReturn: &modelsubsetsconfig.ModelSubset{
					ModelName:   "modelX",
					PodSelector: &modelsubsetsconfig.PodSelector{MatchLabels: map[string]string{"group": "A"}},
				},
				hasModelSubset: true,
				poolToReturn: defaultPool,
				modelToReturn: &v1alpha2.InferenceModel{Spec: v1alpha2.InferenceModelSpec{ModelName: "modelX"}},
			},
			mockScheduler:           &mockScheduler{scheduleResults: successfulScheduleResult},
			expectedSchedulerPodNames: []string{"pod-A1", "pod-A2"},
		},
		{
			name:      "model with subset B, scheduler sees only group B pods",
			modelName: "modelY",
			mockDatastore: &mockDirectorDatastore{
				podsToReturn: allMockPods,
				modelSubsetToReturn: &modelsubsetsconfig.ModelSubset{
					ModelName:   "modelY",
					PodSelector: &modelsubsetsconfig.PodSelector{MatchLabels: map[string]string{"group": "B"}},
				},
				hasModelSubset: true,
				poolToReturn: defaultPool,
				modelToReturn: &v1alpha2.InferenceModel{Spec: v1alpha2.InferenceModelSpec{ModelName: "modelY"}},
			},
			mockScheduler:           &mockScheduler{scheduleResults: successfulScheduleResult},
			expectedSchedulerPodNames: []string{"pod-B1"},
		},
		{
			name:      "model with no specific subset, scheduler sees all pods",
			modelName: "modelZ",
			mockDatastore: &mockDirectorDatastore{ // GetModelSubsetConfig will return (nil, false)
				podsToReturn: allMockPods,
				hasModelSubset: false, // This ensures GetModelSubsetConfig returns false
				poolToReturn: defaultPool,
				modelToReturn: &v1alpha2.InferenceModel{Spec: v1alpha2.InferenceModelSpec{ModelName: "modelZ"}},
			},
			mockScheduler:           &mockScheduler{scheduleResults: successfulScheduleResult},
			expectedSchedulerPodNames: []string{"pod-A1", "pod-A2", "pod-B1"},
		},
		{
			name:      "model with subset matching no pods, error before scheduling",
			modelName: "modelW",
			mockDatastore: &mockDirectorDatastore{
				podsToReturn: allMockPods,
				modelSubsetToReturn: &modelsubsetsconfig.ModelSubset{
					ModelName:   "modelW",
					PodSelector: &modelsubsetsconfig.PodSelector{MatchLabels: map[string]string{"group": "C"}}, // No pod has group C
				},
				hasModelSubset: true,
				poolToReturn: defaultPool,
				modelToReturn: &v1alpha2.InferenceModel{Spec: v1alpha2.InferenceModelSpec{ModelName: "modelW"}},
			},
			mockScheduler:      &mockScheduler{}, // Scheduler won't be called
			wantErrMsgContains: "no pods available for model modelW with specified subset criteria",
		},
		{
			name:      "no pods in pool, no subset defined, error before scheduling",
			modelName: "modelV",
			mockDatastore: &mockDirectorDatastore{
				podsToReturn: []backendmetrics.PodMetrics{}, // No pods at all
				hasModelSubset: false,
				poolToReturn: defaultPool,
				modelToReturn: &v1alpha2.InferenceModel{Spec: v1alpha2.InferenceModelSpec{ModelName: "modelV"}},
			},
			mockScheduler:      &mockScheduler{},
			wantErrMsgContains: "no pods available in pool for model modelV",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure the mockScheduler for this test iteration is clean for capturing CalledWithPods
			currentMockScheduler := &mockScheduler{
				scheduleResults: tt.mockScheduler.scheduleResults, // Preserve scheduled results/error if needed by test logic
				scheduleErr:     tt.mockScheduler.scheduleErr,
			}

			director := NewDirectorWithConfig(tt.mockDatastore, currentMockScheduler, &mockSaturationDetector{isSaturated: false}, NewConfig())
			reqCtx := &handlers.RequestContext{
				Request: &handlers.Request{
					Body:    map[string]interface{}{"model": tt.modelName, "prompt": "test"},
					Headers: map[string]string{requtil.RequestIdHeaderKey: "test-id"},
				},
			}

			_, err := director.HandleRequest(ctx, reqCtx)

			if tt.wantErrMsgContains != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrMsgContains)
			} else {
				assert.NoError(t, err)
				var schedulerCalledWithPodNames []string
				for _, p := range currentMockScheduler.CalledWithPods {
					schedulerCalledWithPodNames = append(schedulerCalledWithPodNames, p.GetPod().NamespacedName.Name)
				}
				assert.ElementsMatch(t, tt.expectedSchedulerPodNames, schedulerCalledWithPodNames, "Scheduler was called with incorrect set of pod names")
			}
		})
	}
}
// --- End of TestDirector_HandleRequest_ModelSubsetting ---
