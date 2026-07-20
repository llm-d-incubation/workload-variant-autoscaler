/*
Copyright 2025 The llm-d Authors

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

// Package collector provides replica metrics collection functionality.
//
// This package provides ReplicaMetricsCollector which collects replica-level
// metrics for both saturation analysis and queueing model analysis using the
// source infrastructure. Saturation metrics (KV cache, queue length, token
// capacity) and queueing model metrics (scheduler dispatch rate, max batch
// size) are collected together and exposed via the shared ReplicaMetrics struct.
//
// # Pod label fallback
//
// Every processing block in Refresh() extracts a pod identity from Prometheus
// labels using a two-step fallback:
//
//	podName := value.Labels["pod"]
//	if podName == "" {
//	    podName = value.Labels["pod_name"]
//	}
//
// Engine metrics are typically scraped via a PodMonitor or ServiceMonitor that
// applies the Prometheus operator's default target-relabeling, which produces
// a "pod" label. Some scrape configurations (e.g., raw Prometheus scrape jobs,
// kube-state-metrics–style configs) instead expose the pod identity as
// "pod_name". The fallback handles both conventions so the collector works
// regardless of how the Prometheus scrape is configured.
package collector

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	saturation_v2 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation_v2"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

// ReplicaMetricsCollector collects replica-level metrics for both saturation
// analysis and queueing model analysis using the source infrastructure.
type ReplicaMetricsCollector struct {
	source    source.MetricsSource
	k8sClient client.Client
	recorder  record.EventRecorder
	locator   locator.PodLocator
	// metricsAvailableState tracks whether metrics were available in the previous
	// cycle for each VA (keyed by namespace/name). Used for edge-triggered events.
	metricsAvailableState map[string]bool
	mu                    sync.Mutex
}

// NewReplicaMetricsCollector creates a new replica metrics collector.
func NewReplicaMetricsCollector(metricsSource source.MetricsSource, k8sClient client.Client, recorder record.EventRecorder, podLocator locator.PodLocator) *ReplicaMetricsCollector {
	return &ReplicaMetricsCollector{
		source:                metricsSource,
		k8sClient:             k8sClient,
		recorder:              recorder,
		locator:               podLocator,
		metricsAvailableState: make(map[string]bool),
	}
}

// recordUnattributedReadyPodsEvent emits a Warning/UnattributedReadyPods K8s event for va.
// Deduplication: at most one event per VA per cycle; vaEventTracker records which VAs have
// already received an event this cycle so repeated calls are no-ops for those VAs.
func (c *ReplicaMetricsCollector) recordUnattributedReadyPodsEvent(
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	readyCount int32,
	vaEventTracker map[string]bool,
) {
	if c.recorder == nil {
		return
	}
	key := utils.GetNamespacedKey(va.Namespace, va.Name)
	if vaEventTracker != nil {
		if _, ok := vaEventTracker[key]; ok { // one event per VA per cycle
			return
		}
	}
	c.recorder.Event(va, corev1.EventTypeWarning, constants.K8SEventUnattributedReadyPods,
		fmt.Sprintf("%s has %d ready pod(s) but none attributed; "+
			"verify the llm-d.ai/variant pod label on the scale target equals %q",
			va.Name, readyCount, va.Name))
	if vaEventTracker != nil {
		vaEventTracker[key] = true
	}
}

func (c *ReplicaMetricsCollector) recordMetricsUnavailableEvent(
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	vaEventTracker map[string]bool,
	reason string,
) {
	if c.recorder == nil {
		return
	}

	for _, va := range variantAutoscalings {
		key := utils.GetNamespacedKey(va.Namespace, va.Name)
		if vaEventTracker != nil {
			if _, ok := vaEventTracker[key]; ok { // ensures only one event is recorded per VA
				continue
			}
		}
		c.recorder.Event(va, corev1.EventTypeWarning, constants.K8SEventMetricsUnavailable, reason)
		if vaEventTracker != nil {
			vaEventTracker[key] = true
		}
	}
}

// CollectReplicaMetrics collects per-replica metrics for all replicas of a model and records
// K8S events on failures. This wrapper ensures MetricsUnavailable events are emitted when
// metrics collection fails or returns no data, using edge-triggered emission (only on
// transitions from available → unavailable) to avoid flooding the event stream.
//
// The collected metrics serve both the saturation analyzer and the queueing model analyzer:
//   - Saturation metrics: KV cache usage, queue length, token capacity, prefix cache hit rate
//   - Queueing model metrics: scheduler dispatch rate (arrival rate), max batch size
//
// Parameters:
//   - ctx: Context for the operation
//   - modelID: The model identifier to collect metrics for
//   - namespace: The namespace where the model is deployed
//   - scaleTargets: Map of Deployment/LWS namespace/name to Deployment/LWS
//   - variantAutoscalings: Map of VariantAutoscaling namespace/name to VariantAutoscaling object
//   - variantCosts: Map of VariantAutoscaling namespace/name to cost value
//
// Returns:
//   - []interfaces.ReplicaMetrics: Per-pod metrics for saturation and queueing model analysis
//   - error: Any error that occurred during collection
func (c *ReplicaMetricsCollector) CollectReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	vaEventTracker map[string]bool,
	variantCosts map[string]float64,
) ([]interfaces.ReplicaMetrics, error) {
	replicaMetrics, err := c.collectReplicaMetrics(ctx, modelID, namespace, scaleTargets, variantAutoscalings, variantCosts)

	// Determine if metrics are available in this cycle
	metricsAvailable := err == nil && len(replicaMetrics) > 0

	// Check previous state and emit events only on available → unavailable transitions
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, va := range variantAutoscalings {
		previouslyAvailable, seen := c.metricsAvailableState[key]

		// Edge-triggered: only emit event on available → unavailable transition
		// Don't emit on first observation (we don't know previous state - VA may have started at zero)
		shouldEmitEvent := seen && previouslyAvailable && !metricsAvailable

		if shouldEmitEvent {
			if err != nil {
				c.recordMetricsUnavailableEvent(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{key: va}, vaEventTracker, "Failed to collect metrics for model")
			} else if len(replicaMetrics) == 0 {
				c.recordMetricsUnavailableEvent(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{key: va}, vaEventTracker, "No saturation metrics available for model")
			}
		}

		// Update state for next cycle
		c.metricsAvailableState[key] = metricsAvailable
	}

	// Warn when a VA has Ready pods but none are attributed to it this cycle.
	// Only runs when the model produced at least one attributed replica — model-wide
	// emptiness is the availability path above; the scrape-lag gate keeps quiet there.
	if err == nil && len(replicaMetrics) > 0 {
		attributed := make(map[string]int, len(variantAutoscalings))
		for i := range replicaMetrics {
			attributed[replicaMetrics[i].VariantName]++
		}
		for _, va := range variantAutoscalings {
			if attributed[va.Name] > 0 {
				continue
			}
			stKey := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
			st, ok := scaleTargets[stKey]
			if !ok || st == nil {
				continue
			}
			if ready := st.GetStatusReadyReplicas(); ready > 0 {
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("VA has ready pods but none attributed",
					"va", va.Name, "namespace", va.Namespace, "readyReplicas", ready)
				c.recordUnattributedReadyPodsEvent(va, ready, vaEventTracker)
			}
		}
	}

	if err != nil {
		return nil, err
	}
	return replicaMetrics, nil
}

// buildInstanceKey returns (instanceKey, podName, vaName) for a series's labels.
// vaName comes from the llm_d_ai_variant label when present (the legacy /
// shadow-pod fast path). When absent and a pod label is present, falls back
// to the locator's owner-walk for Deployment / LWS layouts. Returns
// vaName="" when neither path resolves; the caller treats that as "skip".
func (c *ReplicaMetricsCollector) buildInstanceKey(ctx context.Context, namespace string, labels map[string]string) (instanceKey, podName, vaName string) {
	podName = labels["pod"]
	if podName == "" {
		podName = labels["pod_name"]
	}

	vaName = labels[constants.VariantLabelPrometheusKey]
	if vaName == "" && podName != "" && c.locator != nil {
		ms, err := c.locator.Locate(ctx, namespace, podName)
		switch {
		case err != nil:
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("locator.Locate failed; treating pod as unmanaged",
				"pod", podName, "namespace", namespace, "error", err)
		case ms == nil:
			// No managed scaler in the pod's owner chain — the pod is unmanaged.
			// Leaves vaName="" so the caller skips it.
		default:
			// The synthetic VariantAutoscaling is always keyed by the scaler name
			// (the HPA or ScaledObject name), so use it directly.
			switch {
			case ms.HPA != nil:
				vaName = ms.HPA.Name
			case ms.ScaledObject != nil:
				vaName = ms.ScaledObject.Name
			}
		}
	}

	instance := labels["instance"]
	port := ""
	if instance != "" && podName != "" {
		if idx := strings.LastIndex(instance, ":"); idx != -1 {
			port = instance[idx+1:]
		}
	}

	switch {
	case podName != "" && port != "":
		instanceKey = podName + ":" + port
	case instance != "":
		instanceKey = instance
	case podName != "":
		instanceKey = podName
	default:
		return "", "", ""
	}
	return instanceKey, podName, vaName
}

// collectReplicaMetrics is the internal implementation that collects per-replica metrics.
func (c *ReplicaMetricsCollector) collectReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	variantCosts map[string]float64,
) ([]interfaces.ReplicaMetrics, error) {
	logger := ctrl.LoggerFrom(ctx)

	params := map[string]string{
		source.ParamModelID:   modelID,
		source.ParamNamespace: namespace,
	}

	// Determine which inference engines this model's variants run. Engine-specific
	// queries are refreshed once per present engine (vLLM pods emit vllm:* series,
	// SGLang pods emit sglang:* series — the per-pod results are disjoint and are
	// merged back under the logical query name below). For vLLM-only models this
	// is identical to the previous fixed query list.
	engines := inferenceengine.Present(scaleTargets)

	// Log the engine detected for each scale target, plus the resolved engine set.
	// inferenceengine.Detect defaults to vLLM when a leader pod template is nil or
	// unresolvable, or when an SGLang image/command isn't matched — so a misdetected
	// SGLang variant silently gets vllm:* queries and emits nothing. Logging the
	// per-target engine lets operators tell "wrong engine detected" apart from
	// "engine correct, but no metrics".
	if debug := logger.V(logging.DEBUG); debug.Enabled() {
		for key, st := range scaleTargets {
			debug.Info("Detected inference engine for scale target",
				"scaleTarget", key, "engine", inferenceengine.Detect(st).String())
		}
		engineNames := make([]string, len(engines))
		for i, e := range engines {
			engineNames[i] = e.String()
		}
		debug.Info("Resolved inference engines for model",
			"modelID", modelID, "namespace", namespace, "engines", engineNames)
	}

	// Refresh all Prometheus-sourced queries:
	// - Saturation: KV cache, queue length, cache config, prefix cache hit rate
	// - Shared (saturation + queueing model): avg input tokens, avg output tokens
	// - Queueing model: scheduler dispatch rate, avg TTFT, avg ITL
	// - Throughput analyzer: generation token rate, instantaneous KV usage (k*), request rate
	queries := buildEngineQueryList(engines, engineSpecificReplicaQueries, agnosticReplicaQueries)

	// Execute the query with timing
	startTime := time.Now()
	results, err := c.source.Refresh(ctx, source.RefreshSpec{
		Queries: queries,
		Params:  params,
	})
	duration := time.Since(startTime).Seconds()
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeKVCache)
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeQueueLength)
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeCacheConfig)

	if err != nil {
		reason := utils.CategorizePrometheusError(err)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeKVCache, reason)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeQueueLength, reason)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeCacheConfig, reason)
		return nil, fmt.Errorf("failed to refresh replica metrics: %w", err)
	}

	// Re-key engine-specific results under their logical query names so the per-pod
	// processing below is engine-agnostic. For SGLang-only models this renames the
	// "sglang/<query>" results to "<query>"; for mixed-engine models it concatenates
	// the per-engine series. The structural cache-config difference is handled by a
	// dedicated SGLang pass after the vLLM cache-config block.
	mergeEngineResults(results, engines, engineSpecificReplicaQueries)

	// podMetricData holds per-pod metric values and timestamps
	type podMetricData struct {
		podName        string // Actual pod name for K8s API lookups
		vaName         string // VariantAutoscaling name extracted from llm_d_ai_variant label
		kvUsage        float64
		kvTimestamp    time.Time
		hasKv          bool
		queueLen       int
		queueTimestamp time.Time
		hasQueue       bool
		// V2 fields for token-based capacity analysis
		numGpuBlocks                int64
		blockSize                   int64
		avgOutputTokens             float64
		avgOutputTokensTimestamp    time.Time
		avgInputTokens              float64
		avgInputTokensTimestamp     time.Time
		prefixCacheHitRate          float64
		prefixCacheHitRateTimestamp time.Time
		hasCacheConfig              bool
		cacheConfigTimestamp        time.Time
		// Queueing model fields
		arrivalRate          float64
		hasArrivalRate       bool
		arrivalRateTimestamp time.Time
		avgTTFT              float64
		avgTTFTTimestamp     time.Time
		avgITL               float64
		avgITLTimestamp      time.Time
		// Throughput analyzer fields
		generationTokenRate float64
		kvUsageInstant      float64
		requestRate         float64
	}

	// trackMetricFreshness determines the freshness status of metrics in podMetricData
	// and increments the corresponding counters in the freshness status map.
	trackMetricFreshness := func(
		vaName string,
		data *podMetricData,
		collectedAt time.Time,
		freshnessMap map[string]map[string]int,
	) {
		// Initialize inner map if needed
		if freshnessMap[vaName] == nil {
			freshnessMap[vaName] = make(map[string]int)
		}

		thresholds := config.DefaultFreshnessThresholds()

		// Helper to track a single timestamp
		trackTimestamp := func(timestamp time.Time) {
			var status string
			if timestamp.IsZero() {
				status = "missing"
			} else {
				age := collectedAt.Sub(timestamp)
				status = thresholds.DetermineStatus(age)
			}
			freshnessMap[vaName][status]++
		}

		// Track all metric timestamps
		trackTimestamp(data.kvTimestamp)
		trackTimestamp(data.queueTimestamp)
		trackTimestamp(data.avgOutputTokensTimestamp)
		trackTimestamp(data.avgInputTokensTimestamp)
		trackTimestamp(data.prefixCacheHitRateTimestamp)
		trackTimestamp(data.cacheConfigTimestamp)
		trackTimestamp(data.arrivalRateTimestamp)
		trackTimestamp(data.avgTTFTTimestamp)
		trackTimestamp(data.avgITLTimestamp)
	}

	// Extract per-pod metrics from results
	podData := make(map[string]*podMetricData)

	// Process KV cache results
	if result := results[registration.QueryKvCacheUsage]; result != nil {
		if result.HasError() {
			return nil, fmt.Errorf("KV cache query failed: %w", result.Error)
		}
		for _, value := range result.Values {
			instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
			if instanceKey == "" {
				continue
			}

			if podData[instanceKey] == nil {
				podData[instanceKey] = &podMetricData{
					podName: podName,
					vaName:  vaName,
				}
			}
			podData[instanceKey].kvUsage = value.Value
			podData[instanceKey].kvTimestamp = value.Timestamp
			podData[instanceKey].hasKv = true

			logger.V(logging.DEBUG).Info("KV cache metric",
				"instanceKey", instanceKey,
				"pod", podName,
				"usage", value.Value,
				"usagePercent", value.Value*100)
		}
	}

	// Process queue length results
	if result := results[registration.QueryQueueLength]; result != nil {
		if result.HasError() {
			return nil, fmt.Errorf("queue length query failed: %w", result.Error)
		}
		for _, value := range result.Values {
			instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
			if instanceKey == "" {
				continue
			}

			if podData[instanceKey] == nil {
				podData[instanceKey] = &podMetricData{
					podName: podName,
					vaName:  vaName,
				}
			}
			podData[instanceKey].queueLen = int(value.Value)
			podData[instanceKey].queueTimestamp = value.Timestamp
			podData[instanceKey].hasQueue = true

			logger.V(logging.DEBUG).Info("Queue metric",
				"instanceKey", instanceKey,
				"pod", podName,
				"queueLength", int(value.Value))
		}
	}

	// Process cache config info results (V2)
	//
	// vllm:cache_config_info has no model_name label (see QueryCacheConfigInfo),
	// so it is queried namespace-wide and may include pods of other models in the
	// same namespace. Attach cache config only to instances already discovered by
	// the model-scoped KV/queue queries above; skip unknown instances so foreign
	// pods are not introduced into this model's metrics (and do not inflate the
	// discovered-pods / freshness counters).
	if result := results[registration.QueryCacheConfigInfo]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				data := podData[instanceKey]
				if data == nil {
					// Instance not seen by the model-scoped queries: it belongs to a
					// different model (or lacks KV/queue metrics) — not one of ours.
					continue
				}

				// Parse num_gpu_blocks and block_size from string labels
				if blocksStr, ok := value.Labels["num_gpu_blocks"]; ok && blocksStr != "" {
					if blocks, err := strconv.ParseInt(blocksStr, 10, 64); err == nil {
						data.numGpuBlocks = blocks
					}
				}
				if sizeStr, ok := value.Labels["block_size"]; ok && sizeStr != "" {
					if size, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
						data.blockSize = size
					}
				}
				if data.numGpuBlocks > 0 && data.blockSize > 0 {
					data.hasCacheConfig = true
					data.cacheConfigTimestamp = value.Timestamp
				}

				logger.V(logging.DEBUG).Info("Cache config info metric",
					"instanceKey", instanceKey,
					"pod", podName,
					"numGpuBlocks", data.numGpuBlocks,
					"blockSize", data.blockSize)
			}
		}
	}

	// Process SGLang cache config (structural difference from vLLM).
	//
	// SGLang exposes total KV-cache token capacity directly via
	// sglang:max_total_num_tokens (the value), rather than as
	// num_gpu_blocks/block_size labels. We map the capacity onto the existing
	// numGpuBlocks × blockSize computation by setting blockSize = 1 and
	// numGpuBlocks = capacity, so the downstream TotalKvCapacityTokens math is
	// unchanged. Only runs when an SGLang variant is present for this model.
	if containsEngine(engines, inferenceengine.EngineSGLang) {
		sglangCacheKey := registration.EngineQuery(inferenceengine.EngineSGLang, registration.QueryCacheConfigInfo)
		if result := results[sglangCacheKey]; result != nil && !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				data := podData[instanceKey]
				if data == nil {
					// Not seen by the model-scoped KV/queue queries — skip.
					continue
				}
				capacity := int64(value.Value)
				if capacity > 0 {
					data.numGpuBlocks = capacity
					data.blockSize = 1
					data.hasCacheConfig = true
					data.cacheConfigTimestamp = value.Timestamp
				}

				logger.V(logging.DEBUG).Info("SGLang cache config metric",
					"instanceKey", instanceKey,
					"pod", podName,
					"totalKvCapacityTokens", capacity)
			}
		}
	}

	// Process average output tokens results (V2)
	if result := results[registration.QueryAvgOutputTokens]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				// NaN check: rate division by zero produces NaN
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
					podData[instanceKey].avgOutputTokens = value.Value
					podData[instanceKey].avgOutputTokensTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process average input tokens results (V2)
	if result := results[registration.QueryAvgInputTokens]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				// NaN check: rate division by zero produces NaN
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
					podData[instanceKey].avgInputTokens = value.Value
					podData[instanceKey].avgInputTokensTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process prefix cache hit rate results (V2)
	if result := results[registration.QueryPrefixCacheHitRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				// NaN check: rate division by zero produces NaN when no prefix cache queries
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 && value.Value <= 1 {
					podData[instanceKey].prefixCacheHitRate = value.Value
					podData[instanceKey].prefixCacheHitRateTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process scheduler dispatch rate results (arrival rate per instance)
	if result := results[registration.QuerySchedulerDispatchRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				// The scheduler metric has pod_name and port labels to identify the engine instance.
				// Build a composite key: pod_name:port to support multiple instances per pod.
				podName := value.Labels["pod_name"]
				port := value.Labels["port"]

				if podName == "" {
					podName = value.Labels["pod"]
				}
				if podName == "" {
					logger.Info("Scheduler dispatch rate metric missing both 'pod' and 'pod_name' labels, skipping",
						"labels", value.Labels,
						"model", modelID,
						"namespace", namespace)
					continue
				}

				// Create composite key: pod_name:port for unique instance identification
				instanceKey := podName
				if port != "" {
					instanceKey = podName + ":" + port
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
					}
				}
				// NaN check: rate can produce NaN if no successful attempts
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[instanceKey].arrivalRate = value.Value
					podData[instanceKey].hasArrivalRate = true
					podData[instanceKey].arrivalRateTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Scheduler dispatch rate metric",
						"instance", instanceKey,
						"pod", podName,
						"port", port,
						"arrivalRate", value.Value)
				}
			}
		}
	}

	// Process average TTFT results (seconds)
	if result := results[registration.QueryAvgTTFT]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value > 0 {
					podData[instanceKey].avgTTFT = value.Value
					podData[instanceKey].avgTTFTTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Avg TTFT metric",
						"instanceKey", instanceKey,
						"pod", podName,
						"avgTTFTSeconds", value.Value)
				}
			}
		}
	}

	// Process average ITL results (seconds)
	if result := results[registration.QueryAvgITL]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value > 0 {
					podData[instanceKey].avgITL = value.Value
					podData[instanceKey].avgITLTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Avg ITL metric",
						"instanceKey", instanceKey,
						"pod", podName,
						"avgITLSeconds", value.Value)
				}
			}
		}
	}

	// Process generation token rate results (tokens/sec) — throughput analyzer μ_dec^obs
	if result := results[registration.QueryGenerationTokenRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				if podData[instanceKey] == nil {
					continue // skip pods the KV/queue queries didn't see (scrape skew)
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[instanceKey].generationTokenRate = value.Value
				}
			}
		}
	}

	// Process instantaneous KV usage (k*) results (0.0–1.0) — throughput analyzer k*
	if result := results[registration.QueryKvUsageInstant]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				if podData[instanceKey] == nil {
					continue // skip pods the KV/queue queries didn't see (scrape skew)
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 && value.Value <= 1 {
					podData[instanceKey].kvUsageInstant = value.Value
				}
			}
		}
	}

	// Process engine request completion rate (req/s) — throughput analyzer fallback λ_req
	if result := results[registration.QueryRequestRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				if podData[instanceKey] == nil {
					continue // skip pods the KV/queue queries didn't see (scrape skew)
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[instanceKey].requestRate = value.Value
				}
			}
		}
	}

	// Pre-compute MaxBatchSize per scale target from container args.
	// MaxBatchSize is not a Prometheus metric; it is parsed from the Deployment/LWS
	// spec using the argument parser for the variant's detected engine
	// (vLLM --max-num-seqs / SGLang --max-running-requests).
	// Map key is scale target key (namespace/name).
	scaleTargetMaxBatchSize := make(map[string]int64, len(scaleTargets))
	for key, scaleTarget := range scaleTargets {
		params := saturation_v2.ParseEngineArgs(inferenceengine.Detect(scaleTarget), scaleTarget)
		scaleTargetMaxBatchSize[key] = params.MaxNumSeqs
	}

	// Track metrics freshness status per pod
	vaMetricsFreshnessStatus := make(map[string]map[string]int)

	// Build replica metrics from pod data
	replicaMetrics := make([]interfaces.ReplicaMetrics, 0, len(podData))
	collectedAt := time.Now()

	for instanceKey, data := range podData {
		// Use the actual pod name (not instance IP:port) for logging
		podName := data.podName
		if podName == "" {
			// Fallback: if pod name wasn't extracted from labels, use instanceKey
			// This handles cases where the metric doesn't have pod label
			podName = instanceKey
		}

		// Extract VA name directly from metrics (llm_d_ai_variant label)
		// This replaces the previous ownership traversal approach
		vaName := data.vaName

		// Skip pods that have no metrics at all. This can happen when the query returns pods that
		// were scaled up then scaled down, i.e. no longer running in the namespace.
		if !data.hasKv && !data.hasQueue {
			continue
		}

		kvUsage := data.kvUsage
		queueLen := data.queueLen

		if !data.hasKv {
			logger.Info("Pod missing KV cache metrics, using 0",
				"pod", podName,
				"instance", instanceKey,
				"model", modelID,
				"namespace", namespace)
			kvUsage = 0
		}
		if !data.hasQueue {
			logger.Info("Pod missing queue metrics, using 0",
				"pod", podName,
				"instance", instanceKey,
				"model", modelID,
				"namespace", namespace)
			queueLen = 0
		}

		if vaName == "" {
			// Neither the llm-d.ai/variant label nor the pod locator attributed
			// this pod to a managed scaler. Count it so the otherwise-silent skip
			// is observable; the pod is unattributed, so the metric is keyed by
			// namespace and reason only.
			metrics.IncPodMappingMiss(namespace, constants.PodMappingMissUnresolved)
			logger.Info("Skipping pod that doesn't match any scale target",
				"pod", podName,
				"instance", instanceKey,
				"scale targets", getScaleTargetNames(scaleTargets))
			continue
		}

		variantKey := utils.GetNamespacedKey(namespace, vaName)
		// Get accelerator name from Deployment/LWS nodeSelector/nodeAffinity or VA label
		acceleratorName := ""
		if va, ok := variantAutoscalings[variantKey]; ok && va != nil {
			// Find the scale target for this VA
			key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
			if scaleTarget, found := scaleTargets[key]; found {
				// Get accelerator name from Deployment/LWS nodeSelector/nodeAffinity or VA label
				acceleratorName = utils.GetAcceleratorNameFromScaleTarget(va, scaleTarget)
			} else {
				// Deployment/LWS not cached, fall back to VA label via nil scale target
				acceleratorName = utils.GetAcceleratorNameFromScaleTarget(va, nil)
			}
		}

		// Look up cost by VariantAutoscaling namespace/name
		cost := saturation.DefaultVariantCost
		if variantCosts != nil {
			if c, ok := variantCosts[variantKey]; ok {
				cost = c
			}
		}

		// Compute V2 derived fields (zero-valued when unavailable, backward compatible)
		var totalKvCapacityTokens int64
		var tokensInUse int64
		if data.hasCacheConfig {
			// Overflow-safe multiplication: check before computing
			if data.numGpuBlocks > 0 && data.blockSize > math.MaxInt64/data.numGpuBlocks {
				totalKvCapacityTokens = math.MaxInt64
			} else {
				totalKvCapacityTokens = data.numGpuBlocks * data.blockSize
			}
			// Use math.Round for accurate float-to-int conversion and clamp to valid range
			rounded := math.Round(kvUsage * float64(totalKvCapacityTokens))
			if rounded < 0 {
				rounded = 0
			} else if rounded > float64(totalKvCapacityTokens) {
				rounded = float64(totalKvCapacityTokens)
			}
			tokensInUse = int64(rounded)
		}

		// Look up MaxBatchSize from the scale target's engine args via the VA's ScaleTargetRef
		var maxBatchSize int64
		if va, ok := variantAutoscalings[variantKey]; ok && va != nil {
			key := utils.GetNamespacedKey(namespace, va.Spec.ScaleTargetRef.Name)
			if mbs, ok := scaleTargetMaxBatchSize[key]; ok {
				maxBatchSize = mbs
			}
		}

		if (data.hasKv || data.hasQueue) && !data.hasArrivalRate {
			logger.Info("Pod has engine metrics but no dispatch rate — possible pod/pod_name label mismatch", "pod", podName, "model", modelID, "namespace", namespace)
		}

		// Track freshness for metrics in this pod
		trackMetricFreshness(vaName, data, collectedAt, vaMetricsFreshnessStatus)
		metric := interfaces.ReplicaMetrics{
			PodName:               podName,
			ModelID:               modelID,
			Namespace:             namespace,
			VariantName:           vaName,
			AcceleratorName:       acceleratorName,
			KvCacheUsage:          kvUsage,
			QueueLength:           queueLen,
			Cost:                  cost,
			NumGpuBlocks:          data.numGpuBlocks,
			BlockSize:             data.blockSize,
			TotalKvCapacityTokens: totalKvCapacityTokens,
			TokensInUse:           tokensInUse,
			AvgOutputTokens:       data.avgOutputTokens,
			AvgInputTokens:        data.avgInputTokens,
			PrefixCacheHitRate:    data.prefixCacheHitRate,
			ArrivalRate:           data.arrivalRate,
			MaxBatchSize:          maxBatchSize,
			AvgTTFT:               data.avgTTFT,
			AvgITL:                data.avgITL,
			GenerationTokenRate:   data.generationTokenRate,
			KvUsageInstant:        data.kvUsageInstant,
			RequestRate:           data.requestRate,
			Metadata: &interfaces.ReplicaMetricsMetadata{
				CollectedAt:     collectedAt,
				Age:             0, // Fresh
				FreshnessStatus: "fresh",
			},
		}

		replicaMetrics = append(replicaMetrics, metric)
	}

	for vaName, statuses := range vaMetricsFreshnessStatus {
		for status, count := range statuses {
			metrics.SetMetricsFreshnessStatus(vaName, status, count)
		}
	}

	// Only set this after all pods have been processed, making sure not to include pods without metrics (which are skipped above).
	// This ensures that the discovered pod count reflects only those pods that produced replica metrics.
	metrics.SetMetricsPodsDiscovered(namespace, len(replicaMetrics))
	logger.V(logging.DEBUG).Info("Collected replica metrics",
		"modelID", modelID,
		"namespace", namespace,
		"replicaCount", len(replicaMetrics))

	return replicaMetrics, nil
}

// CollectSchedulerQueueMetrics collects model-level queue metrics from the
// llm-d inference scheduler flow control layer. These metrics are not per-pod
// but per-model, representing requests queued upstream before reaching the engine.
// Returns nil (not an error) when flow control metrics are unavailable.
func (c *ReplicaMetricsCollector) CollectSchedulerQueueMetrics(
	ctx context.Context,
	modelID string,
) *interfaces.SchedulerQueueMetrics {
	logger := ctrl.LoggerFrom(ctx)

	params := map[string]string{
		source.ParamModelID: modelID,
	}

	queries := []string{
		registration.QuerySchedulerQueueSize,
		registration.QuerySchedulerQueueBytes,
	}

	results, err := c.source.Refresh(ctx, source.RefreshSpec{
		Queries: queries,
		Params:  params,
	})
	if err != nil {
		logger.V(logging.DEBUG).Info("Scheduler queue metrics unavailable",
			"modelID", modelID, "error", err)
		return nil
	}

	var queueSize, queueBytes int64
	hasData := false

	if result := results[registration.QuerySchedulerQueueSize]; result != nil && !result.HasError() {
		for _, value := range result.Values {
			if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
				queueSize += int64(value.Value)
				hasData = true
			}
		}
	}

	if result := results[registration.QuerySchedulerQueueBytes]; result != nil && !result.HasError() {
		for _, value := range result.Values {
			if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
				queueBytes += int64(value.Value)
				hasData = true
			}
		}
	}

	if !hasData {
		return nil
	}

	logger.V(logging.DEBUG).Info("Collected scheduler queue metrics",
		"modelID", modelID,
		"queueSize", queueSize,
		"queueBytes", queueBytes)

	return &interfaces.SchedulerQueueMetrics{
		QueueSize:  queueSize,
		QueueBytes: queueBytes,
	}
}

// getScaleTargetNames extracts scale target names from the scale target map.
func getScaleTargetNames(scaleTargets map[string]scaletarget.ScaleTargetAccessor) []string {
	names := make([]string, 0, len(scaleTargets))
	for _, scaleTarget := range scaleTargets {
		names = append(names, scaleTarget.GetName())
	}
	return names
}
