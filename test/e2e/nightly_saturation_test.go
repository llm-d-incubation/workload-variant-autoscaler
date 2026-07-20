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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
)

// Nightly saturation tests exercise the full WVA decision loop against a live vLLM process on the
// OCP nightly cluster — the ONLY place V2's true token-capacity path runs. On kind + the simulator,
// saturation_v2_test.go proves V2 path selection and the KEDA pipeline, but only the percentage
// fallback: the simulator emits no vllm:cache_config_info, so TotalKvCapacityTokens == 0 and the
// engine routes to computeReplicaCapacityFallback. Real vLLM emits the cache-config and token
// histograms that drive the k1/k2 token-accounting path, which is what this test covers.
//
// Ownership — the guide deploy step (guides/workload-autoscaling/scripts/nightly-deploy-ocp.sh in
// llm-d/llm-d) owns EVERYTHING except the load: the vLLM Deployment, the inference gateway, the WVA
// config, and the KEDA ScaledObject (annotated llm-d.ai/managed) + its TriggerAuthentication. The
// deploy step itself already asserts the metric pipeline is live (ScaledObject Fallback=False). So
// this test does NOT create any scaler/auth/config — it DISCOVERS the guide's managed ScaledObject,
// follows its scaleTargetRef to the Deployment (rather than trusting the reusable workflow's
// injected DEPLOYMENT env, which does not match this guide), then drives a burst and asserts the
// dynamic scale-up/down — the behavior the deploy's static Fallback check cannot cover.
//
// Requires: USE_SIMULATOR=false, ENVIRONMENT=openshift, and the guide stack deployed first.
//
// The burst: N concurrent requests with high max_tokens. With vLLM --max-num-seqs=1, all but one
// enter the waiting queue, pushing num_requests_waiting above the saturation threshold so WVA
// recommends a scale-up.

const (
	nightlyMaxTokens    = 1500 // keep requests in-flight long enough for WVA to detect saturation; must be < max-model-len
	nightlyScaleDownSec = 600  // pod model load (~4min) + WVA cycle + the HPA scale-down stabilization window + buffer
)

// discoverNightlyScaler finds the guide-deployed WVA-managed ScaledObject (llm-d.ai/managed=true)
// and returns its name plus the Deployment it targets. This is the source of truth for the target
// names — the reusable workflow injects a DEPLOYMENT that does not match the workload-autoscaling
// guide, so we deliberately do not trust it. If DEPLOYMENT is set and a managed ScaledObject
// targets it, that one is preferred; otherwise the single managed ScaledObject is used.
func discoverNightlyScaler() (scaledObjectName, deploymentName string) {
	GinkgoHelper()
	soList := &kedav1alpha1.ScaledObjectList{}
	Expect(crClient.List(ctx, soList, client.InNamespace(cfg.LLMDNamespace))).To(Succeed(),
		"listing ScaledObjects in %s", cfg.LLMDNamespace)

	var managed []kedav1alpha1.ScaledObject
	for i := range soList.Items {
		so := soList.Items[i]
		if so.Annotations[annotations.Managed] == "true" &&
			so.Spec.ScaleTargetRef != nil && so.Spec.ScaleTargetRef.Name != "" {
			managed = append(managed, so)
		}
	}
	Expect(managed).NotTo(BeEmpty(),
		"no WVA-managed ScaledObject (llm-d.ai/managed=true) found in namespace %s — the guide deploy step must run first",
		cfg.LLMDNamespace)

	chosen := managed[0]
	if cfg.NightlyDeployment != "" {
		for i := range managed {
			if managed[i].Spec.ScaleTargetRef.Name == cfg.NightlyDeployment {
				chosen = managed[i]
				break
			}
		}
	}
	GinkgoWriter.Printf("Nightly scaler discovered: ScaledObject=%s target Deployment=%s (variant_name=%s)\n",
		chosen.Name, chosen.Spec.ScaleTargetRef.Name, chosen.Name)
	return chosen.Name, chosen.Spec.ScaleTargetRef.Name
}

// discoverNightlyGateway finds the inference-gateway Service name. If GATEWAY_NAME is set (injected
// by the reusable workflow) it is used directly; otherwise the namespace is scanned.
func discoverNightlyGateway() string {
	GinkgoHelper()

	if cfg.GatewayName != "" {
		GinkgoWriter.Printf("Nightly gateway: %s (from GATEWAY_NAME)\n", cfg.GatewayName)
		return cfg.GatewayName
	}

	svcs, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred(), "should list services in llmd namespace")
	for _, svc := range svcs.Items {
		if strings.Contains(svc.Name, "inference-gateway") || strings.Contains(svc.Name, "epp") {
			GinkgoWriter.Printf("Nightly gateway: %s\n", svc.Name)
			return svc.Name
		}
	}
	if cfg.EPPServiceName != "" {
		return cfg.EPPServiceName
	}
	Fail("inference-gateway service not found in namespace — run the guide deploy step first")
	return ""
}

// checkGPUCapacity skips the test if fewer than minReplicas GPU slots are available across all
// schedulable nodes, so the scale-out assertion does not churn UnexpectedAdmissionError pods when
// the GPU node is unhealthy or at capacity.
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

// nightlyKEDAHPADesiredReplicas returns the DesiredReplicas and minReplicas of the KEDA-managed HPA
// targeting deploymentName. KEDA names the HPA automatically, so it is found by scaleTargetRef.
func nightlyKEDAHPADesiredReplicas(g Gomega, deploymentName string) (desired int32, minReplicas int32) {
	hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	minReplicas = 1
	for i := range hpaList.Items {
		if hpaList.Items[i].Spec.ScaleTargetRef.Name == deploymentName {
			if hpaList.Items[i].Spec.MinReplicas != nil {
				minReplicas = *hpaList.Items[i].Spec.MinReplicas
			}
			return hpaList.Items[i].Status.DesiredReplicas, minReplicas
		}
	}
	g.Expect(false).To(BeTrue(), "KEDA-managed HPA targeting %s not found", deploymentName)
	return 0, minReplicas
}

// expectScaledObjectNotInFallback asserts the ScaledObject is not serving KEDA fallback replicas.
// KEDA suppresses Prometheus query errors and serves spec.fallback when configured; a Fallback=True
// condition means KEDA is NOT reading a live wva_desired_replicas series, so any scale assertion
// would prove nothing about the metric path. (The deploy step already asserts this once; we re-check
// under load to confirm it stayed live.)
func expectScaledObjectNotInFallback(soName string) {
	GinkgoHelper()
	so := &kedav1alpha1.ScaledObject{}
	Expect(crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)).To(Succeed(),
		"ScaledObject %s should exist", soName)
	for _, c := range so.Status.Conditions {
		if string(c.Type) == "Fallback" {
			Expect(c.Status).NotTo(Equal(metav1.ConditionTrue),
				"ScaledObject %s is in KEDA fallback — KEDA is not reading wva_desired_replicas "+
					"(replica count is coming from spec.fallback, not from WVA)", soName)
		}
	}
}

// assertNightlySaturationScaleUp submits the burst job and asserts WVA detects saturation, KEDA
// drives the HPA to >= 2 replicas from a LIVE metric read (not fallback), the Deployment actuates,
// and the burst job completes.
func assertNightlySaturationScaleUp(jobName, gatewayService, soName, deploymentName string) {
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

	By("Waiting for the KEDA HPA to desire >= 2 replicas (WVA detected saturation)")
	Eventually(func(g Gomega) {
		desired, _ := nightlyKEDAHPADesiredReplicas(g, deploymentName)
		g.Expect(desired).To(BeNumerically(">=", 2),
			"HPA should desire >= 2 replicas when the vLLM queue is saturated")
	}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

	By("Asserting the ScaledObject read a live wva_desired_replicas series (not KEDA fallback)")
	expectScaledObjectNotInFallback(soName)

	By("Waiting for the Deployment to actuate >= 2 Ready replicas")
	Eventually(func(g Gomega) {
		dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
			"Deployment should scale to >= 2 Ready replicas under saturation")
	}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

	By("Waiting for the burst job to complete (all concurrent requests served)")
	Eventually(func(g Gomega) {
		j, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(j.Status.Succeeded).To(BeNumerically(">", 0))
	}, 2*time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalVerySlowSec)*time.Second).Should(Succeed())
}

// assertNightlySaturationScaleDown asserts the HPA returns to minReplicas after the burst drains.
// Must run after assertNightlySaturationScaleUp (Deployment at >= 2 replicas). WVA requires >= 2
// non-saturated replicas before approving scale-down, so we wait for both pods Ready first.
func assertNightlySaturationScaleDown(deploymentName string) {
	GinkgoHelper()

	By("Waiting for the scaled-up Deployment to have 2 Ready replicas (WVA scale-down needs both pods non-saturated)")
	Eventually(func(g Gomega) {
		dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
			"Deployment should have >= 2 Ready replicas before scale-down can be approved")
	}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

	By("Waiting for the KEDA HPA to return to minReplicas after the queue drains " +
		"(WVA requires >= 2 pods non-saturated; the HPA scale-down stabilization window applies)")
	Eventually(func(g Gomega) {
		desired, minReplicas := nightlyKEDAHPADesiredReplicas(g, deploymentName)
		g.Expect(desired).To(Equal(minReplicas),
			"HPA should return to minReplicas=%d after the queue drains", minReplicas)
	}, nightlyScaleDownSec*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())
}

// The nightly is V2-first (V1 is deprecated). It observes the guide-deployed scaler; the guide's WVA
// config selects the V2 saturation path, and BeforeAll confirms it before driving load.
var _ = Describe("Nightly Saturation — V2 (real vLLM, guide-deployed scaler)", Label("nightly"), Ordered, func() {
	const burstJobName = "nightly-saturation-burst"
	var (
		gatewayService   string
		scaledObjectName string
		deploymentName   string
	)

	BeforeAll(func() {
		if cfg.UseSimulator {
			Skip("nightly saturation tests require USE_SIMULATOR=false (real vLLM)")
		}
		if cfg.Environment != envOpenShift {
			Skip("nightly saturation tests require ENVIRONMENT=openshift")
		}
		scaledObjectName, deploymentName = discoverNightlyScaler()
		checkGPUCapacity(deploymentName, 2)
		gatewayService = discoverNightlyGateway()

		By("Confirming WVA selected the V2 analyzer path for the discovered model")
		expectAnalyzerPathLog("V2", cfg.ModelID)
	})

	AfterEach(func() { deleteNightlyBurstJob(burstJobName) })

	It("should scale up under saturation, reading a live metric (not fallback)", func() {
		By("Submitting a concurrent burst to saturate the vLLM queue")
		assertNightlySaturationScaleUp(burstJobName, gatewayService, scaledObjectName, deploymentName)
	})

	It("should scale back down after the burst completes", func() {
		assertNightlySaturationScaleDown(deploymentName)
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
