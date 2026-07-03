package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
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
	// QueryEPPPredictedTTFT. This EPP build does not yet emit predicted TPOT, so
	// the fallback to actual (normalized) TPOT is what currently drives the value.
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

// RegisterEPPSaturationQueries registers queries used by the EPP saturation analyzer.
//
// The analyzer derives saturation itself (saturation = max(TTFT/SLO, TPOT/SLO))
// from these latency signals rather than reading a pre-computed saturation gauge
// from the EPP, so the SLO policy lives in WVA config. Each query prefers the
// predicted latency and falls back to the actual latency via PromQL `or`.
func RegisterEPPSaturationQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	registry.MustRegister(source.QueryTemplate{
		Name:        QueryEPPPredictedTTFT,
		Type:        source.QueryTypePromQL,
		Template:    predictedOrActual("llm_d_epp_request_predicted_ttft_seconds", "llm_d_epp_request_ttft_seconds"),
		Params:      []string{},
		Description: "Pool P90 TTFT (seconds), predicted-preferred with actual fallback; analyzer divides by the TTFT SLO",
	})

	registry.MustRegister(source.QueryTemplate{
		Name:        QueryEPPPredictedTPOT,
		Type:        source.QueryTypePromQL,
		Template:    predictedOrActual("llm_d_epp_request_predicted_tpot_seconds", "llm_d_epp_request_ntpot_seconds"),
		Params:      []string{},
		Description: "Pool P90 TPOT (seconds), predicted-preferred with actual (normalized) fallback; analyzer divides by the TPOT SLO",
	})
}
