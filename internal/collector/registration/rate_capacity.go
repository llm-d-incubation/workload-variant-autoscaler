// Package registration provides query registration for metrics sources.
// This file holds the per-pod rate queries shared by the V2 saturation analyzer's
// rate-anchored capacity estimator and the throughput analyzer.
package registration

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// RegisterRateCapacityQueries registers the per-pod rate queries the V2 saturation
// analyzer needs for rate-anchored capacity: the request completion rate (mu) and
// the instantaneous KV utilization.
//
// Both were originally registered by RegisterThroughputAnalyzerQueries, which runs
// only when the throughput analyzer is enabled. The rate-anchored capacity estimator
// must not depend on that analyzer, so the definitions live here and both registrars
// use the register-if-absent helpers below. Either may run first, and running both
// is a no-op for the shared queries rather than a duplicate-registration panic.
func RegisterRateCapacityQueries(sourceRegistry *source.SourceRegistry) {
	metricsSource := sourceRegistry.Get("prometheus")
	if metricsSource == nil {
		ctrl.Log.V(logging.DEBUG).Info("Prometheus source not registered, skipping rate capacity query registration")
		return
	}
	registry := metricsSource.QueryList()

	registerIfAbsent(registry, kvUsageInstantQuery())
	registerIfAbsent(registry, queueLengthInstantQuery())
	registerIfAbsent(registry, requestRateQuery())
	registerIfAbsent(registry, promptTokenRateQuery())
	registerIfAbsent(registry, inferenceTimeQuery())
	registerForEngineIfAbsent(registry, inferenceengine.EngineSGLang, sglangKvUsageInstantQuery())
	registerForEngineIfAbsent(registry, inferenceengine.EngineSGLang, sglangQueueLengthInstantQuery())
	registerForEngineIfAbsent(registry, inferenceengine.EngineSGLang, sglangRequestRateQuery())
	registerForEngineIfAbsent(registry, inferenceengine.EngineSGLang, sglangPromptTokenRateQuery())
}

// registerIfAbsent registers tmpl unless a query of that name already exists.
// Unlike MustRegister it does not panic on a second registration, which is what
// allows two independent registrars to declare the same shared query.
func registerIfAbsent(registry *source.QueryList, tmpl source.QueryTemplate) {
	if registry.Get(tmpl.Name) != nil {
		return
	}
	registry.MustRegister(tmpl)
}

// registerForEngineIfAbsent is registerIfAbsent for an engine-scoped query name.
// engine is a parameter rather than a constant so a second engine can be added
// without reshaping the call sites, matching registerForEngine.
func registerForEngineIfAbsent(registry *source.QueryList, engine inferenceengine.Engine, tmpl source.QueryTemplate) { //nolint:unparam // mirrors registerForEngine; more engines are expected
	tmpl.Name = EngineQuery(engine, tmpl.Name)
	registerIfAbsent(registry, tmpl)
}

// kvUsageInstantQuery is the per-pod instantaneous KV cache utilization (0.0–1.0).
//
// Deliberately NOT max_over_time: the saturation analyzer's demand path wants the
// 1-minute peak (erring high is the safe direction for demand), while a capacity
// estimate anchored on a peak over-states what the replica can hold. Both readings
// of the same underlying metric are needed, for opposite purposes.
func kvUsageInstantQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryKvUsageInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (vllm:kv_cache_usage_perc{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Instantaneous KV cache utilization per pod (0.0–1.0); operating point for capacity estimation",
	}
}

// queueLengthInstantQuery is the per-pod count of requests waiting right now.
//
// QueryQueueLength is the same counter under max_over_time(...[1m]), which the demand
// path wants: erring high on how much work is outstanding is the safe direction. A
// capacity estimator cannot use it, because the peak latches for a full minute after
// a queue drains — long enough to record occupancy from a replica that is now keeping
// up comfortably and call it the occupancy at which the replica could not keep up.
// The gate and the measurement have to be read at the same instant.
func queueLengthInstantQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryQueueLengthInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (vllm:num_requests_waiting{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Requests waiting per pod right now; the gate for capacity measurement",
	}
}

// sglangQueueLengthInstantQuery is the SGLang form of queueLengthInstantQuery.
func sglangQueueLengthInstantQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryQueueLengthInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (sglang:num_queue_reqs{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Requests waiting per pod right now; the gate for capacity measurement (SGLang)",
	}
}

// requestRateQuery is the per-pod request completion rate (req/s), derived from the
// generation-tokens histogram _count, which increments once per completed request.
//
// While a replica has a backlog its completion rate is its service rate, whichever
// resource binds — that is what makes it usable as mu for capacity estimation.
func requestRateQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryRequestRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(vllm:request_generation_tokens_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Request completion rate per pod (req/s); service rate under backlog, fallback for λ_dec without EPP",
	}
}

// promptTokenRateQuery is the per-pod rate of prompts processed (req/s), from the
// prompt-tokens histogram _count, which increments once per prefilled request.
//
// A prefill replica in a disaggregated deployment completes few or no generations,
// so the generation-tokens counter that measures a decode replica's service rate
// says nothing about it. Prompts processed is the same quantity for the prefill
// role, and the counter is already collected for average input tokens.
func promptTokenRateQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryPromptTokenRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(vllm:request_prompt_tokens_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Prompts processed per pod (req/s); service rate of a prefill replica",
	}
}

// sglangPromptTokenRateQuery is the SGLang form of promptTokenRateQuery.
func sglangPromptTokenRateQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryPromptTokenRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(sglang:prompt_tokens_histogram_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Prompts processed per pod (req/s); service rate of a prefill replica (SGLang)",
	}
}

// inferenceTimeQuery is the average seconds a request spends in the RUNNING phase —
// being served, with time queued excluded.
//
// This is the residence Little's law needs, measured rather than derived. It is
// deliberately NOT in EngineSpecificQueries and has no SGLang form, because SGLang
// publishes nothing equivalent: its seventeen metrics expose end-to-end latency, time
// to first token and time per output token, all of which either include queue wait or
// cover only part of the request. Left engine-agnostic, the bare query simply returns
// nothing on an SGLang fleet and serviceResidence derives the figure instead.
func inferenceTimeQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name: QueryInferenceTime,
		Type: source.QueryTypePromQL,
		Template: `max by (instance, pod, llm_d_ai_variant) (` +
			`rate(vllm:request_inference_time_seconds_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]) / ` +
			`rate(vllm:request_inference_time_seconds_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Seconds per request in the RUNNING phase, queue wait excluded (vLLM only)",
	}
}

// sglangKvUsageInstantQuery is the SGLang form of kvUsageInstantQuery.
func sglangKvUsageInstantQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryKvUsageInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (sglang:token_usage{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Instantaneous KV cache utilization per pod (0.0–1.0); operating point for capacity estimation (SGLang)",
	}
}

// sglangRequestRateQuery is the SGLang form of requestRateQuery.
func sglangRequestRateQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryRequestRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(sglang:generation_tokens_histogram_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Request completion rate per pod (req/s); service rate under backlog, fallback for λ_dec without EPP (SGLang)",
	}
}
