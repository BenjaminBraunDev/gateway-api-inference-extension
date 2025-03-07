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

package backend

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"go.uber.org/multierr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

const (
	// Hardcoded vLLM specific LoRA metrics
	LoraRequestInfoRunningAdaptersMetricName = "running_lora_adapters"
	LoraRequestInfoWaitingAdaptersMetricName = "waiting_lora_adapters"
	LoraRequestInfoMaxAdaptersMetricName     = "max_lora"
)

type PodMetricsClientImpl struct {
	MetricMapping *MetricMapping
}

// FetchMetrics fetches metrics from a given pod.
func (p *PodMetricsClientImpl) FetchMetrics(
	ctx context.Context,
	existing *datastore.PodMetrics,
	port int32,
) (*datastore.PodMetrics, error) {
	logger := log.FromContext(ctx)
	loggerDefault := logger.V(logutil.DEFAULT)

	url := "http://" + existing.Address + ":" + strconv.Itoa(int(port)) + "/metrics"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		loggerDefault.Error(err, "Failed create HTTP request", "method", http.MethodGet, "url", url)
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		loggerDefault.Error(err, "Failed to fetch metrics", "pod", existing.NamespacedName)
		return nil, fmt.Errorf("failed to fetch metrics from %s: %w", existing.NamespacedName, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		loggerDefault.Error(nil, "Unexpected status code returned", "pod", existing.NamespacedName, "statusCode", resp.StatusCode)
		return nil, fmt.Errorf("unexpected status code from %s: %v", existing.NamespacedName, resp.StatusCode)
	}

	parser := expfmt.TextParser{}
	metricFamilies, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, err
	}
	return p.promToPodMetrics(logger, metricFamilies, existing)
}

// promToPodMetrics updates internal pod metrics with scraped Prometheus metrics.
func (p *PodMetricsClientImpl) promToPodMetrics(
	logger logr.Logger,
	metricFamilies map[string]*dto.MetricFamily,
	existing *datastore.PodMetrics,
) (*datastore.PodMetrics, error) {
	var errs error
	updated := existing.Clone()

	if p.MetricMapping.RunningRequests != nil {
		running, err := p.getMetric(logger, metricFamilies, *p.MetricMapping.RunningRequests)
		if err == nil {
			updated.RunningQueueSize = int(running.GetGauge().GetValue())
		} else {
			errs = multierr.Append(errs, err)
		}
	}

	if p.MetricMapping.AllRequests != nil {
		all, err := p.getMetric(logger, metricFamilies, *p.MetricMapping.AllRequests)
		if err == nil {
			updated.WaitingQueueSize = int(all.GetGauge().GetValue()) - updated.RunningQueueSize
		} else {
			errs = multierr.Append(errs, err)
		}
	}

	if p.MetricMapping.WaitingRequests != nil {
		waiting, err := p.getMetric(logger, metricFamilies, *p.MetricMapping.WaitingRequests)
		if err == nil {
			updated.WaitingQueueSize = int(waiting.GetGauge().GetValue())
		} else {
			errs = multierr.Append(errs, err)
		}
	}

	if p.MetricMapping.KVCacheUsage != nil {
		usage, err := p.getMetric(logger, metricFamilies, *p.MetricMapping.KVCacheUsage)
		if err == nil {
			updated.KVCacheUsagePercent = usage.GetGauge().GetValue()
		} else {
			errs = multierr.Append(errs, err)
		}
	} else if p.MetricMapping.UsedKVCacheBlocks != nil && p.MetricMapping.MaxKVCacheBlocks != nil {
		used, err := p.getMetric(logger, metricFamilies, *p.MetricMapping.UsedKVCacheBlocks)
		if err != nil {
			errs = multierr.Append(errs, err)
		}
		max, err := p.getMetric(logger, metricFamilies, *p.MetricMapping.MaxKVCacheBlocks)
		if err != nil {
			errs = multierr.Append(errs, err)
		}
		if err == nil {
			usage := 0.0
			if max.GetGauge().GetValue() > 0 {
				usage = used.GetGauge().GetValue() / max.GetGauge().GetValue()
			}
			updated.KVCacheUsagePercent = usage
		}
	}

	// Handle LoRA metrics (only if all LoRA MetricSpecs are present)
	if p.MetricMapping.LoraRequestInfo != nil {
		loraMetrics, _, err := p.getLatestLoraMetric(logger, metricFamilies)
		errs = multierr.Append(errs, err)

		if loraMetrics != nil {
			updated.ActiveModels = make(map[string]int)
			for _, label := range loraMetrics.GetLabel() {
				if label.GetName() == LoraRequestInfoRunningAdaptersMetricName {
					if label.GetValue() != "" {
						adapterList := strings.Split(label.GetValue(), ",")
						for _, adapter := range adapterList {
							updated.ActiveModels[adapter] = 0
						}
					}
				}
				if label.GetName() == LoraRequestInfoWaitingAdaptersMetricName {
					if label.GetValue() != "" {
						adapterList := strings.Split(label.GetValue(), ",")
						for _, adapter := range adapterList {
							updated.ActiveModels[adapter] = 0
						}
					}
				}
				if label.GetName() == LoraRequestInfoMaxAdaptersMetricName {
					if label.GetValue() != "" {
						updated.MaxActiveModels, err = strconv.Atoi(label.GetValue())
						if err != nil {
							errs = multierr.Append(errs, err)
						}
					}
				}
			}
		}
	}

	return updated, errs
}

// getLatestLoraMetric gets latest lora metric series in gauge metric family `vllm:lora_requests_info`
// reason its specially fetched is because each label key value pair permutation generates new series
// and only most recent is useful. The value of each series is the creation timestamp so we can
// retrieve the latest by sorting the value.
func (p *PodMetricsClientImpl) getLatestLoraMetric(logger logr.Logger, metricFamilies map[string]*dto.MetricFamily) (*dto.Metric, time.Time, error) {
	if p.MetricMapping.LoraRequestInfo == nil {
		return nil, time.Time{}, nil // No LoRA metrics configured
	}

	loraRequests, ok := metricFamilies[p.MetricMapping.LoraRequestInfo.MetricName]
	if !ok {
		logger.V(logutil.DEFAULT).Error(nil, "Metric family not found", "name", p.MetricMapping.LoraRequestInfo.MetricName)
		return nil, time.Time{}, fmt.Errorf("metric family %q not found", p.MetricMapping.LoraRequestInfo.MetricName)
	}

	var latest *dto.Metric
	var latestTs float64 // Use float64, as Gauge.Value is float64

	// Iterate over all metrics in the family.
	for _, m := range loraRequests.GetMetric() {
		running := ""
		waiting := ""
		// Check if the metric has the expected LoRA labels.  This is important!
		hasRequiredLabels := false
		for _, lp := range m.GetLabel() {
			switch lp.GetName() {
			case LoraRequestInfoRunningAdaptersMetricName:
				running = lp.GetValue()
				hasRequiredLabels = true
			case LoraRequestInfoWaitingAdaptersMetricName:
				waiting = lp.GetValue()
				hasRequiredLabels = true
			}
		}
		//Skip if it does not have the lora labels
		if !hasRequiredLabels {
			continue
		}
		// Ignore metrics with both labels empty.
		if running == "" && waiting == "" {
			continue
		}

		// Select the metric with the *largest Gauge Value* (which represents the timestamp).
		if m.GetGauge().GetValue() > latestTs {
			latestTs = m.GetGauge().GetValue()
			latest = m
		}
	}
	if latest == nil {
		logger.V(logutil.TRACE).Info("Metric value Empty", "value", latest, "metric", p.MetricMapping.LoraRequestInfo.MetricName)
		return nil, time.Time{}, nil
	}

	// Convert the gauge value (creation timestamp) to time.Time.
	return latest, time.Unix(0, int64(latestTs*1e9)), nil // Convert nanoseconds to time.Time
}

// getMetric retrieves a specific metric based on MetricSpec.
func (p *PodMetricsClientImpl) getMetric(logger logr.Logger, metricFamilies map[string]*dto.MetricFamily, spec MetricSpec) (*dto.Metric, error) {
	mf, ok := metricFamilies[spec.MetricName]
	if !ok {
		logger.V(logutil.DEFAULT).Error(nil, "Metric family not found", "name", spec.MetricName)
		return nil, fmt.Errorf("metric family %q not found", spec.MetricName)
	}

	if len(mf.GetMetric()) == 0 {
		return nil, fmt.Errorf("no metrics available for %q", spec.MetricName)
	}
	// if there is a specified label, return only that metric in the family
	if spec.Labels != nil {
		return getLabeledMetric(logger, mf, spec)
	}
	return getLatestMetric(logger, mf)
}

// getLatestMetric gets the latest metric of a family (for metrics without labels).
func getLatestMetric(logger logr.Logger, mf *dto.MetricFamily) (*dto.Metric, error) {
	var latestTs int64
	var latest *dto.Metric
	for _, m := range mf.GetMetric() {
		if m.GetTimestampMs() >= latestTs {
			latestTs = m.GetTimestampMs()
			latest = m
		}
	}

	if latest == nil {
		return nil, fmt.Errorf("no metrics found for %q", mf.GetName())
	}

	logger.V(logutil.TRACE).Info("Latest metric value selected", "value", latest, "metric", mf.GetName())
	return latest, nil
}

// getLabeledMetric gets the latest metric with matching labels.
func getLabeledMetric(logger logr.Logger, mf *dto.MetricFamily, spec MetricSpec) (*dto.Metric, error) {
	var latestMetric *dto.Metric
	var latestTimestamp int64 = -1 // Initialize to -1 so any timestamp is greater

	for _, m := range mf.GetMetric() {
		if labelsMatch(m.GetLabel(), spec.Labels) {
			if m.GetTimestampMs() > latestTimestamp {
				latestTimestamp = m.GetTimestampMs()
				latestMetric = m
			}
		}
	}

	if latestMetric != nil {
		logger.V(logutil.TRACE).Info("Labeled metric found", "value", latestMetric, "metric", spec.MetricName)
		return latestMetric, nil
	}

	return nil, fmt.Errorf("no matching labeled metric found for %q with labels %v", spec.MetricName, spec.Labels)
}

// labelsMatch checks if a metric's labels contain all the labels in the spec.
func labelsMatch(metricLabels []*dto.LabelPair, specLabels map[string]string) bool {
	if len(specLabels) == 0 {
		return true // No specific labels required
	}

	for specName, specValue := range specLabels {
		found := false
		for _, label := range metricLabels {
			if label.GetName() == specName && label.GetValue() == specValue {
				found = true
				break
			}
		}
		if !found {
			return false // A required label is missing
		}
	}
	return true // All required labels are present
}
