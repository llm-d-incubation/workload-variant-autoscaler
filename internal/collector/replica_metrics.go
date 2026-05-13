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
// vLLM metrics are typically scraped via a PodMonitor or ServiceMonitor that
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
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	saturation_v2 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation_v2"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// ReplicaMetricsCollector collects replica-level metrics for both saturation
// analysis and queueing model analysis using the source infrastructure.
type ReplicaMetricsCollector struct {
	source      source.MetricsSource
	k8sClient   client.Client
	podVAMapper *source.PodVAMapper
}

// NewReplicaMetricsCollector creates a new replica metrics collector.
func NewReplicaMetricsCollector(metricsSource source.MetricsSource, k8sClient client.Client) *ReplicaMetricsCollector {
	return &ReplicaMetricsCollector{
		source:      metricsSource,
		k8sClient:   k8sClient,
		podVAMapper: source.NewPodVAMapper(k8sClient),
	}
}

// CollectReplicaMetrics collects per-replica metrics for all replicas of a model.
// The collected metrics serve both the saturation analyzer and the queueing model analyzer:
//   - Saturation metrics: KV cache usage, queue length, token capacity, prefix cache hit rate
//   - Queueing model metrics: scheduler dispatch rate (arrival rate), max batch size
//
// Prometheus-sourced metrics are fetched via registered query templates.
// MaxBatchSize is parsed from the Deployment/LWS's container args (--max-num-seqs).
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
	variantCosts map[string]float64,
) ([]interfaces.ReplicaMetrics, error) {
	logger := ctrl.LoggerFrom(ctx)

	params := map[string]string{
		source.ParamModelID:   modelID,
		source.ParamNamespace: namespace,
	}

	// Refresh all Prometheus-sourced queries:
	// - Saturation: KV cache, queue length, cache config, prefix cache hit rate
	// - Shared (saturation + queueing model): avg input tokens, avg output tokens
	// - Queueing model: scheduler dispatch rate, avg TTFT, avg ITL
	// - Throughput analyzer: generation token rate, instantaneous KV usage (k*), vLLM request rate
	queries := []string{
		registration.QueryKvCacheUsage,
		registration.QueryQueueLength,
		registration.QueryCacheConfigInfo,
		registration.QueryAvgOutputTokens,
		registration.QueryAvgInputTokens,
		registration.QueryPrefixCacheHitRate,
		registration.QuerySchedulerDispatchRate,
		registration.QueryAvgTTFT,
		registration.QueryAvgITL,
		registration.QueryGenerationTokenRate,
		registration.QueryKvUsageInstant,
		registration.QueryVLLMRequestRate,
	}

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

	// podMetricData holds per-pod metric values and timestamps
	type podMetricData struct {
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
		vllmRequestRate     float64
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
			podName := value.Labels["pod"]
			if podName == "" {
				podName = value.Labels["pod_name"]
			}
			if podName == "" {
				continue
			}

			if podData[podName] == nil {
				podData[podName] = &podMetricData{}
			}
			podData[podName].kvUsage = value.Value
			podData[podName].kvTimestamp = value.Timestamp
			podData[podName].hasKv = true

			logger.V(logging.DEBUG).Info("KV cache metric",
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
			podName := value.Labels["pod"]
			if podName == "" {
				podName = value.Labels["pod_name"]
			}
			if podName == "" {
				continue
			}

			if podData[podName] == nil {
				podData[podName] = &podMetricData{}
			}
			podData[podName].queueLen = int(value.Value)
			podData[podName].queueTimestamp = value.Timestamp
			podData[podName].hasQueue = true

			logger.V(logging.DEBUG).Info("Queue metric",
				"pod", podName,
				"queueLength", int(value.Value))
		}
	}

	// Process cache config info results (V2)
	if result := results[registration.QueryCacheConfigInfo]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				podName := value.Labels["pod"]
				if podName == "" {
					podName = value.Labels["pod_name"]
				}
				if podName == "" {
					continue
				}

				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}

				// Parse num_gpu_blocks and block_size from string labels
				if blocksStr, ok := value.Labels["num_gpu_blocks"]; ok && blocksStr != "" {
					if blocks, err := strconv.ParseInt(blocksStr, 10, 64); err == nil {
						podData[podName].numGpuBlocks = blocks
					}
				}
				if sizeStr, ok := value.Labels["block_size"]; ok && sizeStr != "" {
					if size, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
						podData[podName].blockSize = size
					}
				}
				if podData[podName].numGpuBlocks > 0 && podData[podName].blockSize > 0 {
					podData[podName].hasCacheConfig = true
					podData[podName].cacheConfigTimestamp = value.Timestamp
				}

				logger.V(logging.DEBUG).Info("Cache config info metric",
					"pod", podName,
					"numGpuBlocks", podData[podName].numGpuBlocks,
					"blockSize", podData[podName].blockSize)
			}
		}
	}

	// Process average output tokens results (V2)
	if result := results[registration.QueryAvgOutputTokens]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				podName := value.Labels["pod"]
				if podName == "" {
					podName = value.Labels["pod_name"]
				}
				if podName == "" {
					continue
				}

				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}
				// NaN check: rate division by zero produces NaN
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
					podData[podName].avgOutputTokens = value.Value
					podData[podName].avgOutputTokensTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process average input tokens results (V2)
	if result := results[registration.QueryAvgInputTokens]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				podName := value.Labels["pod"]
				if podName == "" {
					podName = value.Labels["pod_name"]
				}
				if podName == "" {
					continue
				}

				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}
				// NaN check: rate division by zero produces NaN
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
					podData[podName].avgInputTokens = value.Value
					podData[podName].avgInputTokensTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process prefix cache hit rate results (V2)
	if result := results[registration.QueryPrefixCacheHitRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				podName := value.Labels["pod"]
				if podName == "" {
					podName = value.Labels["pod_name"]
				}
				if podName == "" {
					continue
				}

				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}
				// NaN check: rate division by zero produces NaN when no prefix cache queries
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 && value.Value <= 1 {
					podData[podName].prefixCacheHitRate = value.Value
					podData[podName].prefixCacheHitRateTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process scheduler dispatch rate results (arrival rate per pod)
	if result := results[registration.QuerySchedulerDispatchRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				// The scheduler metric has both "pod" (EPP scrape target) and "pod_name"
				// (the vLLM endpoint pod). Prefer pod_name so we join with vLLM metrics.
				podName := value.Labels["pod_name"]
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

				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}
				// NaN check: rate can produce NaN if no successful attempts
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[podName].arrivalRate = value.Value
					podData[podName].hasArrivalRate = true
					podData[podName].arrivalRateTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Scheduler dispatch rate metric",
						"pod", podName,
						"arrivalRate", value.Value)
				}
			}
		}
	}

	// Process average TTFT results (seconds)
	if result := results[registration.QueryAvgTTFT]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				podName := value.Labels["pod"]
				if podName == "" {
					podName = value.Labels["pod_name"]
				}
				if podName == "" {
					continue
				}

				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value > 0 {
					podData[podName].avgTTFT = value.Value
					podData[podName].avgTTFTTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Avg TTFT metric",
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
				podName := value.Labels["pod"]
				if podName == "" {
					podName = value.Labels["pod_name"]
				}
				if podName == "" {
					continue
				}

				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value > 0 {
					podData[podName].avgITL = value.Value
					podData[podName].avgITLTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Avg ITL metric",
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
				podName := value.Labels["pod"]
				if podName == "" {
					podName = value.Labels["pod_name"]
				}
				if podName == "" {
					continue
				}
				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[podName].generationTokenRate = value.Value
				}
			}
		}
	}

	// Process instantaneous KV usage (k*) results (0.0–1.0) — throughput analyzer k*
	if result := results[registration.QueryKvUsageInstant]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				podName := value.Labels["pod"]
				if podName == "" {
					podName = value.Labels["pod_name"]
				}
				if podName == "" {
					continue
				}
				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 && value.Value <= 1 {
					podData[podName].kvUsageInstant = value.Value
				}
			}
		}
	}

	// Process vLLM request completion rate (req/s) — throughput analyzer fallback λ_req
	if result := results[registration.QueryVLLMRequestRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				podName := value.Labels["pod"]
				if podName == "" {
					podName = value.Labels["pod_name"]
				}
				if podName == "" {
					continue
				}
				if podData[podName] == nil {
					podData[podName] = &podMetricData{}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[podName].vllmRequestRate = value.Value
				}
			}
		}
	}

	// Pre-compute MaxBatchSize per scale target from container args.
	// MaxBatchSize (--max-num-seqs) is not a Prometheus metric; it is parsed
	// from the Deployment/LWS spec using the vLLM argument parser.
	// Map key is scale target key (namespace/name).
	scaleTargetMaxBatchSize := make(map[string]int64, len(scaleTargets))
	for key, scaleTarget := range scaleTargets {
		params := saturation_v2.ParseVLLMArgs(scaleTarget)
		scaleTargetMaxBatchSize[key] = params.MaxNumSeqs
	}

	// Track metrics freshness status per pod
	vaMetricsFreshnessStatus := make(map[string]map[string]int)

	// Build replica metrics from pod data
	replicaMetrics := make([]interfaces.ReplicaMetrics, 0, len(podData))
	metrics.SetMetricsPodsDiscovered(namespace, len(podData))
	collectedAt := time.Now()

	for podName, data := range podData {
		// Match Pod to VariantAutoscaling using indexed lookup
		vaName := c.podVAMapper.FindVAForPod(ctx, podName, namespace, scaleTargets)

		// Track freshness for metrics in this pod for this variant right away
		trackMetricFreshness(vaName, data, collectedAt, vaMetricsFreshnessStatus)

		// Skip pods that have no metrics at all
		if !data.hasKv && !data.hasQueue {
			continue
		}

		kvUsage := data.kvUsage
		queueLen := data.queueLen

		if !data.hasKv {
			logger.Info("Pod missing KV cache metrics, using 0",
				"pod", podName,
				"model", modelID,
				"namespace", namespace)
			kvUsage = 0
		}
		if !data.hasQueue {
			logger.Info("Pod missing queue metrics, using 0",
				"pod", podName,
				"model", modelID,
				"namespace", namespace)
			queueLen = 0
		}

		if vaName == "" {
			logger.Info("Skipping pod that doesn't match any scale target",
				"pod", podName,
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

		// Look up MaxBatchSize from the scale target's vLLM args via the VA's ScaleTargetRef
		var maxBatchSize int64
		if va, ok := variantAutoscalings[variantKey]; ok && va != nil {
			key := utils.GetNamespacedKey(namespace, va.Spec.ScaleTargetRef.Name)
			if mbs, ok := scaleTargetMaxBatchSize[key]; ok {
				maxBatchSize = mbs
			}
		}

		if (data.hasKv || data.hasQueue) && !data.hasArrivalRate {
			logger.Info("Pod has vLLM metrics but no dispatch rate — possible pod/pod_name label mismatch", "pod", podName, "model", modelID, "namespace", namespace)
		}

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
			VLLMRequestRate:       data.vllmRequestRate,
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

	logger.V(logging.DEBUG).Info("Collected replica metrics",
		"modelID", modelID,
		"namespace", namespace,
		"replicaCount", len(replicaMetrics))

	return replicaMetrics, nil
}

// CollectSchedulerQueueMetrics collects model-level queue metrics from the
// llm-d inference scheduler flow control layer. These metrics are not per-pod
// but per-model, representing requests queued upstream before reaching vLLM.
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
