/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Test idle scale-to-zero with HPA", Ordered, func() {
	var (
		namespace         string
		deployName        string
		serviceName       string
		serviceMonName    string
		configMapName     string
		appLabel          string
		modelID           string
		accelerator       string
		ctx               context.Context
		initialReplicas   int32
		retentionDuration time.Duration
		inferenceModel    *unstructured.Unstructured
	)

	BeforeAll(func() {
		Skip("HPA scale-to-zero requires minReplicas: 0 (alpha feature) and Prometheus Adapter - skipping until enabled")

		initializeK8sClient()

		ctx = context.Background()
		namespace = llmDNamespace
		deployName = "hpa-idle-sto-zero-deployment"
		serviceName = "hpa-idle-sto-zero-service"
		serviceMonName = "hpa-idle-sto-zero-servicemonitor"
		configMapName = "model-scale-to-zero-config"
		appLabel = "hpa-idle-sto-zero-test"
		modelID = "test-hpa-idle-sto-zero-model"
		accelerator = a100Acc
		initialReplicas = 1
		retentionDuration = 4 * time.Minute

		By("ensuring unique app label and model")
		utils.ValidateAppLabelUniqueness(namespace, appLabel, k8sClient, crClient)
		utils.ValidateVariantAutoscalingUniqueness(namespace, modelID, accelerator, crClient)

		By("creating scale-to-zero ConfigMap")
		configMapKey := strings.ReplaceAll(modelID, "/", "-")
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configMapName,
				Namespace: controllerNamespace,
			},
			Data: map[string]string{
				fmt.Sprintf("model.%s", configMapKey): fmt.Sprintf(`modelID: "%s"
enableScaleToZero: true
retentionPeriod: "4m"`, modelID),
			},
		}
		_, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Create(ctx, configMap, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ConfigMap: %s", configMapName))

		By("creating vllme deployment")
		deployment := utils.CreateVllmeDeployment(namespace, deployName, modelID, appLabel)
		deployment.Spec.Replicas = &initialReplicas
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Deployment: %s", deployName))

		By("creating vllme service")
		service := utils.CreateVllmeService(namespace, serviceName, appLabel, 30001)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Service: %s", serviceName))

		By("creating ServiceMonitor for vllme metrics")
		serviceMonitor := utils.CreateVllmeServiceMonitor(serviceMonName, controllerMonitoringNamespace, appLabel)
		err = crClient.Create(ctx, serviceMonitor)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ServiceMonitor: %s", serviceMonName))

		By("creating VariantAutoscaling resource")
		variantAutoscaling := utils.CreateVariantAutoscalingResource(namespace, deployName, modelID, accelerator)
		err = crClient.Create(ctx, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create VariantAutoscaling: %s", deployName))

		By("creating InferenceModel for the deployment")
		inferenceModel = utils.CreateInferenceModel(deployName, namespace, modelID)
		err = crClient.Create(ctx, inferenceModel)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create InferenceModel: %s", modelID))
	})

	It("deployment should be running initially", func() {
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get Deployment")
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Deployment should have 1 ready replica")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("should scale to zero after retention period with no traffic", func() {
		By("waiting for initial controller reconciliation")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(va.Status.CurrentAlloc.NumReplicas).To(Equal(int32(1)))
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("creating HPA for deployment")
		minReplicas := int32(0) // Scale-to-zero: HPA alpha feature
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployName + "-hpa",
				Namespace: namespace,
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       deployName,
				},
				MinReplicas: &minReplicas,
				MaxReplicas: 10,
				Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
					ScaleUp: &autoscalingv2.HPAScalingRules{
						StabilizationWindowSeconds: func() *int32 { v := int32(0); return &v }(),
						Policies: []autoscalingv2.HPAScalingPolicy{
							{
								Type:          autoscalingv2.PodsScalingPolicy,
								Value:         10,
								PeriodSeconds: 15,
							},
						},
					},
					ScaleDown: &autoscalingv2.HPAScalingRules{
						StabilizationWindowSeconds: func() *int32 { v := int32(0); return &v }(),
						Policies: []autoscalingv2.HPAScalingPolicy{
							{
								Type:          autoscalingv2.PodsScalingPolicy,
								Value:         10,
								PeriodSeconds: 15,
							},
						},
					},
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ExternalMetricSourceType,
						External: &autoscalingv2.ExternalMetricSource{
							Metric: autoscalingv2.MetricIdentifier{
								Name: "inferno_desired_replicas",
								Selector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"variant_name":       deployName,
										"exported_namespace": namespace,
										"accelerator_type":   accelerator,
										"variant_id":         deployName,
									},
								},
							},
							Target: autoscalingv2.MetricTarget{
								Type:         autoscalingv2.AverageValueMetricType,
								AverageValue: resource.NewQuantity(1, resource.DecimalSI),
							},
						},
					},
				},
			},
		}
		_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create HPA: %s", hpa.Name))
		_, _ = fmt.Fprintf(GinkgoWriter, "✓ HPA created: %s\n", hpa.Name)

		By("waiting for HPA to be ready")
		Eventually(func(g Gomega) {
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, deployName+"-hpa", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get HPA")
			g.Expect(hpa.Status.Conditions).NotTo(BeEmpty(), "HPA should have conditions")

			for _, condition := range hpa.Status.Conditions {
				if condition.Type == autoscalingv2.ScalingActive && condition.Status == corev1.ConditionTrue {
					_, _ = fmt.Fprintf(GinkgoWriter, "✓ HPA is active\n")
					return
				}
			}
			g.Expect(true).To(BeFalse(), "HPA should be active")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for retention period to pass with no traffic")
		waitDuration := retentionDuration + (90 * time.Second) // retention + buffer
		_, _ = fmt.Fprintf(GinkgoWriter, "Waiting %v for retention period + buffer (no traffic)...\n", waitDuration)
		time.Sleep(waitDuration)

		By("verifying controller sets DesiredOptimizedAlloc to 0")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred())

			_, _ = fmt.Fprintf(GinkgoWriter, "VA Status: DesiredOptimized=%d, Current=%d, Reason=%q, LastUpdate=%v\n",
				va.Status.DesiredOptimizedAlloc.NumReplicas, va.Status.CurrentAlloc.NumReplicas,
				va.Status.DesiredOptimizedAlloc.Reason, va.Status.DesiredOptimizedAlloc.LastUpdate.Time)

			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(0)),
				"Controller should set desired replicas to 0 after retention period")
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying HPA scales deployment to 0 replicas")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			if deployment.Status.Replicas == 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "✓ HPA successfully scaled deployment to 0 replicas\n")
			}
			g.Expect(deployment.Status.Replicas).To(Equal(int32(0)))
		}, 5*time.Minute, 10*time.Second).Should(Succeed(),
			"HPA should scale deployment to 0 replicas")

		By("verifying CurrentAlloc reflects the scaled-down state")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(va.Status.CurrentAlloc.NumReplicas).To(Equal(int32(0)))
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "✓ Idle scale-to-zero with HPA completed successfully\n")
	})

	AfterAll(func() {
		// Skip cleanup if test was skipped (k8sClient will be nil)
		if k8sClient == nil {
			return
		}

		By("cleaning up HPA")
		err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, deployName+"-hpa", metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete HPA: %s", deployName+"-hpa"))

		By("cleaning up VariantAutoscaling resource")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployName,
				Namespace: namespace,
			},
		}
		err = crClient.Delete(ctx, variantAutoscaling)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete VariantAutoscaling: %s", deployName))

		By("deleting InferenceModel")
		err = crClient.Delete(ctx, inferenceModel)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete InferenceModel: %s", modelID))

		By("cleaning up ServiceMonitor")
		serviceMonitor := &unstructured.Unstructured{}
		serviceMonitor.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "monitoring.coreos.com",
			Version: "v1",
			Kind:    "ServiceMonitor",
		})
		serviceMonitor.SetName(serviceMonName)
		serviceMonitor.SetNamespace(controllerMonitoringNamespace)
		err = crClient.Delete(ctx, serviceMonitor)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete ServiceMonitor: %s", serviceMonName))

		By("cleaning up Service")
		err = k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Service: %s", serviceName))

		By("cleaning up Deployment")
		err = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deployName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", deployName))

		By("waiting for all pods to be deleted")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app=%s", appLabel),
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list Pods")
			g.Expect(podList.Items).To(BeEmpty(), fmt.Sprintf("All Pods with label %s should be deleted", appLabel))
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("cleaning up ConfigMap")
		err = k8sClient.CoreV1().ConfigMaps(controllerNamespace).Delete(ctx, configMapName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete ConfigMap: %s", configMapName))
	})
})

var _ = Describe("Test traffic-based scale-to-zero with HPA", Ordered, func() {
	var (
		namespace         string
		deployName        string
		serviceName       string
		serviceMonName    string
		configMapName     string
		appLabel          string
		modelID           string
		accelerator       string
		ctx               context.Context
		initialReplicas   int32
		retentionDuration time.Duration
		inferenceModel    *unstructured.Unstructured
	)

	BeforeAll(func() {
		Skip("HPA scale-to-zero requires minReplicas: 0 (alpha feature) and Prometheus Adapter - skipping until enabled")

		initializeK8sClient()

		ctx = context.Background()
		namespace = llmDNamespace
		deployName = "hpa-traffic-sto-zero-deployment"
		serviceName = "hpa-traffic-sto-zero-service"
		serviceMonName = "hpa-traffic-sto-zero-servicemonitor"
		configMapName = "model-scale-to-zero-config"
		appLabel = "hpa-traffic-sto-zero-test"
		// Use "default/default" to leverage existing infrastructure (same as KEDA test)
		modelID = defaultModelId
		accelerator = a100Acc
		initialReplicas = 1
		retentionDuration = 4 * time.Minute

		By("ensuring unique app label and model")
		utils.ValidateAppLabelUniqueness(namespace, appLabel, k8sClient, crClient)
		utils.ValidateVariantAutoscalingUniqueness(namespace, modelID, accelerator, crClient)

		By("creating scale-to-zero ConfigMap")
		configMapKey := strings.ReplaceAll(modelID, "/", "-")
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configMapName,
				Namespace: controllerNamespace,
			},
			Data: map[string]string{
				fmt.Sprintf("model.%s", configMapKey): fmt.Sprintf(`modelID: "%s"
enableScaleToZero: true
retentionPeriod: "4m"`, modelID),
			},
		}
		_, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Create(ctx, configMap, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ConfigMap: %s", configMapName))

		By("creating vllme deployment")
		deployment := utils.CreateVllmeDeployment(namespace, deployName, modelID, appLabel)
		deployment.Spec.Replicas = &initialReplicas
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Deployment: %s", deployName))

		By("creating vllme service")
		service := utils.CreateVllmeService(namespace, serviceName, appLabel, 30002)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Service: %s", serviceName))

		By("creating ServiceMonitor for vllme metrics")
		serviceMonitor := utils.CreateVllmeServiceMonitor(serviceMonName, controllerMonitoringNamespace, appLabel)
		err = crClient.Create(ctx, serviceMonitor)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ServiceMonitor: %s", serviceMonName))

		By("creating VariantAutoscaling resource")
		variantAutoscaling := utils.CreateVariantAutoscalingResource(namespace, deployName, modelID, accelerator)
		err = crClient.Create(ctx, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create VariantAutoscaling: %s", deployName))

		By("creating InferenceModel for the deployment")
		// Use "default/default" to match traffic and leverage existing infrastructure
		inferenceModel = utils.CreateInferenceModel(deployName, namespace, defaultModelId)
		err = crClient.Create(ctx, inferenceModel)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create InferenceModel with model: %s", defaultModelId))
		_, _ = fmt.Fprintf(GinkgoWriter, "✓ InferenceModel created with modelName=%s\n", defaultModelId)
	})

	It("should scale to zero after traffic stops and retention period expires", func() {
		By("waiting for deployment to have ready replicas")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "Deployment replicas: Ready=%d, Available=%d, Target=%d\n",
				deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas, *deployment.Spec.Replicas)
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("creating HPA for deployment after initial reconciliation")
		// Wait for initial reconciliation before creating HPA
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(va.Status.CurrentAlloc.NumReplicas).To(Equal(int32(1)))
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		minReplicas := int32(0)
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployName + "-hpa",
				Namespace: namespace,
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       deployName,
				},
				MinReplicas: &minReplicas,
				MaxReplicas: 10,
				Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
					ScaleUp: &autoscalingv2.HPAScalingRules{
						StabilizationWindowSeconds: func() *int32 { v := int32(0); return &v }(),
						Policies: []autoscalingv2.HPAScalingPolicy{
							{
								Type:          autoscalingv2.PodsScalingPolicy,
								Value:         10,
								PeriodSeconds: 15,
							},
						},
					},
					ScaleDown: &autoscalingv2.HPAScalingRules{
						StabilizationWindowSeconds: func() *int32 { v := int32(0); return &v }(),
						Policies: []autoscalingv2.HPAScalingPolicy{
							{
								Type:          autoscalingv2.PodsScalingPolicy,
								Value:         10,
								PeriodSeconds: 15,
							},
						},
					},
				},
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ExternalMetricSourceType,
						External: &autoscalingv2.ExternalMetricSource{
							Metric: autoscalingv2.MetricIdentifier{
								Name: "inferno_desired_replicas",
								Selector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"variant_name":       deployName,
										"exported_namespace": namespace,
										"accelerator_type":   accelerator,
										"variant_id":         deployName,
									},
								},
							},
							Target: autoscalingv2.MetricTarget{
								Type:         autoscalingv2.AverageValueMetricType,
								AverageValue: resource.NewQuantity(1, resource.DecimalSI),
							},
						},
					},
				},
			},
		}
		_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, _ = fmt.Fprintf(GinkgoWriter, "✓ HPA created: %s\n", hpa.Name)

		By("waiting for HPA to be ready")
		Eventually(func(g Gomega) {
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, deployName+"-hpa", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(hpa.Status.Conditions).NotTo(BeEmpty())

			for _, condition := range hpa.Status.Conditions {
				if condition.Type == autoscalingv2.ScalingActive && condition.Status == corev1.ConditionTrue {
					_, _ = fmt.Fprintf(GinkgoWriter, "✓ HPA is active\n")
					return
				}
			}
			g.Expect(true).To(BeFalse(), "HPA should be active")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("setting up port-forward to gateway for traffic generation")
		port := 8002
		portForwardCmd := utils.SetUpPortForward(k8sClient, ctx, gatewayName, namespace, port, 80)
		defer func() {
			err := utils.StopCmd(portForwardCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for gateway: %s", gatewayName))
		}()

		By("waiting for port-forward to be ready")
		err = utils.VerifyPortForwardReadiness(ctx, port, fmt.Sprintf("http://localhost:%d/v1", port))
		Expect(err).NotTo(HaveOccurred(), "Port-forward should be ready within timeout")

		By("starting traffic generation")
		loadRate := 10
		// Use "default/default" for traffic to match InferenceModel and leverage existing infrastructure
		loadGenCmd := utils.StartLoadGenerator(loadRate, 100, port, defaultModelId)
		defer func() {
			err := utils.StopCmd(loadGenCmd)
			if err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Warning: Failed to stop load generator: %v\n", err)
			}
		}()

		_, _ = fmt.Fprintf(GinkgoWriter, "Starting traffic generation at %d req/s...\n", loadRate)

		By("waiting for vLLM metrics rate data to accumulate")
		_, _ = fmt.Fprintf(GinkgoWriter, "Waiting 90 seconds for rate([1m]) data accumulation...\n")
		time.Sleep(90 * time.Second)

		By("waiting for controller to process traffic and emit metrics")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred())

			_, _ = fmt.Fprintf(GinkgoWriter, "Waiting for controller: DesiredOptimized=%d, Current=%d, Reason=%q\n",
				va.Status.DesiredOptimizedAlloc.NumReplicas, va.Status.CurrentAlloc.NumReplicas,
				va.Status.DesiredOptimizedAlloc.Reason)

			g.Expect(va.Status.CurrentAlloc.NumReplicas).To(Equal(int32(1)))
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(1)))
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "✓ Metrics: current=1, desired=1\n")

		By("stopping traffic generation")
		err = utils.StopCmd(loadGenCmd)
		Expect(err).NotTo(HaveOccurred())
		_, _ = fmt.Fprintf(GinkgoWriter, "✓ Traffic stopped\n")

		By("waiting for retention period to pass with zero traffic")
		waitDuration := retentionDuration + (90 * time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Waiting %v for retention period + buffer (no traffic)...\n", waitDuration)
		time.Sleep(waitDuration)

		By("verifying controller sets DesiredOptimizedAlloc to 0")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred())

			_, _ = fmt.Fprintf(GinkgoWriter, "VA Status: DesiredOptimized=%d, Current=%d, Reason=%q\n",
				va.Status.DesiredOptimizedAlloc.NumReplicas, va.Status.CurrentAlloc.NumReplicas,
				va.Status.DesiredOptimizedAlloc.Reason)

			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(0)))
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying HPA scales deployment to 0 replicas")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			if deployment.Status.Replicas == 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "✓ HPA successfully scaled deployment to 0 replicas\n")
			}
			g.Expect(deployment.Status.Replicas).To(Equal(int32(0)))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying CurrentAlloc reflects the scaled-down state")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(va.Status.CurrentAlloc.NumReplicas).To(Equal(int32(0)))
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "✓ Scale-to-zero after traffic stops completed successfully\n")
	})

	AfterAll(func() {
		// Skip cleanup if test was skipped (k8sClient will be nil)
		if k8sClient == nil {
			return
		}

		By("cleaning up HPA")
		err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, deployName+"-hpa", metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred())

		By("cleaning up VariantAutoscaling resource")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployName,
				Namespace: namespace,
			},
		}
		err = crClient.Delete(ctx, variantAutoscaling)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred())

		By("deleting InferenceModel")
		err = crClient.Delete(ctx, inferenceModel)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred())

		By("cleaning up ServiceMonitor")
		serviceMonitor := &unstructured.Unstructured{}
		serviceMonitor.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "monitoring.coreos.com",
			Version: "v1",
			Kind:    "ServiceMonitor",
		})
		serviceMonitor.SetName(serviceMonName)
		serviceMonitor.SetNamespace(controllerMonitoringNamespace)
		err = crClient.Delete(ctx, serviceMonitor)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred())

		By("cleaning up Service")
		err = k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred())

		By("cleaning up Deployment")
		err = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deployName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for all pods to be deleted")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app=%s", appLabel),
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podList.Items).To(BeEmpty())
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("cleaning up ConfigMap")
		err = k8sClient.CoreV1().ConfigMaps(controllerNamespace).Delete(ctx, configMapName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred())
	})
})
