package metrics

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// resetMetrics clears package-level metric vars so each test starts fresh.
// Callers MUST invoke InitMetrics before recording any metric afterwards —
// the Record* methods deliberately have no nil guard, so a missed init
// would panic at the first Set call.
func resetMetrics() {
	replicaScalingTotal = nil
	desiredReplicas = nil
	currentReplicas = nil
	desiredRatio = nil
	saturationUtilization = nil
	spareCapacity = nil
	requiredCapacity = nil
	kvCacheTokensUsed = nil
	kvCacheTokensCapacity = nil
	controllerInstance = ""
}

func gatherMetric(registry *prometheus.Registry, name string) *dto.MetricFamily {
	families, err := registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func gaugeValue(mf *dto.MetricFamily, labels map[string]string) (float64, bool) {
	for _, m := range mf.GetMetric() {
		match := true
		for k, v := range labels {
			found := false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == k && lp.GetValue() == v {
					found = true
					break
				}
			}
			if !found {
				match = false
				break
			}
		}
		if match && m.GetGauge() != nil {
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

var _ = Describe("RecordSaturationMetrics", func() {

	var (
		registry *prometheus.Registry
		emitter  *MetricsEmitter
		ctx      context.Context
	)

	BeforeEach(func() {
		resetMetrics()
		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		emitter = NewMetricsEmitter()
		ctx = context.Background()
	})

	It("should record all saturation metrics with correct values", func() {
		emitter.RecordSaturationMetrics(ctx,
			"variant-a", "test-ns", "meta-llama/Llama-3.1-8B", "nvidia-a100", constants.UnitContinuous,
			0.75, 0.25, 5000.0,
			100000, 200000,
		)

		accelLabels := map[string]string{
			constants.LabelVariantName:     "variant-a",
			constants.LabelNamespace:       "test-ns",
			constants.LabelModelName:       "meta-llama/Llama-3.1-8B",
			constants.LabelAcceleratorType: "nvidia-a100",
		}
		modelLabels := map[string]string{
			constants.LabelVariantName: "variant-a",
			constants.LabelNamespace:   "test-ns",
			constants.LabelModelName:   "meta-llama/Llama-3.1-8B",
		}
		requiredLabels := map[string]string{
			constants.LabelVariantName: "variant-a",
			constants.LabelNamespace:   "test-ns",
			constants.LabelModelName:   "meta-llama/Llama-3.1-8B",
			constants.LabelUnit:        constants.UnitContinuous,
		}

		// saturation_utilization
		mf := gatherMetric(registry, constants.WVASaturationUtilization)
		Expect(mf).NotTo(BeNil())
		val, ok := gaugeValue(mf, accelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.75))

		// spare_capacity
		mf = gatherMetric(registry, constants.WVASpareCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, accelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.25))

		// required_capacity (carries unit label)
		mf = gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, requiredLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(5000.0))

		// kv_cache_tokens_used
		mf = gatherMetric(registry, constants.WVAKvCacheTokensUsed)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, modelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(100000.0))

		// kv_cache_tokens_total
		mf = gatherMetric(registry, constants.WVAKvCacheTokensCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, modelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(200000.0))
	})

	It("should distinguish binary and continuous required_capacity series via unit label", func() {
		// Same variant, same namespace, different units → two series
		emitter.RecordSaturationMetrics(ctx,
			"variant-multi", "ns", "model-x", "h100", constants.UnitBinary,
			0.5, 0.5, 1.0, 100, 200,
		)
		emitter.RecordSaturationMetrics(ctx,
			"variant-multi", "ns", "model-x", "h100", constants.UnitContinuous,
			0.5, 0.5, 8000.0, 100, 200,
		)

		mf := gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())

		v1Labels := map[string]string{
			constants.LabelVariantName: "variant-multi",
			constants.LabelNamespace:   "ns",
			constants.LabelModelName:   "model-x",
			constants.LabelUnit:        constants.UnitBinary,
		}
		v2Labels := map[string]string{
			constants.LabelVariantName: "variant-multi",
			constants.LabelNamespace:   "ns",
			constants.LabelModelName:   "model-x",
			constants.LabelUnit:        constants.UnitContinuous,
		}

		v1Val, ok := gaugeValue(mf, v1Labels)
		Expect(ok).To(BeTrue())
		Expect(v1Val).To(Equal(1.0))

		v2Val, ok := gaugeValue(mf, v2Labels)
		Expect(ok).To(BeTrue())
		Expect(v2Val).To(Equal(8000.0))
	})

	It("should include controller_instance label when env var is set", func() {
		// Re-init with controller instance set
		resetMetrics()
		Expect(os.Setenv(ControllerInstanceEnvVar, "controller-1")).To(Succeed())
		DeferCleanup(os.Unsetenv, ControllerInstanceEnvVar)

		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		emitter = NewMetricsEmitter()

		emitter.RecordSaturationMetrics(ctx,
			"variant-b", "prod-ns", "ibm/granite-13b", "nvidia-h100", constants.UnitContinuous,
			0.90, 0.10, 10000.0,
			500000, 600000,
		)

		// Verify controller_instance + model_name on accel-scoped metric
		mf := gatherMetric(registry, constants.WVASaturationUtilization)
		Expect(mf).NotTo(BeNil())
		val, ok := gaugeValue(mf, map[string]string{
			constants.LabelVariantName:        "variant-b",
			constants.LabelNamespace:          "prod-ns",
			constants.LabelModelName:          "ibm/granite-13b",
			constants.LabelAcceleratorType:    "nvidia-h100",
			constants.LabelControllerInstance: "controller-1",
		})
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.90))

		// Verify controller_instance + model_name + unit on required_capacity
		mf = gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, map[string]string{
			constants.LabelVariantName:        "variant-b",
			constants.LabelNamespace:          "prod-ns",
			constants.LabelModelName:          "ibm/granite-13b",
			constants.LabelUnit:               constants.UnitContinuous,
			constants.LabelControllerInstance: "controller-1",
		})
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(10000.0))
	})

	It("should handle zero values correctly", func() {
		emitter.RecordSaturationMetrics(ctx,
			"variant-c", "ns", "model-z", "amd-mi300x", constants.UnitBinary,
			0.0, 0.0, 0.0,
			0, 0,
		)

		accelLabels := map[string]string{
			constants.LabelVariantName:     "variant-c",
			constants.LabelNamespace:       "ns",
			constants.LabelModelName:       "model-z",
			constants.LabelAcceleratorType: "amd-mi300x",
		}
		modelLabels := map[string]string{
			constants.LabelVariantName: "variant-c",
			constants.LabelNamespace:   "ns",
			constants.LabelModelName:   "model-z",
		}
		requiredLabels := map[string]string{
			constants.LabelVariantName: "variant-c",
			constants.LabelNamespace:   "ns",
			constants.LabelModelName:   "model-z",
			constants.LabelUnit:        constants.UnitBinary,
		}

		for _, metricName := range []string{
			constants.WVASaturationUtilization,
			constants.WVASpareCapacity,
		} {
			mf := gatherMetric(registry, metricName)
			Expect(mf).NotTo(BeNil(), "metric %s not found", metricName)
			val, ok := gaugeValue(mf, accelLabels)
			Expect(ok).To(BeTrue(), "gauge not found for %s", metricName)
			Expect(val).To(Equal(0.0), "expected 0 for %s", metricName)
		}

		mf := gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok := gaugeValue(mf, requiredLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.0))

		for _, metricName := range []string{
			constants.WVAKvCacheTokensUsed,
			constants.WVAKvCacheTokensCapacity,
		} {
			mf := gatherMetric(registry, metricName)
			Expect(mf).NotTo(BeNil(), "metric %s not found", metricName)
			val, ok := gaugeValue(mf, modelLabels)
			Expect(ok).To(BeTrue(), "gauge not found for %s", metricName)
			Expect(val).To(Equal(0.0), "expected 0 for %s", metricName)
		}
	})

})
