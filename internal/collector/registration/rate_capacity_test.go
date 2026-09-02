package registration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// The rate-anchored capacity estimator and the throughput analyzer both need the
// per-pod rate queries, and either registrar may run first. That is the whole reason
// the shared definitions moved here and both switched to register-if-absent, so it is
// worth pinning: a rebase that put one of them back on MustRegister would panic the
// controller at startup, and nothing else in the suite would catch it.
var _ = Describe("RegisterRateCapacityQueries", func() {
	var (
		ctx      context.Context
		registry *source.SourceRegistry
	)

	shared := []string{QueryKvUsageInstant, QueryQueueLengthInstant, QueryRequestRate,
		QueryPromptTokenRate, QueryInferenceTime}

	withPrometheus := func() *source.QueryList {
		metricsSource := prometheus.NewPrometheusSource(ctx, &mockPrometheusAPI{},
			prometheus.DefaultPrometheusSourceConfig())
		Expect(registry.Register("prometheus", metricsSource)).To(Succeed())
		return metricsSource.QueryList()
	}

	BeforeEach(func() {
		ctx = context.Background()
		registry = source.NewSourceRegistry()
	})

	It("registers the queries the estimator depends on", func() {
		queryList := withPrometheus()
		RegisterRateCapacityQueries(registry)

		for _, name := range shared {
			Expect(queryList.Get(name)).NotTo(BeNil(), name)
		}
		for _, name := range []string{QueryKvUsageInstant, QueryQueueLengthInstant,
			QueryRequestRate, QueryPromptTokenRate} {
			Expect(queryList.Get(EngineQuery(inferenceengine.EngineSGLang, name))).NotTo(BeNil(), name)
		}
		// Deliberately vLLM-only: SGLang publishes nothing that measures time in the
		// running phase, so the bare query returns nothing there and the analyzer
		// derives the value instead.
		Expect(IsEngineSpecific(QueryInferenceTime)).To(BeFalse())
	})

	It("tolerates the throughput analyzer registering first", func() {
		queryList := withPrometheus()
		RegisterThroughputAnalyzerQueries(registry)

		Expect(func() { RegisterRateCapacityQueries(registry) }).NotTo(Panic())
		for _, name := range shared {
			Expect(queryList.Get(name)).NotTo(BeNil(), name)
		}
	})

	It("tolerates registering first itself", func() {
		queryList := withPrometheus()
		RegisterRateCapacityQueries(registry)

		Expect(func() { RegisterThroughputAnalyzerQueries(registry) }).NotTo(Panic())
		for _, name := range shared {
			Expect(queryList.Get(name)).NotTo(BeNil(), name)
		}
	})

	It("leaves the first registration in place rather than replacing it", func() {
		queryList := withPrometheus()
		RegisterRateCapacityQueries(registry)
		before := queryList.Get(QueryRequestRate).Template

		RegisterRateCapacityQueries(registry)
		Expect(queryList.Get(QueryRequestRate).Template).To(Equal(before))
	})

	It("is a no-op when there is no prometheus source", func() {
		Expect(func() { RegisterRateCapacityQueries(registry) }).NotTo(Panic())
	})
})
