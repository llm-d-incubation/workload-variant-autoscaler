package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// TODO(cleanup): Unify analyzer-path configuration across algorithms
// (saturation config fields vs queueing-model config presence), then simplify
// this spec to a single explicit analyzer selector contract.

// V1 saturation calibration via the simulator's --fake-metrics flag.
//
// kv-cache-usage=0.3 and waiting-requests=2 are chosen so both threshold arcs
// are deterministically exercisable by config alone — no load required:
//
//   - Scale-up path (aggressive thresholds: kvCache=0.05, queue=1):
//     0.3 > 0.05 and 2 >= 1 → V1 sees saturation → recommends scale-up.
//   - No-scale path (conservative thresholds: kvCache=1.00, queue=100):
//     0.3 < 1.00 and 2 < 100 → V1 sees no saturation → no scale-up.
//
// --fake-metrics replaces simulator runtime emission entirely; service traffic
// has no effect on the values V1 reads.
const v1FakeMetricsJSON = `{"kv-cache-usage":0.3,"waiting-requests":2,"running-requests":1}`

const (
	saturationConfigTemplate = `
model_id: ""
namespace: ""
kvCacheThreshold: %.2f
queueLengthThreshold: %d
kvSpareTrigger: %.2f
queueSpareTrigger: %d
scaleUpThreshold: %.2f
scaleDownBoundary: %.2f
analyzerName: %q
`

	// Aggressive V1 thresholds: fake metrics (kv=0.3, queue=2) exceed these → scale-up.
	saturationV1KVCacheThreshold     = 0.05
	saturationV1QueueLengthThreshold = 1
	saturationV1KVSpareTrigger       = 0.01
	saturationV1QueueSpareTrigger    = 1
	saturationV1ScaleUpThreshold     = 0.85
	saturationV1ScaleDownBoundary    = 0.70

	// Conservative V1 thresholds: fake metrics (kv=0.3, queue=2) stay below these → no scale-up.
	saturationV1NoScaleKVCacheThreshold     = 1.00
	saturationV1NoScaleQueueLengthThreshold = 100
	saturationV1NoScaleKVSpareTrigger       = 0.00
	saturationV1NoScaleQueueSpareTrigger    = 0
)

// buildSaturationConfigYAML builds a valid saturation config entry for the requested analyzer mode.
func buildSaturationConfigYAML(analyzerName string) string {
	return fmt.Sprintf(saturationConfigTemplate, 0.80, 1, 0.20, 1, 0.85, 0.70, analyzerName)
}

// buildSaturationConfigYAMLWithThresholds builds a valid saturation config entry with explicit thresholds.
func buildSaturationConfigYAMLWithThresholds(analyzerName string, kvCacheThreshold float64, queueLengthThreshold int, kvSpareTrigger float64, queueSpareTrigger int, scaleUpThreshold float64, scaleDownBoundary float64) string {
	return fmt.Sprintf(
		saturationConfigTemplate,
		kvCacheThreshold,
		queueLengthThreshold,
		kvSpareTrigger,
		queueSpareTrigger,
		scaleUpThreshold,
		scaleDownBoundary,
		analyzerName,
	)
}

// saturationConfigMapName resolves the active saturation ConfigMap name from controller runtime env.
func saturationConfigMapName() string {
	// Match the controller's runtime config map name. Helm deployments often
	// override SATURATION_CONFIG_MAP_NAME with a release-prefixed value and can
	// also change the controller deployment name, so discover it by label first.
	deps, err := k8sClient.AppsV1().Deployments(cfg.WVANamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "control-plane=controller-manager",
	})
	if err != nil || len(deps.Items) == 0 {
		return config.SaturationConfigMapName()
	}
	return saturationConfigMapNameFromDeployment(&deps.Items[0])
}

// saturationConfigMapNameFromDeployment extracts SATURATION_CONFIG_MAP_NAME from manager container env.
func saturationConfigMapNameFromDeployment(dep *appsv1.Deployment) string {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != "manager" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "SATURATION_CONFIG_MAP_NAME" && e.Value != "" {
				return e.Value
			}
		}
	}
	return config.SaturationConfigMapName()
}

// expectAnalyzerPathLog is a Ginkgo helper: it Eventually-waits until WVA
// controller-manager logs contain both the analyzer path marker for mode and
// modelID. It uses testutils.PodLogsLabelSelectorContain for log collection.
func expectAnalyzerPathLog(mode, modelID string) {
	GinkgoHelper()
	const controllerManagerLabel = "control-plane=controller-manager"
	pattern := fmt.Sprintf("Processing model (%s)", mode)
	Eventually(func(g Gomega) {
		ok, logs, logErr := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace, controllerManagerLabel, pattern, 120)
		g.Expect(logErr).NotTo(HaveOccurred())
		g.Expect(ok && strings.Contains(logs, modelID)).To(BeTrue())
	}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

// waitForPositiveDesiredAllocation logs VA progress and waits for a positive desired replica recommendation.
func waitForPositiveDesiredAllocation(ctx context.Context, namespace, vaName string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		va := &variantautoscalingv1alpha1.VariantAutoscaling{}
		getErr := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: namespace}, va)
		g.Expect(getErr).NotTo(HaveOccurred())

		metricsCond := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
		if metricsCond != nil {
			GinkgoWriter.Printf(
				"  VA progress (%s): MetricsAvailable=%s reason=%s message=%q\n",
				vaName,
				metricsCond.Status,
				metricsCond.Reason,
				metricsCond.Message,
			)
		} else {
			GinkgoWriter.Printf("  VA progress (%s): MetricsAvailable=<nil>\n", vaName)
		}

		desired := int32(-1)
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
			desired = *va.Status.DesiredOptimizedAlloc.NumReplicas
			GinkgoWriter.Printf(
				"  VA progress (%s): DesiredOptimizedAlloc replicas=%d accelerator=%q\n",
				vaName,
				desired,
				va.Status.DesiredOptimizedAlloc.Accelerator,
			)
		} else {
			GinkgoWriter.Printf("  VA progress (%s): DesiredOptimizedAlloc replicas=<nil>\n", vaName)
		}

		g.Expect(metricsCond).NotTo(BeNil())
		g.Expect(metricsCond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil())
		g.Expect(desired).To(BeNumerically(">", 0))
	}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

// expectNoScaleUpAboveBaseline asserts that desired replicas do not exceed baseline for a bounded window.
func expectNoScaleUpAboveBaseline(ctx context.Context, namespace, vaName string, baseline int32, windowSec int) {
	GinkgoHelper()
	Consistently(func(g Gomega) {
		va := &variantautoscalingv1alpha1.VariantAutoscaling{}
		getErr := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: namespace}, va)
		g.Expect(getErr).NotTo(HaveOccurred())

		metricsCond := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
		if metricsCond != nil {
			GinkgoWriter.Printf(
				"  Negative-path progress (%s): MetricsAvailable=%s reason=%s\n",
				vaName,
				metricsCond.Status,
				metricsCond.Reason,
			)
		}

		current := int32(0)
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
			current = *va.Status.DesiredOptimizedAlloc.NumReplicas
		}
		GinkgoWriter.Printf(
			"  Negative-path progress (%s): DesiredOptimizedAlloc replicas=%d baseline=%d\n",
			vaName,
			current,
			baseline,
		)
		g.Expect(current).To(BeNumerically("<=", baseline),
			"V1 bounded below-threshold traffic should not increase desired replicas above baseline")
	}, time.Duration(windowSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

// waitForSaturationInfraSignal ensures the VA has begun controller-driven status reporting
// before the bounded threshold trigger runs. This avoids firing traffic before the control
// loop and metrics pipeline have any observable signal for this VA.
func waitForSaturationInfraSignal(ctx context.Context, namespace, vaName string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		va := &variantautoscalingv1alpha1.VariantAutoscaling{}
		getErr := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: namespace}, va)
		g.Expect(getErr).NotTo(HaveOccurred())

		targetResolved := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
		if targetResolved != nil {
			GinkgoWriter.Printf(
				"  Infra preflight (%s): TargetResolved=%s reason=%s message=%q\n",
				vaName,
				targetResolved.Status,
				targetResolved.Reason,
				targetResolved.Message,
			)
		} else {
			GinkgoWriter.Printf("  Infra preflight (%s): TargetResolved=<nil>\n", vaName)
		}

		g.Expect(targetResolved).NotTo(BeNil(), "TargetResolved should be present before threshold trigger")
		g.Expect(targetResolved.Status).To(Equal(metav1.ConditionTrue), "TargetResolved should be true before threshold trigger")
	}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

var _ = Describe("Saturation analyzer path and status propagation", Label("full"), Ordered, func() {
	const (
		poolName     = "saturation-path-pool"
		modelSvcName = "saturation-path-ms"
		// modelDecodeDeployment is the Deployment name fixtures.CreateModelService creates
		// (name + "-decode"), matching llm-d model-service decode pods / labels.
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"
		vaName                = "saturation-path-va"
	)

	var (
		modelID         string
		cmName          string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		cmKey           string
		cmNamespace     string
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"The suite uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		modelID = cfg.ModelID
		cmName = saturationConfigMapName()
		// Use global saturation config for deterministic engine-path selection.
		// Namespace-local ConfigMap watch is opt-in/tracked and can race in e2e.
		cmNamespace = cfg.WVANamespace
		cmKey = "default"

		// Snapshot existing saturation config so the test can restore it.
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating model service + service + ServiceMonitor for saturation path test")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		err = fixtures.CreateModelServiceWithExtraArgs(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID, vaName,
			cfg.UseSimulator, cfg.MaxNumSeqs, []string{"--fake-metrics", v1FakeMetricsJSON})
		Expect(err).NotTo(HaveOccurred())
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000)
		Expect(err).NotTo(HaveOccurred())
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			dep, depErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(depErr).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Creating VA for dedicated saturation analyzer path model")
		err = fixtures.EnsureVariantAutoscalingWithDefaults(
			ctx, crClient, cfg.LLMDNamespace, vaName,
			modelDecodeDeployment, modelID, cfg.AcceleratorType, cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			// Replace the object in two steps (delete + create) instead of updating in place.
			// That avoids resourceVersion conflict retries; a brief gap without the ConfigMap
			// during suite teardown is acceptable for e2e.
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

		By("Cleaning up saturation analyzer path resources")
		_ = crClient.Delete(ctx, &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: vaName, Namespace: cfg.LLMDNamespace},
		})
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	It("uses V2 path when analyzerName is saturation", func() {
		By("Writing model-specific saturation config with analyzerName=saturation")
		err := upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, buildSaturationConfigYAML("saturation"))
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for controller logs to show V2 processing for this model")
		expectAnalyzerPathLog("V2", modelID)
	})

	It("switches to V1 path when analyzerName is unset", func() {
		By("Updating model-specific saturation config with analyzerName unset")
		err := upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, buildSaturationConfigYAML(""))
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for controller logs to show V1 processing for this model")
		expectAnalyzerPathLog("V1", modelID)
	})

	It("propagates saturation results into VA desired allocation and metrics condition", func() {
		By("Waiting for DesiredOptimizedAlloc and MetricsAvailable to be populated")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
			g.Expect(err).NotTo(HaveOccurred())

			metricsCond := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
			g.Expect(metricsCond).NotTo(BeNil(), "MetricsAvailable condition should exist")
			g.Expect(metricsCond.Status).To(Equal(metav1.ConditionTrue), "MetricsAvailable should be true once metrics are collected")

			g.Expect(va.Status.DesiredOptimizedAlloc.Accelerator).NotTo(BeEmpty(),
				"DesiredOptimizedAlloc.Accelerator should be set")
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
				"DesiredOptimizedAlloc.NumReplicas should be set")
			g.Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
				"DesiredOptimizedAlloc.NumReplicas should be non-negative")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("does not recommend additional scale-up for bounded below-threshold V1 traffic", func() {
		var baseline int32

		By("Capturing baseline desired replicas before below-threshold trigger")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			getErr := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil())
			baseline = *va.Status.DesiredOptimizedAlloc.NumReplicas
			GinkgoWriter.Printf("  Negative-path baseline (%s): desired=%d\n", vaName, baseline)
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Configuring conservative V1 thresholds to avoid scale-up")
		err := upsertSaturationConfigEntry(
			ctx,
			cmNamespace,
			cmName,
			cmKey,
			buildSaturationConfigYAMLWithThresholds(
				"",
				saturationV1NoScaleKVCacheThreshold,
				saturationV1NoScaleQueueLengthThreshold,
				saturationV1NoScaleKVSpareTrigger,
				saturationV1NoScaleQueueSpareTrigger,
				saturationV1ScaleUpThreshold,
				saturationV1ScaleDownBoundary,
			),
		)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying controller is using V1 analyzer path")
		expectAnalyzerPathLog("V1", modelID)

		By("Preflighting VA controller signal before asserting no scale-up")
		waitForSaturationInfraSignal(ctx, cfg.LLMDNamespace, vaName)

		By("Verifying desired allocation does not increase above baseline")
		expectNoScaleUpAboveBaseline(ctx, cfg.LLMDNamespace, vaName, baseline, cfg.EventuallyMediumSec)
	})

	It("crosses V1 threshold with bounded requests and recommends scale-up", func() {
		By("Configuring aggressive V1 thresholds and unsetting analyzerName")
		err := upsertSaturationConfigEntry(
			ctx,
			cmNamespace,
			cmName,
			cmKey,
			buildSaturationConfigYAMLWithThresholds(
				"",
				saturationV1KVCacheThreshold,
				saturationV1QueueLengthThreshold,
				saturationV1KVSpareTrigger,
				saturationV1QueueSpareTrigger,
				saturationV1ScaleUpThreshold,
				saturationV1ScaleDownBoundary,
			),
		)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying controller is using V1 analyzer path")
		expectAnalyzerPathLog("V1", modelID)

		By("Preflighting VA controller signal before asserting scale-up")
		waitForSaturationInfraSignal(ctx, cfg.LLMDNamespace, vaName)

		By("Verifying desired allocation recommends a positive replica count")
		waitForPositiveDesiredAllocation(ctx, cfg.LLMDNamespace, vaName)
	})

})

// upsertSaturationConfigEntry creates or updates a saturation ConfigMap data entry.
func upsertSaturationConfigEntry(ctx context.Context, cmNamespace, cmName, key, value string) error {
	cmClient := k8sClient.CoreV1().ConfigMaps(cmNamespace)
	cm, err := cmClient.Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			newCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cmName,
					Namespace: cmNamespace,
				},
				Data: map[string]string{key: value},
			}
			_, createErr := cmClient.Create(ctx, newCM, metav1.CreateOptions{})
			return createErr
		}
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = value
	_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// saturationConfigMapForRecreate returns a copy of orig suitable for Create after Delete,
// with apiserver-owned fields cleared so admission succeeds.
func saturationConfigMapForRecreate(orig *corev1.ConfigMap) *corev1.ConfigMap {
	cm := orig.DeepCopy()
	cm.ResourceVersion = ""
	cm.UID = ""
	cm.Generation = 0
	cm.CreationTimestamp = metav1.Time{}
	cm.DeletionTimestamp = nil
	cm.DeletionGracePeriodSeconds = nil
	cm.ManagedFields = nil
	cm.Finalizers = nil
	return cm
}
