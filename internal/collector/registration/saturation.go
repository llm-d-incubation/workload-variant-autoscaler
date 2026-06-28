package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
)

// Query name constants for type-safe query references.
const (
	// Saturation queries (per-pod peak metrics over time windows)
	QueryKvCacheUsage = "kv_cache_usage"
	QueryQueueLength  = "queue_length"

	// V2 queries (token-based capacity analysis)
	QueryCacheConfigInfo    = "cache_config_info"
	QueryAvgOutputTokens    = "avg_output_tokens"
	QueryAvgInputTokens     = "avg_input_tokens"
	QueryPrefixCacheHitRate = "prefix_cache_hit_rate"

	// Scheduler flow control queries (model-level, from inference scheduler)
	QuerySchedulerQueueSize  = "scheduler_queue_size"
	QuerySchedulerQueueBytes = "scheduler_queue_bytes"
)

// RegisterSaturationQueries registers queries used by the saturation analyzer.
func RegisterSaturationQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// KV cache usage per instance (peak over last minute)
	// Uses max_over_time to catch saturation events between scrapes
	// Preserves instance (IP:port for multi-vLLM pods), pod (for pod lookup), and llm_d_ai_variant (for direct pod-to-VA mapping)
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryKvCacheUsage,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (max_over_time(vllm:kv_cache_usage_perc{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Peak KV cache utilization per instance (0.0-1.0) over last minute",
	})

	// Queue length per instance (peak over last minute)
	// Uses max_over_time to catch burst traffic
	// Preserves instance (IP:port for multi-vLLM pods), pod (for pod lookup), and llm_d_ai_variant (for direct pod-to-VA mapping)
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryQueueLength,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (max_over_time(vllm:num_requests_waiting{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Peak queue length per instance over last minute",
	})

	// --- V2 queries for token-based capacity analysis ---

	// Cache config info per instance (static labels with block size and GPU blocks count)
	// Uses max to deduplicate when multiple series exist per instance with different label combinations
	// Used by Saturation Analyzer V2 for token capacity computation
	// Preserves instance (IP:port for multi-vLLM pods), pod (for pod lookup), llm_d_ai_variant (for direct pod-to-VA mapping), and config labels
	//
	// NOTE: vllm:cache_config_info is an info-style metric. Unlike vLLM's regular
	// gauges/counters, it is NOT labeled with model_name — its label set is derived
	// from CacheConfig fields (num_gpu_blocks, block_size, cache_dtype, ...) plus
	// "engine". Filtering it by model_name would match nothing, so it is queried
	// namespace-wide and the collector correlates the results to this model's pods
	// by instance key (see CollectReplicaMetrics, which attaches cache config only
	// to instances already discovered by the model-scoped KV/queue queries).
	// Do not add a model_name matcher here.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryCacheConfigInfo,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant, num_gpu_blocks, block_size) (vllm:cache_config_info{namespace="{{.namespace}}"})`,
		Params:      []string{source.ParamNamespace},
		Description: "KV cache configuration info per instance (num_gpu_blocks and block_size as labels)",
	})

	// Average output (generation) tokens per completed request
	// Used for output-length-dependent k2 estimation
	// Preserves instance (IP:port for multi-vLLM pods), pod (for pod lookup), and llm_d_ai_variant (for direct pod-to-VA mapping)
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgOutputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (rate(vllm:request_generation_tokens_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]) / rate(vllm:request_generation_tokens_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Average output tokens per completed request (5m rate)",
	})

	// Average input (prompt) tokens per completed request
	// Used in k2 derivation formula: k2 = N_max × (I + O/2)
	// Preserves instance (IP:port for multi-vLLM pods), pod (for pod lookup), and llm_d_ai_variant (for direct pod-to-VA mapping)
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgInputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (rate(vllm:request_prompt_tokens_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]) / rate(vllm:request_prompt_tokens_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Average input tokens per completed request (5m rate)",
	})

	// Prefix cache hit rate per instance (5m rate)
	// Used to reduce estimated input token demand for scheduler-queued requests.
	// Returns 0..1 where 1 means all prefix lookups were cache hits.
	// Preserves instance (IP:port for multi-vLLM pods), pod (for pod lookup), and llm_d_ai_variant (for direct pod-to-VA mapping)
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryPrefixCacheHitRate,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (rate(vllm:prefix_cache_hits{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]) / rate(vllm:prefix_cache_queries{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Prefix cache hit rate per instance (0.0-1.0, 5m rate)",
	})

	// --- Scheduler flow control queries (model-level) ---
	// These come from the llm-d inference scheduler, not vLLM pods.
	// They use target_model_name when available, falling back to model_name.
	// The "or" clause handles cases where target_model_name is not set.
	//
	// TODO(#2309): These metrics currently lack a namespace label in the upstream
	// gateway-api-inference-extension EPP. If the same model name exists in
	// different namespaces, these queries will aggregate across all of them.
	// Once the upstream adds a namespace label, these queries should filter by it.

	// Number of requests queued in the scheduler's flow control layer
	registry.MustRegister(source.QueryTemplate{
		Name: QuerySchedulerQueueSize,
		Type: source.QueryTypePromQL,
		Template: `sum(inference_extension_flow_control_queue_size{target_model_name="{{.modelID}}"})` +
			` or sum(inference_extension_flow_control_queue_size{model_name="{{.modelID}}",target_model_name=""})`,
		Params:      []string{source.ParamModelID},
		Description: "Total requests queued in scheduler flow control for this model",
	})

	// Total bytes of request bodies queued in the scheduler's flow control layer
	registry.MustRegister(source.QueryTemplate{
		Name: QuerySchedulerQueueBytes,
		Type: source.QueryTypePromQL,
		Template: `sum(inference_extension_flow_control_queue_bytes{target_model_name="{{.modelID}}"})` +
			` or sum(inference_extension_flow_control_queue_bytes{model_name="{{.modelID}}",target_model_name=""})`,
		Params:      []string{source.ParamModelID},
		Description: "Total bytes queued in scheduler flow control for this model",
	})

}
