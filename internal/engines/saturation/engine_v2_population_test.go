package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

var _ = Describe("Engine config-population helpers", func() {

	Describe("scoreForAnalyzer", func() {
		It("returns the configured score when the analyzer is present with a positive score", func() {
			cfg := config.SaturationScalingConfig{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "saturation", Score: 2.5},
					{Name: "throughput", Score: 0.5},
				},
			}
			Expect(scoreForAnalyzer("saturation", cfg)).To(Equal(2.5))
			Expect(scoreForAnalyzer("throughput", cfg)).To(Equal(0.5))
		})

		It("returns 1.0 when the analyzer is absent from config", func() {
			Expect(scoreForAnalyzer("unknown", config.SaturationScalingConfig{})).To(Equal(1.0))
		})

		It("returns 1.0 when the analyzer's Score is zero (field not set in config)", func() {
			cfg := config.SaturationScalingConfig{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "saturation", Score: 0},
				},
			}
			Expect(scoreForAnalyzer("saturation", cfg)).To(Equal(1.0))
		})

		It("returns the first matching score when multiple entries share a name", func() {
			cfg := config.SaturationScalingConfig{
				Analyzers: []config.AnalyzerScoreConfig{
					{Name: "sat", Score: 3.0},
					{Name: "sat", Score: 7.0}, // duplicate — first wins
				},
			}
			Expect(scoreForAnalyzer("sat", cfg)).To(Equal(3.0))
		})
	})
})
