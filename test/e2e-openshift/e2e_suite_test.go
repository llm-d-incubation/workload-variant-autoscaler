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

package e2eopenshift

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
)

var (
	controllerNamespace = getEnvString("CONTROLLER_NAMESPACE", "workload-variant-autoscaler-system")
	monitoringNamespace = getEnvString("MONITORING_NAMESPACE", "openshift-user-workload-monitoring")
	llmDNamespace       = getEnvString("LLMD_NAMESPACE", "llm-d-inference-scheduling")
	gatewayName         = getEnvString("GATEWAY_NAME", "infra-inference-scheduling-inference-gateway")
	modelID             = getEnvString("MODEL_ID", "unsloth/Meta-Llama-3.1-8B")
	deployment          = getEnvString("DEPLOYMENT", "ms-inference-scheduling-llm-d-modelservice-decode")
	requestRate         = getEnvInt("REQUEST_RATE", 20)
	numPrompts          = getEnvInt("NUM_PROMPTS", 3000)
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

func getEnvString(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "Failed to parse env variable, using fallback")
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to parse env variable as int, using fallback")
		return fallback
	}
	return fallback
}

// TestE2EOpenShift runs the end-to-end (e2e) test suite for OpenShift deployments.
// These tests assume that the infrastructure (WVA, llm-d, Prometheus, etc.) is already deployed.
func TestE2EOpenShift(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting workload-variant-autoscaler OpenShift integration test suite\n")
	RunSpecs(t, "e2e-openshift suite")
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

var _ = BeforeSuite(func() {
	if os.Getenv("KUBECONFIG") == "" {
		Skip("KUBECONFIG is not set; skipping OpenShift e2e test")
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=\n")
	_, _ = fmt.Fprintf(GinkgoWriter, "Using the following configuration:\n")
	_, _ = fmt.Fprintf(GinkgoWriter, "-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=\n")
	_, _ = fmt.Fprintf(GinkgoWriter, "CONTROLLER_NAMESPACE=%s\n", controllerNamespace)
	_, _ = fmt.Fprintf(GinkgoWriter, "MONITORING_NAMESPACE=%s\n", monitoringNamespace)
	_, _ = fmt.Fprintf(GinkgoWriter, "LLMD_NAMESPACE=%s\n", llmDNamespace)
	_, _ = fmt.Fprintf(GinkgoWriter, "GATEWAY_NAME=%s\n", gatewayName)
	_, _ = fmt.Fprintf(GinkgoWriter, "MODEL_ID=%s\n", modelID)
	_, _ = fmt.Fprintf(GinkgoWriter, "DEPLOYMENT=%s\n", deployment)
	_, _ = fmt.Fprintf(GinkgoWriter, "REQUEST_RATE=%d\n", requestRate)
	_, _ = fmt.Fprintf(GinkgoWriter, "NUM_PROMPTS=%d\n", numPrompts)

	initializeK8sClient()

	ctx := context.Background()

	By("verifying that the controller-manager pods are running")
	Eventually(func(g Gomega) {
		podList, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
		})
		if err != nil {
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list manager pods")
		}
		g.Expect(podList.Items).NotTo(BeEmpty(), "Pod list should not be empty")
		for _, pod := range podList.Items {
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), fmt.Sprintf("Pod %s is not running", pod.Name))
		}
	}, 2*time.Minute, 1*time.Second).Should(Succeed())

	By("verifying that llm-d infrastructure is running")
	Eventually(func(g Gomega) {
		// Check Gateway
		deploymentList, err := k8sClient.AppsV1().Deployments(llmDNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list deployments in llm-d namespace")
		}
		g.Expect(deploymentList.Items).NotTo(BeEmpty(), "llm-d deployments should exist")

		// Check that vLLM deployment exists
		vllmDeployment, err := k8sClient.AppsV1().Deployments(llmDNamespace).Get(ctx, deployment, metav1.GetOptions{})
		if err != nil {
			g.Expect(err).NotTo(HaveOccurred(), "vLLM deployment should exist")
		}
		g.Expect(vllmDeployment.Status.ReadyReplicas).To(BeNumerically(">", 0), "At least one vLLM replica should be ready")
	}, 5*time.Minute, 5*time.Second).Should(Succeed())

	By("verifying that Prometheus Adapter is running")
	Eventually(func(g Gomega) {
		podList, err := k8sClient.CoreV1().Pods(monitoringNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=prometheus-adapter",
		})
		if err != nil {
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list Prometheus Adapter pods")
		}
		g.Expect(podList.Items).NotTo(BeEmpty(), "Prometheus Adapter pod should exist")
		for _, pod := range podList.Items {
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), fmt.Sprintf("Prometheus Adapter pod %s should be running", pod.Name))
		}
	}, 2*time.Minute, 1*time.Second).Should(Succeed())

	_, _ = fmt.Fprintf(GinkgoWriter, "Infrastructure verification complete\n")
})

var _ = AfterSuite(func() {
	_, _ = fmt.Fprintf(GinkgoWriter, "OpenShift e2e test suite completed\n")
})
