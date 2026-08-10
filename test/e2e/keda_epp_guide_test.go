package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kedaEPPGuideScaledObject   = "optimized-baseline-keda-epp"
	kedaEPPGuideDeployment     = "optimized-baseline-nvidia-gpu-vllm-decode"
	kedaEPPGuideAuthentication = "keda-prometheus-auth"
	kedaEPPGuideQueueTrigger   = "epp-queue-size"
	kedaEPPGuideRunTrigger     = "epp-running-requests"
	kedaEPPGuideQueueQuery     = `sum(llm_d_epp_flow_control_queue_size{namespace="llm-d-optimized-baseline",service="optimized-baseline-epp",model_name="Qwen/Qwen3-32B"})`
	kedaEPPGuideRunQuery       = `sum(llm_d_epp_request_running{namespace="llm-d-optimized-baseline",service="optimized-baseline-epp",model_name="Qwen/Qwen3-32B"})`
	kedaEPPGuideQueueThreshold = "1"
	kedaEPPGuideRunThreshold   = "16"
	kedaEPPGuideHPAName        = "keda-hpa-optimized-baseline"
	kedaEPPGuideServiceMonitor = "optimized-baseline-epp-monitor"
	kedaEPPGuidePrometheusSvc  = "kube-prometheus-stack-prometheus"
	kedaEPPGuideMinReplicas    = int32(1)
	kedaEPPGuideMaxReplicas    = int32(8)
	kedaEPPGuideStableCount    = 3
	kedaEPPGuideRequestLimit   = 1500
	kedaEPPGuideAPITimeout     = 10 * time.Second
	kedaEPPGuideProbeTimeout   = 60 * time.Second
	kedaEPPGuideLogTailLines   = int64(300)
	kedaEPPGuideLogSinceSecs   = int64(1800)
	kedaEPPGuideMaxLogStreams  = 2
	kedaEPPGuideCurlImage      = "quay.io/curl/curl:8.11.1@sha256:2db4e6a8fd6a0e4d0db5828b2722cf6db15c3005178a4c65588b903e4784ba11"
)

var _ = Describe("KEDA EPP guide contract", Label("keda-epp-guide"), Ordered, func() {
	var requestPods []string

	deleteRequests := func() {
		deleteKEDAEPPGuidePods(requestPods)
	}

	BeforeAll(func() {
		Expect(cfg.DeployWVA).To(BeFalse(), "the direct-KEDA guide spec must run with DEPLOY_WVA=false")
		Expect(cfg.UseSimulator).To(BeTrue(), "the direct-KEDA guide spec uses a deterministic simulator workload")
		Expect(cfg.ModelID).To(Equal("Qwen/Qwen3-32B"), "the simulator and canonical EPP metrics must use the same model")
		warmupBudget := kedaEPPGuideWarmupObservationBudget()
		deterministicBudget := kedaEPPGuideDeterministicObservationBudget()
		maxObservationBudget := max(warmupBudget, deterministicBudget)
		requestLifetime := time.Duration(kedaEPPGuideRequestLimit) * time.Second
		GinkgoWriter.Printf(
			"KEDA+EPP timing hierarchy: warmup=%s deterministic=%s required=%s requestLifetime=%s completeSpec=%s\n",
			warmupBudget,
			deterministicBudget,
			maxObservationBudget,
			requestLifetime,
			kedaEPPGuideCompleteSpecBudget(),
		)
		Expect(requestLifetime).To(
			BeNumerically(">", maxObservationBudget),
			"request lifetime must strictly exceed the complete warmup and deterministic observation bounds",
		)

		By("observing the pre-created guide-owned deterministic simulator")
		DeferCleanup(func() {
			deleteContext, cancelDelete := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
			defer cancelDelete()
			err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(
				deleteContext,
				kedaEPPGuideDeployment,
				metav1.DeleteOptions{},
			)
			Expect(err == nil || apierrors.IsNotFound(err)).To(
				BeTrue(),
				"failed to delete guide-owned simulator Deployment: %v",
				err,
			)
		})

		DeferCleanup(deleteRequests)

		By("waiting for the guide-owned simulator to be available")
		Eventually(func(g Gomega) {
			callContext, cancelCall := kedaEPPGuideCallContext()
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(
				callContext,
				kedaEPPGuideDeployment,
				metav1.GetOptions{},
			)
			cancelCall()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("observes the canonical guide and performs one bounded 1-to-2 transition", func() {
		scaledObject := waitForKEDAEPPGuideScaledObjectCreated()
		Expect(scaledObject.Spec.ScaleTargetRef).NotTo(BeNil())
		Expect(scaledObject.Spec.ScaleTargetRef.APIVersion).To(Equal("apps/v1"))
		Expect(scaledObject.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
		Expect(scaledObject.Spec.ScaleTargetRef.Name).To(Equal(kedaEPPGuideDeployment))
		Expect(scaledObject.Spec.MinReplicaCount).NotTo(BeNil())
		Expect(*scaledObject.Spec.MinReplicaCount).To(Equal(kedaEPPGuideMinReplicas))
		Expect(scaledObject.Spec.MaxReplicaCount).NotTo(BeNil())
		Expect(*scaledObject.Spec.MaxReplicaCount).To(Equal(kedaEPPGuideMaxReplicas))
		Expect(scaledObject.Spec.Advanced).NotTo(BeNil())
		Expect(scaledObject.Spec.Advanced.HorizontalPodAutoscalerConfig).NotTo(BeNil())
		Expect(scaledObject.Spec.Advanced.HorizontalPodAutoscalerConfig.Name).To(Equal(kedaEPPGuideHPAName))
		assertKEDAEPPGuideHPABehavior(scaledObject.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior)
		Expect(scaledObject.Spec.Triggers).To(HaveLen(2))

		seenTriggers := map[string]bool{}
		for _, trigger := range scaledObject.Spec.Triggers {
			Expect(seenTriggers).NotTo(HaveKey(trigger.Name), "canonical trigger %s must be unique", trigger.Name)
			seenTriggers[trigger.Name] = true
			Expect(trigger.Type).To(Equal("prometheus"))
			Expect(trigger.MetricType).To(Equal(autoscalingv2.AverageValueMetricType))
			Expect(trigger.AuthenticationRef).NotTo(BeNil(), "trigger %s must retain its guide authenticationRef", trigger.Name)
			Expect(trigger.AuthenticationRef.Name).To(Equal(kedaEPPGuideAuthentication), "trigger %s must retain its guide authenticationRef", trigger.Name)

			var expectedQuery, expectedThreshold string
			switch trigger.Name {
			case kedaEPPGuideQueueTrigger:
				expectedQuery = kedaEPPGuideQueueQuery
				expectedThreshold = kedaEPPGuideQueueThreshold
			case kedaEPPGuideRunTrigger:
				expectedQuery = kedaEPPGuideRunQuery
				expectedThreshold = kedaEPPGuideRunThreshold
			default:
				Fail(fmt.Sprintf("unexpected Prometheus trigger %q on the canonical ScaledObject", trigger.Name))
			}
			Expect(trigger.Metadata).To(HaveKeyWithValue("query", expectedQuery), "trigger %s must retain its exact canonical query", trigger.Name)
			Expect(trigger.Metadata).To(HaveKeyWithValue("threshold", expectedThreshold), "trigger %s must retain its exact canonical threshold", trigger.Name)
			_, err := resource.ParseQuantity(trigger.Metadata["threshold"])
			Expect(err).NotTo(HaveOccurred(), "trigger %s should have a numeric threshold", trigger.Name)
		}
		Expect(seenTriggers).To(HaveKey(kedaEPPGuideQueueTrigger))
		Expect(seenTriggers).To(HaveKey(kedaEPPGuideRunTrigger))

		By("sending a bounded warmup before requiring lazy EPP metric-series liveness")
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())
		waitForKEDAEPPGuideRequestPods(requestPods)
		assertKEDAEPPGuideServiceMonitor()
		assertKEDAEPPGuideSecureTrust()
		waitForKEDAEPPGuideDirectMetric(kedaEPPGuideRunTrigger, 1)
		waitForKEDAEPPGuideDirectMetric(kedaEPPGuideQueueTrigger, 0)
		waitForKEDAEPPGuidePrometheusTarget()
		waitForKEDAEPPGuidePrometheusValue(kedaEPPGuideRunQuery, 1)
		waitForKEDAEPPGuidePrometheusValue(kedaEPPGuideQueueQuery, 0)

		By("ending warmup and proving it cannot contaminate the deterministic phases")
		deleteKEDAEPPGuidePods(requestPods)
		waitForKEDAEPPGuidePodsDeleted(requestPods)
		requestPods = nil
		waitForKEDAEPPGuidePrometheusValue(kedaEPPGuideRunQuery, 0)
		waitForKEDAEPPGuidePrometheusValue(kedaEPPGuideQueueQuery, 0)

		By("requiring KEDA liveness only after both per-model series exist")
		scaledObject = waitForKEDAEPPGuideScaledObjectReady()
		Expect(scaledObject.Status.HpaName).To(Equal(kedaEPPGuideHPAName))
		hpa := waitForKEDAEPPGuideHPA(scaledObject)
		Expect(hpa.Spec.MinReplicas).NotTo(BeNil())
		Expect(*hpa.Spec.MinReplicas).To(Equal(kedaEPPGuideMinReplicas))
		Expect(hpa.Spec.MaxReplicas).To(Equal(kedaEPPGuideMaxReplicas))
		Expect(hpa.Spec.ScaleTargetRef.APIVersion).To(Equal("apps/v1"))
		Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
		Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(kedaEPPGuideDeployment))
		assertKEDAEPPGuideHPABehavior(hpa.Spec.Behavior)
		Expect(hpa.Spec.Metrics).To(HaveLen(2))

		queueTarget := resource.MustParse(kedaEPPGuideQueueThreshold)
		runningTarget := resource.MustParse(kedaEPPGuideRunThreshold)
		metricNames := map[string]string{}
		for _, metric := range hpa.Spec.Metrics {
			Expect(metric.Type).To(Equal(autoscalingv2.ExternalMetricSourceType))
			Expect(metric.External).NotTo(BeNil())
			Expect(metric.External.Metric.Selector).NotTo(BeNil())
			Expect(metric.External.Metric.Selector.MatchLabels).To(Equal(map[string]string{
				"scaledobject.keda.sh/name": kedaEPPGuideScaledObject,
			}))
			Expect(metric.External.Metric.Selector.MatchExpressions).To(BeEmpty())
			Expect(metric.External.Target.Type).To(Equal(autoscalingv2.AverageValueMetricType))
			Expect(metric.External.Target.AverageValue).NotTo(BeNil())

			switch {
			case metric.External.Target.AverageValue.Cmp(queueTarget) == 0:
				metricNames[kedaEPPGuideQueueTrigger] = metric.External.Metric.Name
			case metric.External.Target.AverageValue.Cmp(runningTarget) == 0:
				metricNames[kedaEPPGuideRunTrigger] = metric.External.Metric.Name
			default:
				Fail(fmt.Sprintf("generated HPA metric %q does not match canonical target 1 or 16", metric.External.Metric.Name))
			}
		}
		Expect(metricNames).To(HaveKey(kedaEPPGuideQueueTrigger))
		Expect(metricNames).To(HaveKey(kedaEPPGuideRunTrigger))
		Expect(metricNames[kedaEPPGuideQueueTrigger]).NotTo(Equal(metricNames[kedaEPPGuideRunTrigger]))
		Eventually(func(g Gomega) {
			for triggerName, metricName := range metricNames {
				value, err := kedaEPPGuideExternalMetric(metricName)
				g.Expect(err).NotTo(HaveOccurred(), "external metric for %s should be live", triggerName)
				g.Expect(value).To(BeNumerically("==", 0), "external metric for %s should be reset after warmup", triggerName)
			}
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		waitForKEDAEPPGuideBaseline(scaledObject.Status.HpaName)
		assertKEDAOperatorLogsClean()

		By("starting one bounded request")
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())
		waitForKEDAEPPGuideRequestPods(requestPods)

		By("waiting until KEDA observes exactly one running request")
		Eventually(func(g Gomega) {
			value, err := kedaEPPGuideExternalMetric(metricNames[kedaEPPGuideRunTrigger])
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(value).To(BeNumerically("==", 1))
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("starting one additional request that remains queued")
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())
		waitForKEDAEPPGuidePhaseA(
			scaledObject.Status.HpaName,
			metricNames[kedaEPPGuideRunTrigger],
			metricNames[kedaEPPGuideQueueTrigger],
			requestPods,
		)

		By("starting exactly one additional request for the bounded scale transition")
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())

		waitForKEDAEPPGuideScaleUp(
			scaledObject.Status.HpaName,
			metricNames[kedaEPPGuideRunTrigger],
			metricNames[kedaEPPGuideQueueTrigger],
			requestPods,
		)

		By("terminating the bounded stimulus after exact-two evidence")
		deleteRequests()
		assertKEDAOperatorLogsClean()
	})
})

func waitForKEDAEPPGuideScaledObjectCreated() *kedav1alpha1.ScaledObject {
	GinkgoHelper()

	var scaledObject *kedav1alpha1.ScaledObject
	Eventually(func(g Gomega) {
		current := &kedav1alpha1.ScaledObject{}
		callContext, cancelCall := kedaEPPGuideCallContext()
		err := crClient.Get(callContext, client.ObjectKey{
			Namespace: cfg.LLMDNamespace,
			Name:      kedaEPPGuideScaledObject,
		}, current)
		cancelCall()
		g.Expect(err).NotTo(HaveOccurred())
		scaledObject = current
	}, time.Duration(cfg.EventuallyMediumSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	return scaledObject
}

func waitForKEDAEPPGuideScaledObjectReady() *kedav1alpha1.ScaledObject {
	GinkgoHelper()

	deadline := time.Now().Add(time.Duration(cfg.EventuallyExtendedSec) * time.Second)
	for time.Now().Before(deadline) {
		scaledObject := &kedav1alpha1.ScaledObject{}
		callContext, cancelCall := kedaEPPGuideCallContext()
		err := crClient.Get(callContext, client.ObjectKey{
			Namespace: cfg.LLMDNamespace,
			Name:      kedaEPPGuideScaledObject,
		}, scaledObject)
		cancelCall()
		if err == nil {
			ready := scaledObject.Status.Conditions.GetReadyCondition()
			if ready.Status == metav1.ConditionTrue && scaledObject.Status.HpaName != "" {
				return scaledObject
			}
		} else if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		time.Sleep(time.Duration(cfg.PollIntervalSec) * time.Second)
	}

	Fail(fmt.Sprintf("ScaledObject %s/%s did not reach Ready=True", cfg.LLMDNamespace, kedaEPPGuideScaledObject))
	return nil
}

func waitForKEDAEPPGuideHPA(scaledObject *kedav1alpha1.ScaledObject) *autoscalingv2.HorizontalPodAutoscaler {
	GinkgoHelper()

	var hpa *autoscalingv2.HorizontalPodAutoscaler
	Eventually(func(g Gomega) {
		callContext, cancelCall := kedaEPPGuideCallContext()
		current, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(
			callContext,
			scaledObject.Status.HpaName,
			metav1.GetOptions{},
		)
		cancelCall()
		g.Expect(err).NotTo(HaveOccurred())

		owned := false
		for _, owner := range current.OwnerReferences {
			if owner.UID == scaledObject.UID &&
				owner.APIVersion == "keda.sh/v1alpha1" &&
				owner.Kind == "ScaledObject" &&
				owner.Name == scaledObject.Name &&
				owner.Controller != nil && *owner.Controller {
				owned = true
				break
			}
		}
		g.Expect(owned).To(BeTrue(), "generated HPA should be owned by the current ScaledObject UID")
		hpa = current
	}).Should(Succeed())
	return hpa
}

func waitForKEDAEPPGuideBaseline(hpaName string) {
	GinkgoHelper()

	stable := 0
	Eventually(func(g Gomega) {
		hpaContext, cancelHPA := kedaEPPGuideCallContext()
		hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(hpaContext, hpaName, metav1.GetOptions{})
		cancelHPA()
		g.Expect(err).NotTo(HaveOccurred())
		deploymentContext, cancelDeployment := kedaEPPGuideCallContext()
		deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(deploymentContext, kedaEPPGuideDeployment, metav1.GetOptions{})
		cancelDeployment()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(deployment.Spec.Replicas).NotTo(BeNil())

		if hpa.Status.CurrentReplicas == 1 &&
			hpa.Status.DesiredReplicas == 1 &&
			*deployment.Spec.Replicas == 1 &&
			deployment.Status.Replicas == 1 &&
			deployment.Status.ReadyReplicas == 1 {
			stable++
		} else {
			stable = 0
		}
		g.Expect(stable).To(BeNumerically(">=", kedaEPPGuideStableCount))
	}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

func createKEDAEPPGuideRequestPod() string {
	GinkgoHelper()

	targetURL := fmt.Sprintf(
		"http://%s.%s.svc.cluster.local:80/v1/chat/completions",
		cfg.EPPServiceName,
		cfg.LLMDNamespace,
	)
	payload := fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"bounded deterministic contract probe"}],"max_tokens":8,"temperature":0}`,
		cfg.ModelID,
	)
	callContext, cancelCall := kedaEPPGuideCallContext()
	pod, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Create(callContext, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "keda-epp-guide-request-",
			Labels: map[string]string{
				"app.kubernetes.io/name": "keda-epp-guide-request",
				"test-resource":          boolTrue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To(int64(0)),
			Containers: []corev1.Container{{
				Name:    "request",
				Image:   kedaEPPGuideCurlImage,
				Command: []string{"sh", "-ec"},
				Args: []string{
					fmt.Sprintf(
						`exec curl --fail --silent --show-error --connect-timeout 10 --max-time %d -H 'Content-Type: application/json' --data-binary "$PAYLOAD" "$TARGET_URL"`,
						kedaEPPGuideRequestLimit,
					),
				},
				Env: []corev1.EnvVar{
					{Name: "TARGET_URL", Value: targetURL},
					{Name: "PAYLOAD", Value: payload},
				},
			}},
		},
	}, metav1.CreateOptions{})
	cancelCall()
	Expect(err).NotTo(HaveOccurred())
	return pod.Name
}

func deleteKEDAEPPGuidePods(names []string) {
	GinkgoHelper()

	grace := int64(0)
	for _, name := range names {
		deleteContext, cancelDelete := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
		err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(deleteContext, name, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
		})
		cancelDelete()
		if err != nil && !apierrors.IsNotFound(err) {
			GinkgoWriter.Printf("WARNING: failed to delete guide pod %s: %v\n", name, err)
		}
	}
}

func waitForKEDAEPPGuidePodsDeleted(names []string) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		for _, name := range names {
			callContext, cancelCall := kedaEPPGuideCallContext()
			_, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(callContext, name, metav1.GetOptions{})
			cancelCall()
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "guide pod %s should be deleted, got %v", name, err)
		}
	}, time.Duration(cfg.EventuallyMediumSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).Should(Succeed())
}

func assertKEDAEPPGuideServiceMonitor() {
	GinkgoHelper()

	serviceMonitor := &promoperator.ServiceMonitor{}
	Eventually(func(g Gomega) {
		callContext, cancelCall := kedaEPPGuideCallContext()
		err := crClient.Get(callContext, client.ObjectKey{
			Namespace: cfg.LLMDNamespace,
			Name:      kedaEPPGuideServiceMonitor,
		}, serviceMonitor)
		cancelCall()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(serviceMonitor.Spec.Endpoints).To(HaveLen(1))
		g.Expect(serviceMonitor.Spec.Endpoints[0].Port).To(Equal("http-metrics"))
		g.Expect(serviceMonitor.Spec.Endpoints[0].Path).To(Equal("/metrics"))
	}, time.Duration(cfg.EventuallyMediumSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

func assertKEDAEPPGuideSecureTrust() {
	GinkgoHelper()

	for namespace, name := range map[string]string{
		cfg.LLMDNamespace: "keda-prometheus-auth",
		cfg.KEDANamespace: "llmd-prometheus-ca",
	} {
		callContext, cancelCall := kedaEPPGuideCallContext()
		secret, err := k8sClient.CoreV1().Secrets(namespace).Get(callContext, name, metav1.GetOptions{})
		cancelCall()
		Expect(err).NotTo(HaveOccurred())
		Expect(secret.Data).To(HaveLen(1), "%s/%s must contain only the public CA", namespace, name)
		Expect(secret.Data).To(HaveKey("ca.crt"), "%s/%s must contain the public CA", namespace, name)
	}

	callContext, cancelCall := kedaEPPGuideCallContext()
	operator, err := k8sClient.AppsV1().Deployments(cfg.KEDANamespace).Get(callContext, "keda-operator", metav1.GetOptions{})
	cancelCall()
	Expect(err).NotTo(HaveOccurred())
	Expect(operator.Spec.Template.Spec.Containers).NotTo(BeEmpty())
	operatorContainer := operator.Spec.Template.Spec.Containers[0]
	Expect(operatorContainer.Args).To(ContainElement("--ca-dir=/custom/ca"))

	foundMount := false
	for _, mount := range operatorContainer.VolumeMounts {
		if mount.Name == "llmd-prometheus-ca" && mount.MountPath == "/custom/ca" && mount.ReadOnly {
			foundMount = true
		}
	}
	Expect(foundMount).To(BeTrue(), "KEDA operator must mount the public Prometheus CA read-only")
}

func waitForKEDAEPPGuideDirectMetric(triggerName string, expected float64) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		output, err := runKEDAEPPGuideCurlProbe(
			"direct-metrics",
			[]string{
				"--fail", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "30",
				fmt.Sprintf("http://%s.%s.svc.cluster.local:9090/metrics", cfg.EPPServiceName, cfg.LLMDNamespace),
			},
			false,
		)
		g.Expect(err).NotTo(HaveOccurred())
		aggregate, matches, err := kedaEPPGuideDirectMetricValue(output, triggerName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(matches).To(BeNumerically(">=", 1), "direct EPP metric %s must expose at least one matching per-model series", triggerName)
		g.Expect(aggregate).To(BeNumerically("==", expected))
	}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

func kedaEPPGuideDirectMetricValue(output, triggerName string) (float64, int, error) {
	metricName := map[string]string{
		kedaEPPGuideQueueTrigger: "llm_d_epp_flow_control_queue_size",
		kedaEPPGuideRunTrigger:   "llm_d_epp_request_running",
	}[triggerName]
	if metricName == "" {
		return 0, 0, fmt.Errorf("unknown guide trigger %q", triggerName)
	}

	matches := 0
	aggregate := float64(0)
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, metricName+"{") ||
			!strings.Contains(line, `model_name="Qwen/Qwen3-32B"`) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0, matches, fmt.Errorf("unexpected direct metric line %q", line)
		}
		parsed, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0, matches, fmt.Errorf("parse direct metric value %q: %w", fields[1], err)
		}
		matches++
		aggregate += parsed
	}
	return aggregate, matches, nil
}

type kedaEPPGuidePrometheusQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func waitForKEDAEPPGuidePrometheusValue(query string, expected float64) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		value, cardinality, err := kedaEPPGuidePrometheusValue(query)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cardinality).To(Equal(1), "PromQL must return exactly one result rather than treating absence as zero")
		g.Expect(value).To(BeNumerically("==", expected))
	}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

func kedaEPPGuidePrometheusValue(query string) (float64, int, error) {
	prometheusURL := fmt.Sprintf(
		"https://%s.%s.svc.cluster.local:9090/api/v1/query",
		kedaEPPGuidePrometheusSvc,
		cfg.MonitoringNS,
	)
	output, err := runKEDAEPPGuideCurlProbe(
		"prom-query",
		[]string{
			"--fail", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "30",
			"--cacert", "/var/run/prometheus-ca/ca.crt", "--get", "--data-urlencode", "query=" + query,
			prometheusURL,
		},
		true,
	)
	if err != nil {
		return 0, 0, err
	}

	response := kedaEPPGuidePrometheusQueryResponse{}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return 0, 0, fmt.Errorf("decode Prometheus query response: %w", err)
	}
	if response.Status != "success" {
		return 0, 0, fmt.Errorf("Prometheus query status is %q", response.Status)
	}
	if len(response.Data.Result) != 1 {
		return 0, len(response.Data.Result), nil
	}
	if len(response.Data.Result[0].Value) != 2 {
		return 0, 1, fmt.Errorf("Prometheus result has %d value fields, want 2", len(response.Data.Result[0].Value))
	}
	var stringValue string
	if err := json.Unmarshal(response.Data.Result[0].Value[1], &stringValue); err != nil {
		return 0, 1, fmt.Errorf("decode Prometheus numeric value: %w", err)
	}
	value, err := strconv.ParseFloat(stringValue, 64)
	if err != nil {
		return 0, 1, fmt.Errorf("parse Prometheus numeric value %q: %w", stringValue, err)
	}
	return value, 1, nil
}

type kedaEPPGuidePrometheusTargetsResponse struct {
	Status string `json:"status"`
	Data   struct {
		ActiveTargets []struct {
			Labels    map[string]string `json:"labels"`
			Health    string            `json:"health"`
			LastError string            `json:"lastError"`
		} `json:"activeTargets"`
	} `json:"data"`
}

func waitForKEDAEPPGuidePrometheusTarget() {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		prometheusURL := fmt.Sprintf(
			"https://%s.%s.svc.cluster.local:9090/api/v1/targets?state=active",
			kedaEPPGuidePrometheusSvc,
			cfg.MonitoringNS,
		)
		output, err := runKEDAEPPGuideCurlProbe(
			"prom-targets",
			[]string{
				"--fail", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "30",
				"--cacert", "/var/run/prometheus-ca/ca.crt", prometheusURL,
			},
			true,
		)
		g.Expect(err).NotTo(HaveOccurred())
		response := kedaEPPGuidePrometheusTargetsResponse{}
		g.Expect(json.Unmarshal([]byte(output), &response)).To(Succeed())
		g.Expect(response.Status).To(Equal("success"))

		matched := 0
		for _, target := range response.Data.ActiveTargets {
			if target.Labels["namespace"] == cfg.LLMDNamespace && target.Labels["service"] == cfg.EPPServiceName {
				matched++
				g.Expect(target.Health).To(Equal("up"), "EPP Prometheus target is unhealthy: %s", target.LastError)
			}
		}
		g.Expect(matched).To(Equal(1), "Prometheus must discover exactly one EPP target")
	}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

func runKEDAEPPGuideCurlProbe(nameSuffix string, args []string, mountPrometheusCA bool) (string, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "keda-epp-guide-" + nameSuffix + "-",
			Labels: map[string]string{
				"app.kubernetes.io/name": "keda-epp-guide-probe",
				"test-resource":          boolTrue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To(int64(0)),
			Containers: []corev1.Container{{
				Name:  "probe",
				Image: kedaEPPGuideCurlImage,
				Args:  args,
			}},
		},
	}
	if mountPrometheusCA {
		pod.Spec.Volumes = []corev1.Volume{{
			Name: "prometheus-ca",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: "keda-prometheus-auth",
			}},
		}}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
			Name:      "prometheus-ca",
			MountPath: "/var/run/prometheus-ca",
			ReadOnly:  true,
		}}
	}

	callContext, cancelCall := kedaEPPGuideCallContext()
	created, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Create(callContext, pod, metav1.CreateOptions{})
	cancelCall()
	if err != nil {
		return "", fmt.Errorf("create %s probe: %w", nameSuffix, err)
	}
	defer func() {
		grace := int64(0)
		deleteContext, cancelDelete := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
		defer cancelDelete()
		_ = k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(deleteContext, created.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
		})
	}()

	deadline := time.Now().Add(kedaEPPGuideProbeTimeout)
	for time.Now().Before(deadline) {
		getContext, cancelGet := kedaEPPGuideCallContext()
		current, getErr := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(getContext, created.Name, metav1.GetOptions{})
		cancelGet()
		if getErr != nil {
			return "", fmt.Errorf("get %s probe %s: %w", nameSuffix, created.Name, getErr)
		}
		if current.Status.Phase == corev1.PodSucceeded || current.Status.Phase == corev1.PodFailed {
			logContext, cancelLogs := context.WithTimeout(ctx, kedaEPPGuideAPITimeout)
			logs, logErr := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).GetLogs(created.Name, &corev1.PodLogOptions{}).DoRaw(logContext)
			cancelLogs()
			if logErr != nil {
				return "", fmt.Errorf("read %s probe %s logs: %w", nameSuffix, created.Name, logErr)
			}
			if current.Status.Phase == corev1.PodFailed {
				return string(logs), fmt.Errorf("%s probe %s failed: %s", nameSuffix, created.Name, strings.TrimSpace(string(logs)))
			}
			return string(logs), nil
		}
		time.Sleep(time.Duration(cfg.PollIntervalQuickSec) * time.Second)
	}
	return "", fmt.Errorf("%s probe %s did not complete within %s", nameSuffix, created.Name, kedaEPPGuideProbeTimeout)
}

func waitForKEDAEPPGuideRequestPods(names []string) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		for _, name := range names {
			callContext, cancelCall := kedaEPPGuideCallContext()
			pod, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(callContext, name, metav1.GetOptions{})
			cancelCall()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pod.Status.Phase).To(
				Equal(corev1.PodRunning),
				"request pod %s should remain active: %s",
				name,
				kedaEPPGuidePodState(pod),
			)
			g.Expect(pod.Status.ContainerStatuses).To(HaveLen(1))
			g.Expect(pod.Status.ContainerStatuses[0].State.Running).NotTo(BeNil(), "request pod %s curl should be running", name)
		}
	}, time.Duration(cfg.EventuallyMediumSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).Should(Succeed())
}

func kedaEPPGuideExternalMetric(metricName string) (float64, error) {
	gvr := schema.GroupVersionResource{
		Group:    "external.metrics.k8s.io",
		Version:  "v1beta1",
		Resource: metricName,
	}
	callContext, cancelCall := kedaEPPGuideCallContext()
	values, err := dynamicClient.Resource(gvr).Namespace(cfg.LLMDNamespace).List(callContext, metav1.ListOptions{
		LabelSelector: "scaledobject.keda.sh/name=" + kedaEPPGuideScaledObject,
	})
	cancelCall()
	if err != nil {
		return 0, err
	}
	if len(values.Items) != 1 {
		return 0, fmt.Errorf("external metric %s returned %d items, want exactly one", metricName, len(values.Items))
	}
	value, found, err := nestedMetricValue(values.Items[0].Object)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("external metric %s has no value", metricName)
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("external metric %s returned invalid value %q: %w", metricName, value, err)
	}
	return quantity.AsApproximateFloat64(), nil
}

func waitForKEDAEPPGuidePhaseA(hpaName, runningMetricName, queueMetricName string, requestPods []string) {
	GinkgoHelper()

	deadline := time.Now().Add(time.Duration(cfg.EventuallyExtendedSec) * time.Second)
	stable := 0
	sample := 0

	for time.Now().Before(deadline) {
		sample++
		assertKEDAEPPGuideRequestsActive(requestPods)

		hpaContext, cancelHPA := kedaEPPGuideCallContext()
		hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(hpaContext, hpaName, metav1.GetOptions{})
		cancelHPA()
		Expect(err).NotTo(HaveOccurred())
		deploymentContext, cancelDeployment := kedaEPPGuideCallContext()
		deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(deploymentContext, kedaEPPGuideDeployment, metav1.GetOptions{})
		cancelDeployment()
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Spec.Replicas).NotTo(BeNil())

		values := map[string]int32{
			"hpaCurrent":       hpa.Status.CurrentReplicas,
			"hpaDesired":       hpa.Status.DesiredReplicas,
			"deploymentSpec":   *deployment.Spec.Replicas,
			"deploymentStatus": deployment.Status.Replicas,
			"deploymentReady":  deployment.Status.ReadyReplicas,
		}
		for name, value := range values {
			if value > 2 {
				Fail(fmt.Sprintf("bounded guide stimulus observed %s=%d (>2)", name, value))
			}
		}

		runningValue, runningErr := kedaEPPGuideExternalMetric(runningMetricName)
		queueValue, queueErr := kedaEPPGuideExternalMetric(queueMetricName)
		exactPhaseA := runningErr == nil &&
			queueErr == nil &&
			runningValue == 1 &&
			queueValue == 1 &&
			hpa.Status.CurrentReplicas == 1 &&
			hpa.Status.DesiredReplicas == 1 &&
			*deployment.Spec.Replicas == 1 &&
			deployment.Status.Replicas == 1 &&
			deployment.Status.ReadyReplicas == 1
		if exactPhaseA {
			stable++
		} else {
			stable = 0
		}

		GinkgoWriter.Printf(
			"guide phase A sample=%d hpa=%d/%d deployment=%d/%d/%d rawRunning=%v runningErr=%v rawQueue=%v queueErr=%v stable=%d\n",
			sample,
			hpa.Status.CurrentReplicas,
			hpa.Status.DesiredReplicas,
			*deployment.Spec.Replicas,
			deployment.Status.Replicas,
			deployment.Status.ReadyReplicas,
			runningValue,
			runningErr,
			queueValue,
			queueErr,
			stable,
		)

		if stable >= kedaEPPGuideStableCount {
			return
		}
		time.Sleep(time.Duration(cfg.PollIntervalQuickSec) * time.Second)
	}

	Fail("guide Phase A did not produce stable one-running/one-queued exact-one state")
}

func nestedMetricValue(object map[string]any) (string, bool, error) {
	value, found := object["value"]
	if !found {
		return "", false, nil
	}
	stringValue, ok := value.(string)
	if !ok {
		return "", true, fmt.Errorf("metric value has type %T, want string", value)
	}
	return stringValue, true, nil
}

func waitForKEDAEPPGuideScaleUp(hpaName, runningMetricName, queueMetricName string, requestPods []string) {
	GinkgoHelper()
	Expect(requestPods).To(HaveLen(3), "Phase B must retain exactly three bounded request pods")

	deadline := time.Now().Add(time.Duration(cfg.ScaleUpTimeout) * time.Second)
	desiredTransitionSeen := false
	phaseBMetricsSeen := false
	stable := 0
	sample := 0

	for time.Now().Before(deadline) {
		sample++
		assertKEDAEPPGuideRequestsActive(requestPods)

		hpaContext, cancelHPA := kedaEPPGuideCallContext()
		hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(hpaContext, hpaName, metav1.GetOptions{})
		cancelHPA()
		Expect(err).NotTo(HaveOccurred())
		deploymentContext, cancelDeployment := kedaEPPGuideCallContext()
		deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(deploymentContext, kedaEPPGuideDeployment, metav1.GetOptions{})
		cancelDeployment()
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Spec.Replicas).NotTo(BeNil())

		values := map[string]int32{
			"hpaCurrent":       hpa.Status.CurrentReplicas,
			"hpaDesired":       hpa.Status.DesiredReplicas,
			"deploymentSpec":   *deployment.Spec.Replicas,
			"deploymentStatus": deployment.Status.Replicas,
			"deploymentReady":  deployment.Status.ReadyReplicas,
		}
		for name, value := range values {
			if value > 2 {
				Fail(fmt.Sprintf("bounded guide stimulus observed %s=%d (>2)", name, value))
			}
		}

		runningValue, runningErr := kedaEPPGuideExternalMetric(runningMetricName)
		queueValue, queueErr := kedaEPPGuideExternalMetric(queueMetricName)
		if runningErr == nil && runningValue > 2 {
			Fail(fmt.Sprintf("bounded guide stimulus observed rawRunning=%v (>2)", runningValue))
		}
		if queueErr == nil && queueValue > 2 {
			Fail(fmt.Sprintf("bounded guide stimulus observed rawQueue=%v (>2)", queueValue))
		}
		if runningErr == nil && queueErr == nil && runningValue == 1 && queueValue == 2 {
			phaseBMetricsSeen = true
		}

		if !desiredTransitionSeen && hpa.Status.DesiredReplicas != 1 {
			if hpa.Status.DesiredReplicas != 2 {
				Fail(fmt.Sprintf(
					"first HPA desired transition was 1 -> %d, want exactly 1 -> 2",
					hpa.Status.DesiredReplicas,
				))
			}
			desiredTransitionSeen = true
		}

		exactTwo := hpa.Status.CurrentReplicas == 2 &&
			hpa.Status.DesiredReplicas == 2 &&
			*deployment.Spec.Replicas == 2 &&
			deployment.Status.Replicas == 2 &&
			deployment.Status.ReadyReplicas == 2
		if exactTwo {
			stable++
		} else {
			stable = 0
		}

		GinkgoWriter.Printf(
			"guide scale sample=%d hpa=%d/%d deployment=%d/%d/%d rawRunning=%v runningErr=%v rawQueue=%v queueErr=%v phaseBMetrics=%v transition=%v stable=%d\n",
			sample,
			hpa.Status.CurrentReplicas,
			hpa.Status.DesiredReplicas,
			*deployment.Spec.Replicas,
			deployment.Status.Replicas,
			deployment.Status.ReadyReplicas,
			runningValue,
			runningErr,
			queueValue,
			queueErr,
			phaseBMetricsSeen,
			desiredTransitionSeen,
			stable,
		)

		if desiredTransitionSeen && phaseBMetricsSeen && stable >= kedaEPPGuideStableCount {
			Expect(requestPods).To(HaveLen(3), "Phase B success must retain exactly three bounded request pods")
			assertKEDAEPPGuideRequestsActive(requestPods)
			return
		}
		time.Sleep(time.Duration(cfg.PollIntervalQuickSec) * time.Second)
	}

	Fail(fmt.Sprintf(
		"bounded guide stimulus did not produce stable exact-two state (desiredTransitionSeen=%v phaseBMetricsSeen=%v)",
		desiredTransitionSeen,
		phaseBMetricsSeen,
	))
}

func assertKEDAEPPGuideHPABehavior(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior) {
	GinkgoHelper()
	Expect(behavior).NotTo(BeNil(), "canonical HPA behavior must be present")
	assertKEDAEPPGuideHPAScalingRules("scaleUp", behavior.ScaleUp, 0)
	assertKEDAEPPGuideHPAScalingRules("scaleDown", behavior.ScaleDown, 300)
}

func assertKEDAEPPGuideHPAScalingRules(name string, rules *autoscalingv2.HPAScalingRules, stabilizationWindow int32) {
	GinkgoHelper()
	Expect(rules).NotTo(BeNil(), "canonical HPA %s rules must be present", name)
	Expect(rules.StabilizationWindowSeconds).NotTo(BeNil(), "canonical HPA %s stabilization must be explicit", name)
	Expect(*rules.StabilizationWindowSeconds).To(Equal(stabilizationWindow), "canonical HPA %s stabilization drifted", name)
	Expect(rules.Policies).To(HaveLen(1), "canonical HPA %s must retain one policy", name)
	Expect(rules.Policies[0].Type).To(Equal(autoscalingv2.PercentScalingPolicy), "canonical HPA %s policy type drifted", name)
	Expect(rules.Policies[0].Value).To(Equal(int32(100)), "canonical HPA %s policy value drifted", name)
	Expect(rules.Policies[0].PeriodSeconds).To(Equal(int32(15)), "canonical HPA %s policy period drifted", name)
}

func assertKEDAEPPGuideRequestsActive(names []string) {
	GinkgoHelper()

	for _, name := range names {
		callContext, cancelCall := kedaEPPGuideCallContext()
		pod, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(callContext, name, metav1.GetOptions{})
		cancelCall()
		Expect(err).NotTo(HaveOccurred())
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			Fail(fmt.Sprintf("bounded request pod %s exited before scale evidence (phase=%s)", name, pod.Status.Phase))
		}
	}
}

func kedaEPPGuideEventuallyBudget(timeoutSeconds, apiCallsPerAttempt int) time.Duration {
	return time.Duration(timeoutSeconds)*time.Second +
		time.Duration(apiCallsPerAttempt)*kedaEPPGuideAPITimeout
}

// A probe attempt can spend one API deadline creating the Pod, the probe
// deadline waiting for it, one final Get deadline, one log-read deadline, and
// one best-effort delete deadline. Eventually may start that full attempt just
// before its own deadline, so both bounds are additive.
func kedaEPPGuideProbeAttemptBudget() time.Duration {
	return kedaEPPGuideProbeTimeout + 4*kedaEPPGuideAPITimeout
}

func kedaEPPGuideProbeObservationBudget() time.Duration {
	return time.Duration(cfg.EventuallyLongSec)*time.Second + kedaEPPGuideProbeAttemptBudget()
}

func kedaEPPGuideWarmupObservationBudget() time.Duration {
	return kedaEPPGuideAPITimeout + // create the warmup request
		kedaEPPGuideEventuallyBudget(cfg.EventuallyMediumSec, 1) + // request Pod startup
		kedaEPPGuideEventuallyBudget(cfg.EventuallyMediumSec, 1) + // ServiceMonitor
		3*kedaEPPGuideAPITimeout + // two CA Secrets and the KEDA operator Deployment
		5*kedaEPPGuideProbeObservationBudget() // two direct metrics, target, two PromQL queries
}

func kedaEPPGuideDeterministicObservationBudget() time.Duration {
	return kedaEPPGuideAPITimeout + // first request create
		kedaEPPGuideEventuallyBudget(cfg.EventuallyMediumSec, 1) + // first request Pod startup
		kedaEPPGuideEventuallyBudget(cfg.EventuallyLongSec, 1) + // running external metric
		kedaEPPGuideAPITimeout + // second request create
		kedaEPPGuideEventuallyBudget(cfg.EventuallyExtendedSec, 6) + // Phase A: two Pods, HPA, Deployment, two metrics
		kedaEPPGuideAPITimeout + // third request create
		kedaEPPGuideEventuallyBudget(cfg.ScaleUpTimeout, 7) // Phase B: three Pods, HPA, Deployment, two metrics
}

// This is the complete successful-spec observation bound. It includes every
// bounded wait in BeforeAll and the It body, materially extending API calls,
// both stage-boundary log checks, and explicit/deferred stimulus cleanup. The
// suite preflight and failure diagnostics are reserved outside this bound by
// the Ginkgo/Go/CI timeout hierarchy in the Make target and workflow.
func kedaEPPGuideCompleteSpecBudget() time.Duration {
	operatorLogBudget := time.Duration(1+kedaEPPGuideMaxLogStreams) * kedaEPPGuideAPITimeout
	return kedaEPPGuideEventuallyBudget(cfg.PodReadyTimeout, 1) + // simulator Ready in BeforeAll
		kedaEPPGuideEventuallyBudget(cfg.EventuallyMediumSec, 1) + // ScaledObject created
		kedaEPPGuideWarmupObservationBudget() +
		kedaEPPGuideAPITimeout + // delete warmup request
		kedaEPPGuideEventuallyBudget(cfg.EventuallyMediumSec, 1) + // warmup deletion observed
		2*kedaEPPGuideProbeObservationBudget() + // both PromQL series reset
		kedaEPPGuideEventuallyBudget(cfg.EventuallyExtendedSec, 1) + // ScaledObject Ready
		kedaEPPGuideEventuallyBudget(cfg.EventuallyStandardSec, 1) + // generated HPA
		kedaEPPGuideEventuallyBudget(cfg.EventuallyLongSec, 2) + // both external metrics reset
		kedaEPPGuideEventuallyBudget(cfg.EventuallyExtendedSec, 2) + // stable one-replica baseline
		operatorLogBudget +
		kedaEPPGuideDeterministicObservationBudget() +
		3*kedaEPPGuideAPITimeout + // explicit request deletion
		operatorLogBudget +
		4*kedaEPPGuideAPITimeout // deferred three-request and simulator deletion
}

func kedaEPPGuideCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, kedaEPPGuideAPITimeout)
}

func kedaEPPGuidePodState(pod *corev1.Pod) string {
	containerStates := make([]string, 0, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		state := "unknown"
		switch {
		case status.State.Waiting != nil:
			state = "waiting:" + status.State.Waiting.Reason
		case status.State.Running != nil:
			state = "running"
		case status.State.Terminated != nil:
			state = fmt.Sprintf("terminated:%s:%d", status.State.Terminated.Reason, status.State.Terminated.ExitCode)
		}
		containerStates = append(containerStates, status.Name+"="+state)
	}
	return fmt.Sprintf("phase=%s containers=[%s]", pod.Status.Phase, strings.Join(containerStates, ","))
}

func assertKEDAOperatorLogsClean() {
	GinkgoHelper()

	operatorLogs, err := readKEDAEPPGuideOperatorLogs()
	Expect(err).NotTo(HaveOccurred())
	Expect(operatorLogs).NotTo(BeEmpty(), "KEDA operator pod should exist before guide validation")

	for _, operatorLog := range operatorLogs {
		lower := strings.ToLower(operatorLog.content)
		for _, pattern := range []string{"x509", "unknown authority"} {
			if strings.Contains(lower, pattern) {
				Fail(fmt.Sprintf("KEDA operator logs from %s contain %q: %s", operatorLog.source, pattern, strings.TrimSpace(operatorLog.content)))
			}
		}
	}
}

type kedaEPPGuideOperatorLog struct {
	source  string
	content string
}

func readKEDAEPPGuideOperatorLogs() ([]kedaEPPGuideOperatorLog, error) {
	listContext, cancelList := context.WithTimeout(ctx, 10*time.Second)
	pods, err := k8sClient.CoreV1().Pods(cfg.KEDANamespace).List(listContext, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=keda-operator",
	})
	cancelList()
	if err != nil {
		return nil, fmt.Errorf("list KEDA operator pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no KEDA operator pods found in namespace %s", cfg.KEDANamespace)
	}

	operatorLogs := make([]kedaEPPGuideOperatorLog, 0, kedaEPPGuideMaxLogStreams)
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			if len(operatorLogs) >= kedaEPPGuideMaxLogStreams {
				return operatorLogs, nil
			}
			logContext, cancelLogs := context.WithTimeout(ctx, 10*time.Second)
			logs, err := k8sClient.CoreV1().Pods(cfg.KEDANamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container:    container.Name,
				SinceSeconds: ptr.To(kedaEPPGuideLogSinceSecs),
				TailLines:    ptr.To(kedaEPPGuideLogTailLines),
			}).DoRaw(logContext)
			cancelLogs()
			if err != nil {
				return nil, fmt.Errorf("read KEDA operator logs for %s/%s: %w", pod.Name, container.Name, err)
			}
			operatorLogs = append(operatorLogs, kedaEPPGuideOperatorLog{
				source:  pod.Name + "/" + container.Name,
				content: string(logs),
			})
		}
	}
	return operatorLogs, nil
}

func dumpKEDAEPPGuideOperatorLogs() {
	operatorLogs, err := readKEDAEPPGuideOperatorLogs()
	if err != nil {
		GinkgoWriter.Printf("Failed to collect KEDA operator logs: %v\n", err)
		return
	}
	for _, operatorLog := range operatorLogs {
		GinkgoWriter.Printf("\n=== KEDA operator logs: %s ===\n%s\n", operatorLog.source, operatorLog.content)
	}
}

func dumpKEDAEPPGuideDiagnostics() {
	GinkgoWriter.Println("\n=== KEDA+EPP guide focused diagnostics ===")
	dumpKEDAEPPGuideOperatorLogs()

	for _, namespace := range []string{cfg.LLMDNamespace, cfg.MonitoringNS, cfg.KEDANamespace} {
		listContext, cancelList := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
		pods, err := k8sClient.CoreV1().Pods(namespace).List(listContext, metav1.ListOptions{})
		cancelList()
		if err != nil {
			GinkgoWriter.Printf("pods namespace=%s error=%v\n", namespace, err)
		} else {
			for _, pod := range pods.Items {
				GinkgoWriter.Printf("pod namespace=%s name=%s %s\n", namespace, pod.Name, kedaEPPGuidePodState(&pod))
			}
		}

		eventContext, cancelEvents := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
		events, err := k8sClient.CoreV1().Events(namespace).List(eventContext, metav1.ListOptions{})
		cancelEvents()
		if err != nil {
			GinkgoWriter.Printf("events namespace=%s error=%v\n", namespace, err)
		} else {
			for _, event := range events.Items {
				GinkgoWriter.Printf(
					"event namespace=%s object=%s/%s reason=%s message=%s\n",
					namespace,
					event.InvolvedObject.Kind,
					event.InvolvedObject.Name,
					event.Reason,
					event.Message,
				)
			}
		}
	}

	deploymentContext, cancelDeployments := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
	deployments, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).List(deploymentContext, metav1.ListOptions{})
	cancelDeployments()
	if err != nil {
		GinkgoWriter.Printf("deployments namespace=%s error=%v\n", cfg.LLMDNamespace, err)
	} else {
		for _, deployment := range deployments.Items {
			desired := int32(-1)
			if deployment.Spec.Replicas != nil {
				desired = *deployment.Spec.Replicas
			}
			GinkgoWriter.Printf(
				"deployment namespace=%s name=%s desired=%d current=%d ready=%d available=%d\n",
				cfg.LLMDNamespace,
				deployment.Name,
				desired,
				deployment.Status.Replicas,
				deployment.Status.ReadyReplicas,
				deployment.Status.AvailableReplicas,
			)
		}
	}

	scaledObject := &kedav1alpha1.ScaledObject{}
	soContext, cancelSO := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
	err = crClient.Get(soContext, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: kedaEPPGuideScaledObject}, scaledObject)
	cancelSO()
	if err != nil {
		GinkgoWriter.Printf("scaledobject namespace=%s name=%s error=%v\n", cfg.LLMDNamespace, kedaEPPGuideScaledObject, err)
	} else {
		GinkgoWriter.Printf(
			"scaledobject namespace=%s name=%s hpa=%s conditions=%+v triggers=%+v\n",
			cfg.LLMDNamespace,
			scaledObject.Name,
			scaledObject.Status.HpaName,
			scaledObject.Status.Conditions,
			scaledObject.Spec.Triggers,
		)
	}

	hpaContext, cancelHPA := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
	hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(hpaContext, kedaEPPGuideHPAName, metav1.GetOptions{})
	cancelHPA()
	if err != nil {
		GinkgoWriter.Printf("hpa namespace=%s name=%s error=%v\n", cfg.LLMDNamespace, kedaEPPGuideHPAName, err)
	} else {
		GinkgoWriter.Printf("hpa namespace=%s name=%s spec=%+v status=%+v owners=%+v\n", cfg.LLMDNamespace, hpa.Name, hpa.Spec, hpa.Status, hpa.OwnerReferences)
	}

	serviceMonitor := &promoperator.ServiceMonitor{}
	smContext, cancelSM := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
	err = crClient.Get(smContext, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: kedaEPPGuideServiceMonitor}, serviceMonitor)
	cancelSM()
	if err != nil {
		GinkgoWriter.Printf("servicemonitor namespace=%s name=%s error=%v\n", cfg.LLMDNamespace, kedaEPPGuideServiceMonitor, err)
	} else {
		GinkgoWriter.Printf("servicemonitor namespace=%s name=%s selector=%+v endpoints=%+v\n", cfg.LLMDNamespace, serviceMonitor.Name, serviceMonitor.Spec.Selector, serviceMonitor.Spec.Endpoints)
	}

	directMetrics, directErr := runKEDAEPPGuideCurlProbe(
		"diagnostic-metrics",
		[]string{
			"--fail", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "30",
			fmt.Sprintf("http://%s.%s.svc.cluster.local:9090/metrics", cfg.EPPServiceName, cfg.LLMDNamespace),
		},
		false,
	)
	if directErr != nil {
		GinkgoWriter.Printf("direct EPP metrics error=%v\n", directErr)
	} else {
		for _, line := range strings.Split(directMetrics, "\n") {
			if strings.HasPrefix(line, "llm_d_epp_flow_control_queue_size{") || strings.HasPrefix(line, "llm_d_epp_request_running{") {
				GinkgoWriter.Printf("direct EPP metric %s\n", line)
			}
		}
	}

	for name, query := range map[string]string{
		"queue":   kedaEPPGuideQueueQuery,
		"running": kedaEPPGuideRunQuery,
	} {
		value, cardinality, queryErr := kedaEPPGuidePrometheusValue(query)
		GinkgoWriter.Printf("Prometheus query=%s cardinality=%d value=%v error=%v\n", name, cardinality, value, queryErr)
	}
	GinkgoWriter.Println("=== end KEDA+EPP guide focused diagnostics ===")
}
