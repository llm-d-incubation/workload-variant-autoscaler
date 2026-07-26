package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
	kedaEPPGuideScaledObject = "optimized-baseline-keda-epp"
	kedaEPPGuideDeployment   = "optimized-baseline-nvidia-gpu-vllm-decode"
	kedaEPPGuideQueueTrigger = "epp-queue-size"
	kedaEPPGuideRunTrigger   = "epp-running-requests"
	kedaEPPGuideStableCount  = 3
	kedaEPPGuideRequestLimit = 900
	kedaEPPGuideAPITimeout   = 10 * time.Second
	kedaEPPGuideCurlImage    = "quay.io/curl/curl:8.11.1@sha256:2db4e6a8fd6a0e4d0db5828b2722cf6db15c3005178a4c65588b903e4784ba11"
)

var _ = Describe("KEDA EPP guide contract", Label("keda-epp-guide"), Ordered, func() {
	var requestPods []string

	deleteRequests := func() {
		grace := int64(0)
		for _, name := range requestPods {
			deleteContext, cancelDelete := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
			err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(deleteContext, name, metav1.DeleteOptions{
				GracePeriodSeconds: &grace,
			})
			cancelDelete()
			if err != nil && !apierrors.IsNotFound(err) {
				GinkgoWriter.Printf("WARNING: failed to delete request pod %s: %v\n", name, err)
			}
		}
	}

	BeforeAll(func() {
		Expect(cfg.DeployWVA).To(BeFalse(), "the direct-KEDA guide spec must run with DEPLOY_WVA=false")
		DeferCleanup(deleteRequests)
	})

	It("observes the canonical guide and performs one bounded 1-to-2 transition", func() {
		scaledObject := waitForKEDAEPPGuideScaledObject()
		Expect(scaledObject.Spec.ScaleTargetRef.Name).To(Equal(kedaEPPGuideDeployment))
		Expect(scaledObject.Spec.Triggers).To(HaveLen(2))

		var queueThreshold, runningThreshold resource.Quantity
		for _, trigger := range scaledObject.Spec.Triggers {
			Expect(trigger.Type).To(Equal("prometheus"))
			Expect(trigger.MetricType).To(Equal(autoscalingv2.AverageValueMetricType))

			threshold, err := resource.ParseQuantity(trigger.Metadata["threshold"])
			Expect(err).NotTo(HaveOccurred(), "trigger %s should have a numeric threshold", trigger.Name)
			switch trigger.Name {
			case kedaEPPGuideQueueTrigger:
				queueThreshold = threshold
			case kedaEPPGuideRunTrigger:
				runningThreshold = threshold
			default:
				Fail(fmt.Sprintf("unexpected Prometheus trigger %q on the canonical ScaledObject", trigger.Name))
			}
		}
		Expect(queueThreshold.IsZero()).To(BeFalse(), "queue trigger threshold should be non-zero")
		Expect(runningThreshold.IsZero()).To(BeFalse(), "running trigger threshold should be non-zero")

		hpa := waitForKEDAEPPGuideHPA(scaledObject)
		Expect(hpa.Spec.ScaleTargetRef.APIVersion).To(Equal("apps/v1"))
		Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
		Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(kedaEPPGuideDeployment))
		Expect(hpa.Spec.Metrics).To(HaveLen(2))

		metricNames := map[string]string{}
		for _, metric := range hpa.Spec.Metrics {
			Expect(metric.Type).To(Equal(autoscalingv2.ExternalMetricSourceType))
			Expect(metric.External).NotTo(BeNil())
			Expect(metric.External.Target.Type).To(Equal(autoscalingv2.AverageValueMetricType))
			Expect(metric.External.Target.AverageValue).NotTo(BeNil())

			switch {
			case metric.External.Target.AverageValue.Cmp(queueThreshold) == 0:
				metricNames[kedaEPPGuideQueueTrigger] = metric.External.Metric.Name
			case metric.External.Target.AverageValue.Cmp(runningThreshold) == 0:
				metricNames[kedaEPPGuideRunTrigger] = metric.External.Metric.Name
			default:
				Fail(fmt.Sprintf("generated HPA metric %q does not match a live guide trigger target", metric.External.Metric.Name))
			}
		}
		Expect(metricNames).To(HaveKey(kedaEPPGuideQueueTrigger))
		Expect(metricNames).To(HaveKey(kedaEPPGuideRunTrigger))
		Expect(metricNames[kedaEPPGuideQueueTrigger]).NotTo(Equal(metricNames[kedaEPPGuideRunTrigger]))

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

		By("starting exactly two additional requests that remain queued")
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())

		waitForKEDAEPPGuideScaleUp(
			scaledObject.Status.HpaName,
			metricNames[kedaEPPGuideQueueTrigger],
			requestPods,
		)

		By("terminating the bounded stimulus after exact-two evidence")
		deleteRequests()
		assertKEDAOperatorLogsClean()
	})
})

func waitForKEDAEPPGuideScaledObject() *kedav1alpha1.ScaledObject {
	GinkgoHelper()

	deadline := time.Now().Add(time.Duration(cfg.EventuallyExtendedSec) * time.Second)
	for time.Now().Before(deadline) {
		assertKEDAOperatorLogsClean()

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

func waitForKEDAEPPGuideScaleUp(hpaName, queueMetricName string, requestPods []string) {
	GinkgoHelper()

	deadline := time.Now().Add(time.Duration(cfg.ScaleUpTimeout) * time.Second)
	desiredTransitionSeen := false
	queueTwoSeen := false
	stable := 0
	sample := 0

	for time.Now().Before(deadline) {
		sample++
		assertKEDAOperatorLogsClean()
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

		queueValue, metricErr := kedaEPPGuideExternalMetric(queueMetricName)
		if metricErr == nil && queueValue == 2 {
			queueTwoSeen = true
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
			"guide scale sample=%d hpa=%d/%d deployment=%d/%d/%d rawQueue=%v queueErr=%v transition=%v stable=%d\n",
			sample,
			hpa.Status.CurrentReplicas,
			hpa.Status.DesiredReplicas,
			*deployment.Spec.Replicas,
			deployment.Status.Replicas,
			deployment.Status.ReadyReplicas,
			queueValue,
			metricErr,
			desiredTransitionSeen,
			stable,
		)

		if desiredTransitionSeen && queueTwoSeen && stable >= kedaEPPGuideStableCount {
			return
		}
		time.Sleep(time.Duration(cfg.PollIntervalQuickSec) * time.Second)
	}

	Fail(fmt.Sprintf(
		"bounded guide stimulus did not produce stable exact-two state (desiredTransitionSeen=%v queueTwoSeen=%v)",
		desiredTransitionSeen,
		queueTwoSeen,
	))
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
		for _, pattern := range []string{"x509", "unknown authority", "triggererror"} {
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

	var operatorLogs []kedaEPPGuideOperatorLog
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			logContext, cancelLogs := context.WithTimeout(ctx, 10*time.Second)
			logs, err := k8sClient.CoreV1().Pods(cfg.KEDANamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container: container.Name,
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
