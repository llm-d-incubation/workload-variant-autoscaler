// Package constants provides centralized constant definitions for the autoscaler.
// This file contains metric-related constants (VLLM input metrics, WVA output metrics, and metric label names).
package constants

// VLLM Input Metrics
// These metric names are used to query VLLM (vLLM inference engine) metrics from Prometheus.
// The metrics are emitted by VLLM servers and consumed by the collector to make scaling decisions.
const (
	// VLLMNumRequestRunning tracks the current number of running requests.
	// Used to validate metrics availability.
	VLLMNumRequestRunning = "vllm:num_requests_running"

	// VLLMRequestSuccessTotal tracks the total number of successful requests.
	// Used to calculate arrival rate.
	VLLMRequestSuccessTotal = "vllm:request_success_total"

	// VLLMRequestPromptTokensSum tracks the sum of prompt tokens across all requests.
	// Used with VLLMRequestPromptTokensCount to calculate average output tokens.
	VLLMRequestPromptTokensSum = "vllm:request_prompt_tokens_sum"

	// VLLMRequestPromptTokensCount tracks the count of requests for token generation.
	// Used with VLLMRequestPromptTokensSum to calculate average output tokens.
	VLLMRequestPromptTokensCount = "vllm:request_prompt_tokens_count"

	// VLLMRequestGenerationTokensSum tracks the sum of generated tokens across all requests.
	// Used with VLLMRequestGenerationTokensCount to calculate average output tokens.
	VLLMRequestGenerationTokensSum = "vllm:request_generation_tokens_sum"

	// VLLMRequestGenerationTokensCount tracks the count of requests for token generation.
	// Used with VLLMRequestGenerationTokensSum to calculate average output tokens.
	VLLMRequestGenerationTokensCount = "vllm:request_generation_tokens_count"

	// VLLMTimeToFirstTokenSecondsSum tracks the sum of TTFT (Time To First Token) across all requests.
	// Used with VLLMTimeToFirstTokenSecondsCount to calculate TTFT.
	VLLMTimeToFirstTokenSecondsSum = "vllm:time_to_first_token_seconds_sum"

	// VLLMTimeToFirstTokenSecondsCount tracks the count of requests for TTFT.
	// Used with VLLMTimeToFirstTokenSecondsSum to calculate TTFT.
	VLLMTimeToFirstTokenSecondsCount = "vllm:time_to_first_token_seconds_count"

	// VLLMTimePerOutputTokenSecondsSum tracks the sum of time per output token across all requests.
	// Used with VLLMTimePerOutputTokenSecondsCount to calculate ITL (Inter-Token Latency).
	VLLMTimePerOutputTokenSecondsSum = "vllm:time_per_output_token_seconds_sum"

	// VLLMTimePerOutputTokenSecondsCount tracks the count of requests for time per output token.
	// Used with VLLMTimePerOutputTokenSecondsSum to calculate ITL (Inter-Token Latency).
	VLLMTimePerOutputTokenSecondsCount = "vllm:time_per_output_token_seconds_count"

	// VLLMKvCacheUsagePerc tracks the KV cache utilization as a percentage (0.0-1.0).
	// Used by saturation analyzer to detect KV cache saturation and prevent OOM errors.
	VLLMKvCacheUsagePerc = "vllm:kv_cache_usage_perc"

	// VLLMNumRequestsWaiting tracks the number of requests waiting in the queue.
	// Used by saturation analyzer to detect request queue saturation.
	VLLMNumRequestsWaiting = "vllm:num_requests_waiting"

	// VLLMCacheConfigInfo is an info-style gauge that exposes KV cache configuration as labels.
	// Labels include num_gpu_blocks, block_size, cache_dtype, etc.
	// Value is always 1.0. Used by Saturation Analyzer V2 for token capacity computation.
	VLLMCacheConfigInfo = "vllm:cache_config_info"

	// VLLMPrefixCacheHits is a counter of prefix cache block hits.
	// Used with VLLMPrefixCacheQueries to compute prefix cache hit rate.
	VLLMPrefixCacheHits = "vllm:prefix_cache_hits"

	// VLLMPrefixCacheQueries is a counter of prefix cache block queries.
	// Used with VLLMPrefixCacheHits to compute prefix cache hit rate.
	VLLMPrefixCacheQueries = "vllm:prefix_cache_queries"
)

// llm-d Inference Scheduler Flow Control Metrics
// These metrics come from the Gateway API Inference Extension EPP (Endpoint Picker)
// flow control layer, not from vLLM pods. They are model-scoped (not per-pod).
//
// TODO(#2309): These metrics currently lack a namespace label upstream.
// If the same model and inference pool names exist in different namespaces,
// the metrics will collide. See gateway-api-inference-extension issue #2309.
const (
	// SchedulerFlowControlQueueSize is the number of requests queued in the
	// inference scheduler's flow control layer.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: no namespace label — see TODO(#2309) above.
	SchedulerFlowControlQueueSize = "inference_extension_flow_control_queue_size"

	// SchedulerFlowControlQueueBytes is the total bytes of request bodies queued
	// in the inference scheduler's flow control layer.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: no namespace label — see TODO(#2309) above.
	SchedulerFlowControlQueueBytes = "inference_extension_flow_control_queue_bytes"
)

// WVA Output Metrics
// These metric names are used to emit WVA (Workload Variant Autoscaler) metrics to Prometheus.
// The metrics expose scaling decisions and current state for monitoring and alerting.
const (
	// WVAReplicaScalingTotal is a counter that tracks the total number of scaling operations.
	// Labels: variant_name, namespace, direction (up/down), reason, accelerator_type
	WVAReplicaScalingTotal = "wva_replica_scaling_total"

	// WVADesiredReplicas is a gauge that tracks the desired number of replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVADesiredReplicas = "wva_desired_replicas"

	// WVACurrentReplicas is a gauge that tracks the current number of replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVACurrentReplicas = "wva_current_replicas"

	// WVADesiredRatio is a gauge that tracks the ratio of desired to current replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVADesiredRatio = "wva_desired_ratio"

	// WVAOptimizationDurationSeconds is a histogram that tracks the duration of each optimization cycle.
	// Labels: status (success, error)
	WVAOptimizationDurationSeconds = "wva_optimization_duration_seconds"

	// WVAModelsProcessed is a gauge that tracks the number of models processed in the last optimization cycle.
	WVAModelsProcessed = "wva_models_processed"
)

// Metric Label Names
// Common label names used across metrics for consistency.
const (
	LabelModelName          = "model_name"
	LabelNamespace          = "namespace"
	LabelVariantName        = "variant_name"
	LabelDirection          = "direction"
	LabelReason             = "reason"
	LabelAcceleratorType    = "accelerator_type"
	LabelControllerInstance = "controller_instance"
	LabelStatus             = "status"
)
