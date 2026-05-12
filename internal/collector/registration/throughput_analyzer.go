// Package registration provides query registration for metrics sources.
// This file registers queries used by the throughput analyzer (ThroughputAnalyzer).
package registration

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// Query name constants for throughput analyzer metrics.
//
// Only three queries are registered here — those that are genuinely new and not
// provided by other analyzer registrations. The remaining TA inputs are already
// collected and exposed via interfaces.ReplicaMetrics; the TA reads those fields
// directly instead of re-registering duplicate PromQL templates.
//
// TA notation → ReplicaMetrics field (query / registration):
//
//	KV_max  (total KV token capacity) → TotalKvCapacityTokens  (QueryCacheConfigInfo       / RegisterSaturationQueries)
//	ITL_obs (observed ITL, seconds)   → AvgITL                 (QueryAvgITL                / RegisterQueueingModelQueries)
//	OL      (avg output tokens)       → AvgOutputTokens        (QueryAvgOutputTokens       / RegisterSaturationQueries)
//	IL      (avg input tokens)        → AvgInputTokens         (QueryAvgInputTokens        / RegisterSaturationQueries)
//	H%      (prefix cache hit rate)   → PrefixCacheHitRate     (QueryPrefixCacheHitRate    / RegisterSaturationQueries)
//	λ_req   (per-pod arrival rate)    → ArrivalRate            (QuerySchedulerDispatchRate / RegisterQueueingModelQueries)
//	         λ_dec = Σ_{r∈V}(ArrivalRate_r × AvgOutputTokens_r) per variant V, computed in analyzer
const (
	// QueryGenerationTokenRate is the query name for the observed generation
	// (decode) token rate per pod (tokens/sec).
	// This is the direct observable proxy for μ_dec^obs — how many tokens each
	// replica is currently generating per second.
	// Source: vllm:request_generation_tokens_sum (histogram _sum counter)
	QueryGenerationTokenRate = "generation_token_rate"

	// QueryKvUsageInstant is the query name for the instantaneous KV cache utilization
	// fraction per pod (0.0–1.0). Used as k* (current operating point) in the ITL
	// model: ITL(k) = A·k + B.
	//
	// Same underlying metric as QueryKvCacheUsage (vllm:kv_cache_usage_perc), but
	// without max_over_time. QueryKvCacheUsage wraps the gauge in max_over_time[1m]
	// to give the saturation analyzer a conservative peak. This query reads the raw
	// gauge so the throughput analyzer sees the current operating point, not a
	// 1-minute high-water mark that could overestimate load and trigger premature
	// scale-up after a transient spike.
	//
	// max by (pod): deduplication only. vllm:kv_cache_usage_perc is a single scalar
	// gauge per vLLM process; there is one series per pod in normal deployment. The
	// max by (pod) collapses any duplicate series that arise when a pod is scraped by
	// multiple targets (e.g., PodMonitor + ServiceMonitor). Since duplicates carry the
	// same value, max = avg — the choice has no effect on correctness.
	// Source: vllm:kv_cache_usage_perc (gauge)
	QueryKvUsageInstant = "kv_usage_instant"

	// QueryVLLMRequestRate is the query name for the vLLM-side request completion
	// rate per pod (req/s), derived from the generation tokens histogram count.
	//
	// Used as a fallback for λ_dec estimation when EPP/scheduler metrics are
	// unavailable (ArrivalRate == 0 for all pods). Per variant V, the analyzer computes:
	//   λ_dec_vllm = Σ_{r∈V}(VLLMRequestRate_r × AvgOutputTokens_r)
	//
	// Note: measures completed requests (served demand), not arriving requests.
	// It undercounts when requests are queued in the scheduler. Use
	// ArrivalRate (via QuerySchedulerDispatchRate) as the primary demand source.
	// Source: vllm:request_generation_tokens_count (histogram _count counter)
	QueryVLLMRequestRate = "vllm_request_rate"
)

// RegisterThroughputAnalyzerQueries registers the three TA-exclusive queries.
// It must be called once at engine startup alongside other analyzer registrations.
//
// Registered queries:
//   - QueryGenerationTokenRate — μ_dec^obs: observed decode token rate per pod
//   - QueryKvUsageInstant      — k*: instantaneous KV cache utilization per pod
//   - QueryVLLMRequestRate     — fallback λ_req: completion rate per pod when EPP absent
//
// Additional TA inputs are read from interfaces.ReplicaMetrics fields populated by
// RegisterSaturationQueries (TotalKvCapacityTokens, AvgOutputTokens, AvgInputTokens,
// PrefixCacheHitRate) and RegisterQueueingModelQueries (AvgITL, ArrivalRate).
// See the package-level constant block for the full TA notation → field mapping.
//
// μ_dec is computed using a linear ITL model:
//
//	ITL(k)   = A·k + B            (calibrated from AvgITL × k* pairs over time)
//	IL_eff   = IL × (1 - H%)
//	KV_req   = IL_eff + OL/2
//	N_dec(k) = k × KV_max / KV_req
//	μ_dec    = N_dec(k_sat) / ITL(k_sat)
//
// Per variant V (summed over that variant's replicas only):
// λ_dec primary:  Σ_{r∈V}(ArrivalRate_r × AvgOutputTokens_r)     [EPP deployed]
// λ_dec fallback: Σ_{r∈V}(VLLMRequestRate_r × AvgOutputTokens_r) [EPP absent]
func RegisterThroughputAnalyzerQueries(sourceRegistry *source.SourceRegistry) {
	metricsSource := sourceRegistry.Get("prometheus")
	if metricsSource == nil {
		ctrl.Log.V(logging.DEBUG).Info("Prometheus source not registered, skipping throughput analyzer query registration")
		return
	}
	registry := metricsSource.QueryList()

	// Per-pod observed generation (decode) token rate (tokens/sec).
	// Computed as the rate of the _sum histogram counter over 1m.
	// Uses sum by (pod) because request_generation_tokens_sum is an additive
	// counter — multiple histogram buckets per pod must be summed.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryGenerationTokenRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (pod) (rate(vllm:request_generation_tokens_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Observed generation (decode) token rate per pod (tokens/sec), proxy for μ_dec^obs",
	})

	// Per-pod instantaneous KV cache utilization (0.0–1.0).
	// Uses max by (pod) to consolidate any duplicate series to a single per-pod value.
	// Does NOT use max_over_time: the throughput analyzer needs the current
	// operating point k*, not the worst-case peak used by the saturation analyzer.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryKvUsageInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (pod) (vllm:kv_cache_usage_perc{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Instantaneous KV cache utilization per pod (0.0–1.0), used as k* in the ITL model",
	})

	// Per-pod vLLM request completion rate (req/s).
	// Derived from the generation tokens histogram _count (increments once per
	// completed request). Used as a fallback for λ_dec when EPP/scheduler metrics
	// are unavailable; per variant V, the analyzer falls back to:
	//   λ_dec_vllm = Σ_{r∈V}(VLLMRequestRate_r × AvgOutputTokens_r)
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryVLLMRequestRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (pod) (rate(vllm:request_generation_tokens_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "vLLM request completion rate per pod (req/s); fallback for λ_dec when EPP metrics are unavailable",
	})
}
