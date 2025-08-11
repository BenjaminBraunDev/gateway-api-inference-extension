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

package runner

import (
	"context"
	"flag"
	"fmt"
	"net/http/pprof"
	"os"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	conformance_epp "sigs.k8s.io/gateway-api-inference-extension/conformance/testing-epp"
	"sigs.k8s.io/gateway-api-inference-extension/internal/runnable"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/common/config/loader"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	latencypredictor "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/latencypredictorasync"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics/collectors"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/requestcontrol"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/requestcontrol/plugins/slorequest"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/multi/prefix"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/picker"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/profile"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/scorer"
	runserver "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/server"
	envutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/env"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

var (
	grpcPort = flag.Int(
		"grpcPort",
		runserver.DefaultGrpcPort,
		"The gRPC port used for communicating with Envoy proxy")
	grpcHealthPort = flag.Int(
		"grpcHealthPort",
		runserver.DefaultGrpcHealthPort,
		"The port used for gRPC liveness and readiness probes")
	metricsPort = flag.Int(
		"metricsPort",
		runserver.DefaultMetricsPort,
		"The metrics port")
	destinationEndpointHintKey = flag.String(
		"destinationEndpointHintKey",
		runserver.DefaultDestinationEndpointHintKey,
		"Header and response metadata key used by Envoy to route to the appropriate pod. This must match Envoy configuration.")
	destinationEndpointHintMetadataNamespace = flag.String(
		"DestinationEndpointHintMetadataNamespace",
		runserver.DefaultDestinationEndpointHintMetadataNamespace,
		"The key for the outer namespace struct in the metadata field of the extproc response that is used to wrap the"+
			"target endpoint. If not set, then an outer namespace struct should not be created.")
	poolName = flag.String(
		"poolName",
		runserver.DefaultPoolName,
		"Name of the InferencePool this Endpoint Picker is associated with.")
	poolNamespace = flag.String(
		"poolNamespace",
		runserver.DefaultPoolNamespace,
		"Namespace of the InferencePool this Endpoint Picker is associated with.")
	refreshMetricsInterval = flag.Duration(
		"refreshMetricsInterval",
		runserver.DefaultRefreshMetricsInterval,
		"interval to refresh metrics")
	refreshPrometheusMetricsInterval = flag.Duration(
		"refreshPrometheusMetricsInterval",
		runserver.DefaultRefreshPrometheusMetricsInterval,
		"interval to flush prometheus metrics")
	logVerbosity = flag.Int(
		"v",
		logging.DEFAULT,
		"number for the log level verbosity")
	secureServing = flag.Bool(
		"secureServing",
		runserver.DefaultSecureServing,
		"Enables secure serving. Defaults to true.")
	healthChecking = flag.Bool(
		"healthChecking",
		runserver.DefaultHealthChecking,
		"Enables health checking")
	certPath = flag.String(
		"certPath",
		runserver.DefaultCertPath,
		"The path to the certificate for secure serving. The certificate and private key files "+
			"are assumed to be named tls.crt and tls.key, respectively. If not set, and secureServing is enabled, "+
			"then a self-signed certificate is used.")
	// metric flags
	totalQueuedRequestsMetric = flag.String(
		"totalQueuedRequestsMetric",
		runserver.DefaultTotalQueuedRequestsMetric,
		"Prometheus metric for the number of queued requests.")
	totalRunningRequestsMetric = flag.String("totalRunningRequestsMetric",
		runserver.DefaultTotalRunningRequestsMetric,
		"Prometheus metric for the number of running requests.")
	kvCacheUsagePercentageMetric = flag.String(
		"kvCacheUsagePercentageMetric",
		runserver.DefaultKvCacheUsagePercentageMetric,
		"Prometheus metric for the fraction of KV-cache blocks currently in use (from 0 to 1).")
	// LoRA metrics
	loraInfoMetric = flag.String(
		"loraInfoMetric",
		runserver.DefaultLoraInfoMetric,
		"Prometheus metric for the LoRA info metrics (must be in vLLM label format).")
	// configuration flags
	configFile = flag.String(
		"configFile",
		runserver.DefaultConfigFile,
		"The path to the configuration file")
	configText = flag.String(
		"configText",
		runserver.DefaultConfigText,
		"The configuration specified as text, in lieu of a file")

	modelServerMetricsPort = flag.Int("modelServerMetricsPort", 0, "Port to scrape metrics from pods. "+
		"Default value will be set to InferencePool.Spec.TargetPortNumber if not set.")
	modelServerMetricsPath = flag.String("modelServerMetricsPath", "/metrics", "Path to scrape metrics from pods")

	// Latency Predictor Flag
	enableLatencyPredictor = flag.Bool("enable-latency-predictor", false, "Enable the regression-based latency predictor and scheduler scorer.")

	setupLog = ctrl.Log.WithName("setup")

	// Environment variables
	schedulerV2                       = envutil.GetEnvBool("EXPERIMENTAL_USE_SCHEDULER_V2", false, setupLog)
	prefixCacheScheduling             = envutil.GetEnvBool("ENABLE_PREFIX_CACHE_SCHEDULING", false, setupLog)
	reqHeaderBasedSchedulerForTesting = envutil.GetEnvBool("ENABLE_REQ_HEADER_BASED_SCHEDULER_FOR_TESTING", false, setupLog)
)

// NewRunner initializes a new EPP Runner and returns its pointer.
func NewRunner() *Runner {
	return &Runner{
		requestControlConfig: requestcontrol.NewConfig(), // default requestcontrol config has empty plugin list
	}
}

// Runner is used to run epp with its plugins
type Runner struct {
	requestControlConfig *requestcontrol.Config
	schedulerConfig      *scheduling.SchedulerConfig
}

func (r *Runner) WithRequestControlConfig(requestControlConfig *requestcontrol.Config) *Runner {
	r.requestControlConfig = requestControlConfig
	return r
}

func (r *Runner) WithSchedulerConfig(schedulerConfig *scheduling.SchedulerConfig) *Runner {
	r.schedulerConfig = schedulerConfig
	return r
}

func bindEnvToFlags() {
	// map[ENV_VAR]flagName   – add more as needed
	for env, flg := range map[string]string{
		"GRPC_PORT":                     "grpcPort",
		"GRPC_HEALTH_PORT":              "grpcHealthPort",
		"MODEL_SERVER_METRICS_PORT":     "modelServerMetricsPort",
		"MODEL_SERVER_METRICS_PATH":     "modelServerMetricsPath",
		"DESTINATION_ENDPOINT_HINT_KEY": "destinationEndpointHintKey",
		"POOL_NAME":                     "poolName",
		"POOL_NAMESPACE":                "poolNamespace",
		// durations & bools work too; flag.Set expects the *string* form
		"REFRESH_METRICS_INTERVAL": "refreshMetricsInterval",
		"SECURE_SERVING":           "secureServing",
	} {
		if v := os.Getenv(env); v != "" {
			// ignore error; Parse() will catch invalid values later
			_ = flag.Set(flg, v)
		}
	}
}

func (r *Runner) Run(ctx context.Context) error {
	// Defaults already baked into flag declarations
	// Load env vars as "soft" overrides
	bindEnvToFlags()

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	initLogging(&opts)

	// Validate flags
	if err := validateFlags(); err != nil {
		setupLog.Error(err, "Failed to validate flags")
		return err
	}

	// Print all flag values
	flags := make(map[string]any)
	flag.VisitAll(func(f *flag.Flag) {
		flags[f.Name] = f.Value
	})
	setupLog.Info("Flags processed", "flags", flags)

	// --- Load Configurations from Environment Variables ---
	sdConfig := saturationdetector.LoadConfigFromEnv()

	// --- Get Kubernetes Config ---
	cfg, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "Failed to get Kubernetes rest config")
		return err
	}

	// --- Setup Datastore ---
	mapping, err := backendmetrics.NewMetricMapping(
		*totalQueuedRequestsMetric,
		*totalRunningRequestsMetric,
		*kvCacheUsagePercentageMetric,
		*loraInfoMetric,
	)
	if err != nil {
		setupLog.Error(err, "Failed to create metric mapping from flags.")
		return err
	}
	verifyMetricMapping(*mapping, setupLog)
	pmf := backendmetrics.NewPodMetricsFactory(&backendmetrics.PodMetricsClientImpl{
		MetricMapping:          mapping,
		ModelServerMetricsPort: int32(*modelServerMetricsPort),
		ModelServerMetricsPath: *modelServerMetricsPath,
	}, *refreshMetricsInterval)

	datastore := datastore.NewDatastore(ctx, pmf)

	// --- Setup Metrics Server ---
	customCollectors := []prometheus.Collector{collectors.NewInferencePoolMetricsCollector(datastore)}
	metrics.Register(customCollectors...)
	metrics.RecordInferenceExtensionInfo()
	metricsServerOptions := metricsserver.Options{
		BindAddress:    fmt.Sprintf(":%d", *metricsPort),
		FilterProvider: filters.WithAuthenticationAndAuthorization,
	}

	poolNamespacedName := types.NamespacedName{
		Name:      *poolName,
		Namespace: *poolNamespace,
	}
	mgr, err := runserver.NewDefaultManager(poolNamespacedName, cfg, metricsServerOptions)
	if err != nil {
		setupLog.Error(err, "Failed to create controller manager")
		return err
	}
	err = setupPprofHandlers(mgr)
	if err != nil {
		setupLog.Error(err, "Failed to setup pprof handlers")
		return err
	}

	err = r.parseConfiguration(ctx)
	if err != nil {
		setupLog.Error(err, "Failed to parse the configuration")
		return err
	}

	// ===================================================================
	// == Latency Predictor Integration
	// ===================================================================
	var predictor latencypredictor.PredictorInterface // Use the interface type
	if *enableLatencyPredictor {
		setupLog.Info("Latency predictor is enabled. Initializing...")
		predictor = latencypredictor.New(latencypredictor.ConfigFromEnv(), ctrl.Log.WithName("latency-predictor"))

		concretePredictor := predictor.(*latencypredictor.Predictor)
		if err := mgr.Add(runnable.NoLeaderElection(&predictorRunnable{predictor: concretePredictor})); err != nil {
			setupLog.Error(err, "Failed to register latency predictor runnable")
			return err
		}
	} else {
		setupLog.Info("Latency predictor is disabled.")
		predictor = nil // This will be a true nil interface
	}

	// ===================================================================

	err = r.parseConfiguration(ctx)
	if err != nil {
		setupLog.Error(err, "Failed to parse the configuration")
		return err
	}

	// ===================================================================
	// --- Initialize Core EPP Components ---
	scheduler, err := r.initializeScheduler(predictor, datastore)
	if err != nil {
		setupLog.Error(err, "Failed to create scheduler")
		return err
	}

	saturationDetector := saturationdetector.NewDetector(sdConfig, datastore, ctrl.Log)

	if *enableLatencyPredictor {
		r.requestControlConfig.AddPlugins(slorequest.New(datastore, predictor))
	}

	director := requestcontrol.NewDirectorWithConfig(datastore, scheduler, saturationDetector, r.requestControlConfig)

	// --- Setup ExtProc Server Runner ---
	serverRunner := &runserver.ExtProcServerRunner{
		GrpcPort:                                 *grpcPort,
		DestinationEndpointHintMetadataNamespace: *destinationEndpointHintMetadataNamespace,
		DestinationEndpointHintKey:               *destinationEndpointHintKey,
		PoolNamespacedName:                       poolNamespacedName,
		Datastore:                                datastore,
		SecureServing:                            *secureServing,
		HealthChecking:                           *healthChecking,
		CertPath:                                 *certPath,
		RefreshPrometheusMetricsInterval:         *refreshPrometheusMetricsInterval,
		Director:                                 director,
		SaturationDetector:                       saturationDetector,
		LatencyPredictor:                         predictor,
	}
	if err := serverRunner.SetupWithManager(ctx, mgr); err != nil {
		setupLog.Error(err, "Failed to setup EPP controllers")
		return err
	}

	// --- Add Runnables to Manager ---
	if err := registerHealthServer(mgr, ctrl.Log.WithName("health"), datastore, *grpcHealthPort); err != nil {
		return err
	}

	if err := registerExtProcServer(mgr, serverRunner, ctrl.Log.WithName("ext-proc")); err != nil {
		return err
	}

	// --- Start Manager ---
	setupLog.Info("Controller manager starting")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Error starting controller manager")
		return err
	}
	setupLog.Info("Controller manager terminated")
	return nil
}

func (r *Runner) initializeScheduler(predictor latencypredictor.PredictorInterface, datastore datastore.Datastore) (*scheduling.Scheduler, error) {
	if r.schedulerConfig != nil {
		return scheduling.NewSchedulerWithConfig(r.schedulerConfig), nil
	}

	scheduler := scheduling.NewScheduler()
	if schedulerV2 {
		var schedulerProfile *framework.SchedulerProfile
		if *enableLatencyPredictor {
			// SLO-aware profile
			schedulerProfile = framework.NewSchedulerProfile().
				WithScorers(framework.NewWeightedScorer(scorer.NewSLOScorer(predictor, datastore), 1)).
				WithPicker(picker.NewMaxScorePicker(picker.DefaultMaxNumOfEndpoints))
		} else {
			// Default profile
			schedulerProfile = framework.NewSchedulerProfile().
				WithScorers(framework.NewWeightedScorer(scorer.NewQueueScorer(), envutil.GetEnvInt("QUEUE_SCORE_WEIGHT", scorer.DefaultQueueScorerWeight, setupLog)),
					framework.NewWeightedScorer(scorer.NewKVCacheScorer(), envutil.GetEnvInt("KV_CACHE_SCORE_WEIGHT", scorer.DefaultKVCacheScorerWeight, setupLog))).
				WithPicker(picker.NewMaxScorePicker(picker.DefaultMaxNumOfEndpoints))
		}

		if prefixCacheScheduling {
			prefixScorerWeight := envutil.GetEnvInt("PREFIX_CACHE_SCORE_WEIGHT", prefix.DefaultScorerWeight, setupLog)
			if err := schedulerProfile.AddPlugins(framework.NewWeightedScorer(prefix.New(loadPrefixCacheConfig()), prefixScorerWeight)); err != nil {
				return nil, fmt.Errorf("failed to register scheduler plugins - %w", err)
			}
		}

		schedulerConfig := scheduling.NewSchedulerConfig(profile.NewSingleProfileHandler(), map[string]*framework.SchedulerProfile{"default": schedulerProfile})
		scheduler = scheduling.NewSchedulerWithConfig(schedulerConfig)
	}

	if reqHeaderBasedSchedulerForTesting {
		scheduler = conformance_epp.NewReqHeaderBasedScheduler()
	}

	return scheduler, nil
}

func (r *Runner) parseConfiguration(ctx context.Context) error {
	if *configText == "" && *configFile == "" {
		return nil
	}

	var configBytes []byte
	if *configText != "" {
		configBytes = []byte(*configText)
	} else if *configFile != "" {
		var err error
		configBytes, err = os.ReadFile(*configFile)
		if err != nil {
			return fmt.Errorf("failed to load config from a file '%s' - %w", *configFile, err)
		}
	}

	handle := newEppHandle(ctx)
	config, err := loader.LoadConfig(configBytes, handle)
	if err != nil {
		return fmt.Errorf("failed to load the configuration - %w", err)
	}

	r.schedulerConfig, err = loader.LoadSchedulerConfig(config.SchedulingProfiles, handle)
	if err != nil {
		return fmt.Errorf("failed to create Scheduler configuration - %w", err)
	}

	r.requestControlConfig.AddPlugins(handle.GetAllPlugins()...)

	return nil
}

func initLogging(opts *zap.Options) {
	useV := true
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "zap-log-level" {
			useV = false
		}
	})
	if useV {
		lvl := -1 * (*logVerbosity)
		opts.Level = uberzap.NewAtomicLevelAt(zapcore.Level(int8(lvl)))
	}

	logger := zap.New(zap.UseFlagOptions(opts), zap.RawZapOpts(uberzap.AddCaller()))
	ctrl.SetLogger(logger)
}

func loadPrefixCacheConfig() prefix.Config {
	baseLogger := log.Log.WithName("env-config")

	return prefix.Config{
		HashBlockSize:          envutil.GetEnvInt("PREFIX_CACHE_HASH_BLOCK_SIZE", prefix.DefaultHashBlockSize, baseLogger),
		MaxPrefixBlocksToMatch: envutil.GetEnvInt("PREFIX_CACHE_MAX_PREFIX_BLOCKS", prefix.DefaultMaxPrefixBlocks, baseLogger),
		LRUCapacityPerServer:   envutil.GetEnvInt("PREFIX_CACHE_LRU_CAPACITY_PER_SERVER", prefix.DefaultLRUCapacityPerServer, baseLogger),
	}
}

func registerExtProcServer(mgr manager.Manager, runner *runserver.ExtProcServerRunner, logger logr.Logger) error {
	if err := mgr.Add(runner.AsRunnable(logger)); err != nil {
		setupLog.Error(err, "Failed to register ext-proc gRPC server runnable")
		return err
	}
	setupLog.Info("ExtProc server runner added to manager.")
	return nil
}

func registerHealthServer(mgr manager.Manager, logger logr.Logger, ds datastore.Datastore, port int) error {
	srv := grpc.NewServer()
	healthPb.RegisterHealthServer(srv, &healthServer{
		logger:    logger,
		datastore: ds,
	})
	if err := mgr.Add(
		runnable.NoLeaderElection(runnable.GRPCServer("health", srv, port))); err != nil {
		setupLog.Error(err, "Failed to register health server")
		return err
	}
	return nil
}

func validateFlags() error {
	if *poolName == "" {
		return fmt.Errorf("required %q flag not set", "poolName")
	}
	if *configText != "" && *configFile != "" {
		return fmt.Errorf("both the %q and %q flags can not be set at the same time", "configText", "configFile")
	}

	return nil
}

func verifyMetricMapping(mapping backendmetrics.MetricMapping, logger logr.Logger) {
	if mapping.TotalQueuedRequests == nil {
		logger.Info("Not scraping metric: TotalQueuedRequests")
	}
	if mapping.KVCacheUtilization == nil {
		logger.Info("Not scraping metric: KVCacheUtilization")
	}
	if mapping.LoraRequestInfo == nil {
		logger.Info("Not scraping metric: LoraRequestInfo")
	}
	if mapping.TotalRunningRequests == nil {
		logger.Info("Not scraping metric: TotalRunningRequests")
	}
}

func setupPprofHandlers(mgr ctrl.Manager) error {
	var err error
	profiles := []string{
		"heap",
		"goroutine",
		"allocs",
		"threadcreate",
		"block",
		"mutex",
	}
	for _, p := range profiles {
		err = mgr.AddMetricsServerExtraHandler("/debug/pprof/"+p, pprof.Handler(p))
		if err != nil {
			return err
		}
	}
	return nil
}

type predictorRunnable struct {
	predictor *latencypredictor.Predictor
}

func (p *predictorRunnable) Start(ctx context.Context) error {
	setupLog.Info("Starting latency predictor...")
	p.predictor.Start(ctx)
	<-ctx.Done()
	setupLog.Info("Stopping latency predictor...")
	p.predictor.Stop()
	return nil
}
