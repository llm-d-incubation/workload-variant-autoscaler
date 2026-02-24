package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	cfg        E2EConfig
	k8sClient  *kubernetes.Clientset
	crClient   client.Client
	restConfig *rest.Config
	ctx        context.Context
	cancel     context.CancelFunc
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Test Suite")
}

var _ = BeforeSuite(func() {
	// Initialize controller-runtime logger to avoid warnings when using log.FromContext
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("Loading configuration from environment")
	cfg = LoadConfigFromEnv()

	GinkgoWriter.Printf("=== E2E Test Configuration ===\n")
	GinkgoWriter.Printf("Environment: %s\n", cfg.Environment)
	GinkgoWriter.Printf("WVA Namespace: %s\n", cfg.WVANamespace)
	GinkgoWriter.Printf("LLMD Namespace: %s\n", cfg.LLMDNamespace)
	GinkgoWriter.Printf("Use Simulator: %v\n", cfg.UseSimulator)
	GinkgoWriter.Printf("Scale-to-Zero Enabled: %v\n", cfg.ScaleToZeroEnabled)
	GinkgoWriter.Printf("Model ID: %s\n", cfg.ModelID)
	GinkgoWriter.Printf("Load Strategy: %s\n", cfg.LoadStrategy)
	GinkgoWriter.Printf("==============================\n\n")

	By("Initializing Kubernetes client")
	var err error
	if _, statErr := os.Stat(cfg.Kubeconfig); statErr == nil {
		restConfig, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
		Expect(err).NotTo(HaveOccurred(), "Failed to load kubeconfig")
	} else {
		GinkgoWriter.Printf("Kubeconfig not found at %s, falling back to in-cluster config\n", cfg.Kubeconfig)
		restConfig, err = rest.InClusterConfig()
		Expect(err).NotTo(HaveOccurred(), "Failed to load in-cluster config (no kubeconfig file and not running in-cluster)")
	}

	k8sClient, err = kubernetes.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Kubernetes clientset")

	s := runtime.NewScheme()
	err = clientgoscheme.AddToScheme(s)
	Expect(err).NotTo(HaveOccurred(), "Failed to add client-go scheme")
	err = variantautoscalingv1alpha1.AddToScheme(s)
	Expect(err).NotTo(HaveOccurred(), "Failed to add VA scheme")
	// Add prometheus-operator scheme for ServiceMonitor support
	err = promoperator.AddToScheme(s)
	Expect(err).NotTo(HaveOccurred(), "Failed to add prometheus-operator scheme")

	crClient, err = client.New(restConfig, client.Options{Scheme: s})
	Expect(err).NotTo(HaveOccurred(), "Failed to create controller-runtime client")

	ctx, cancel = context.WithCancel(context.Background())

	By("Verifying WVA controller is running")
	Eventually(func(g Gomega) {
		pods, err := k8sClient.CoreV1().Pods(cfg.WVANamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "control-plane=controller-manager",
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).NotTo(BeEmpty(), "WVA controller pod not found")

		// Check at least one pod is running
		runningPods := 0
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				runningPods++
			}
		}
		g.Expect(runningPods).To(BeNumerically(">", 0), "No running WVA controller pods")
	}, 2*time.Minute, 5*time.Second).Should(Succeed(), "WVA controller should be running")

	By("Verifying llm-d infrastructure")
	// Verify Gateway CRDs exist
	Eventually(func(g Gomega) {
		_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("inference.networking.k8s.io/v1")
		g.Expect(err).NotTo(HaveOccurred(), "llm-d CRDs should be installed")
	}, 30*time.Second, 5*time.Second).Should(Succeed())

	By("Verifying Prometheus is available")
	Eventually(func(g Gomega) {
		pods, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=prometheus",
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).NotTo(BeEmpty(), "Prometheus pod not found")
	}, 2*time.Minute, 5*time.Second).Should(Succeed(), "Prometheus should be running")

	GinkgoWriter.Println("BeforeSuite completed successfully - infrastructure ready")
})

var _ = AfterSuite(func() {
	By("Cleaning up any leftover test resources")
	if k8sClient != nil && crClient != nil {
		// Clean up any resources with test labels that might have been left behind
		cleanupTestResources(ctx, k8sClient, crClient, cfg.LLMDNamespace)
	}

	if cancel != nil {
		cancel()
	}

	// Optionally delete Kind cluster (opt-in via DELETE_CLUSTER=true)
	// Default: keep cluster for debugging (safer for developers)
	// Also supports INFRA_TEARDOWN_SKIP for backward compatibility
	deleteCluster := os.Getenv("DELETE_CLUSTER") == "true"
	skipTeardown := os.Getenv("INFRA_TEARDOWN_SKIP") == "true"

	// Only delete cluster if explicitly requested and not skipped
	if deleteCluster && !skipTeardown && cfg.Environment == "kind-emulator" {
		By("Deleting Kind cluster")
		clusterName := os.Getenv("CLUSTER_NAME")
		if clusterName == "" {
			clusterName = "kind-wva-gpu-cluster"
		}
		GinkgoWriter.Printf("Deleting Kind cluster: %s\n", clusterName)

		// Use the teardown script if it exists, otherwise use kind directly
		teardownScript := filepath.Join("deploy", "kind-emulator", "teardown.sh")
		if _, err := os.Stat(teardownScript); err == nil {
			cmd := exec.Command("bash", teardownScript)
			cmd.Env = append(os.Environ(), "KIND_NAME="+clusterName)
			output, err := cmd.CombinedOutput()
			if err != nil {
				GinkgoWriter.Printf("Warning: Failed to delete cluster via teardown script: %v\nOutput: %s\n", err, string(output))
				// Fallback to direct kind delete
				deleteKindClusterDirectly(clusterName)
			} else {
				GinkgoWriter.Printf("Cluster deleted successfully\n")
			}
		} else {
			// Fallback to direct kind delete if script doesn't exist
			deleteKindClusterDirectly(clusterName)
		}
	} else if deleteCluster && skipTeardown {
		GinkgoWriter.Printf("Skipping cluster deletion (INFRA_TEARDOWN_SKIP=true overrides DELETE_CLUSTER=true)\n")
	} else if cfg.Environment == "kind-emulator" {
		GinkgoWriter.Printf("Keeping Kind cluster for debugging (set DELETE_CLUSTER=true to delete)\n")
	}
})

// deleteKindClusterDirectly deletes the Kind cluster using kind command directly
func deleteKindClusterDirectly(clusterName string) {
	cmd := exec.Command("kind", "delete", "cluster", "--name", clusterName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		GinkgoWriter.Printf("Warning: Failed to delete cluster '%s': %v\nOutput: %s\n", clusterName, err, string(output))
	} else {
		GinkgoWriter.Printf("Cluster '%s' deleted successfully\n", clusterName)
	}
}

// cleanupTestResources removes any test resources that might have leaked
func cleanupTestResources(ctx context.Context, k8sClient *kubernetes.Clientset, crClient client.Client, namespace string) {
	GinkgoWriter.Println("Cleaning up test resources...")

	// Helper function to check if resource name matches test patterns
	isTestResource := func(name string) bool {
		return strings.HasPrefix(name, "test-") || strings.HasPrefix(name, "smoke-") || strings.HasPrefix(name, "saturation-") || strings.HasPrefix(name, "error-test-") || strings.HasPrefix(name, "target-condition-")
	}

	// List and delete test VAs
	vaList := &variantautoscalingv1alpha1.VariantAutoscalingList{}
	if err := crClient.List(ctx, vaList, client.InNamespace(namespace)); err == nil {
		for _, va := range vaList.Items {
			if isTestResource(va.Name) {
				GinkgoWriter.Printf("Cleaning up leftover VA: %s\n", va.Name)
				deleteResourceWithVerification(ctx, func() error {
					return crClient.Delete(ctx, &va)
				}, func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: va.Name, Namespace: namespace}, &va)
					return errors.IsNotFound(err)
				}, "VA", va.Name)
			}
		}
	}

	// Clean up test HPAs
	hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, hpa := range hpaList.Items {
			if isTestResource(hpa.Name) {
				GinkgoWriter.Printf("Cleaning up leftover HPA: %s\n", hpa.Name)
				deleteResourceWithVerification(ctx, func() error {
					return k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, hpa.Name, metav1.DeleteOptions{})
				}, func() bool {
					_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, hpa.Name, metav1.GetOptions{})
					return errors.IsNotFound(err)
				}, "HPA", hpa.Name)
			}
		}
	}

	// Clean up test deployments
	deployList, err := k8sClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, deploy := range deployList.Items {
			if isTestResource(deploy.Name) {
				GinkgoWriter.Printf("Cleaning up leftover Deployment: %s\n", deploy.Name)
				deleteResourceWithVerification(ctx, func() error {
					return k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploy.Name, metav1.DeleteOptions{})
				}, func() bool {
					_, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deploy.Name, metav1.GetOptions{})
					return errors.IsNotFound(err)
				}, "Deployment", deploy.Name)
			}
		}
	}

	// Clean up test jobs
	jobList, err := k8sClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, job := range jobList.Items {
			if isTestResource(job.Name) {
				GinkgoWriter.Printf("Cleaning up leftover Job: %s\n", job.Name)
				propagation := metav1.DeletePropagationBackground
				deleteResourceWithVerification(ctx, func() error {
					return k8sClient.BatchV1().Jobs(namespace).Delete(ctx, job.Name, metav1.DeleteOptions{
						PropagationPolicy: &propagation,
					})
				}, func() bool {
					_, err := k8sClient.BatchV1().Jobs(namespace).Get(ctx, job.Name, metav1.GetOptions{})
					return errors.IsNotFound(err)
				}, "Job", job.Name)
			}
		}
	}

	// Clean up test services
	svcList, err := k8sClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, svc := range svcList.Items {
			if isTestResource(svc.Name) {
				GinkgoWriter.Printf("Cleaning up leftover Service: %s\n", svc.Name)
				deleteResourceWithVerification(ctx, func() error {
					return k8sClient.CoreV1().Services(namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
				}, func() bool {
					_, err := k8sClient.CoreV1().Services(namespace).Get(ctx, svc.Name, metav1.GetOptions{})
					return errors.IsNotFound(err)
				}, "Service", svc.Name)
			}
		}
	}

	// Clean up test ServiceMonitors in monitoring namespace
	monitoringNS := cfg.MonitoringNS
	if monitoringNS != "" {
		smList := &promoperator.ServiceMonitorList{}
		if err := crClient.List(ctx, smList, client.InNamespace(monitoringNS)); err == nil {
			for _, sm := range smList.Items {
				smName := sm.Name
				if isTestResource(smName) {
					GinkgoWriter.Printf("Cleaning up leftover ServiceMonitor: %s\n", smName)
					deleteResourceWithVerification(ctx, func() error {
						return crClient.Delete(ctx, &promoperator.ServiceMonitor{
							ObjectMeta: metav1.ObjectMeta{
								Name:      smName,
								Namespace: monitoringNS,
							},
						})
					}, func() bool {
						err := crClient.Get(ctx, client.ObjectKey{Name: smName, Namespace: monitoringNS}, &promoperator.ServiceMonitor{})
						return errors.IsNotFound(err)
					}, "ServiceMonitor", smName)
				}
			}
		}
	}
}

// deleteResourceWithVerification deletes a resource and verifies it's actually deleted
// deleteFunc: function that performs the deletion
// verifyFunc: function that returns true when resource is confirmed deleted
// resourceType: human-readable resource type for logging
// resourceName: name of the resource for logging
func deleteResourceWithVerification(ctx context.Context, deleteFunc func() error, verifyFunc func() bool, resourceType, resourceName string) {
	// Attempt deletion
	if err := deleteFunc(); err != nil {
		if !errors.IsNotFound(err) {
			GinkgoWriter.Printf("Warning: Failed to delete %s %s: %v\n", resourceType, resourceName, err)
		}
		return
	}

	// Verify deletion with timeout
	timeout := 2 * time.Minute
	interval := 5 * time.Second
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if verifyFunc() {
			GinkgoWriter.Printf("Successfully deleted %s %s\n", resourceType, resourceName)
			return
		}
		time.Sleep(interval)
	}

	GinkgoWriter.Printf("Warning: %s %s may not have been fully deleted after %v\n", resourceType, resourceName, timeout)
}

// cleanupResource deletes a resource and waits for deletion to complete
// This is a convenience wrapper for common cleanup patterns
func cleanupResource(ctx context.Context, resourceType, namespace, name string, deleteFunc func() error, verifyFunc func() bool) {
	deleteResourceWithVerification(ctx, deleteFunc, verifyFunc, resourceType, name)
}
