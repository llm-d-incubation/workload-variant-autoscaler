package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// V2 smoke calibration via the simulator's --fake-metrics flag.
//
// kv-cache-usage = 0.3 is the operating point that deterministically exercises
// both arcs of the V2 cost-aware optimizer with the suite's chosen thresholds:
//
//   - At 1 replica with scaleUpThreshold = 0.30, replicaDemand crosses the
//     threshold and the optimizer's required-capacity signal becomes positive
//     → scale-up. (Drives the "should recommend scale-up" It below.)
//   - At 2 replicas with the canonical-ordering thresholds scaleUpThreshold =
//     0.95 and scaleDownBoundary = 0.85, the cost-aware optimizer's
//     scale-down rule (cost_aware_optimizer.go: math.Floor(remaining /
//     vc.PerReplicaCapacity)) sees a remaining-capacity ≥ one full per-replica
//     budget → remove 1 replica. (Drives the "should recommend scale-down"
//     It below.)
//
// --fake-metrics replaces simulator runtime emission entirely; service traffic
// has no effect on the values V2 reads. That is the point — the suite
// exercises V2's decision logic against deterministic inputs.
//
// --fake-metrics format:
//
//	https://github.com/llm-d/llm-d-inference-sim/blob/main/docs/configuration.md
const v2SmokeFakeMetricsJSON = `{"kv-cache-usage":0.3,"running-requests":1,"waiting-requests":0}`

// V2 saturation config knobs. The kvCacheThreshold / queueLength* /
// *SpareTrigger fields are V1-specific and have no effect on the V2
// token-based path; they are filled with safe defaults to satisfy
// buildSaturationConfigYAMLWithThresholds.
const (
	v2SmokeKvCacheThreshold     = 0.80
	v2SmokeQueueLengthThreshold = 50
	v2SmokeKvSpareTrigger       = 0.10
	v2SmokeQueueSpareTrigger    = 5

	// Aggressive on scale-up, conservative on scale-down so the path-selection
	// and scale-up tests are stable. The scale-down test raises
	// scaleDownBoundary at runtime to exercise the scale-down arc without
	// disturbing earlier preconditions.
	v2SmokeScaleUpThreshold  = 0.30
	v2SmokeScaleDownBoundary = 0.20
)

var _ = Describe("Saturation V2 engine", Label("smoke", "full"), Ordered, func() {
	const (
		poolName              = "v2-smoke-pool"
		modelSvcName          = "v2-smoke-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"
		vaName                = "v2-smoke-va"
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmKey           string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		// The suite depends on the simulator's --fake-metrics flag to drive
		// deterministic kv-cache-usage values into V2. That flag only exists
		// on llm-d-inference-sim — real vLLM rejects it and the model
		// Deployment fails to start. Skip cleanly on non-simulator runs
		// (e.g., OpenShift-style CI against real vLLM) rather than producing
		// a broken Deployment and timing out on readiness.
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"The suite uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		modelID = cfg.ModelID
		cmName = saturationConfigMapName()
		cmNamespace = cfg.WVANamespace
		cmKey = "default"

		By("Snapshotting existing saturation ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating model service + service + ServiceMonitor for V2 smoke tests")
		// Configure the simulator with --fake-metrics so V2 reads deterministic
		// kv_cache_usage_perc and request-count signals instead of relying on
		// the simulator's runtime emission, which doesn't always reach V2's
		// token-budget magnitude under bounded smoke load. See the
		// v2SmokeFakeMetricsJSON comment for the math.
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID, vaName,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", v2SmokeFakeMetricsJSON},
		)).To(Succeed())
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment,
		)).To(Succeed())

		By("Waiting for the V2 smoke model deployment to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Creating VA for V2 smoke (defaults: minReplicas=1, maxReplicas=10)")
		Expect(fixtures.EnsureVariantAutoscalingWithDefaults(
			ctx, crClient, cfg.LLMDNamespace, vaName,
			modelDecodeDeployment, modelID, cfg.AcceleratorType, cfg.ControllerInstance,
		)).To(Succeed())

		By("Installing V2 saturation config so all subsequent It() blocks share state")
		// Done in BeforeAll (rather than inside the first It) so the suite's
		// behavior does not depend on Ordered execution to set up V2's
		// preconditions for later tests.
		cfgYAML := buildSaturationConfigYAMLWithThresholds(
			"saturation",
			v2SmokeKvCacheThreshold, v2SmokeQueueLengthThreshold,
			v2SmokeKvSpareTrigger, v2SmokeQueueSpareTrigger,
			v2SmokeScaleUpThreshold, v2SmokeScaleDownBoundary,
		)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, cfgYAML)).To(Succeed())
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			propagation := metav1.DeletePropagationBackground
			if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete saturation configmap %s before restore: %v\n", cmName, err)
			}
			toCreate := saturationConfigMapForRecreate(cmOriginal)
			if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to recreate saturation configmap %s: %v\n", cmName, err)
			}
		} else {
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
		}

		By("Cleaning up V2 smoke resources")
		_ = crClient.Delete(ctx, &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: vaName, Namespace: cfg.LLMDNamespace},
		})
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	// Verifies V2 path selection and that a positive desired allocation emerges
	// from steady-state metrics alone (no extra load fired). The V2 saturation
	// config is installed in BeforeAll, so this It body just verifies the
	// engine took the V2 path and the resulting status fields.
	It("should select V2 path and produce a positive desired allocation", func() {
		By("Asserting controller logs show V2 path selected for our model")
		expectAnalyzerPathLog("V2", modelID)

		By("Waiting for VA to receive a positive desired allocation")
		waitForPositiveDesiredAllocation(ctx, cfg.LLMDNamespace, vaName)

		By("Asserting MetricsAvailable=True and accelerator is set")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			g.Expect(crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace, Name: vaName,
			}, va)).To(Succeed())
			g.Expect(va.Status.DesiredOptimizedAlloc.Accelerator).NotTo(BeEmpty(), "accelerator should be resolved")
			cond := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
			g.Expect(cond).NotTo(BeNil(), "MetricsAvailable condition should be set")
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Verifies that V2 recommends scale-up when --fake-metrics drives a
	// kv-cache-usage above scaleUpThreshold. See v2SmokeFakeMetricsJSON for
	// the calibration math. No load trigger is required.
	It("should recommend scale-up when token utilization crosses scaleUpThreshold", func() {
		By("Asserting V2 recommends more than 1 replica from fake-metrics demand")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			g.Expect(crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace, Name: vaName,
			}, va)).To(Succeed())
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil())
			g.Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).
				To(BeNumerically(">", int32(1)),
					"V2 should recommend more than 1 replica when fake kv-cache-usage is above scaleUpThreshold")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Verifies that with the deployment running at 2 replicas and thresholds
	// chosen so the cost-aware optimizer's scale-down rule fires, V2
	// recommends a smaller target. Uses canonical-ordering thresholds
	// (scaleUpThreshold > scaleDownBoundary):
	//
	//   scaleUpThreshold  = 0.95 (high, so kv=0.3 demand does not trigger scale-up)
	//   scaleDownBoundary = 0.85 (chosen so spareCapacity at 2 replicas exceeds
	//                             one full per-replica capacity — see calibration
	//                             comment on v2SmokeFakeMetricsJSON for the math)
	//
	// VA enforces minReplicas=1, so the only valid scale-down outcome from 2 is 1.
	// Assert Equal(1) so any regression that lands at 0 (MinReplicas violated) or
	// 2 (no scale-down) fails loudly with a precise diff rather than passing on
	// the previously-defensive "<2" bound.
	It("should recommend scale-down when load drops below scaleDownBoundary", func() {
		By("Patching deployment to 2 replicas to give V2 a non-floor starting point")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			dep.Spec.Replicas = ptr.To(int32(2))
			_, updateErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Update(ctx, dep, metav1.UpdateOptions{})
			g.Expect(updateErr).NotTo(HaveOccurred())
		}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).
			Should(Succeed())

		By("Waiting for both replicas to be Ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Switching to canonical-ordering thresholds (scaleUp=0.95, scaleDown=0.85)")
		const (
			scaleDownTestUpThreshold = 0.95
			scaleDownTestBoundary    = 0.85
		)
		cfgYAML := buildSaturationConfigYAMLWithThresholds(
			"saturation",
			v2SmokeKvCacheThreshold, v2SmokeQueueLengthThreshold,
			v2SmokeKvSpareTrigger, v2SmokeQueueSpareTrigger,
			scaleDownTestUpThreshold, scaleDownTestBoundary,
		)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, cfgYAML)).To(Succeed())

		By("Asserting V2 recommends exactly 1 replica (minReplicas floor)")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			g.Expect(crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace, Name: vaName,
			}, va)).To(Succeed())
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil())
			g.Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).
				To(Equal(int32(1)),
					"V2 should drop from 2 to 1 (MinReplicas floor) when load is below scaleDownBoundary")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})
})
