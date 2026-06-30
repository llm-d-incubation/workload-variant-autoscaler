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

package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
)

// Query name constants for EPP saturation metrics.
const (
	// QueryEPPPredictedTTFT is the pool's recent average time-to-first-token in
	// seconds. It prefers the EPP's *predicted* TTFT and falls back to *actual*
	// TTFT when the predicted series is unavailable (PromQL `or`). The analyzer
	// divides this by the configured TTFT SLO to derive saturation. Returns no
	// value (NaN) when there is no recent traffic — the analyzer treats that as 0.
	QueryEPPPredictedTTFT = "epp_predicted_ttft_seconds"

	// QueryEPPPredictedTPOT is the pool's recent average time-per-output-token in
	// seconds, predicted-preferred with actual fallback, analogous to
	// QueryEPPPredictedTTFT. This EPP build does not yet emit predicted TPOT, so
	// the fallback to actual (normalized) TPOT is what currently drives the value.
	QueryEPPPredictedTPOT = "epp_predicted_tpot_seconds"
)

// avgRate builds a PromQL expression for the time-windowed average of a
// histogram (sum/count rate over [1m]).
func avgRate(metric string) string {
	return "sum(rate(" + metric + "_sum[1m])) / sum(rate(" + metric + "_count[1m]))"
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
		Template:    "(" + avgRate("llm_d_epp_request_predicted_ttft_seconds") + ") or (" + avgRate("llm_d_epp_request_ttft_seconds") + ")",
		Params:      []string{},
		Description: "Pool average TTFT (seconds), predicted-preferred with actual fallback; analyzer divides by the TTFT SLO",
	})

	registry.MustRegister(source.QueryTemplate{
		Name:        QueryEPPPredictedTPOT,
		Type:        source.QueryTypePromQL,
		Template:    "(" + avgRate("llm_d_epp_request_predicted_tpot_seconds") + ") or (" + avgRate("llm_d_epp_request_ntpot_seconds") + ")",
		Params:      []string{},
		Description: "Pool average TPOT (seconds), predicted-preferred with actual (normalized) fallback; analyzer divides by the TPOT SLO",
	})
}
