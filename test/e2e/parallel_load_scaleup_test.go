package e2e

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// sanitizeK8sName converts a human-readable name to a valid Kubernetes resource name.
// Kubernetes names must be lowercase alphanumeric, may contain '-' and '.', and
// must start and end with an alphanumeric character.
func sanitizeK8sName(name string) string {
	// Convert to lowercase and replace spaces with hyphens
	result := strings.ToLower(name)
	result = strings.ReplaceAll(result, " ", "-")
	// Remove any characters that aren't lowercase alphanumeric or hyphens
	re := regexp.MustCompile(`[^a-z0-9-]`)
	result = re.ReplaceAllString(result, "")
	// Trim leading/trailing hyphens
	result = strings.Trim(result, "-")
	return result
}

// Load generation configuration constants
// These values were tuned empirically to achieve ~2-3 replica scale-up without excessive scaling.
// Original values (baseLoadWorkers=10, batchSize=50, batchSleepDuration=0.1) caused cascade
// scaling to 8+ replicas because WVA reconciles frequently (around every 30s with the tested
// configuration) while pods take 5-7 min to become ready.
//
// NOTE: These values are environment-specific and may need tuning for different hardware
// configurations, model sizes, or inference implementations. The current values target
// sustainable load that triggers scale-up but caps at ~3 replicas under the tested
// reconciliation interval and pod readiness characteristics.
const (
	baseLoadWorkers         = 2     // Reduced from 10 to limit concurrent load (targets max ~3 replicas)
	baseReplicas            = 2     // The replica count baseLoadWorkers is tuned for
	maxSingleReplicaWorkers = 1     // Max workers when deployment has only 1 replica (prevents queue explosion)
	batchSize               = 10    // Reduced from 50 to limit concurrent requests per batch
	curlTimeoutSeconds      = 180   // Timeout for each curl request (increased for longer outputs)
	batchSleepDuration      = "0.5" // Increased from 0.1 to slow request rate between batches
	maxTokensPerRequest     = 400   // Moderate tokens for ~3s requests, sustains queue during test
	requestsPerWorker       = 1100  // Number of requests each worker sends
)

var lowLoad = cfg.NumPrompts <= 2000 && cfg.RequestRate <= 8

// ParallelLoadScaleUpTest validates scale-up behavior under sustained parallel load.
// This test uses multiple parallel Kubernetes Jobs to generate load, simulating realistic
// traffic patterns. It verifies the complete scale-up flow:
// 1. VA detects load and recommends scale-up
// 2. HPA reads the metric and updates desired replicas
// 3. Deployment scales up to match recommendations
// 4. Deployment maintains scaled state under load
var _ = Describe("Parallel Load Scale-Up Test", Label("full"), Ordered, func() {
	var (
		poolName          = "parallel-load-pool"
		modelServiceName  = "parallel-load-ms"
		vaName            = "parallel-load-va"
		hpaName           = "parallel-load-hpa"
		deploymentName    string
		serviceName       string
		jobBaseName       string
		initialReplicas   int32
		initialOptimized  int32
		hpaMinReplicas    int32
		scaledReplicas    int32
		scaledOptimized   int32
		scaledLoadWorkers int // Load workers scaled to initial replicas
	)

	BeforeAll(func() {
		deploymentName = modelServiceName + "-decode"
		serviceName = modelServiceName + "-service"
		jobBaseName = sanitizeK8sName(modelServiceName)

		By("Creating model service deployment")
		err := fixtures.CreateModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		// Register cleanup for deployment
		DeferCleanup(func() {
			cleanupResource(ctx, "Deployment", cfg.LLMDNamespace, deploymentName,
				func() error {
					return k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
				},
				func() bool {
					_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
		})

		By("Creating service to expose model server")
		err = fixtures.CreateService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, deploymentName, 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		// Register cleanup for service
		DeferCleanup(func() {
			cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceName,
				func() error {
					return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
				},
				func() bool {
					_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
		})

		By("Creating ServiceMonitor for metrics scraping")
		err = fixtures.CreateServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, deploymentName)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor")

		// Register cleanup for ServiceMonitor
		DeferCleanup(func() {
			serviceMonitorName := modelServiceName + "-monitor"
			cleanupResource(ctx, "ServiceMonitor", cfg.MonitoringNS, serviceMonitorName,
				func() error {
					return crClient.Delete(ctx, &promoperator.ServiceMonitor{
						ObjectMeta: metav1.ObjectMeta{
							Name:      serviceMonitorName,
							Namespace: cfg.MonitoringNS,
						},
					})
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: serviceMonitorName, Namespace: cfg.MonitoringNS}, &promoperator.ServiceMonitor{})
					return errors.IsNotFound(err)
				})
		})

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")
			g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1), "Deployment should have at least 1 ready replica")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("Creating VariantAutoscaling resource")
		err = fixtures.CreateVariantAutoscalingWithDefaults(
			ctx, crClient, cfg.LLMDNamespace, vaName,
			deploymentName, cfg.ModelID, cfg.AcceleratorType,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

		// Register cleanup for VA
		DeferCleanup(func() {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				},
			}
			cleanupResource(ctx, "VariantAutoscaling", cfg.LLMDNamespace, vaName,
				func() error {
					return crClient.Delete(ctx, va)
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
					return errors.IsNotFound(err)
				})
		})

		By("Creating HPA for the deployment")
		minReplicas := int32(1)
		if cfg.ScaleToZeroEnabled {
			minReplicas = 0
		}
		hpaMinReplicas = minReplicas
		err = fixtures.CreateHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName, deploymentName, vaName, minReplicas, 10)
		Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")

		// Register cleanup for HPA
		DeferCleanup(func() {
			hpaNameFull := hpaName + "-hpa"
			cleanupResource(ctx, "HPA", cfg.LLMDNamespace, hpaNameFull,
				func() error {
					return k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaNameFull, metav1.DeleteOptions{})
				},
				func() bool {
					_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaNameFull, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
		})

		By("Recording initial state of deployment")
		deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")
		initialReplicas = deploy.Status.ReadyReplicas
		if initialReplicas == 0 {
			initialReplicas = 1 // Avoid division issues, treat 0 as 1
		}

		// Scale load workers proportionally to initial replicas
		// Formula: scaledWorkers = baseLoadWorkers * initialReplicas / baseReplicas
		// This ensures consistent load pressure per replica
		scaledLoadWorkers = int(math.Round(float64(baseLoadWorkers*initialReplicas) / float64(baseReplicas)))
		if scaledLoadWorkers < 1 {
			scaledLoadWorkers = 1 // Minimum 1 worker
		}
		// Cap workers for single-replica deployments to avoid cold-start overwhelm
		if initialReplicas == 1 && scaledLoadWorkers > maxSingleReplicaWorkers {
			scaledLoadWorkers = maxSingleReplicaWorkers
		}
		GinkgoWriter.Printf("Initial ready replicas: %d\n", initialReplicas)
		GinkgoWriter.Printf("Scaled load workers: %d (base: %d for %d replicas)\n", scaledLoadWorkers, baseLoadWorkers, baseReplicas)

		// Wait for VA to stabilize at minReplicas before recording initial state
		// This ensures we're measuring scale-up from load, not residual scale from prior activity
		By("Waiting for VA to stabilize at minReplicas")
		Eventually(func(g Gomega) {
			currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace,
				Name:      vaName,
			}, currentVA)
			g.Expect(err).NotTo(HaveOccurred())
			optimized := int32(currentVA.Status.DesiredOptimizedAlloc.NumReplicas)
			GinkgoWriter.Printf("Waiting for VA to be ready: optimized=%d, minReplicas=%d\n", optimized, hpaMinReplicas)
			// Wait for optimized >= minReplicas (allows for initial 0 during engine startup)
			g.Expect(optimized).To(BeNumerically(">=", hpaMinReplicas), "VA should have optimized >= minReplicas")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		// Wait for deployment to be fully stable (no pods in transition)
		By("Waiting for deployment to stabilize (no pods in transition)")
		Eventually(func(g Gomega) {
			currentDeploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			specReplicas := *currentDeploy.Spec.Replicas
			statusReplicas := currentDeploy.Status.Replicas
			readyReplicas := currentDeploy.Status.ReadyReplicas
			GinkgoWriter.Printf("Waiting for deployment stability: spec=%d, status=%d, ready=%d\n",
				specReplicas, statusReplicas, readyReplicas)
			// All replica counts must match - no pods starting or terminating
			g.Expect(statusReplicas).To(Equal(specReplicas), "Status replicas should match spec")
			g.Expect(readyReplicas).To(Equal(specReplicas), "Ready replicas should match spec")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		// Re-read VA to get stabilized state
		va := &variantautoscalingv1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: cfg.LLMDNamespace,
			Name:      vaName,
		}, va)
		Expect(err).NotTo(HaveOccurred())
		initialOptimized = int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
		GinkgoWriter.Printf("Initial optimized replicas (after stabilization): %d\n", initialOptimized)
	})

	It("should verify external metrics API is accessible", func() {
		By("Querying external metrics API for wva_desired_replicas")
		Eventually(func(g Gomega) {
			result, err := k8sClient.RESTClient().
				Get().
				AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + cfg.LLMDNamespace + "/" + constants.WVADesiredReplicas).
				DoRaw(ctx)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query external metrics API")
			g.Expect(string(result)).To(ContainSubstring(constants.WVADesiredReplicas), "Metric should be available")
			g.Expect(string(result)).To(ContainSubstring(vaName), "Metric should be for the correct variant")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("should create and run parallel load generation jobs", func() {
		By("Cleaning up any existing jobs")
		fixtures.DeleteParallelLoadJobs(ctx, k8sClient, jobBaseName, cfg.LLMDNamespace, scaledLoadWorkers)
		time.Sleep(2 * time.Second)

		By("Waiting for service endpoints to exist")
		targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8000/v1/completions", serviceName, cfg.LLMDNamespace)
		Eventually(func(g Gomega) {
			endpoints, err := k8sClient.CoreV1().Endpoints(cfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Service endpoints should exist")
			g.Expect(endpoints.Subsets).NotTo(BeEmpty(), "Service should have endpoints")

			readyCount := 0
			for _, subset := range endpoints.Subsets {
				readyCount += len(subset.Addresses)
			}
			GinkgoWriter.Printf("Service %s has %d ready endpoints\n", serviceName, readyCount)
			g.Expect(readyCount).To(BeNumerically(">", 0), "Service should have at least one ready endpoint")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By(fmt.Sprintf("Creating %d parallel load generation jobs", scaledLoadWorkers))
		loadCfg := fixtures.LoadConfig{
			Strategy:     cfg.LoadStrategy,
			RequestRate:  0, // Not used for parallel load pattern
			NumPrompts:   requestsPerWorker,
			InputTokens:  cfg.InputTokens,
			OutputTokens: maxTokensPerRequest,
			ModelID:      cfg.ModelID,
		}
		err := fixtures.CreateParallelLoadJobs(ctx, k8sClient, jobBaseName, cfg.LLMDNamespace, targetURL, scaledLoadWorkers, loadCfg)
		Expect(err).NotTo(HaveOccurred(), "Should be able to create load generation jobs")

		// Register cleanup for load jobs
		DeferCleanup(func() {
			fixtures.DeleteParallelLoadJobs(ctx, k8sClient, jobBaseName, cfg.LLMDNamespace, scaledLoadWorkers)
		})

		By("Waiting for job pods to be running")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("experiment=%s", jobBaseName),
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list job pods")
			g.Expect(len(podList.Items)).To(BeNumerically(">=", scaledLoadWorkers), "All job pods should exist")

			runningCount := 0
			for _, pod := range podList.Items {
				if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodSucceeded {
					runningCount++
				}
			}
			g.Expect(runningCount).To(BeNumerically(">=", scaledLoadWorkers),
				fmt.Sprintf("At least %d job pods should be running, got %d", scaledLoadWorkers, runningCount))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		GinkgoWriter.Printf("All %d load generation jobs are running\n", scaledLoadWorkers)
	})

	It("should detect increased load and trigger scale-up", func() {
		By("Waiting for load generation to ramp up (30 seconds)")
		time.Sleep(30 * time.Second)

		By("Monitoring VariantAutoscaling for scale-up")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace,
				Name:      vaName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			scaledOptimized = int32(va.Status.DesiredOptimizedAlloc.NumReplicas)

			GinkgoWriter.Printf("VA optimized replicas: %d (initial: %d, minReplicas: %d)\n",
				scaledOptimized, initialOptimized, hpaMinReplicas)

			if !lowLoad {
				// Scale-up means we should have MORE replicas than our initial stabilized state
				g.Expect(scaledOptimized).To(BeNumerically(">", initialOptimized),
					fmt.Sprintf("WVA should recommend more replicas than initial under load (current: %d, initial: %d)", scaledOptimized, initialOptimized))
			} else {
				GinkgoWriter.Printf("Low load detected, skipping scale-up recommendation check\n")
			}
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("Monitoring HPA for scale-up")
		Eventually(func(g Gomega) {
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get HPA")

			GinkgoWriter.Printf("HPA desiredReplicas: %d, currentReplicas: %d\n",
				hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas)

			if !lowLoad {
				// HPA should also desire more replicas than initial
				g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">", initialOptimized),
					fmt.Sprintf("HPA should desire more replicas than initial (desired: %d, initial: %d)", hpa.Status.DesiredReplicas, initialOptimized))
			}
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		GinkgoWriter.Printf("WVA detected load and recommended %d replicas (up from %d)\n", scaledOptimized, initialOptimized)
	})

	It("should scale deployment to match recommended replicas", func() {
		By("Monitoring deployment for actual scale-up")
		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")

			scaledReplicas = deploy.Status.ReadyReplicas
			GinkgoWriter.Printf("Current ready replicas: %d (initial: %d, desired: %d)\n",
				scaledReplicas, initialReplicas, scaledOptimized)

			if !lowLoad {
				g.Expect(deploy.Status.Replicas).To(BeNumerically(">", hpaMinReplicas),
					fmt.Sprintf("Deployment should have more total replicas than minReplicas under high load (current: %d, min: %d)", deploy.Status.Replicas, hpaMinReplicas))
				g.Expect(scaledReplicas).To(BeNumerically(">=", scaledOptimized),
					fmt.Sprintf("Deployment should have at least %d ready replicas to match optimizer recommendation", scaledOptimized))
			} else {
				GinkgoWriter.Printf("Low load detected, skipping scale-up check\n")
			}
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		GinkgoWriter.Printf("Deployment scaled to %d replicas (up from %d, target was %d)\n", scaledReplicas, initialReplicas, scaledOptimized)
	})

	It("should maintain scaled state while load is active", func() {
		By("Verifying deployment stays scaled for at least 30 seconds")
		Consistently(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")
			g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", scaledOptimized),
				fmt.Sprintf("Deployment should maintain at least %d replicas while job is running", scaledOptimized))
		}, 30*time.Second, 5*time.Second).Should(Succeed())

		GinkgoWriter.Printf("Deployment maintained %d replicas under load (target: %d)\n", scaledReplicas, scaledOptimized)
	})
})
