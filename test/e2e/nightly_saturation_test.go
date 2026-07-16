package e2e

import (
	"fmt"
	"strings"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Nightly saturation tests exercise the full WVA decision loop against a live vLLM process on the
// OCP nightly cluster — the ONLY place V2's true token-capacity path runs. On kind + the simulator,
// saturation_v2_test.go proves V2 path selection and the KEDA pipeline, but only the percentage-based
// fallback: the simulator emits no vllm:cache_config_info, so TotalKvCapacityTokens == 0 and the
// engine routes to computeReplicaCapacityFallback. Real vLLM emits the cache-config and token
// histograms that drive the k1/k2 token-accounting path, which is what these tests cover.
//
// They require:
//   - USE_SIMULATOR=false (real vLLM)
//   - ENVIRONMENT=openshift
//   - vLLM Deployment pre-deployed with --max-num-seqs=1 (owned by the infra deploy step)
//   - the WVA controller running in the cluster
//
// KEDA is the only scaler backend. The nightly registers the vLLM Deployment with WVA via an
// annotated KEDA ScaledObject (llm-d.ai/managed) — no VariantAutoscaling CR — and observes the
// recommendation through the KEDA-managed HPA's DesiredReplicas and the Deployment's replica count.
// WVA emits wva_desired_replicas{variant_name=<ScaledObject name>}; the ScaledObject's trigger
// queries that series from Thanos (bearer-auth via a ClusterTriggerAuthentication), so the pods are
// attributed to the variant by the collector's ScaledObject→scaleTargetRef owner-walk.
//
// The burst mechanism: N concurrent requests with high max_tokens. With max_num_seqs=1, all but one
// request immediately enter the vLLM waiting queue, pushing num_requests_waiting above the saturation
// threshold and driving WVA to recommend a scale-up.
//
// Resource ownership:
//   - Infra deploy step owns: vLLM Deployment, ServiceAccount, gateway Service, PodMonitor,
//     ClusterTriggerAuthentication for Thanos.
//   - These tests own: the annotated ScaledObject and the saturation ConfigMap entry.

const (
	// nightlyQueueThreshold is the queueLengthThreshold written into the V1 saturation config by
	// BeforeAll. cfg.NightlyBurstSize must exceed this for the burst to saturate the queue.
	nightlyQueueThreshold = 5
	nightlyMaxTokens      = 1500 // keeps requests in-flight long enough for WVA to detect saturation; must be < max-model-len (2048)
	nightlyScaleDownSec   = 600  // pod model load (~4min) + WVA cycle (30s) + 300s HPA scale-down stabilization + buffer
	nightlyMaxReplicas    = 4
	nightlyVariantCost    = "10.0" // GPU cost for the nightly variant; lower than the kind default (30.0) to match the OCP GPU budget

	// OCP monitoring endpoints. KEDA queries thanos-querier directly via bearer-token auth provided
	// by the ClusterTriggerAuthentication (the WVA controller SA token holds cluster-monitoring-view).
	nightlyOCPPrometheusURL   = "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"
	nightlyOCPKEDATriggerAuth = "ai-inference-keda-thanos"
)

// discoverNightlyGateway finds the inference-gateway Service name. The gateway is part of the infra
// deploy step and must exist before the test runs. If GATEWAY_NAME is set (injected by the
// llm-d-infra reusable workflow) it is used directly; otherwise the namespace is scanned for a
// Service whose name contains "inference-gateway".
func discoverNightlyGateway() string {
	GinkgoHelper()

	if cfg.GatewayName != "" {
		GinkgoWriter.Printf("Nightly gateway: %s (from GATEWAY_NAME)\n", cfg.GatewayName)
		return cfg.GatewayName
	}

	svcs, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred(), "should list services in llmd namespace")
	for _, svc := range svcs.Items {
		if strings.Contains(svc.Name, "inference-gateway") {
			GinkgoWriter.Printf("Nightly gateway: %s\n", svc.Name)
			return svc.Name
		}
	}
	if cfg.EPPServiceName != "" {
		return cfg.EPPServiceName
	}
	Fail("inference-gateway service not found in namespace — run the infra deploy step first")
	return ""
}

// createNightlyWVAResources registers the vLLM Deployment with WVA by creating an annotated KEDA
// ScaledObject that targets it. The ScaledObject is both the WVA discovery source and the scaler.
// On OCP the trigger points at thanos-querier with bearer auth. The ScaledObject object name is
// scaleTargetName+"-so"; WVA uses it as the variant_name label, and the KEDA query filters on it.
func createNightlyWVAResources() {
	GinkgoHelper()
	Expect(fixtures.EnsureScaledObject(
		ctx, crClient,
		cfg.LLMDNamespace, cfg.NightlyDeployment, cfg.NightlyDeployment, nightlyScaledObjectName(),
		1, nightlyMaxReplicas, cfg.MonitoringNS,
		fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, nightlyVariantCost),
		fixtures.WithScaledObjectPrometheusServer(nightlyOCPPrometheusURL),
		fixtures.WithScaledObjectClusterTriggerAuth(nightlyOCPKEDATriggerAuth),
	)).To(Succeed(), "creating nightly ScaledObject")
	GinkgoWriter.Printf("Nightly ScaledObject created: %s (target=%s)\n", nightlyScaledObjectName(), cfg.NightlyDeployment)
}

// deleteNightlyWVAResources removes the ScaledObject created by createNightlyWVAResources.
func deleteNightlyWVAResources() {
	if err := fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, cfg.NightlyDeployment); err != nil {
		GinkgoWriter.Printf("Warning: failed to delete nightly ScaledObject: %v\n", err)
	}
}

// nightlyScaledObjectName is the ScaledObject object name (base + "-so"), which WVA uses as the
// variant_name label on wva_desired_replicas and which the KEDA query filters on.
func nightlyScaledObjectName() string { return cfg.NightlyDeployment + "-so" }

// checkGPUCapacity skips the test if fewer than minReplicas GPU slots are available across all
// schedulable nodes. This prevents the test from churning UnexpectedAdmissionError pods when the
// GPU node is unhealthy or at capacity. It compares allocatable minus requested (from non-terminal
// pods) for the gpu resource used by the vLLM deployment.
func checkGPUCapacity(deploymentName string, minReplicas int) {
	GinkgoHelper()

	dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		GinkgoWriter.Printf("Warning: could not read deployment %s to check GPU capacity: %v\n", deploymentName, err)
		return
	}
	var gpuResource corev1.ResourceName
	for _, c := range dep.Spec.Template.Spec.Containers {
		for rName := range c.Resources.Limits {
			if strings.Contains(string(rName), "gpu") || strings.Contains(string(rName), "GPU") {
				gpuResource = rName
				break
			}
		}
		if gpuResource != "" {
			break
		}
	}
	if gpuResource == "" {
		return // no GPU resource in deployment spec; skip capacity check
	}

	nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred(), "listing nodes for GPU capacity check")
	var totalAllocatable int64
	for _, node := range nodes.Items {
		if node.Spec.Unschedulable {
			continue
		}
		tainted := false
		for _, t := range node.Spec.Taints {
			if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
				tainted = true
				break
			}
		}
		if tainted {
			continue
		}
		if q, ok := node.Status.Allocatable[gpuResource]; ok {
			totalAllocatable += q.Value()
		}
	}

	allPods, err := k8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred(), "listing pods for GPU capacity check")
	var totalRequested int64
	for _, pod := range allPods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if q, ok := c.Resources.Requests[gpuResource]; ok {
				totalRequested += q.Value()
			}
		}
	}

	available := totalAllocatable - totalRequested
	if available < int64(minReplicas) {
		Skip(fmt.Sprintf(
			"insufficient GPU capacity for scale-out test: need %d %s slots, have %d allocatable and %d requested (available=%d); node GPU may be lost or at capacity",
			minReplicas, gpuResource, totalAllocatable, totalRequested, available,
		))
	}
	GinkgoWriter.Printf("GPU capacity check: %s allocatable=%d requested=%d available=%d (need %d)\n",
		gpuResource, totalAllocatable, totalRequested, available, minReplicas)
}

// snapshotNightlySaturationCM captures the current saturation ConfigMap state before the test
// modifies it, so AfterAll can restore it.
func snapshotNightlySaturationCM() (name string, original *corev1.ConfigMap, existed bool) {
	GinkgoHelper()
	name = saturationConfigMapName()
	cm, err := k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return name, cm.DeepCopy(), true
	}
	if !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "failed reading saturation ConfigMap")
	}
	return name, nil, false
}

// restoreNightlySaturationCM restores the saturation ConfigMap to its pre-test state using the
// delete+create pattern to avoid resourceVersion conflicts.
func restoreNightlySaturationCM(cmName string, cmOriginal *corev1.ConfigMap, cmExistedBefore bool) {
	propagation := metav1.DeletePropagationBackground
	if err := k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Delete(ctx, cmName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil && !errors.IsNotFound(err) {
		GinkgoWriter.Printf("Warning: failed to delete saturation ConfigMap before restore: %v\n", err)
	}
	if cmExistedBefore && cmOriginal != nil {
		toCreate := saturationConfigMapForRecreate(cmOriginal)
		if _, err := k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
			GinkgoWriter.Printf("Warning: failed to restore saturation ConfigMap: %v\n", err)
		}
	}
}

// deleteNightlyBurstJob removes the burst job and its pods. Safe to call in AfterEach.
func deleteNightlyBurstJob(jobName string) {
	propagation := metav1.DeletePropagationBackground
	_ = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	_ = k8sClient.CoreV1().Pods(cfg.LLMDNamespace).DeleteCollection(ctx,
		metav1.DeleteOptions{PropagationPolicy: &propagation},
		metav1.ListOptions{LabelSelector: "job-name=" + jobName},
	)
}

// nightlyKEDAHPADesiredReplicas returns the DesiredReplicas of the KEDA-managed HPA targeting the
// nightly Deployment. KEDA names the HPA automatically, so it is found by scaleTargetRef rather
// than by a fixed name.
func nightlyKEDAHPADesiredReplicas(g Gomega) (desired int32, minReplicas int32) {
	hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	minReplicas = 1
	for i := range hpaList.Items {
		if hpaList.Items[i].Spec.ScaleTargetRef.Name == cfg.NightlyDeployment {
			if hpaList.Items[i].Spec.MinReplicas != nil {
				minReplicas = *hpaList.Items[i].Spec.MinReplicas
			}
			return hpaList.Items[i].Status.DesiredReplicas, minReplicas
		}
	}
	g.Expect(false).To(BeTrue(), "KEDA-managed HPA targeting %s not found", cfg.NightlyDeployment)
	return 0, minReplicas
}

// expectScaledObjectNotInFallback asserts the ScaledObject is not serving KEDA fallback replicas.
// KEDA suppresses Prometheus query errors and serves spec.fallback when configured; a Fallback=True
// condition means KEDA is NOT reading a live wva_desired_replicas series, so any scale assertion
// would be proving nothing about the metric path.
func expectScaledObjectNotInFallback(soName string) {
	GinkgoHelper()
	so := &kedav1alpha1.ScaledObject{}
	Expect(crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)).To(Succeed(),
		"ScaledObject %s should exist", soName)
	for _, c := range so.Status.Conditions {
		if string(c.Type) == "Fallback" {
			Expect(c.Status).NotTo(Equal(metav1.ConditionTrue),
				"ScaledObject %s is in KEDA fallback — the Thanos query is not returning wva_desired_replicas "+
					"(check the ClusterTriggerAuthentication bearer token and the metric series)", soName)
		}
	}
}

// assertNightlySaturationScaleUp submits the burst job and asserts WVA detects saturation, KEDA
// drives the HPA to >= 2 replicas from a LIVE metric read (not fallback), the Deployment actuates,
// and the burst job completes.
func assertNightlySaturationScaleUp(jobName, gatewayService string) {
	GinkgoHelper()

	deleteNightlyBurstJob(jobName)
	job := createNightlySaturationBurstJob(jobName, cfg.LLMDNamespace, gatewayService, cfg.ModelID, cfg.NightlyBurstSize, nightlyMaxTokens)
	_, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "burst job should be created")

	By("Waiting for burst job pod to start sending requests")
	Eventually(func(g Gomega) {
		pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "job-name=" + jobName,
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).NotTo(BeEmpty())
		phase := pods.Items[0].Status.Phase
		g.Expect(phase).To(Or(Equal(corev1.PodRunning), Equal(corev1.PodSucceeded)))
	}, time.Duration(cfg.EventuallyStandardSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

	By("Waiting for KEDA HPA to desire >= 2 replicas (WVA detected saturation)")
	Eventually(func(g Gomega) {
		desired, _ := nightlyKEDAHPADesiredReplicas(g)
		g.Expect(desired).To(BeNumerically(">=", 2),
			"HPA should desire >= 2 replicas when the vLLM queue is saturated")
	}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

	By("Asserting the ScaledObject read a live wva_desired_replicas series (not KEDA fallback)")
	expectScaledObjectNotInFallback(nightlyScaledObjectName())

	By("Waiting for the Deployment to actuate >= 2 Ready replicas")
	Eventually(func(g Gomega) {
		dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, cfg.NightlyDeployment, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
			"Deployment should scale to >= 2 Ready replicas under saturation")
	}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

	By("Waiting for burst job to complete (all concurrent requests served)")
	Eventually(func(g Gomega) {
		j, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(j.Status.Succeeded).To(BeNumerically(">", 0))
	}, 2*time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalVerySlowSec)*time.Second).Should(Succeed())
}

// assertNightlySaturationScaleDown asserts the HPA returns to minReplicas after the burst drains.
// Must run after assertNightlySaturationScaleUp (Deployment at >= 2 replicas). WVA requires
// >= 2 non-saturated replicas before approving scale-down, so we wait for both pods Ready first.
func assertNightlySaturationScaleDown() {
	GinkgoHelper()

	By("Waiting for the scaled-up Deployment to have 2 Ready replicas (WVA scale-down needs both pods non-saturated)")
	Eventually(func(g Gomega) {
		dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, cfg.NightlyDeployment, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
			"Deployment should have >= 2 Ready replicas before scale-down can be approved")
	}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

	By("Waiting for the KEDA HPA to return to minReplicas after the queue drains " +
		"(WVA requires >= 2 pods non-saturated; the 300s HPA scale-down stabilization window applies)")
	Eventually(func(g Gomega) {
		desired, minReplicas := nightlyKEDAHPADesiredReplicas(g)
		g.Expect(desired).To(Equal(minReplicas),
			"HPA should return to minReplicas=%d after the queue drains", minReplicas)
	}, nightlyScaleDownSec*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())
}

// V2 is the primary nightly scenario: it is the only place the real token-capacity (non-fallback)
// path runs, since the simulator cannot emit vllm:cache_config_info.
var _ = Describe("Nightly Saturation — V2 Token Analyzer", Label("nightly"), Ordered, func() {
	const burstJobName = "nightly-saturation-burst-v2"
	var (
		gatewayService  string
		cmName          string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		if cfg.UseSimulator {
			Skip("nightly saturation tests require USE_SIMULATOR=false (real vLLM)")
		}
		if cfg.Environment != envOpenShift {
			Skip("nightly saturation tests require ENVIRONMENT=openshift")
		}
		checkGPUCapacity(cfg.NightlyDeployment, 2)
		gatewayService = discoverNightlyGateway()
		createNightlyWVAResources()

		cmName, cmOriginal, cmExistedBefore = snapshotNightlySaturationCM()
		v2Config := buildSaturationConfigYAML("saturation")
		Expect(upsertSaturationConfigEntry(ctx, cfg.WVANamespace, cmName, "default", v2Config)).To(Succeed(),
			"writing V2 saturation config")
		By("Waiting for WVA to pick up V2 config and log the V2 analyzer path")
		expectAnalyzerPathLog("V2", cfg.ModelID)
	})

	AfterAll(func() {
		deleteNightlyWVAResources()
		By("Restoring saturation ConfigMap to pre-test state")
		if cmName != "" {
			restoreNightlySaturationCM(cmName, cmOriginal, cmExistedBefore)
		}
	})

	AfterEach(func() { deleteNightlyBurstJob(burstJobName) })

	It("should scale up via the V2 token analyzer under saturation, reading a live metric (not fallback)", func() {
		By("Submitting a concurrent burst to saturate the vLLM queue via the V2 analyzer")
		assertNightlySaturationScaleUp(burstJobName, gatewayService)
	})

	It("should scale back down after the burst completes", func() {
		assertNightlySaturationScaleDown()
	})
})

var _ = Describe("Nightly Saturation — V1 Threshold Analyzer", Label("nightly"), Ordered, func() {
	const burstJobName = "nightly-saturation-burst-v1"
	var (
		gatewayService  string
		cmName          string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		if cfg.UseSimulator {
			Skip("nightly saturation tests require USE_SIMULATOR=false (real vLLM)")
		}
		if cfg.Environment != envOpenShift {
			Skip("nightly saturation tests require ENVIRONMENT=openshift")
		}
		checkGPUCapacity(cfg.NightlyDeployment, 2)
		gatewayService = discoverNightlyGateway()
		createNightlyWVAResources()

		cmName, cmOriginal, cmExistedBefore = snapshotNightlySaturationCM()
		Expect(cfg.NightlyBurstSize).To(BeNumerically(">", nightlyQueueThreshold),
			"E2E_NIGHTLY_BURST_SIZE must exceed queueLengthThreshold=%d to saturate the queue", nightlyQueueThreshold)
		v1Config := buildSaturationConfigYAMLWithThresholds("", 0.85, nightlyQueueThreshold, 0.10, 3, 0.85, 0.70)
		Expect(upsertSaturationConfigEntry(ctx, cfg.WVANamespace, cmName, "default", v1Config)).To(Succeed(),
			"writing V1 saturation config")
		By("Waiting for WVA to pick up V1 config")
		expectAnalyzerPathLog("V1", cfg.ModelID)
	})

	AfterAll(func() {
		deleteNightlyWVAResources()
		By("Restoring saturation ConfigMap to pre-test state")
		if cmName != "" {
			restoreNightlySaturationCM(cmName, cmOriginal, cmExistedBefore)
		}
	})

	AfterEach(func() { deleteNightlyBurstJob(burstJobName) })

	It("should scale up when the queue exceeds the threshold", func() {
		By("Submitting a concurrent burst to saturate the vLLM queue via the V1 threshold analyzer")
		assertNightlySaturationScaleUp(burstJobName, gatewayService)
	})

	It("should scale back down after the burst completes", func() {
		assertNightlySaturationScaleDown()
	})
})

// createNightlySaturationBurstJob creates a Job that sends burstSize concurrent requests with high
// max_tokens to force them into the vLLM waiting queue (requires --max-num-seqs=1).
func createNightlySaturationBurstJob(name, namespace, gatewayService, modelID string, burstSize, maxTokens int) *batchv1.Job {
	backoffLimit := int32(0)

	// All requests are fired in the background so they are concurrent. 'wait' collects them; the
	// job exits 0 once all finish (success or error).
	script := fmt.Sprintf(`#!/bin/sh
echo "Nightly saturation burst: %d concurrent requests to %s:80 model=%s max_tokens=%d"
N=%d
i=1
while [ $i -le $N ]; do
  curl -s --max-time 600 -X POST http://%s:80/v1/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"%s","prompt":"Saturation test prompt for nightly WVA e2e. Respond in detail.","max_tokens":%d}' \
    -o /dev/null &
  i=$((i + 1))
done
echo "All $N requests dispatched. Waiting for completion..."
wait
echo "Burst complete."
`, burstSize, gatewayService, modelID, maxTokens,
		burstSize, gatewayService, modelID, maxTokens)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": "true"},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"test-resource": "true"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "burst-curl",
							Image:   "quay.io/curl/curl:8.11.1",
							Command: []string{"sh", "-c", script},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
}
