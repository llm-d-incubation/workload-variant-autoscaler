// This file provides queueing model analyzer metrics collection using the source
// infrastructure with registered query templates.
package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
)

// Query name constants for queueing model analyzer metrics.
const (
	// QuerySchedulerDispatchRate is the query name for per-endpoint request dispatch rate from scheduler.
	// This represents the arrival rate (requests/sec) being dispatched to each replica by the scheduler.
	// Source: inference_extension_scheduler_attempts_total (gateway-api-inference-extension)
	QuerySchedulerDispatchRate = "scheduler_dispatch_rate"

	// QueryAvgTTFT is the query name for average time-to-first-token per pod (in seconds).
	// Source: vllm:time_to_first_token_seconds histogram
	QueryAvgTTFT = "avg_ttft"

	// QueryAvgITL is the query name for average inter-token latency per pod (in seconds).
	// Source: vllm:time_per_output_token_seconds histogram
	QueryAvgITL = "avg_itl"
)

// RegisterQueueingModelQueries registers queries used by the queueing model analyzer.
func RegisterQueueingModelQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// Scheduler dispatch rate per endpoint (per-pod arrival rate)
	// Records successful scheduling attempts with endpoint and model information.
	// Metric labels: status, pod_name, namespace, port, model_name, target_model_name
	// We filter by status="success" and match model identity using target_model_name
	// (resolved model after routing, e.g. specific LoRA adapter) with fallback to
	// model_name (original request model) when target_model_name is not set.
	// This follows the same pattern as scheduler flow control queries.
	// Uses sum (not max) because dispatch rate is an additive counter — multiple
	// series per pod should be summed. Uses rate() over 1m window for requests/sec.
	registry.MustRegister(source.QueryTemplate{
		Name: QuerySchedulerDispatchRate,
		Type: source.QueryTypePromQL,
		Template: `sum by (pod_name, namespace) (rate(inference_extension_scheduler_attempts_total{status="success",namespace="{{.namespace}}",target_model_name="{{.modelID}}"}[1m]))` +
			` or sum by (pod_name, namespace) (rate(inference_extension_scheduler_attempts_total{status="success",namespace="{{.namespace}}",model_name="{{.modelID}}",target_model_name=""}[1m]))`,
		Params: []string{source.ParamNamespace, source.ParamModelID},
		Description: "Request dispatch rate per endpoint (requests/sec) from scheduler, " +
			"representing the arrival rate to each replica for a specific model",
	})

	// Average time-to-first-token per pod (seconds).
	// Uses histogram _sum/_count from vLLM over a 1m rate window.
	// Used by queueing model tuner as the observed TTFT for Kalman filter updates.
	registry.MustRegister(source.QueryTemplate{
		Name:     QueryAvgTTFT,
		Type:     source.QueryTypePromQL,
		Template: `max by (pod) (rate(vllm:time_to_first_token_seconds_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]) / rate(vllm:time_to_first_token_seconds_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:   []string{source.ParamNamespace, source.ParamModelID},
		Description: "Average time-to-first-token per pod (seconds), " +
			"used by queueing model tuner for parameter learning",
	})

	// Average inter-token latency per pod (seconds).
	// Uses histogram _sum/_count from vLLM over a 1m rate window.
	// Used by queueing model tuner as the observed ITL for Kalman filter updates.
	registry.MustRegister(source.QueryTemplate{
		Name:     QueryAvgITL,
		Type:     source.QueryTypePromQL,
		Template: `max by (pod) (rate(vllm:time_per_output_token_seconds_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]) / rate(vllm:time_per_output_token_seconds_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:   []string{source.ParamNamespace, source.ParamModelID},
		Description: "Average inter-token latency per pod (seconds), " +
			"used by queueing model tuner for parameter learning",
	})

	// Note: MaxBatchSize (max_num_seqs) is not available as a Prometheus metric from vLLM.
	// It is sourced from the Deployment's container args using the deployment parser
	// (see saturation_v2.ParseVLLMArgs). The collector populates ReplicaMetrics.MaxBatchSize
	// by parsing the --max-num-seqs flag from the pod's parent Deployment spec.
}
