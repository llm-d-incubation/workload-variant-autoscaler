package registration

import (
	"os"
	"regexp"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Environment variables overriding the EPP latency metric names the saturation
// queries are built from. The defaults are the llm-d EPP's metric contract;
// override them when the EPP build exposes the histograms under different
// names (e.g. the upstream gateway-api-inference-extension
// `inference_objective_*` family, or a renamed fork).
const (
	EnvEPPPredictedTTFTMetric = "WVA_EPP_PREDICTED_TTFT_METRIC"
	EnvEPPActualTTFTMetric    = "WVA_EPP_ACTUAL_TTFT_METRIC"
	EnvEPPPredictedTPOTMetric = "WVA_EPP_PREDICTED_TPOT_METRIC"
	EnvEPPActualTPOTMetric    = "WVA_EPP_ACTUAL_TPOT_METRIC"
)

// Default EPP latency metric names (histogram base names, without the
// `_bucket` suffix).
const (
	DefaultEPPPredictedTTFTMetric = "llm_d_epp_request_predicted_ttft_seconds"
	DefaultEPPActualTTFTMetric    = "llm_d_epp_request_ttft_seconds"
	DefaultEPPPredictedTPOTMetric = "llm_d_epp_request_predicted_tpot_seconds"
	DefaultEPPActualTPOTMetric    = "llm_d_epp_request_streaming_tpot_seconds"
)

// Query name constants for EPP saturation metrics.
const (
	// QueryEPPPredictedTTFT is the pool's recent P90 time-to-first-token in
	// seconds. It prefers the EPP's *predicted* TTFT and falls back to *actual*
	// TTFT when the predicted series is unavailable (PromQL `or`). The analyzer
	// divides this by the configured TTFT SLO to derive saturation. Returns no
	// value (NaN) when there is no recent traffic — the analyzer treats that as 0.
	QueryEPPPredictedTTFT = "epp_predicted_ttft_seconds"

	// QueryEPPPredictedTPOT is the pool's recent P90 time-per-output-token in
	// seconds, predicted-preferred with actual fallback, analogous to
	// QueryEPPPredictedTTFT. The fallback is the actual streaming TPOT
	// histogram, covering EPP builds that don't emit predicted TPOT.
	QueryEPPPredictedTPOT = "epp_predicted_tpot_seconds"
)

// p90Rate builds a PromQL expression for the time-windowed P90 of a histogram
// (histogram_quantile over 1m bucket rates).
//
// The tail quantile — not the mean — is the control signal on purpose. The SLO
// objective is per-request (tail) attainment, and the mean of predicted latency
// stays flat under moderate load (continuous batching absorbs load with little
// mean-latency movement) and only rises after the queue already exists — a
// trailing indicator. The P90 rises as soon as queueing variance appears, giving
// the scale-up signal an early-warning property that the mean lacks. (Observed
// live: a mean-based signal read 0.19–0.28 saturation at a load level where P90
// pressure was already building, firing scale-up ~2–4 min after the knee instead
// of at its onset.)
func p90Rate(metric string) string {
	return "histogram_quantile(0.9, sum(rate(" + metric + "_bucket[1m])) by (le))"
}

// predictedOrActual builds the predicted-preferred / actual-fallback expression.
// The predicted side is filtered with `>= 0` before the `or`: a predicted
// histogram that exists but is not incrementing (stalled predictor sidecar, or a
// build that registers the histogram without ever observing into it) yields a
// present-but-NaN sample (histogram_quantile over all-zero rates), and PromQL
// `or` would return that NaN instead of falling back — the analyzer would then
// read NaN as 0 latency and scale down under real load. NaN fails every
// comparison, so `>= 0` drops the dead predicted sample and lets `or` consult
// the actual-latency signal.
func predictedOrActual(predicted, actual string) string {
	return "((" + p90Rate(predicted) + ") >= 0) or (" + p90Rate(actual) + ")"
}

// validMetricName matches legal Prometheus metric names. Overrides that fail
// it are rejected (falling back to the default) so a typo'd or malformed env
// var cannot produce a broken — or injected — PromQL expression.
var validMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// metricFromEnv returns the metric name from the given environment variable,
// or def when the variable is unset or not a valid Prometheus metric name.
func metricFromEnv(envVar, def string) string {
	name := os.Getenv(envVar)
	if name == "" {
		return def
	}
	if !validMetricName.MatchString(name) {
		ctrl.Log.Info("Ignoring invalid EPP metric name override",
			"envVar", envVar, "value", name, "default", def)
		return def
	}
	return name
}

// RegisterEPPSaturationQueries registers queries used by the EPP saturation analyzer.
//
// The analyzer derives saturation itself (saturation = max(TTFT/SLO, TPOT/SLO))
// from these latency signals rather than reading a pre-computed saturation gauge
// from the EPP, so the SLO policy lives in WVA config. Each query prefers the
// predicted latency and falls back to the actual latency via PromQL `or`. The
// metric names default to the llm-d EPP contract and can be overridden with the
// WVA_EPP_*_METRIC environment variables.
func RegisterEPPSaturationQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	registry.MustRegister(source.QueryTemplate{
		Name: QueryEPPPredictedTTFT,
		Type: source.QueryTypePromQL,
		Template: predictedOrActual(
			metricFromEnv(EnvEPPPredictedTTFTMetric, DefaultEPPPredictedTTFTMetric),
			metricFromEnv(EnvEPPActualTTFTMetric, DefaultEPPActualTTFTMetric)),
		Params:      []string{},
		Description: "Pool P90 TTFT (seconds), predicted-preferred with actual fallback; analyzer divides by the TTFT SLO",
	})

	registry.MustRegister(source.QueryTemplate{
		Name: QueryEPPPredictedTPOT,
		Type: source.QueryTypePromQL,
		Template: predictedOrActual(
			metricFromEnv(EnvEPPPredictedTPOTMetric, DefaultEPPPredictedTPOTMetric),
			metricFromEnv(EnvEPPActualTPOTMetric, DefaultEPPActualTPOTMetric)),
		Params:      []string{},
		Description: "Pool P90 TPOT (seconds), predicted-preferred with actual streaming-TPOT fallback; analyzer divides by the TPOT SLO",
	})
}
