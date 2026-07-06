package registration

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
)

var _ = Describe("RegisterEPPSaturationQueries", func() {
	var registry *source.SourceRegistry

	// register builds a fresh registry and runs query registration, picking up
	// whatever WVA_EPP_*_METRIC environment is in effect.
	register := func() {
		registry = source.NewSourceRegistry()
		metricsSource := prometheus.NewPrometheusSource(context.Background(), &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig())
		Expect(registry.Register("prometheus", metricsSource)).To(Succeed())
		RegisterEPPSaturationQueries(registry)
	}

	template := func(queryName string) string {
		q := registry.Get("prometheus").QueryList().Get(queryName)
		Expect(q).NotTo(BeNil())
		return q.Template
	}

	unsetOverrides := func() {
		for _, envVar := range []string{
			EnvEPPPredictedTTFTMetric, EnvEPPActualTTFTMetric,
			EnvEPPPredictedTPOTMetric, EnvEPPActualTPOTMetric,
		} {
			Expect(os.Unsetenv(envVar)).To(Succeed())
		}
	}

	BeforeEach(unsetOverrides)
	AfterEach(unsetOverrides)

	It("defaults to the llm-d EPP metric names, predicted-preferred with actual fallback", func() {
		register()

		ttft := template(QueryEPPPredictedTTFT)
		Expect(ttft).To(ContainSubstring("llm_d_epp_request_predicted_ttft_seconds_bucket"))
		Expect(ttft).To(ContainSubstring("llm_d_epp_request_ttft_seconds_bucket"))
		// The dead-predictor guard: filter the predicted side before `or`.
		Expect(ttft).To(ContainSubstring(">= 0) or"))

		tpot := template(QueryEPPPredictedTPOT)
		Expect(tpot).To(ContainSubstring("llm_d_epp_request_predicted_tpot_seconds_bucket"))
		Expect(tpot).To(ContainSubstring("llm_d_epp_request_streaming_tpot_seconds_bucket"))
	})

	It("honors WVA_EPP_*_METRIC overrides", func() {
		Expect(os.Setenv(EnvEPPPredictedTTFTMetric, "inference_objective_request_predicted_ttft_seconds")).To(Succeed())
		Expect(os.Setenv(EnvEPPActualTTFTMetric, "inference_objective_request_ttft_seconds")).To(Succeed())
		Expect(os.Setenv(EnvEPPPredictedTPOTMetric, "inference_objective_request_predicted_tpot_seconds")).To(Succeed())
		Expect(os.Setenv(EnvEPPActualTPOTMetric, "inference_objective_request_tpot_seconds")).To(Succeed())
		register()

		ttft := template(QueryEPPPredictedTTFT)
		Expect(ttft).To(ContainSubstring("inference_objective_request_predicted_ttft_seconds_bucket"))
		Expect(ttft).To(ContainSubstring("inference_objective_request_ttft_seconds_bucket"))
		Expect(ttft).NotTo(ContainSubstring("llm_d_epp"))

		tpot := template(QueryEPPPredictedTPOT)
		Expect(tpot).To(ContainSubstring("inference_objective_request_predicted_tpot_seconds_bucket"))
		Expect(tpot).To(ContainSubstring("inference_objective_request_tpot_seconds_bucket"))
		Expect(tpot).NotTo(ContainSubstring("llm_d_epp"))
	})

	It("rejects an override that is not a valid Prometheus metric name", func() {
		Expect(os.Setenv(EnvEPPActualTPOTMetric, `foo{namespace="x"}) or vector(0`)).To(Succeed())
		register()

		tpot := template(QueryEPPPredictedTPOT)
		Expect(tpot).NotTo(ContainSubstring("vector(0"))
		Expect(tpot).To(ContainSubstring("llm_d_epp_request_streaming_tpot_seconds_bucket"))
	})
})
