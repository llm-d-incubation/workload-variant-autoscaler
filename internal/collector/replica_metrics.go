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
	saturation_v2 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation_v2"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
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
	}

	results, err := c.source.Refresh(ctx, source.RefreshSpec{
		Queries: queries,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to refresh saturation metrics: %w", err)
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
		numGpuBlocks       int64
		blockSize          int64
		avgOutputTokens    float64
		avgInputTokens     float64
		prefixCacheHitRate float64
		hasCacheConfig     bool
		// Queueing model fields
		arrivalRate    float64
		hasArrivalRate bool
		avgTTFT        float64
		avgITL         float64
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

					logger.V(logging.DEBUG).Info("Avg ITL metric",
						"pod", podName,
						"avgITLSeconds", value.Value)
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

	// Build replica metrics from pod data
	replicaMetrics := make([]interfaces.ReplicaMetrics, 0, len(podData))
	collectedAt := time.Now()

	for podName, data := range podData {
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

		// Match Pod to VariantAutoscaling using indexed lookup
		vaName := c.podVAMapper.FindVAForPod(ctx, podName, namespace, scaleTargets)

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
			Metadata: &interfaces.ReplicaMetricsMetadata{
				CollectedAt:     collectedAt,
				Age:             0, // Fresh
				FreshnessStatus: "fresh",
			},
		}

		replicaMetrics = append(replicaMetrics, metric)
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
