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
	"os"
	"os/exec"
	"strings"
	"time"

	v1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	controllerNamespace           = "workload-variant-autoscaler-system"
	controllerMonitoringNamespace = "workload-variant-autoscaler-monitoring"
	llmDNamespace                 = "llm-d-sim"
	gatewayName                   = "infra-sim-inference-gateway"
)

const (
	defaultModelId = "default/default"
	llamaModelId   = "meta/llama0-70b"
	a100Acc        = "A100"
	// mi300xAcc            = "MI300X"
)

var (
	k8sClient *kubernetes.Clientset
	crClient  client.Client
	scheme    = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

// initializeK8sClient initializes the Kubernetes client for testing
func initializeK8sClient() {
	cfg, err := func() (*rest.Config, error) {
		if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
			return clientcmd.BuildConfigFromFlags("", kubeconfig)
		}
		return rest.InClusterConfig()
	}()
	if err != nil {
		Skip("failed to load kubeconfig: " + err.Error())
	}

	// Suppress warnings to avoid spam in test output
	cfg.WarningHandler = rest.NoWarnings{}

	k8sClient, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		Skip("failed to create kubernetes client: " + err.Error())
	}

	// Initialize controller-runtime client for custom resources
	crClient, err = client.New(cfg, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		Skip("failed to create controller-runtime client: " + err.Error())
	}
}

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", controllerNamespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", controllerNamespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})
		// +kubebuilder:scaffold:e2e-webhooks-checks
	})
})

var _ = Describe("Test workload-variant-autoscaler with vllme deployment - single VA - critical requests", Ordered, func() {
	var (
		namespace      string
		deployName     string
		serviceName    string
		serviceMonName string
		inferenceModel *unstructured.Unstructured
		appLabel       string
		loadGenCmd     *exec.Cmd
		portForwardCmd *exec.Cmd
		port           int
		loadRate       int
		modelName      string
		ctx            context.Context
	)

	BeforeAll(func() {
		if os.Getenv("KUBECONFIG") == "" {
			Skip("KUBECONFIG is not set; skipping e2e test")
		}

		initializeK8sClient()

		ctx = context.Background()
		namespace = llmDNamespace
		deployName = "vllme-deployment"
		serviceName = "vllme-service"
		serviceMonName = "vllme-servicemonitor"
		appLabel = "vllme"
		port = 8000
		loadRate = 30
		modelName = defaultModelId

		By("ensuring unique app label for deployment and service")
		utils.ValidateAppLabelUniqueness(namespace, appLabel, k8sClient, crClient)
		utils.ValidateVariantAutoscalingUniqueness(namespace, defaultModelId, a100Acc, crClient)

		By("creating vllme deployment")
		deployment := utils.CreateVllmeDeployment(namespace, deployName, modelName, appLabel)
		_, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Deployment: %s", deployName))

		By("creating vllme service")
		service := utils.CreateVllmeService(namespace, serviceName, appLabel, 30000)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Service: %s", serviceName))

		By("creating ServiceMonitor for vllme metrics")
		serviceMonitor := utils.CreateVllmeServiceMonitor(serviceMonName, controllerMonitoringNamespace, appLabel)
		err = crClient.Create(ctx, serviceMonitor)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ServiceMonitor: %s", serviceMonName))

		By("creating VariantAutoscaling resource")
		variantAutoscaling := utils.CreateVariantAutoscalingResource(namespace, deployName, modelName, a100Acc)
		err = crClient.Create(ctx, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create VariantAutoscaling for: %s", deployName))

		By("adding an InferenceModel for the deployment")
		inferenceModel = utils.CreateInferenceModel(deployName, namespace, modelName)
		err = crClient.Create(ctx, inferenceModel)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create InferenceModel: %s", modelName))

		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	})

	It("deployment should be running", func() {
		Eventually(func() (appsv1.DeploymentStatus, error) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			if err != nil {
				return appsv1.DeploymentStatus{}, err
			}
			return deployment.Status, nil
		}, 8*time.Minute, 10*time.Second).Should(And(
			HaveField("ReadyReplicas", BeNumerically("==", 1)),
			HaveField("Replicas", BeNumerically("==", 1)),
		))
	})

	It("deployment should have correct deployment labels", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		By("verifying deployment selector labels")
		selector := deployment.Spec.Selector.MatchLabels
		Expect(selector).To(HaveKeyWithValue("app", appLabel))
		Expect(selector).To(HaveKeyWithValue("llm-d.ai/inferenceServing", "true"))
		Expect(selector).To(HaveKeyWithValue("llm-d.ai/model", "ms-sim-llm-d-modelservice"))

		By("verifying pod template labels")
		podLabels := deployment.Spec.Template.Labels
		Expect(podLabels).To(HaveKeyWithValue("app", appLabel))
		Expect(podLabels).To(HaveKeyWithValue("llm-d.ai/inferenceServing", "true"))
		Expect(podLabels).To(HaveKeyWithValue("llm-d.ai/model", "ms-sim-llm-d-modelservice"))
	})

	It("deployment should have correct resource configuration", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		By("verifying container resource limits")
		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceName("nvidia.com/gpu"), resource.MustParse("1")))

		By("verifying environment variables")
		var modelNameEnv, maxBatchSizeEnv *corev1.EnvVar
		for _, env := range container.Env {
			if env.Name == "MODEL_NAME" {
				modelNameEnv = &env
			}
			if env.Name == "MAX_BATCH_SIZE" {
				maxBatchSizeEnv = &env
			}
		}
		Expect(modelNameEnv).NotTo(BeNil(), "MODEL_NAME environment variable should be set")
		Expect(modelNameEnv.Value).To(Equal("default/default"))
		Expect(maxBatchSizeEnv).NotTo(BeNil(), "MAX_BATCH_SIZE environment variable should be set")
		Expect(maxBatchSizeEnv.Value).To(Equal("8"))
	})

	It("deployment should have corresponding service with correct selector", func() {
		service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Service: %s", serviceName))

		By("verifying service selector")
		Expect(service.Spec.Selector).To(HaveKeyWithValue("app", appLabel))
	})

	It("deployment should create pods with correct labels", func() {
		Eventually(func() ([]corev1.Pod, error) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + appLabel,
			})
			if err != nil {
				return nil, err
			}
			return podList.Items, nil
		}, 2*time.Minute, 5*time.Second).Should(HaveLen(1))

		podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + appLabel,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(podList.Items).To(HaveLen(1))

		pod := podList.Items[0]
		By("verifying pod labels")
		Expect(pod.Labels).To(HaveKeyWithValue("app", appLabel))
		Expect(pod.Labels).To(HaveKeyWithValue("llm-d.ai/inferenceServing", "true"))
		Expect(pod.Labels).To(HaveKeyWithValue("llm-d.ai/model", "ms-sim-llm-d-modelservice"))
	})

	It("should have correct GPU resource requests", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceName("nvidia.com/gpu"), resource.MustParse("1")))
	})

	It("should have VariantAutoscaling resource created", func() {
		By("verifying VariantAutoscaling resource exists")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      deployName,
		}, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling: %s", deployName))

		By("verifying VariantAutoscaling spec")
		Expect(variantAutoscaling.Spec.ModelID).To(Equal("default/default"))
		Expect(variantAutoscaling.Spec.SLOClassRef.Name).To(Equal("premium"))
		// Single-variant architecture: Each VariantAutoscaling represents one accelerator type
		Expect(variantAutoscaling.Spec.Accelerator).NotTo(BeEmpty())
		Expect(variantAutoscaling.Spec.VariantID).NotTo(BeEmpty())
	})

	It("should have VariantAutoscaling with correct ownerReference to Deployment", func() {
		By("first ensuring both Deployment and VariantAutoscaling exist")
		Eventually(func(g Gomega) {
			_, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch Deployment for: %s", deployName))
			// NOTE: Replica checks excluded - using Prometheus metrics validation instead
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("ensuring VariantAutoscaling resource exists")
		Eventually(func(g Gomega) {
			variantAutoscaling := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, variantAutoscaling)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("VariantAutoscaling should exist for: %s", deployName))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for VariantAutoscaling to have ownerReference set by controller")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch Deployment for: %s", deployName))

			variantAutoscaling := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, variantAutoscaling)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

			g.Expect(variantAutoscaling.OwnerReferences).To(HaveLen(1), fmt.Sprintf("VariantAutoscaling should have exactly one ownerReference related to: %s", deployName))

			ownerRef := variantAutoscaling.OwnerReferences[0]
			g.Expect(ownerRef.APIVersion).To(Equal("apps/v1"), fmt.Sprintf("ownerReference should have correct APIVersion for: %s", deployName))
			g.Expect(ownerRef.Kind).To(Equal("Deployment"), fmt.Sprintf("ownerReference should point to a Deployment for: %s", deployName))
			g.Expect(ownerRef.Name).To(Equal(deployment.Name), fmt.Sprintf("ownerReference should point to the correct Deployment name for: %s", deployName))
			g.Expect(ownerRef.UID).To(Equal(deployment.UID), fmt.Sprintf("ownerReference should have the correct UID for: %s", deployName))
			g.Expect(ownerRef.Controller).NotTo(BeNil(), fmt.Sprintf("ownerReference should have Controller field set for: %s", deployName))
			g.Expect(*ownerRef.Controller).To(BeTrue(), fmt.Sprintf("ownerReference Controller should be true for: %s", deployName))
		}, 4*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("should scale out when load increases", func() {
		// Set up port-forwarding for Prometheus to enable metrics queries
		By("setting up port-forward to Prometheus service")
		prometheusPortForwardCmd := utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", controllerMonitoringNamespace, 9090, 9090)
		defer func() {
			err := utils.StopCmd(prometheusPortForwardCmd)
			Expect(err).NotTo(HaveOccurred(), "Should be able to stop Prometheus port-forwarding")
		}()

		By("waiting for Prometheus port-forward to be ready")
		err := utils.VerifyPortForwardReadiness(ctx, 9090, fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", 9090))
		Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready within timeout")

		By("verifying initial state of VariantAutoscaling")
		initialVA := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      deployName,
		}, initialVA)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

		By("getting the service endpoint for load generation")
		// Port-forward the vllme service to send requests to it
		By("setting up port-forward to the vllme service")
		portForwardCmd = utils.SetUpPortForward(k8sClient, ctx, gatewayName, namespace, port, 80)
		defer func() {
			err = utils.StopCmd(portForwardCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for: %s", gatewayName))
		}()

		By("waiting for port-forward to be ready")
		err = utils.VerifyPortForwardReadiness(ctx, port, fmt.Sprintf("http://localhost:%d/v1", port))
		Expect(err).NotTo(HaveOccurred(), "Port-forward should be ready within timeout")

		By("starting load generation to create traffic")
		loadGenCmd = utils.StartLoadGenerator(loadRate, 100, port, modelName)
		defer func() {
			err = utils.StopCmd(loadGenCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", deployName))
		}()

		var currentReplicasProm, desiredReplicasProm float64
		By("waiting for load to be processed and scaling decision to be made")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

			// Verify that the optimized allocation has been computed
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 0),
				fmt.Sprintf("DesiredOptimizedAlloc should have calculated optimized replicas for: %s - actual replicas: %d", va.Name, va.Status.DesiredOptimizedAlloc.NumReplicas))

			// Verify that the number of replicas has scaled up
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 1),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s - actual replicas: %d", va.Name, va.Status.DesiredOptimizedAlloc.NumReplicas))

			// Verify Prometheus replica metrics
			// In single-variant architecture, accelerator is in spec
			currentReplicasProm, desiredReplicasProm, _, err = utils.GetInfernoReplicaMetrics(va.Name, namespace, va.Spec.Accelerator, va.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to query Prometheus metrics for: %s - got error: %v", va.Name, err))

			g.Expect(desiredReplicasProm).To(BeNumerically(">", 1),
				fmt.Sprintf("Prometheus `inferno_desired_replicas` query should show scale-up for VA: %s - actual: %.2f", va.Name, desiredReplicasProm))
			g.Expect(currentReplicasProm).To(BeNumerically(">=", 1),
				fmt.Sprintf("Prometheus `inferno_current_replicas` query should show at least 1 replica for VA: %s - actual: %.2f", va.Name, currentReplicasProm))

			// Verify that the current and desired number of replicas have the same value as Prometheus results
			g.Expect(va.Status.CurrentAlloc.NumReplicas).To(BeNumerically("==", currentReplicasProm),
				fmt.Sprintf("Current replicas %d for VA %s should be the same as Prometheus result: %.2f", va.Status.CurrentAlloc.NumReplicas, deployName, currentReplicasProm))

			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", desiredReplicasProm),
				fmt.Sprintf("Desired replicas %d for VA %s should be the same as Prometheus result: %.2f", va.Status.DesiredOptimizedAlloc.NumReplicas, deployName, desiredReplicasProm))

		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")
		err = utils.LogVariantAutoscalingStatus(ctx, deployName, namespace, crClient)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to log VariantAutoscaling status for: %s", deployName))

		fmt.Printf("Prometheus metrics for VA %s - desired replicas: %d\n",
			deployName,
			int(desiredReplicasProm))
	})

	It("should keep the same replicas if the load stays constant", func() {
		// Set up port-forwarding for Prometheus to enable metrics queries
		By("setting up port-forward to Prometheus service")
		prometheusPortForwardCmd := utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", controllerMonitoringNamespace, 9090, 9090)
		defer func() {
			err := utils.StopCmd(prometheusPortForwardCmd)
			Expect(err).NotTo(HaveOccurred(), "Should be able to stop Prometheus port-forwarding")
		}()

		By("waiting for Prometheus port-forward to be ready")
		err := utils.VerifyPortForwardReadiness(ctx, 9090, fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", 9090))
		Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready within timeout")

		By("setting up port-forward to the vllme service")
		portForwardCmd := utils.SetUpPortForward(k8sClient, ctx, gatewayName, namespace, port, 80)
		defer func() {
			err = utils.StopCmd(portForwardCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for: %s", gatewayName))
		}()
		By("waiting for port-forward to be ready")
		err = utils.VerifyPortForwardReadiness(ctx, port, fmt.Sprintf("http://localhost:%d/v1", port))
		Expect(err).NotTo(HaveOccurred(), "Port-forward should be ready within timeout")

		By("restarting load generation at the same rate")
		loadGenCmd = utils.StartLoadGenerator(loadRate, 100, port, modelName)
		defer func() {
			err = utils.StopCmd(loadGenCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", deployName))
		}()

		By("getting the current number of replicas")
		var initialDesiredReplicas int32
		va := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      deployName,
		}, va)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))
		initialDesiredReplicas = va.Status.DesiredOptimizedAlloc.NumReplicas

		var desiredReplicasProm float64
		By("verifying that the number of replicas remains constant over several minutes with constant load")
		Consistently(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

			// Verify that the desired allocation remains stable with constant load
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(initialDesiredReplicas),
				fmt.Sprintf("DesiredOptimizedAlloc for VA %s should stay at %d replicas with constant load", deployName, initialDesiredReplicas))

			// Verify Prometheus replica metrics
			// In single-variant architecture, accelerator is in spec
			_, desiredReplicasProm, _, err = utils.GetInfernoReplicaMetrics(va.Name, namespace, va.Spec.Accelerator, va.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to query Prometheus metrics for: %s - got error: %v", va.Name, err))

			// Verify that the desired number of replicas has same value as Prometheus result
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", desiredReplicasProm),
				fmt.Sprintf("Desired replicas %d for VA %s should be the same as Prometheus result: %.2f", va.Status.DesiredOptimizedAlloc.NumReplicas, deployName, desiredReplicasProm))

		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")
		err = utils.LogVariantAutoscalingStatus(ctx, deployName, namespace, crClient)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to log VariantAutoscaling status for: %s", deployName))

		fmt.Printf("Prometheus metrics for VA %s - desired replicas: %d\n",
			deployName,
			int(desiredReplicasProm))
	})

	It("should scale in with no load", func() {
		// Set up port-forwarding for Prometheus to enable metrics queries
		By("setting up port-forward to Prometheus service")
		prometheusPortForwardCmd := utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", controllerMonitoringNamespace, 9090, 9090)
		defer func() {
			err := utils.StopCmd(prometheusPortForwardCmd)
			Expect(err).NotTo(HaveOccurred(), "Should be able to stop Prometheus port-forwarding")
		}()

		By("waiting for Prometheus port-forward to be ready")
		err := utils.VerifyPortForwardReadiness(ctx, 9090, fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", 9090))
		Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready within timeout")

		var desiredReplicasProm float64
		By("waiting for scaling down decision to be made")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

			// Verify that the number of replicas has scaled down to 0
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", MinimumReplicas),
				fmt.Sprintf("No load should trigger scale-down to %d recommendation for: %s", MinimumReplicas, va.Name))

			// Verify Prometheus replica metrics
			// In single-variant architecture, accelerator is in spec
			_, desiredReplicasProm, _, err = utils.GetInfernoReplicaMetrics(va.Name, namespace, va.Spec.Accelerator, va.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to query Prometheus metrics for: %s - got error: %v", va.Name, err))

			// Verify that the desired number of replicas has same value as Prometheus result
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", desiredReplicasProm),
				fmt.Sprintf("Current replicas for VA %s should stay at %.2f with no load", deployName, desiredReplicasProm))

		}, 4*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")
		err = utils.LogVariantAutoscalingStatus(ctx, deployName, namespace, crClient)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to log VariantAutoscaling status for: %s", deployName))

		fmt.Printf("Prometheus metrics for VA %s - desired replicas: %d\n",
			deployName,
			int(desiredReplicasProm))
	})

	It("should connect to Prometheus using HTTPS with TLS", func() {
		By("verifying Prometheus is accessible via HTTPS")
		Eventually(func(g Gomega) {
			// Check if Prometheus service is running with TLS
			service, err := k8sClient.CoreV1().Services(controllerMonitoringNamespace).Get(ctx, "kube-prometheus-stack-prometheus", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Prometheus service should exist")
			g.Expect(service.Spec.Ports).To(ContainElement(HaveField("Port", int32(9090))), "Prometheus should be listening on port 9090")

			// Verify TLS secret exists
			secret, err := k8sClient.CoreV1().Secrets(controllerMonitoringNamespace).Get(ctx, "prometheus-web-tls", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "TLS secret should exist")
			g.Expect(secret.Data).To(HaveKey("tls.crt"), "TLS secret should contain certificate")
			g.Expect(secret.Data).To(HaveKey("tls.key"), "TLS secret should contain private key")

		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying controller can connect to Prometheus with TLS")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list controller pods")
			g.Expect(pods.Items).NotTo(BeEmpty(), "Controller pods should exist")

			// Check logs for TLS-related messages
			pod := pods.Items[0]
			logs, err := k8sClient.CoreV1().Pods(controllerNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				// Get all logs instead of just tail lines to find the TLS message from startup
			}).DoRaw(ctx)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get controller logs")

			logString := string(logs)
			g.Expect(logString).To(ContainSubstring("TLS configuration applied to Prometheus HTTPS transport"),
				"Controller should log TLS configuration")
			g.Expect(logString).NotTo(ContainSubstring("http: server gave HTTP response to HTTPS client"),
				"Controller should not have HTTP/HTTPS mismatch errors")

		}, 4*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("should handle TLS certificate verification correctly", func() {
		By("verifying TLS configuration in controller ConfigMap")
		configMap, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Get(ctx, "workload-variant-autoscaler-variantautoscaling-config", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "ConfigMap should exist")

		// Verify HTTPS URL is configured
		Expect(configMap.Data["PROMETHEUS_BASE_URL"]).To(ContainSubstring("https://"),
			"Prometheus URL should use HTTPS")

		// Verify TLS settings are configured
		Expect(configMap.Data["PROMETHEUS_TLS_INSECURE_SKIP_VERIFY"]).To(Equal("true"),
			"TLS insecure skip verify should be enabled for e2e tests")

		By("verifying controller startup with TLS configuration")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list controller pods")

			for _, pod := range pods.Items {
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
					fmt.Sprintf("Pod %s should be running", pod.Name))
			}
		}, 2*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("should have VariantAutoscaling deleted when Deployment is deleted", func() {
		By("deleting the Deployment")
		err := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deployName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", deployName))

		By("verifying VariantAutoscaling is deleted due to owner reference")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      deployName,
		}, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))
		Eventually(func() error {
			return crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, variantAutoscaling)
		}, 4*time.Minute, 2*time.Second).Should(HaveOccurred(), fmt.Sprintf("VariantAutoscaling for: %s should be deleted", deployName))
	})

	AfterAll(func() {
		By("deleting VariantAutoscaling resource")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployName,
				Namespace: namespace,
			},
		}
		err := crClient.Delete(ctx, variantAutoscaling)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete VariantAutoscaling for: %s", deployName))

		By("deleting ServiceMonitor")
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

		By("deleting vllme service")
		err = k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Service: %s", serviceName))

		By("deleting vllme deployment")
		err = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deployName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", deployName))

		By("deleting InferenceModel")
		err = crClient.Delete(ctx, inferenceModel)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete InferenceModel: %s", modelName))

		By("waiting for all pods to be deleted")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + appLabel})
			if err != nil {
				g.Expect(err).NotTo(HaveOccurred(), "Should be able to list Pods")
			}
			g.Expect(podList.Items).To(BeEmpty(), fmt.Sprintf("All Pods labelled: %s should be deleted", appLabel))
		}, 1*time.Minute, 1*time.Second).Should(Succeed())
	})
})

var _ = Describe("Test idle scale-to-zero with KEDA", Ordered, func() {
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
		if os.Getenv("KUBECONFIG") == "" {
			Skip("KUBECONFIG is not set; skipping e2e test")
		}

		initializeK8sClient()

		ctx = context.Background()
		namespace = llmDNamespace
		deployName = "idle-sto-zero-deployment"
		serviceName = "idle-sto-zero-service"
		serviceMonName = "idle-sto-zero-servicemonitor"
		configMapName = "model-scale-to-zero-config"
		appLabel = "idle-sto-zero-test"
		modelID = "test-idle-sto-zero-model"
		accelerator = a100Acc
		initialReplicas = 1
		retentionDuration = 4 * time.Minute // Retention period for scale-to-zero

		By("ensuring unique app label and model")
		utils.ValidateAppLabelUniqueness(namespace, appLabel, k8sClient, crClient)
		utils.ValidateVariantAutoscalingUniqueness(namespace, modelID, accelerator, crClient)

		By("creating scale-to-zero ConfigMap")
		// Sanitize modelID for ConfigMap key (replace "/" with "-" to satisfy K8s validation)
		// ConfigMap keys must match regex: [-._a-zA-Z0-9]+
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

		By("creating vllme deployment with initial replica")
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

		By("creating InferenceModel with 0 traffic for scale-to-zero test")
		inferenceModel = utils.CreateInferenceModel(deployName, namespace, modelID)
		err = crClient.Create(ctx, inferenceModel)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create InferenceModel: %s", modelID))

		By("verifying KEDA is installed")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods("keda-system").List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=keda-operator",
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list KEDA pods")
			g.Expect(podList.Items).NotTo(BeEmpty(), "KEDA operator should be running")
			for _, pod := range podList.Items {
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), fmt.Sprintf("KEDA pod %s should be running", pod.Name))
			}
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	})

	It("deployment should be running initially", func() {
		Eventually(func() (appsv1.DeploymentStatus, error) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			if err != nil {
				return appsv1.DeploymentStatus{}, err
			}
			return deployment.Status, nil
		}, 8*time.Minute, 10*time.Second).Should(And(
			HaveField("ReadyReplicas", BeNumerically("==", initialReplicas)),
			HaveField("Replicas", BeNumerically("==", initialReplicas)),
		))
	})

	It("VariantAutoscaling should be created and reconciled", func() {
		By("verifying VariantAutoscaling exists")
		va := &v1alpha1.VariantAutoscaling{}
		Eventually(func(g Gomega) {
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")
			g.Expect(va.Spec.ModelID).To(Equal(modelID), "ModelID should match")
		}, 1*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("deployment should have correct deployment labels", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		By("verifying deployment selector labels")
		selector := deployment.Spec.Selector.MatchLabels
		Expect(selector).To(HaveKeyWithValue("app", appLabel))

		By("verifying pod template labels")
		podLabels := deployment.Spec.Template.Labels
		Expect(podLabels).To(HaveKeyWithValue("app", appLabel))
	})

	It("deployment should have correct resource configuration", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		By("verifying container resource limits")
		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceName("nvidia.com/gpu"), resource.MustParse("1")))

		By("verifying environment variables")
		var modelNameEnv *corev1.EnvVar
		for _, env := range container.Env {
			if env.Name == "MODEL_NAME" {
				modelNameEnv = &env
			}
		}
		Expect(modelNameEnv).NotTo(BeNil(), "MODEL_NAME environment variable should be set")
	})

	It("deployment should have corresponding service with correct selector", func() {
		service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Service: %s", serviceName))

		By("verifying service selector")
		Expect(service.Spec.Selector).To(HaveKeyWithValue("app", appLabel))
	})

	It("should scale deployment to zero after idle period with no traffic", func() {
		By("ensuring deployment starts at 1 replica for this test")
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		if *deployment.Spec.Replicas == 0 {
			_, _ = fmt.Fprintf(GinkgoWriter, "Scaling deployment from 0 to 1 to test scale-to-zero flow\n")
			deployment.Spec.Replicas = &initialReplicas
			_, err = k8sClient.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to scale up Deployment: %s", deployName))

			By("waiting for deployment to have ready replicas")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))
				g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1),
					"Deployment should have at least 1 ready replica")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())
		}

		// Set up port-forwarding for Prometheus to enable metric verification
		By("setting up port-forward to Prometheus service for metric verification")
		prometheusPortForwardCmd := utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", controllerMonitoringNamespace, 9090, 9090)
		defer func() {
			err := utils.StopCmd(prometheusPortForwardCmd)
			Expect(err).NotTo(HaveOccurred(), "Should be able to stop Prometheus port-forwarding")
		}()

		By("waiting for Prometheus port-forward to be ready")
		err = utils.VerifyPortForwardReadiness(ctx, 9090, fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", 9090))
		Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready within timeout")

		// Wait for controller to reconcile and emit metrics
		By("waiting for controller to reconcile VariantAutoscaling and emit metrics")

		// Check VariantAutoscaling status to verify controller has reconciled
		By("checking VariantAutoscaling status before metric verification")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			_, _ = fmt.Fprintf(GinkgoWriter, "VariantAutoscaling Status: CurrentReplicas=%d, DesiredReplicas=%d, Actuation.Applied=%t, LastUpdate=%v\n",
				va.Status.CurrentAlloc.NumReplicas, va.Status.DesiredOptimizedAlloc.NumReplicas, va.Status.Actuation.Applied,
				va.Status.DesiredOptimizedAlloc.LastUpdate.Time)
			_, _ = fmt.Fprintf(GinkgoWriter, "VariantAutoscaling Spec: Accelerator=%q, VariantID=%q, ModelID=%q\n",
				va.Spec.Accelerator, va.Spec.VariantID, va.Spec.ModelID)
			_, _ = fmt.Fprintf(GinkgoWriter, "DesiredOptimizedAlloc Reason: %q\n",
				va.Status.DesiredOptimizedAlloc.Reason)

			// Check if metric emission condition would be satisfied
			if va.Status.DesiredOptimizedAlloc.NumReplicas < 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "⚠ WARNING: DesiredOptimizedAlloc.NumReplicas=%d is negative - metrics will NOT be emitted!\n",
					va.Status.DesiredOptimizedAlloc.NumReplicas)
			} else if !va.Status.Actuation.Applied {
				_, _ = fmt.Fprintf(GinkgoWriter, "⚠ WARNING: Actuation.Applied=false - metrics have NOT been emitted yet!\n")
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "✓ Metric emission condition satisfied (DesiredReplicas >= 0, Actuation.Applied=true)\n")
			}

			// CRITICAL: Wait for Actuation.Applied to be true, confirming EmitMetrics succeeded
			g.Expect(va.Status.Actuation.Applied).To(BeTrue(),
				"Controller should have successfully emitted metrics (Actuation.Applied should be true)")

			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
				"Controller should have set desired replicas")
		}, 2*time.Minute, 10*time.Second).Should(Succeed(), "Controller should reconcile VariantAutoscaling and emit metrics")

		// Wait additional time for Prometheus to scrape the controller\'s /metrics endpoint
		By("waiting for Prometheus to scrape metrics from controller")
		time.Sleep(30 * time.Second) // Prometheus scrape interval is typically 15-30s

		// Verify metrics are being emitted - use the actual CR values like successful tests do
		By("verifying controller emits replica metrics with correct labels")
		Eventually(func(g Gomega) {
			// Fetch the VariantAutoscaling CR to get actual Spec values
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			// Query metrics using the actual Spec values from the CR (like successful tests)
			currentReplicasProm, desiredReplicasProm, desiredRatioProm, err := utils.GetInfernoReplicaMetrics(
				va.Name, namespace, va.Spec.Accelerator, va.Spec.VariantID)

			if err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metric query failed (Spec.Accelerator=%q, Spec.VariantID=%q): %v\n",
					va.Spec.Accelerator, va.Spec.VariantID, err)
				g.Expect(err).NotTo(HaveOccurred(), "Should be able to query Prometheus metrics")
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "✓ Metrics found: current=%.0f, desired=%.0f, ratio=%.2f\n",
				currentReplicasProm, desiredReplicasProm, desiredRatioProm)

			// For scale-to-zero, we expect desired replicas to be 0
			g.Expect(desiredReplicasProm).To(BeNumerically(">=", 0), "Desired replicas should be non-negative")
		}, 2*time.Minute, 10*time.Second).Should(Succeed(), "Controller should emit metrics with correct labels")

		// Also verify that we can query metrics without accelerator_type and variant_id (should fail or return different results)
		By("verifying query without all labels returns empty or different results (diagnostic)")
		partialQuery := fmt.Sprintf("inferno_desired_replicas{variant_name=\"%s\",exported_namespace=\"%s\"}", deployName, namespace)
		promClient, err := utils.NewPrometheusClient("https://localhost:9090", true)
		Expect(err).NotTo(HaveOccurred())
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		partialValue, partialErr := promClient.QueryWithRetry(ctx2, partialQuery)
		if partialErr != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Partial query (without accelerator_type/variant_id) failed as expected: %v\n", partialErr)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Partial query returned value: %.0f (may be different from full query)\n", partialValue)
		}

		By("creating KEDA ScaledObject for deployment")
		scaledObject := &unstructured.Unstructured{}
		scaledObject.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "keda.sh",
			Version: "v1alpha1",
			Kind:    "ScaledObject",
		})
		scaledObject.SetName(deployName + "-scaler")
		scaledObject.SetNamespace(namespace)
		scaledObject.Object["spec"] = map[string]interface{}{
			"scaleTargetRef": map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"name":       deployName,
			},
			"pollingInterval": int64(5),
			"cooldownPeriod":  int64(30),
			"maxReplicaCount": int64(10),
			"triggers": []interface{}{
				map[string]interface{}{
					"type": "prometheus",
					"metadata": map[string]interface{}{
						"serverAddress":       "https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090",
						"query":               fmt.Sprintf("inferno_desired_replicas{variant_name=\"%s\",exported_namespace=\"%s\",accelerator_type=\"%s\",variant_id=\"%s-%s-1\"}", deployName, namespace, accelerator, modelID, accelerator),
						"threshold":           "1",
						"activationThreshold": "0",
						"metricType":          "AverageValue",
						"unsafeSsl":           "true",
					},
				},
			},
		}
		err = crClient.Create(ctx, scaledObject)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ScaledObject: %s", deployName+"-scaler"))

		By("waiting for KEDA ScaledObject to be ready")
		Eventually(func(g Gomega) {
			so := &unstructured.Unstructured{}
			so.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "keda.sh",
				Version: "v1alpha1",
				Kind:    "ScaledObject",
			})
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName + "-scaler", Namespace: namespace}, so)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get ScaledObject")

			// Check if ScaledObject has conditions and is ready
			conditions, found, err := unstructured.NestedSlice(so.Object, "status", "conditions")
			if err == nil && found {
				for _, condition := range conditions {
					condMap, ok := condition.(map[string]interface{})
					if !ok {
						continue
					}
					condType, _, _ := unstructured.NestedString(condMap, "type")
					condStatus, _, _ := unstructured.NestedString(condMap, "status")
					if condType == "Ready" && condStatus == "True" {
						_, _ = fmt.Fprintf(GinkgoWriter, "✓ KEDA ScaledObject is ready\n")
						return
					}
				}
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "Waiting for KEDA ScaledObject to be ready...\n")
			g.Expect(found).To(BeTrue(), "ScaledObject should have conditions")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		// Note: Prometheus port-forward already set up earlier in the test for metric verification

		By("waiting for retention period to pass with zero traffic")
		_, _ = fmt.Fprintf(GinkgoWriter, "Waiting %v for retention period (no traffic simulated)...\n", retentionDuration)
		time.Sleep(retentionDuration + 30*time.Second) // Add buffer for controller reconciliation

		By("checking totalRequests from Prometheus (what optimizer sees)")
		totalRequestsQuery := fmt.Sprintf(`sum(increase(vllm_request_success_total{model_name="%s",namespace="%s"}[2m]))`, modelID, namespace)
		promClient2, err2 := utils.NewPrometheusClient("https://localhost:9090", true)
		Expect(err2).NotTo(HaveOccurred())
		ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel3()
		totalRequests, err2 := promClient2.QueryWithRetry(ctx3, totalRequestsQuery)
		if err2 != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "⚠ Failed to query totalRequests: %v\n", err2)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "📊 totalRequests over retention period: %.0f (query: %s)\n", totalRequests, totalRequestsQuery)
			if totalRequests > 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "⚠ WARNING: totalRequests > 0 - optimizer will keep 1 replica even with zero user traffic!\n")
			}
		}

		By("verifying controller sets desiredReplicas to 0 in VariantAutoscaling status")
		var desiredReplicasProm float64
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			// Debug output to understand the flow
			_, _ = fmt.Fprintf(GinkgoWriter, "VA Status: DesiredOptimized=%d, Current=%d, Reason=%q, LastUpdate=%v\n",
				va.Status.DesiredOptimizedAlloc.NumReplicas,
				va.Status.CurrentAlloc.NumReplicas,
				va.Status.DesiredOptimizedAlloc.Reason,
				va.Status.DesiredOptimizedAlloc.LastUpdate.Time)

			// Check status conditions to see if optimizer is running or fallback is used
			_, _ = fmt.Fprintf(GinkgoWriter, "  Total Conditions: %d\n", len(va.Status.Conditions))
			if len(va.Status.Conditions) == 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "  ⚠ WARNING: No status conditions set on VA resource\n")
			}
			for _, cond := range va.Status.Conditions {
				if cond.Type == "OptimizationReady" || cond.Type == "MetricsAvailable" {
					_, _ = fmt.Fprintf(GinkgoWriter, "  Condition: %s=%s, Reason=%s, Message=%s\n",
						cond.Type, cond.Status, cond.Reason, cond.Message)
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "  Other Condition: %s=%s\n", cond.Type, cond.Status)
				}
			}

			// First check if optimizer recommended 0
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(0)),
				"Optimizer should recommend 0 replicas for scale-to-zero scenario")

			// Check if status has current allocation with 0 desired replicas
			if va.Status.CurrentAlloc.NumReplicas == 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "✓ Controller set desiredReplicas to 0 in VariantAutoscaling status\n")
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Current desiredReplicas: %d (expected 0)\n", va.Status.CurrentAlloc.NumReplicas)
			}

			g.Expect(va.Status.CurrentAlloc.NumReplicas).To(Equal(int32(0)),
				"Controller should set desiredReplicas to 0 after idle period")

			// Verify Prometheus has the correct metric value
			_, desiredReplicasProm, _, err = utils.GetInfernoReplicaMetrics(va.Name, namespace, va.Spec.Accelerator, va.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query Prometheus metrics")

			// Verify that the desired number of replicas in Prometheus matches VA status
			g.Expect(int32(desiredReplicasProm)).To(Equal(va.Status.CurrentAlloc.NumReplicas),
				"Prometheus inferno_desired_replicas should match VA status (both should be 0)")
		}, 3*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying Prometheus metric is set to 0")
		_, _ = fmt.Fprintf(GinkgoWriter, "✓ Prometheus inferno_desired_replicas metric: %.0f\n", desiredReplicasProm)
		Expect(int32(desiredReplicasProm)).To(Equal(int32(0)), "Prometheus metric should be 0")

		By("verifying KEDA scales deployment to 0 replicas")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get Deployment")

			if deployment.Status.Replicas == 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "✓ KEDA successfully scaled deployment to 0 replicas\n")
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Current replicas: %d (expected 0)\n", deployment.Status.Replicas)
			}

			g.Expect(deployment.Status.Replicas).To(Equal(int32(0)),
				"KEDA should scale deployment to 0 replicas")
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(0)),
				"Deployment should have 0 ready replicas")
		}, 5*time.Minute, 15*time.Second).Should(Succeed())

		By("verifying no pods are running")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app=%s", appLabel),
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list Pods")
			g.Expect(podList.Items).To(BeEmpty(), "No pods should be running after scale-to-zero")
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "✓ Scale-to-zero flow completed successfully\n")
	})

	AfterAll(func() {
		By("cleaning up KEDA ScaledObject")
		scaledObject := &unstructured.Unstructured{}
		scaledObject.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "keda.sh",
			Version: "v1alpha1",
			Kind:    "ScaledObject",
		})
		scaledObject.SetName(deployName + "-scaler")
		scaledObject.SetNamespace(namespace)
		err := crClient.Delete(ctx, scaledObject)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete ScaledObject: %s", deployName+"-scaler"))

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

var _ = Describe("Test traffic-based scale-to-zero with retention period", Ordered, func() {
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
		if os.Getenv("KUBECONFIG") == "" {
			Skip("KUBECONFIG is not set; skipping e2e test")
		}

		initializeK8sClient()

		ctx = context.Background()
		namespace = llmDNamespace
		deployName = "traffic-sto-zero-deployment"
		serviceName = "traffic-sto-zero-service"
		serviceMonName = "traffic-sto-zero-servicemonitor"
		configMapName = "model-scale-to-zero-config"
		appLabel = "traffic-sto-zero-test"
		// EXPERIMENTAL: Use "default/default" to match working test pattern and leverage
		// existing infrastructure metrics. Controller queries for vLLM metrics by modelID.
		// To revert: change defaultModelId back to "test-traffic-sto-zero-model"
		modelID = defaultModelId
		accelerator = a100Acc
		initialReplicas = 1
		retentionDuration = 4 * time.Minute // Retention period for scale-to-zero

		By("ensuring unique app label and model")
		utils.ValidateAppLabelUniqueness(namespace, appLabel, k8sClient, crClient)
		utils.ValidateVariantAutoscalingUniqueness(namespace, modelID, accelerator, crClient)

		By("creating scale-to-zero ConfigMap")
		// Sanitize modelID for ConfigMap key (replace "/" with "-" to satisfy K8s validation)
		// ConfigMap keys must match regex: [-._a-zA-Z0-9]+
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

		By("creating vllme deployment with initial replica")
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
		// EXPERIMENTAL: Use "default/default" as modelName to match traffic generation
		// and leverage existing infrastructure. Gateway routes based on InferenceModel.spec.modelName.
		// To revert: change defaultModelId back to modelID
		inferenceModel = utils.CreateInferenceModel(deployName, namespace, defaultModelId)
		err = crClient.Create(ctx, inferenceModel)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create InferenceModel with model: %s", defaultModelId))
		_, _ = fmt.Fprintf(GinkgoWriter, "✓ InferenceModel created with modelName=%s\n", defaultModelId)

		// Note: KEDA ScaledObject will be created later in the main test after initial reconciliation

		By("verifying KEDA is installed")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods("keda-system").List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=keda-operator",
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list KEDA pods")
			g.Expect(podList.Items).NotTo(BeEmpty(), "KEDA operator should be running")
			for _, pod := range podList.Items {
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), fmt.Sprintf("KEDA pod %s should be running", pod.Name))
			}
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	})

	It("deployment should be running initially", func() {
		Eventually(func() (appsv1.DeploymentStatus, error) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			if err != nil {
				return appsv1.DeploymentStatus{}, err
			}
			return deployment.Status, nil
		}, 8*time.Minute, 10*time.Second).Should(And(
			HaveField("ReadyReplicas", BeNumerically("==", initialReplicas)),
			HaveField("Replicas", BeNumerically("==", initialReplicas)),
		))
	})

	It("VariantAutoscaling should be created and reconciled", func() {
		By("verifying VariantAutoscaling exists")
		va := &v1alpha1.VariantAutoscaling{}
		Eventually(func(g Gomega) {
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")
			g.Expect(va.Spec.ModelID).To(Equal(modelID), "ModelID should match")
		}, 1*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("deployment should have correct deployment labels", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		By("verifying deployment selector labels")
		selector := deployment.Spec.Selector.MatchLabels
		Expect(selector).To(HaveKeyWithValue("app", appLabel))

		By("verifying pod template labels")
		podLabels := deployment.Spec.Template.Labels
		Expect(podLabels).To(HaveKeyWithValue("app", appLabel))
	})

	It("deployment should have correct resource configuration", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		By("verifying container resource limits")
		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceName("nvidia.com/gpu"), resource.MustParse("1")))

		By("verifying environment variables")
		var modelNameEnv *corev1.EnvVar
		for _, env := range container.Env {
			if env.Name == "MODEL_NAME" {
				modelNameEnv = &env
			}
		}
		Expect(modelNameEnv).NotTo(BeNil(), "MODEL_NAME environment variable should be set")
	})

	It("deployment should have corresponding service with correct selector", func() {
		service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Service: %s", serviceName))

		By("verifying service selector")
		Expect(service.Spec.Selector).To(HaveKeyWithValue("app", appLabel))
	})

	It("controller should emit baseline metrics before traffic test", func() {
		// This test verifies controller is emitting metrics BEFORE traffic test
		// Matches working test pattern (single VA, multiple VAs) which only verify controller metrics

		// STEP 1: Ensure deployment is ready first
		By("waiting for deployment to have ready replicas")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))
			_, _ = fmt.Fprintf(GinkgoWriter, "Deployment replicas: Ready=%d, Available=%d, Target=%d\n",
				deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas, *deployment.Spec.Replicas)
			g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1),
				"Deployment should have at least 1 ready replica before baseline metrics test")
		}, 8*time.Minute, 10*time.Second).Should(Succeed())

		By("setting up port-forward to Prometheus for metric verification")
		prometheusPortForwardCmd := utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", controllerMonitoringNamespace, 9090, 9090)
		defer func() {
			err := utils.StopCmd(prometheusPortForwardCmd)
			Expect(err).NotTo(HaveOccurred(), "Should be able to stop Prometheus port-forwarding")
		}()

		By("waiting for Prometheus port-forward to be ready")
		err := utils.VerifyPortForwardReadiness(ctx, 9090, fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", 9090))
		Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready")

		// STEP 2: Wait for controller to see deployment and emit metrics
		By("waiting for controller to reconcile and emit metrics")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			// Verify controller emitted metrics (Actuation.Applied = true)
			g.Expect(va.Status.Actuation.Applied).To(BeTrue(),
				"Controller should have emitted metrics before traffic test")

			// Verify we can query controller-emitted metrics from Prometheus
			_, desiredReplicasProm, _, err := utils.GetInfernoReplicaMetrics(va.Name, namespace, va.Spec.Accelerator, va.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query controller metrics from Prometheus")

			_, _ = fmt.Fprintf(GinkgoWriter, "✓ Controller emitting baseline metrics: desired=%.0f\n", desiredReplicasProm)
		}, 3*time.Minute, 10*time.Second).Should(Succeed(), "Prometheus should be scraping controller metrics")

		// Note: We do NOT verify Prometheus is scraping vLLM pod metrics here.
		// The working tests (single VA, multiple VAs) don't do this verification.
		// The controller handles missing vLLM metrics gracefully via fallback logic.
		// ServiceMonitor discovery happens asynchronously and can take several minutes.
		// By the time traffic is generated, vLLM metrics will be available for the optimizer.
	})

	It("should scale to zero after traffic stops and retention period expires", func() {
		// This test verifies the complete scale-to-zero flow:
		// 1. Start with traffic (30 seconds) -> optimizer keeps replicas
		// 2. Stop traffic and wait retention period -> optimizer scales to 0
		// 3. KEDA scales deployment to 0

		// Declare variables for Prometheus metrics that will be used in multiple Eventually blocks
		var currentReplicasProm, desiredReplicasProm float64

		// Set up Prometheus port-forwarding FIRST (enables metric verification throughout the test)
		By("setting up port-forward to Prometheus service for metric verification")
		prometheusPortForwardCmd := utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", controllerMonitoringNamespace, 9090, 9090)
		defer func() {
			err := utils.StopCmd(prometheusPortForwardCmd)
			if err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Warning: Failed to stop Prometheus port-forwarding: %v\n", err)
			}
		}()

		By("waiting for Prometheus port-forward to be ready")
		err := utils.VerifyPortForwardReadiness(ctx, 9090, fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", 9090))
		Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready within timeout")

		// Ensure deployment starts at 1 replica (previous tests may have scaled to 0)
		By("ensuring deployment starts at 1 replica for traffic generation")
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		initialReplicas := int32(1)
		if *deployment.Spec.Replicas == 0 {
			_, _ = fmt.Fprintf(GinkgoWriter, "Scaling deployment from 0 to 1 for traffic test\n")
			deployment.Spec.Replicas = &initialReplicas
			_, err = k8sClient.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to scale up Deployment: %s", deployName))

			By("waiting for deployment to have ready replicas")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))
				_, _ = fmt.Fprintf(GinkgoWriter, "Deployment replicas: Ready=%d, Available=%d, Target=%d\n",
					deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas, *deployment.Spec.Replicas)
				g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1),
					"Deployment should have at least 1 ready replica")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Deployment already at %d replicas, verifying pod readiness\n", *deployment.Spec.Replicas)

			By("waiting for deployment to have ready replicas")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))
				_, _ = fmt.Fprintf(GinkgoWriter, "Deployment replicas: Ready=%d, Available=%d, Target=%d\n",
					deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas, *deployment.Spec.Replicas)
				g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1),
					"Deployment should have at least 1 ready replica")
			}, 8*time.Minute, 10*time.Second).Should(Succeed())
		}

		// Wait for controller to reconcile BEFORE creating KEDA
		// CRITICAL: Must wait for BOTH current AND desired to be set correctly!
		// If we create KEDA while desired=0 (from first-run fallback), KEDA will scale to 0.
		By("waiting for controller to reconcile and set current=1 and desired>=1")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			_, _ = fmt.Fprintf(GinkgoWriter, "VariantAutoscaling Status: CurrentReplicas=%d, DesiredReplicas=%d, Actuation.Applied=%t, Reason=%q, LastUpdate=%v\n",
				va.Status.CurrentAlloc.NumReplicas, va.Status.DesiredOptimizedAlloc.NumReplicas, va.Status.Actuation.Applied,
				va.Status.DesiredOptimizedAlloc.Reason, va.Status.DesiredOptimizedAlloc.LastUpdate.Time)

			// CRITICAL: Wait for controller to see the current replica count
			g.Expect(va.Status.CurrentAlloc.NumReplicas).To(BeNumerically(">=", 1),
				"Controller should have seen the deployment's 1 replica")

			// CRITICAL: Wait for desired to be set correctly (not stuck at 0 from first run)
			// This prevents KEDA from immediately scaling to 0 when created
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1),
				"Controller should have updated desired to match current (not stuck at 0 from first-run fallback)")

			// Wait for Actuation.Applied to be true (controller emitted metrics)
			g.Expect(va.Status.Actuation.Applied).To(BeTrue(),
				"Controller should have successfully emitted metrics (Actuation.Applied should be true)")
		}, 3*time.Minute, 10*time.Second).Should(Succeed(), "Controller should reconcile and set current=1, desired>=1")

		// Wait for Prometheus to scrape the controller's /metrics endpoint
		By("waiting for Prometheus to scrape metrics from controller")
		time.Sleep(30 * time.Second) // Prometheus scrape interval is typically 15-30s

		// Verify service has endpoints before port-forwarding
		By("verifying service has ready endpoints before port-forward")
		Eventually(func(g Gomega) {
			_, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Service: %s", serviceName))

			endpoints, err := k8sClient.CoreV1().Endpoints(namespace).Get(ctx, serviceName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Endpoints for Service: %s", serviceName))

			// Check that service has at least one ready endpoint
			hasReadyEndpoint := false
			for _, subset := range endpoints.Subsets {
				if len(subset.Addresses) > 0 {
					hasReadyEndpoint = true
					break
				}
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "Service %s endpoints: %d subsets, hasReadyEndpoint=%v\n",
				serviceName, len(endpoints.Subsets), hasReadyEndpoint)

			g.Expect(hasReadyEndpoint).To(BeTrue(),
				fmt.Sprintf("Service %s should have at least one ready endpoint", serviceName))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		// Create KEDA ScaledObject (InferenceModel already created in BeforeSuite)
		// Get VA to access VariantID for KEDA query
		va := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
		Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling for KEDA creation")

		By("creating KEDA ScaledObject for deployment")
		scaledObjectName := deployName + "-scaler"
		scaledObject := &unstructured.Unstructured{}
		scaledObject.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "keda.sh",
			Version: "v1alpha1",
			Kind:    "ScaledObject",
		})
		scaledObject.SetName(scaledObjectName)
		scaledObject.SetNamespace(namespace)
		scaledObject.Object["spec"] = map[string]interface{}{
			"scaleTargetRef": map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"name":       deployName,
			},
			"pollingInterval": int64(5),
			"cooldownPeriod":  int64(30),
			"maxReplicaCount": int64(10),
			"triggers": []interface{}{
				map[string]interface{}{
					"type": "prometheus",
					"metadata": map[string]interface{}{
						"serverAddress":       "https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090",
						"query":               fmt.Sprintf(`inferno_desired_replicas{variant_name="%s",exported_namespace="%s",accelerator_type="%s",variant_id="%s"}`, deployName, namespace, va.Spec.Accelerator, va.Spec.VariantID),
						"threshold":           "1",
						"activationThreshold": "0",
						"metricType":          "AverageValue",
						"unsafeSsl":           "true",
					},
				},
			},
		}

		err = crClient.Create(ctx, scaledObject)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ScaledObject: %s", scaledObjectName))
		_, _ = fmt.Fprintf(GinkgoWriter, "✓ KEDA ScaledObject created: %s\n", scaledObjectName)

		By("waiting for KEDA ScaledObject to be ready")
		Eventually(func(g Gomega) {
			so := &unstructured.Unstructured{}
			so.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "keda.sh",
				Version: "v1alpha1",
				Kind:    "ScaledObject",
			})
			err := crClient.Get(ctx, client.ObjectKey{Name: scaledObjectName, Namespace: namespace}, so)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get ScaledObject")

			// Check if ScaledObject has conditions and is ready
			conditions, found, err := unstructured.NestedSlice(so.Object, "status", "conditions")
			if err == nil && found {
				for _, condition := range conditions {
					condMap, ok := condition.(map[string]interface{})
					if !ok {
						continue
					}
					condType, _, _ := unstructured.NestedString(condMap, "type")
					condStatus, _, _ := unstructured.NestedString(condMap, "status")
					if condType == "Ready" && condStatus == "True" {
						_, _ = fmt.Fprintf(GinkgoWriter, "✓ KEDA ScaledObject is ready\n")
						return
					}
				}
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "Waiting for KEDA ScaledObject to be ready...\n")
			g.Expect(found).To(BeTrue(), "ScaledObject should have conditions")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		// Start traffic generation using gateway infrastructure (like working tests)
		// Gateway routes to our service based on InferenceModel, so we reuse existing
		// infrastructure instead of waiting 6 minutes for ServiceMonitor discovery
		By("setting up port-forward to gateway for traffic generation")
		port := 8001 // Use different port to avoid conflict with other tests
		portForwardCmd := utils.SetUpPortForward(k8sClient, ctx, gatewayName, namespace, port, 80)
		defer func() {
			err := utils.StopCmd(portForwardCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for gateway: %s", gatewayName))
		}()

		By("waiting for port-forward to be ready")
		err = utils.VerifyPortForwardReadiness(ctx, port, fmt.Sprintf("http://localhost:%d/v1", port))
		Expect(err).NotTo(HaveOccurred(), "Port-forward should be ready within timeout")

		By("starting traffic generation")
		loadRate := 10 // 10 requests per second
		// EXPERIMENTAL: Use "default/default" for traffic generation to leverage existing
		// infrastructure ServiceMonitor that Prometheus has already discovered.
		// Our VariantAutoscaling still uses modelID="test-traffic-sto-zero-model" for tracking.
		// To revert: change defaultModelId back to modelID
		loadGenCmd := utils.StartLoadGenerator(loadRate, 100, port, defaultModelId)
		defer func() {
			err := utils.StopCmd(loadGenCmd)
			if err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Warning: Failed to stop load generator: %v\n", err)
			}
		}()

		_, _ = fmt.Fprintf(GinkgoWriter, "Starting traffic generation at %d req/s...\n", loadRate)

		// Wait for vLLM rate([1m]) data to accumulate: 60s + scrape interval
		By("waiting for vLLM metrics rate data to accumulate")
		_, _ = fmt.Fprintf(GinkgoWriter, "Waiting 90 seconds for rate([1m]) data accumulation...\n")
		time.Sleep(90 * time.Second)

		By("waiting for controller to process traffic and emit metrics")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			_, _ = fmt.Fprintf(GinkgoWriter, "Waiting for controller: DesiredOptimized=%d, Current=%d, Reason=%q, LastUpdate=%v\n",
				va.Status.DesiredOptimizedAlloc.NumReplicas, va.Status.CurrentAlloc.NumReplicas,
				va.Status.DesiredOptimizedAlloc.Reason, va.Status.DesiredOptimizedAlloc.LastUpdate.Time)

			// Check status conditions
			_, _ = fmt.Fprintf(GinkgoWriter, "  Total Conditions: %d\n", len(va.Status.Conditions))
			for _, cond := range va.Status.Conditions {
				if cond.Type == "OptimizationReady" || cond.Type == "MetricsAvailable" {
					_, _ = fmt.Fprintf(GinkgoWriter, "  Condition: %s=%s, Reason=%s, Message=%s\n",
						cond.Type, cond.Status, cond.Reason, cond.Message)
				}
			}

			// Verify controller has processed traffic and recommended replicas > 0
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 0),
				"Controller should see traffic and recommend replicas > 0 (if this fails, vLLM metrics may not be available)")

			// Verify metrics were emitted to Prometheus (like idle scale-to-zero test)
			currentReplicasProm, desiredReplicasProm, _, err = utils.GetInfernoReplicaMetrics(va.Name, namespace, va.Spec.Accelerator, va.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query inferno_desired_replicas from Prometheus")
			g.Expect(int32(desiredReplicasProm)).To(BeNumerically(">", 0),
				"Prometheus inferno_desired_replicas should be > 0")

			_, _ = fmt.Fprintf(GinkgoWriter, "✓ Metrics: current=%.0f, desired=%.0f\n", currentReplicasProm, desiredReplicasProm)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		// KEDA was created after controller set desired>=1, so it never saw desired=0
		// Thanks to controller's first-run deployment discovery protection
		_, _ = fmt.Fprintf(GinkgoWriter, "✓ KEDA has been observing desired=%.0f during traffic\n", desiredReplicasProm)

		By("stopping traffic generation")
		err = utils.StopCmd(loadGenCmd)
		Expect(err).NotTo(HaveOccurred(), "Should be able to stop load generator")
		_, _ = fmt.Fprintf(GinkgoWriter, "✓ Traffic stopped\n")

		By("waiting for retention period to pass with zero traffic")
		// Increased buffer: retention period + Prometheus scrape interval (30s) + controller reconciliation (60s)
		waitTime := retentionDuration + 90*time.Second
		_, _ = fmt.Fprintf(GinkgoWriter, "Waiting %v for retention period + buffer (no traffic)...\n", waitTime)
		time.Sleep(waitTime)

		By("verifying controller sets DesiredOptimizedAlloc to 0")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			_, _ = fmt.Fprintf(GinkgoWriter, "VA Status: DesiredOptimized=%d, Current=%d, Reason=%q, LastUpdate=%v\n",
				va.Status.DesiredOptimizedAlloc.NumReplicas,
				va.Status.CurrentAlloc.NumReplicas,
				va.Status.DesiredOptimizedAlloc.Reason,
				va.Status.DesiredOptimizedAlloc.LastUpdate.Time)

			// Check status conditions
			_, _ = fmt.Fprintf(GinkgoWriter, "  Total Conditions: %d\n", len(va.Status.Conditions))
			if len(va.Status.Conditions) == 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "  ⚠ WARNING: No status conditions set on VA resource\n")
			}
			for _, cond := range va.Status.Conditions {
				if cond.Type == "OptimizationReady" || cond.Type == "MetricsAvailable" {
					_, _ = fmt.Fprintf(GinkgoWriter, "  Condition: %s=%s, Reason=%s, Message=%s\n",
						cond.Type, cond.Status, cond.Reason, cond.Message)
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "  Other Condition: %s=%s\n", cond.Type, cond.Status)
				}
			}

			// Verify optimizer recommends 0
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(0)),
				"Optimizer should recommend 0 replicas after retention period with no traffic")

			// Verify Prometheus metric
			_, desiredReplicasProm, _, err = utils.GetInfernoReplicaMetrics(va.Name, namespace, va.Spec.Accelerator, va.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query Prometheus metrics")
			g.Expect(int32(desiredReplicasProm)).To(Equal(int32(0)),
				"Prometheus inferno_desired_replicas should be 0")

		}, 3*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying KEDA scales deployment to 0 replicas")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get Deployment")

			if deployment.Status.Replicas == 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "✓ KEDA successfully scaled deployment to 0 replicas\n")
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Deployment replicas: %d (expected 0)\n", deployment.Status.Replicas)
			}

			g.Expect(deployment.Status.Replicas).To(Equal(int32(0)),
				"KEDA should scale deployment to 0 replicas")
		}, 5*time.Minute, 15*time.Second).Should(Succeed())

		By("verifying CurrentAlloc reflects the scaled-down state")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: deployName, Namespace: namespace}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			g.Expect(va.Status.CurrentAlloc.NumReplicas).To(Equal(int32(0)),
				"CurrentAlloc should reflect deployment scaled to 0")
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "✓ Scale-to-zero after traffic stops completed successfully\n")
	})

	AfterAll(func() {
		By("cleaning up KEDA ScaledObject")
		scaledObject := &unstructured.Unstructured{}
		scaledObject.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "keda.sh",
			Version: "v1alpha1",
			Kind:    "ScaledObject",
		})
		scaledObject.SetName(deployName + "-scaler")
		scaledObject.SetNamespace(namespace)
		err := crClient.Delete(ctx, scaledObject)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete ScaledObject: %s", deployName+"-scaler"))

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

var _ = Describe("Test workload-variant-autoscaler with vllme deployment - multiple VAs - critical requests", Ordered, func() {
	var (
		namespace                string
		firstDeployName          string
		secondDeployName         string
		firstAppLabel            string
		secondAppLabel           string
		firstServiceName         string
		firstServiceMonitorName  string
		secondServiceName        string
		secondServiceMonitorName string
		firstModelName           string
		secondModelName          string
		firstInferenceModel      *unstructured.Unstructured
		secondInferenceModel     *unstructured.Unstructured

		ctx context.Context
	)

	BeforeAll(func() {
		if os.Getenv("KUBECONFIG") == "" {
			Skip("KUBECONFIG is not set; skipping e2e test")
		}

		initializeK8sClient()

		ctx = context.Background()
		namespace = llmDNamespace
		firstDeployName = "vllme-deployment-1"
		firstAppLabel = "vllme-1"
		firstServiceName = "vllme-service-1"
		firstServiceMonitorName = "vllme-servicemonitor-1"
		secondDeployName = "vllme-deployment-2"
		secondServiceName = "vllme-service-2"
		secondServiceMonitorName = "vllme-servicemonitor-2"
		secondAppLabel = "vllme-2"
		firstModelName = defaultModelId
		secondModelName = llamaModelId

		By("ensuring unique app labels for deployment and service")
		utils.ValidateAppLabelUniqueness(namespace, firstAppLabel, k8sClient, crClient)
		utils.ValidateAppLabelUniqueness(namespace, secondAppLabel, k8sClient, crClient)
		utils.ValidateVariantAutoscalingUniqueness(namespace, defaultModelId, a100Acc, crClient)
		utils.ValidateVariantAutoscalingUniqueness(namespace, llamaModelId, a100Acc, crClient)

		By("creating resources for the first deployment")
		firstDeployment := utils.CreateVllmeDeployment(namespace, firstDeployName, firstModelName, firstAppLabel)
		_, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, firstDeployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create first Deployment: %s", firstDeployName))

		firstService := utils.CreateVllmeService(namespace, firstServiceName, firstAppLabel, 30000)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, firstService, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create first Service: %s", firstServiceName))

		firstServiceMonitor := utils.CreateVllmeServiceMonitor(firstServiceMonitorName, controllerMonitoringNamespace, firstAppLabel)
		err = crClient.Create(ctx, firstServiceMonitor)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create first ServiceMonitor: %s", firstServiceMonitorName))

		variantAutoscaling := utils.CreateVariantAutoscalingResource(namespace, firstDeployName, firstModelName, a100Acc)
		err = crClient.Create(ctx, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create first VariantAutoscaling for: %s", firstDeployName))

		By("adding an InferenceModel for the first deployment")
		firstInferenceModel = utils.CreateInferenceModel(firstDeployName, namespace, firstModelName)
		err = crClient.Create(ctx, firstInferenceModel)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create first InferenceModel: %s", firstModelName))

		By("creating resources for the second deployment")
		secondDeployment := utils.CreateVllmeDeployment(namespace, secondDeployName, secondModelName, secondAppLabel)
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, secondDeployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create second Deployment: %s", secondDeployName))

		secondVariantAutoscaling := utils.CreateVariantAutoscalingResource(namespace, secondDeployName, secondModelName, a100Acc)
		err = crClient.Create(ctx, secondVariantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create second VariantAutoscaling for: %s", secondDeployName))

		secondService := utils.CreateVllmeService(namespace, secondServiceName, secondAppLabel, 30001)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, secondService, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create second Service: %s", secondServiceName))

		secondServiceMonitor := utils.CreateVllmeServiceMonitor(secondServiceMonitorName, controllerMonitoringNamespace, secondAppLabel)
		err = crClient.Create(ctx, secondServiceMonitor)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create second ServiceMonitor: %s", secondServiceMonitorName))

		By("adding an InferenceModel for the second deployment")
		secondInferenceModel = utils.CreateInferenceModel(secondDeployName, namespace, secondModelName)
		err = crClient.Create(ctx, secondInferenceModel)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create second InferenceModel: %s", secondModelName))

		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	})

	It("deployments should be running", func() {
		Eventually(func() (appsv1.DeploymentStatus, error) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, firstDeployName, metav1.GetOptions{})
			if err != nil {
				return appsv1.DeploymentStatus{}, err
			}
			return deployment.Status, nil
		}, 8*time.Minute, 10*time.Second).Should(And(
			HaveField("ReadyReplicas", BeNumerically("==", 1)),
			HaveField("Replicas", BeNumerically("==", 1)),
		))

		Eventually(func() (appsv1.DeploymentStatus, error) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, secondDeployName, metav1.GetOptions{})
			if err != nil {
				return appsv1.DeploymentStatus{}, err
			}
			return deployment.Status, nil
		}, 8*time.Minute, 10*time.Second).Should(And(
			HaveField("ReadyReplicas", BeNumerically("==", 1)),
			HaveField("Replicas", BeNumerically("==", 1)),
		))
	})

	It("should have VariantAutoscaling resource created", func() {
		By("verifying VariantAutoscaling resources exist")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      firstDeployName,
		}, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", firstDeployName))

		variantAutoscaling = &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      secondDeployName,
		}, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", secondDeployName))
	})

	It("should scale out when load increases", func() {
		By("verifying initial state of VariantAutoscaling")
		initialVA := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      firstDeployName,
		}, initialVA)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", firstDeployName))

		// Set up port-forwarding for Prometheus to enable metrics queries
		By("setting up port-forward to Prometheus service")
		prometheusPortForwardCmd := utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", controllerMonitoringNamespace, 9090, 9090)
		defer func() {
			err = utils.StopCmd(prometheusPortForwardCmd)
			Expect(err).NotTo(HaveOccurred(), "Should be able to stop Prometheus port-forwarding")
		}()

		By("waiting for Prometheus port-forward to be ready")
		err = utils.VerifyPortForwardReadiness(ctx, 9090, fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", 9090))
		Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready within timeout")

		By("getting the gateway service endpoint for load generation")
		port := 8000
		portForwardCmd := utils.SetUpPortForward(k8sClient, ctx, gatewayName, namespace, port, 80)
		defer func() {
			err = utils.StopCmd(portForwardCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for Service: %s", gatewayName))
		}()

		By("waiting for port-forwards to be ready")
		err = utils.VerifyPortForwardReadiness(ctx, port, fmt.Sprintf("http://localhost:%d/v1", port))
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Port-forward should be ready within timeout for Service: %s", gatewayName))

		By("starting load generation to create traffic for both deployments")
		loadRate1 := 30
		loadGenCmd1 := utils.StartLoadGenerator(loadRate1, 100, port, firstModelName)
		defer func() {
			err = utils.StopCmd(loadGenCmd1)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", firstServiceName))
		}()
		loadRate2 := 30
		loadGenCmd2 := utils.StartLoadGenerator(loadRate2, 100, port, secondModelName)
		defer func() {
			err = utils.StopCmd(loadGenCmd2)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", secondServiceName))
		}()

		var desiredReplicas1, desiredReplicas2 float64
		By("waiting for load to be processed and scaling decision to be made")
		Eventually(func(g Gomega) {
			va1 := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      firstDeployName,
			}, va1)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", firstDeployName))

			// Verify that the number of replicas has scaled up
			g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 1),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s", va1.Name))

			// Verify Prometheus replica metrics
			// In single-variant architecture, accelerator is in spec
			_, desiredReplicas1, _, err = utils.GetInfernoReplicaMetrics(va1.Name, namespace, va1.Spec.Accelerator, va1.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to query Prometheus metrics for: %s - got error: %v", va1.Name, err))

			// Verify that the desired number of replicas has same value as Prometheus result
			g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", desiredReplicas1),
				fmt.Sprintf("Current replicas for VA %s should stay at %d with no load", va1.Name, int(desiredReplicas1)))

			va2 := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      secondDeployName,
			}, va2)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", secondDeployName))

			// Verify that the number of replicas has scaled up
			g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 1),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s", va2.Name))

			// Verify Prometheus replica metrics
			// In single-variant architecture, accelerator is in spec
			_, desiredReplicas2, _, err = utils.GetInfernoReplicaMetrics(va2.Name, namespace, va2.Spec.Accelerator, va2.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to query Prometheus metrics for: %s - got error: %v", va2.Name, err))

			// Verify that the desired number of replicas has same value as Prometheus result
			g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", desiredReplicas2),
				fmt.Sprintf("Current replicas for VA %s should stay at %.2f with no load", va2.Name, desiredReplicas2))

		}, 6*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")
		err = utils.LogVariantAutoscalingStatus(ctx, firstDeployName, namespace, crClient)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to log VariantAutoscaling status for: %s", firstDeployName))

		fmt.Printf("Prometheus metrics for VA %s - desired replicas: %d\n",
			firstDeployName,
			int(desiredReplicas1))

		err = utils.LogVariantAutoscalingStatus(ctx, secondDeployName, namespace, crClient)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to log VariantAutoscaling status for: %s", secondDeployName))

		fmt.Printf("Prometheus metrics for VA %s - desired replicas: %d\n",
			secondDeployName,
			int(desiredReplicas2))
	})

	It("should further scale out if load further increases (even over cluster limits)", func() {
		By("verifying initial state of VariantAutoscaling")
		initialVA1 := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      firstDeployName,
		}, initialVA1)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", firstDeployName))

		initialVA2 := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      secondDeployName,
		}, initialVA2)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", secondDeployName))

		initialOptimizedReplicas1 := initialVA1.Status.DesiredOptimizedAlloc.NumReplicas
		initialOptimizedReplicas2 := initialVA2.Status.DesiredOptimizedAlloc.NumReplicas

		// Set up port-forwarding for Prometheus to enable metrics queries
		By("setting up port-forward to Prometheus service")
		prometheusPortForwardCmd := utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", controllerMonitoringNamespace, 9090, 9090)
		defer func() {
			err = utils.StopCmd(prometheusPortForwardCmd)
			Expect(err).NotTo(HaveOccurred(), "Should be able to stop Prometheus port-forwarding")
		}()

		By("waiting for Prometheus port-forward to be ready")
		err = utils.VerifyPortForwardReadiness(ctx, 9090, fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", 9090))
		Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready within timeout")

		By("getting the gateway service endpoint for load generation")
		// Port-forward the gateway service to send requests to it
		By("setting up port-forward to the gateway service")
		port := 8000
		portForwardCmd := utils.SetUpPortForward(k8sClient, ctx, gatewayName, namespace, port, 80)
		defer func() {
			err = utils.StopCmd(portForwardCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for: %s", gatewayName))
		}()

		By("waiting for port-forwards to be ready")
		err = utils.VerifyPortForwardReadiness(ctx, port, fmt.Sprintf("http://localhost:%d/v1", port))
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Port-forward should be ready within timeout for: %s", firstServiceName))

		By("starting load generation to create traffic for both deployments")
		loadRate1 := 60
		loadGenCmd1 := utils.StartLoadGenerator(loadRate1, 100, port, firstModelName)
		defer func() {
			err = utils.StopCmd(loadGenCmd1)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", firstDeployName))
		}()
		loadRate2 := 60
		loadGenCmd2 := utils.StartLoadGenerator(loadRate2, 100, port, secondModelName)
		defer func() {
			err = utils.StopCmd(loadGenCmd2)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", secondDeployName))
		}()

		var desiredReplicas1, desiredReplicas2 float64
		By("waiting for load to be processed and scaling decision to be made")
		Eventually(func(g Gomega) {
			va1 := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      firstDeployName,
			}, va1)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", firstDeployName))

			// Verify that the number of replicas has scaled up
			g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", initialOptimizedReplicas1),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s - actual replicas: %d", firstDeployName, va1.Status.DesiredOptimizedAlloc.NumReplicas))

			// Verify Prometheus replica metrics
			// In single-variant architecture, accelerator is in spec
			_, desiredReplicas1, _, err = utils.GetInfernoReplicaMetrics(va1.Name, namespace, va1.Spec.Accelerator, va1.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to query Prometheus metrics for: %s - got error: %v", va1.Name, err))

			// Verify that the desired number of replicas has same value as Prometheus result
			g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", desiredReplicas1),
				fmt.Sprintf("Current replicas for VA %s should stay at %.2f with no load", va1.Name, desiredReplicas1))

			va2 := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      secondDeployName,
			}, va2)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", secondDeployName))

			// Verify that the number of replicas has scaled up
			g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", initialOptimizedReplicas2),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s - actual replicas: %d", secondDeployName, va2.Status.DesiredOptimizedAlloc.NumReplicas))

			// Verify Prometheus replica metrics
			// In single-variant architecture, accelerator is in spec
			_, desiredReplicas2, _, err = utils.GetInfernoReplicaMetrics(va2.Name, namespace, va2.Spec.Accelerator, va2.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to query Prometheus metrics for: %s - got error: %v", va2.Name, err))

			// Verify that the desired number of replicas has same value as Prometheus result
			g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", desiredReplicas2),
				fmt.Sprintf("Current replicas for VA %s should stay at %.2f with no load", va2.Name, desiredReplicas2))

		}, 6*time.Minute, 10*time.Second).Should(Succeed())

		By("showing the status of VAs and deployments, including the number of pods in pending state")
		err = utils.LogVariantAutoscalingStatus(ctx, firstDeployName, namespace, crClient)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to log VariantAutoscaling status for: %s", firstDeployName))

		fmt.Printf("Prometheus metrics for VA %s - desired replicas: %d\n",
			firstDeployName,
			int(desiredReplicas1))

		err = utils.LogVariantAutoscalingStatus(ctx, secondDeployName, namespace, crClient)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to log VariantAutoscaling status for: %s", secondDeployName))

		fmt.Printf("Prometheus metrics for VA %s - desired replicas: %d\n",
			secondDeployName,
			int(desiredReplicas2))
	})

	It("should scale in with no load", func() {
		// Set up port-forwarding for Prometheus to enable metrics queries
		By("setting up port-forward to Prometheus service")
		prometheusPortForwardCmd := utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", controllerMonitoringNamespace, 9090, 9090)
		defer func() {
			err := utils.StopCmd(prometheusPortForwardCmd)
			Expect(err).NotTo(HaveOccurred(), "Should be able to stop Prometheus port-forwarding")
		}()

		By("waiting for Prometheus port-forward to be ready")
		err := utils.VerifyPortForwardReadiness(ctx, 9090, fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", 9090))
		Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready within timeout")

		var desiredReplicas1, desiredReplicas2 float64
		By("waiting for scaling down decision to be made")
		Eventually(func(g Gomega) {
			va1 := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      firstDeployName,
			}, va1)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", firstDeployName))

			// Verify that the number of replicas has scaled down to 0
			g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", MinimumReplicas),
				fmt.Sprintf("No load should trigger scale-down recommendation to %d for VA: %s - actual replicas: %d", MinimumReplicas, firstDeployName, va1.Status.CurrentAlloc.NumReplicas))

			// Verify Prometheus replica metrics
			// In single-variant architecture, accelerator is in spec
			_, desiredReplicas1, _, err = utils.GetInfernoReplicaMetrics(va1.Name, namespace, va1.Spec.Accelerator, va1.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to query Prometheus metrics for: %s - got error: %v", va1.Name, err))

			// Verify that the desired number of replicas has same value as Prometheus result
			g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", desiredReplicas1),
				fmt.Sprintf("Current replicas for VA %s should stay at %.2f with no load", va1.Name, desiredReplicas1))

			va2 := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      secondDeployName,
			}, va2)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", secondDeployName))

			// Verify that the number of replicas has scaled down to 0
			g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", MinimumReplicas),
				fmt.Sprintf("High load should trigger scale-up recommendation to %d for VA: %s - actual replicas: %d", MinimumReplicas, secondDeployName, va2.Status.CurrentAlloc.NumReplicas))

			// Verify Prometheus replica metrics
			// In single-variant architecture, accelerator is in spec
			_, desiredReplicas2, _, err = utils.GetInfernoReplicaMetrics(va2.Name, namespace, va2.Spec.Accelerator, va2.Spec.VariantID)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to query Prometheus metrics for: %s - got error: %v", va2.Name, err))

			// Verify that the desired number of replicas has same value as Prometheus result
			g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", desiredReplicas2),
				fmt.Sprintf("Current replicas for VA %s should stay at %.2f with no load", va2.Name, desiredReplicas2))

		}, 4*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")

		err = utils.LogVariantAutoscalingStatus(ctx, firstDeployName, namespace, crClient)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to log VariantAutoscaling status for: %s", firstDeployName))

		fmt.Printf("Prometheus metrics for VA %s - desired replicas: %d\n",
			firstDeployName,
			int(desiredReplicas1))

		err = utils.LogVariantAutoscalingStatus(ctx, secondDeployName, namespace, crClient)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to log VariantAutoscaling status for: %s", secondDeployName))

		fmt.Printf("Prometheus metrics for VA %s - desired replicas: %d\n",
			secondDeployName,
			int(desiredReplicas2))
	})

	AfterAll(func() {
		By("deleting resources for first deployment")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      firstDeployName,
				Namespace: namespace,
			},
		}
		err := crClient.Delete(ctx, variantAutoscaling)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete VariantAutoscaling for: %s", firstDeployName))

		serviceMonitor := &unstructured.Unstructured{}
		serviceMonitor.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "monitoring.coreos.com",
			Version: "v1",
			Kind:    "ServiceMonitor",
		})
		serviceMonitor.SetName(firstServiceMonitorName)
		serviceMonitor.SetNamespace(controllerMonitoringNamespace)
		err = crClient.Delete(ctx, serviceMonitor)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete ServiceMonitor: %s", firstServiceMonitorName))

		err = k8sClient.CoreV1().Services(namespace).Delete(ctx, firstServiceName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Service: %s", firstServiceName))

		err = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, firstDeployName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", firstDeployName))

		err = crClient.Delete(ctx, firstInferenceModel)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete InferenceModel: %s", firstModelName))

		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + firstAppLabel})
			if err != nil {
				g.Expect(err).NotTo(HaveOccurred(), "Should be able to list Pods")
			}
			g.Expect(podList.Items).To(BeEmpty(), fmt.Sprintf("All Pods labelled: %s should be deleted", firstAppLabel))
		}, 1*time.Minute, 1*time.Second).Should(Succeed())

		By("deleting resources for second deployment")
		variantAutoscaling = &v1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secondDeployName,
				Namespace: namespace,
			},
		}
		err = crClient.Delete(ctx, variantAutoscaling)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete VariantAutoscaling for: %s", secondDeployName))

		serviceMonitor = &unstructured.Unstructured{}
		serviceMonitor.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "monitoring.coreos.com",
			Version: "v1",
			Kind:    "ServiceMonitor",
		})
		serviceMonitor.SetName(secondServiceMonitorName)
		serviceMonitor.SetNamespace(controllerMonitoringNamespace)
		err = crClient.Delete(ctx, serviceMonitor)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete ServiceMonitor: %s", secondServiceMonitorName))

		err = k8sClient.CoreV1().Services(namespace).Delete(ctx, secondServiceName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Service: %s", secondServiceName))

		err = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, secondDeployName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", secondDeployName))

		err = crClient.Delete(ctx, secondInferenceModel)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete InferenceModel: %s", secondModelName))

		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + secondAppLabel})
			if err != nil {
				g.Expect(err).NotTo(HaveOccurred(), "Should be able to list Pods")
			}
			g.Expect(podList.Items).To(BeEmpty(), fmt.Sprintf("All Pods labelled: %s should be deleted", secondAppLabel))
		}, 1*time.Minute, 1*time.Second).Should(Succeed())
	})
})
