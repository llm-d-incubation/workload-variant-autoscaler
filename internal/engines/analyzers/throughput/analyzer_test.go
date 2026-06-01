package throughput

import (
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// makeMetrics builds a slice of healthy ReplicaMetrics for a single variant,
// each with a distinct KvCacheUsage to provide k-spread.
func makeMetrics(variant string, count int, baseK float64, kStep float64) []interfaces.ReplicaMetrics {
	metrics := make([]interfaces.ReplicaMetrics, count)
	for i := range metrics {
		metrics[i] = interfaces.ReplicaMetrics{
			PodName:               "pod-" + variant + "-" + string(rune('0'+i)),
			VariantName:           variant,
			KvCacheUsage:          baseK + float64(i)*kStep,
			TotalKvCapacityTokens: 65536,
			AvgInputTokens:        1024,
			AvgOutputTokens:       256,
			PrefixCacheHitRate:    0.0,
			AvgITL:                0.030 + (baseK+float64(i)*kStep)*0.05,
		}
	}
	return metrics
}

var _ = Describe("ThroughputAnalyzer", func() {
	var (
		analyzer  *ThroughputAnalyzer
		ctx       context.Context
		modelID   string
		namespace string
	)

	BeforeEach(func() {
		analyzer = NewThroughputAnalyzer()
		ctx = context.Background()
		modelID = "llama3-8b"
		namespace = "default"
	})

	Describe("Name", func() {
		It("returns the analyzer name", func() {
			Expect(analyzer.Name()).To(Equal(AnalyzerName))
		})
	})

	Describe("VariantState before any observations", func() {
		It("returns false when no data has been observed", func() {
			_, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeFalse())
		})
	})

	Describe("Observe — basic state creation", func() {
		It("creates variant state on first Observe", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.15)
			analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)

			_, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue())
		})

		It("records shape from first call", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.15)
			analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.Shape.AvgInputTokens).To(BeNumerically("~", 1024.0, 0.01))
			Expect(state.Shape.AvgOutputTokens).To(BeNumerically("~", 256.0, 0.01))
		})

		It("adds observations to the window on each Observe call", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.15)
			analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.SampleCount).To(Equal(3))
		})
	})

	Describe("Observe — multi-cycle accumulation", func() {
		It("accumulates observations across multiple cycles until Ready", func() {
			// Each call adds 2 replicas with different k values.
			// After enough cycles the window should become Ready.
			for i := range 6 {
				baseK := 0.20 + float64(i)*0.10
				metrics := makeMetrics("v1", 2, baseK, 0.05)
				analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)
			}

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.SampleCount).To(BeNumerically(">=", DefaultMinSamples))
			Expect(state.KSpread).To(BeNumerically(">=", DefaultMinKSpread))
			Expect(state.ObservationReady).To(BeTrue())
		})
	})

	Describe("Observe — shape change clears window", func() {
		It("clears the observation window when workload shape changes significantly", func() {
			// Build up some observations.
			for i := range 3 {
				metrics := makeMetrics("v1", 3, 0.20+float64(i)*0.10, 0.05)
				analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)
			}
			stateBefore, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(stateBefore.SampleCount).To(BeNumerically(">", 0))

			// Now shift IL by 50% — well beyond the 20% tolerance.
			shifted := makeMetrics("v1", 3, 0.20, 0.10)
			for i := range shifted {
				shifted[i].AvgInputTokens = 1024 * 1.5 // +50%
			}
			analyzer.Observe(ctx, time.Now(), modelID, namespace, shifted)

			stateAfter, _ := analyzer.VariantState(modelID, namespace, "v1")
			// The window was cleared on shape change, then one cycle of 3 observations was added.
			Expect(stateAfter.SampleCount).To(Equal(3))
		})
	})

	Describe("Observe — sanity short-circuit", func() {
		It("skips a variant entirely when sanity checks fail", func() {
			bad := makeMetrics("v1", 3, 0.20, 0.10)
			for i := range bad {
				bad[i].AvgITL = 0 // fails SanityIssueITLNonPositive
			}
			reports := analyzer.Observe(ctx, time.Now(), modelID, namespace, bad)

			Expect(reports["v1"].OK()).To(BeFalse())
			Expect(reports["v1"].Has(SanityIssueITLNonPositive)).To(BeTrue())

			// No state should be created when all metrics are bad.
			// (State IS created but window remains empty because we skipped after sanity fail.)
			state, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue()) // state record was created
			Expect(state.SampleCount).To(Equal(0))
			Expect(state.ObservationReady).To(BeFalse())
		})

		It("returns an OK report for a healthy variant", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.10)
			reports := analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)
			Expect(reports["v1"].OK()).To(BeTrue())
		})
	})

	Describe("Observe — multi-variant isolation", func() {
		It("tracks variants independently", func() {
			metricsV1 := makeMetrics("v1", 3, 0.20, 0.10)
			metricsV2 := makeMetrics("v2", 3, 0.30, 0.10)
			metricsV2[0].AvgInputTokens = 2048 // different shape
			metricsV2[1].AvgInputTokens = 2048
			metricsV2[2].AvgInputTokens = 2048

			combined := append(metricsV1, metricsV2...)
			analyzer.Observe(ctx, time.Now(), modelID, namespace, combined)

			stateV1, okV1 := analyzer.VariantState(modelID, namespace, "v1")
			stateV2, okV2 := analyzer.VariantState(modelID, namespace, "v2")

			Expect(okV1).To(BeTrue())
			Expect(okV2).To(BeTrue())
			Expect(stateV1.Shape.AvgInputTokens).To(BeNumerically("~", 1024.0, 0.01))
			Expect(stateV2.Shape.AvgInputTokens).To(BeNumerically("~", 2048.0, 0.01))
			Expect(stateV1.SampleCount).To(Equal(3))
			Expect(stateV2.SampleCount).To(Equal(3))
		})
	})

	Describe("Analyze", func() {
		It("returns an AnalyzerResult with the correct identifiers", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.10)
			input := interfaces.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: metrics,
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.AnalyzerName).To(Equal(AnalyzerName))
			Expect(result.ModelID).To(Equal(modelID))
			Expect(result.Namespace).To(Equal(namespace))
		})

		It("returns zero RequiredCapacity and SpareCapacity (no scaling signal in PR-3)", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.10)
			input := interfaces.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: metrics,
			}
			result, _ := analyzer.Analyze(ctx, input)
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("updates internal state on each Analyze call", func() {
			for i := range 4 {
				input := interfaces.AnalyzerInput{
					ModelID:        modelID,
					Namespace:      namespace,
					ReplicaMetrics: makeMetrics("v1", 3, 0.20+float64(i)*0.10, 0.05),
				}
				_, err := analyzer.Analyze(ctx, input)
				Expect(err).NotTo(HaveOccurred())
			}
			state, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue())
			Expect(state.SampleCount).To(BeNumerically(">", 0))
		})

		It("sets AnalyzedAt to a recent timestamp", func() {
			before := time.Now()
			input := interfaces.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: makeMetrics("v1", 3, 0.20, 0.10),
			}
			result, _ := analyzer.Analyze(ctx, input)
			Expect(result.AnalyzedAt).To(BeTemporally(">=", before))
		})
	})

	Describe("Observe — empty metrics list", func() {
		It("handles an empty metrics slice gracefully", func() {
			reports := analyzer.Observe(ctx, time.Now(), modelID, namespace, []interfaces.ReplicaMetrics{})
			Expect(reports).To(BeEmpty())
		})
	})

	Describe("Observe — concurrent safety", func() {
		It("is safe for concurrent Observe and VariantState calls", func() {
			const goroutines = 10
			var wg sync.WaitGroup
			wg.Add(goroutines + 1)

			for i := range goroutines {
				go func(i int) {
					defer wg.Done()
					variant := fmt.Sprintf("v%d", i%3)
					metrics := makeMetrics(variant, 2, 0.20+float64(i)*0.05, 0.05)
					analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)
				}(i)
			}

			go func() {
				defer wg.Done()
				analyzer.VariantState(modelID, namespace, "v0")
			}()

			wg.Wait()
			_, ok := analyzer.VariantState(modelID, namespace, "v0")
			Expect(ok).To(BeTrue())
		})
	})
})
