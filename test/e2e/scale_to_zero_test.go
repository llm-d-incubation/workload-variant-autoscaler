package e2e

import (
	"fmt"
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
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Scale-to-zero test validates that deployments scale down to zero replicas when idle
// and scale-to-zero is enabled via ConfigMap.
//
// Note: This test is marked as "flaky" due to HPA stabilization window timing issues.
// The HPA may not scale down immediately after load stops due to its stabilization logic.
var _ = Describe("Scale-To-Zero Feature", Label("full", "flaky"), Ordered, func() {
	var (
		poolName         = "scale-to-zero-pool"
		modelServiceName = "scale-to-zero-ms"
		vaName           = "scale-to-zero-va"
		hpaName          = "scale-to-zero-hpa"
	)

	BeforeAll(func() {
		// Skip if HPAScaleToZero is not enabled
		if !cfg.ScaleToZeroEnabled {
			Skip("HPAScaleToZero feature gate is not enabled; skipping scale-to-zero test")
		}

		// Note: InferencePool should already exist from infra-only deployment
		// We no longer create InferencePools in individual tests

		serviceName := modelServiceName + "-service"
		deploymentName := modelServiceName + "-decode"

		By("Creating model service deployment")
		err := fixtures.CreateModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		// Register cleanup for deployment (runs even if test fails)
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
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Model service should have 1 ready replica")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

		By("Creating VariantAutoscaling resource")
		err = fixtures.CreateVariantAutoscaling(
			ctx, crClient, cfg.LLMDNamespace, vaName,
			deploymentName, cfg.ModelID, cfg.AcceleratorType, 30.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

		By("Creating HPA with minReplicas=0 for scale-to-zero")
		err = fixtures.CreateHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName, deploymentName, vaName, 0, 10)
		Expect(err).NotTo(HaveOccurred(), "Failed to create HPA with scale-to-zero")

		GinkgoWriter.Println("Scale-to-zero test setup complete")
	})

	AfterAll(func() {
		By("Cleaning up scale-to-zero test resources")

		// Delete in reverse dependency order: HPA -> VA
		// Service and Deployment cleanup handled by DeferCleanup
		hpaNameFull := hpaName + "-hpa"

		// Delete HPA
		cleanupResource(ctx, "HPA", cfg.LLMDNamespace, hpaNameFull,
			func() error {
				return k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaNameFull, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaNameFull, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})

		// Delete VA
		va := &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			},
		}
		cleanupResource(ctx, "VA", cfg.LLMDNamespace, vaName,
			func() error {
				return crClient.Delete(ctx, va)
			},
			func() bool {
				err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
				return errors.IsNotFound(err)
			})
	})

	Context("Initial state and scale-up", func() {
		It("should have HPA configured with minReplicas=0", func() {
			By("Verifying HPA allows scale-to-zero")
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			Expect(hpa.Spec.MinReplicas).NotTo(BeNil())
			Expect(*hpa.Spec.MinReplicas).To(Equal(int32(0)), "HPA should have minReplicas=0")

			GinkgoWriter.Println("HPA verified with minReplicas=0 for scale-to-zero")
		})

		It("should scale up under load", func() {
			By("Starting load generation")
			loadCfg := fixtures.LoadConfig{
				Strategy:     cfg.LoadStrategy,
				RequestRate:  cfg.RequestRate,
				NumPrompts:   500, // Shorter load for scale-to-zero test
				InputTokens:  cfg.InputTokens,
				OutputTokens: cfg.OutputTokens,
				ModelID:      cfg.ModelID,
			}

			targetURL := fmt.Sprintf("http://%s-service:8000", modelServiceName)
			err := fixtures.CreateLoadJob(ctx, k8sClient, cfg.LLMDNamespace, "stz-load", targetURL, loadCfg)
			Expect(err).NotTo(HaveOccurred(), "Failed to create load job")

			jobName := "stz-load-load"

			// Register cleanup for load job (runs even if test fails)
			DeferCleanup(func() {
				cleanupResource(ctx, "Job", cfg.LLMDNamespace, jobName,
					func() error {
						propagation := metav1.DeletePropagationBackground
						return k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: &propagation})
					},
					func() bool {
						_, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			})

			By("Waiting for scale-up")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deployment.Status.Replicas).To(BeNumerically(">", 1),
					"Deployment should scale up under load")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			GinkgoWriter.Println("Deployment scaled up under load")

			// Wait for load job to complete
			By("Waiting for load job to complete")
			Eventually(func(g Gomega) {
				job, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(job.Status.Succeeded+job.Status.Failed).To(BeNumerically(">", 0),
					"Load job should complete")
			}, 10*time.Minute, 15*time.Second).Should(Succeed())

			GinkgoWriter.Println("Load job completed")
		})
	})

	Context("Scale-to-zero after idle period", func() {
		It("should scale VA desired replicas to zero when no requests", func() {
			By("Waiting for idle period to trigger scale-to-zero")
			// Note: This depends on scale-to-zero ConfigMap retention period
			// Default retention is typically 2-5 minutes
			time.Sleep(3 * time.Minute)

			By("Monitoring VA for scale-to-zero recommendation")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Namespace: cfg.LLMDNamespace,
					Name:      vaName,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				desiredReplicas := va.Status.DesiredOptimizedAlloc.NumReplicas

				GinkgoWriter.Printf("VA desired replicas: %d (waiting for 0)\n", desiredReplicas)

				// VA should recommend scaling to zero after idle period
				g.Expect(desiredReplicas).To(Equal(0),
					"VA should recommend scaling to zero after idle period")

			}, 10*time.Minute, 15*time.Second).Should(Succeed())

			GinkgoWriter.Println("VA recommended scaling to zero")
		})

		It("should scale actual deployment replicas to zero", func() {
			By("Monitoring deployment for scale-down to zero")
			// Note: HPA stabilization window may delay this
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				currentReplicas := deployment.Status.Replicas
				GinkgoWriter.Printf("Current replicas: %d (waiting for 0)\n", currentReplicas)

				// Deployment should eventually scale to zero
				g.Expect(currentReplicas).To(Equal(int32(0)),
					"Deployment should scale to zero after idle period")

			}, 15*time.Minute, 20*time.Second).Should(Succeed())

			GinkgoWriter.Println("Deployment successfully scaled to zero")
		})

		It("should have zero running pods", func() {
			By("Verifying no pods are running")
			podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app=%s-decode", modelServiceName),
			})
			Expect(err).NotTo(HaveOccurred())

			runningPods := 0
			for _, pod := range podList.Items {
				if pod.Status.Phase == "Running" {
					runningPods++
				}
			}

			Expect(runningPods).To(Equal(0), "No pods should be running after scale-to-zero")
			GinkgoWriter.Println("Verified: No pods running after scale-to-zero")
		})
	})
})

// Scale-to-zero disabled test validates that deployments maintain minimum replicas
// when scale-to-zero is disabled via ConfigMap, even during idle periods.
var _ = Describe("Scale-To-Zero Feature - Disabled", Label("full"), Ordered, func() {
	var (
		poolName          = "scale-to-zero-disabled-pool"
		modelServiceName  = "scale-to-zero-disabled-ms"
		vaName            = "scale-to-zero-disabled-va"
		hpaName           = "scale-to-zero-disabled-hpa"
		configMapName     = "wva-model-scale-to-zero-config" // Use config package constant
		originalConfigMap *corev1.ConfigMap
	)

	BeforeAll(func() {
		By("Saving original ConfigMap state")
		cm, err := k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Get(ctx, configMapName, metav1.GetOptions{})
		if err == nil {
			// Save original for restoration
			originalConfigMap = cm.DeepCopy()
		}

		By("Creating ConfigMap with scale-to-zero disabled")
		// Sanitize model ID for ConfigMap key (replace '/' with '-' as ConfigMap keys must be alphanumeric, '-', '_', or '.')
		sanitizedModelID := strings.ReplaceAll(cfg.ModelID, "/", "-")
		scaleToZeroCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configMapName,
				Namespace: cfg.WVANamespace,
			},
			Data: map[string]string{
				"default": `enable_scale_to_zero: false`,
				sanitizedModelID: fmt.Sprintf(`model_id: %s
enable_scale_to_zero: false`, cfg.ModelID),
			},
		}

		// Delete existing ConfigMap if it exists
		_ = k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Delete(ctx, configMapName, metav1.DeleteOptions{})
		time.Sleep(1 * time.Second) // Brief pause for deletion to propagate

		_, err = k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Create(ctx, scaleToZeroCM, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should be able to create scale-to-zero ConfigMap with disabled setting")

		By("Creating model service deployment")
		err = fixtures.CreateModelService(ctx, k8sClient, cfg.LLMDNamespace,
			modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		By("Creating service to expose model server")
		err = fixtures.CreateService(ctx, k8sClient, cfg.LLMDNamespace,
			modelServiceName, modelServiceName+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		By("Creating ServiceMonitor for metrics scraping")
		err = fixtures.CreateServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, modelServiceName+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor")

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx,
				modelServiceName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)),
				"Model service should have 1 ready replica")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

		By("Creating VariantAutoscaling resource")
		err = fixtures.CreateVariantAutoscaling(
			ctx, crClient, cfg.LLMDNamespace, vaName,
			modelServiceName+"-decode", cfg.ModelID, cfg.AcceleratorType, 10.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

		By("Creating HPA with minReplicas=1 (scale-to-zero disabled)")
		// When scale-to-zero is disabled, HPA should use minReplicas=1
		err = fixtures.CreateHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName,
			modelServiceName+"-decode", vaName, 1, 10)
		Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")

		GinkgoWriter.Println("Scale-to-zero disabled test setup complete")
	})

	AfterAll(func() {
		By("Restoring original ConfigMap or deleting test ConfigMap")
		_ = k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Delete(ctx, configMapName, metav1.DeleteOptions{})
		if originalConfigMap != nil {
			// Restore original ConfigMap
			time.Sleep(1 * time.Second)
			_, _ = k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Create(ctx, originalConfigMap, metav1.CreateOptions{})
		}

		By("Cleaning up test resources")
		// Delete HPA
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx,
			hpaName+"-hpa", metav1.DeleteOptions{})

		// Delete VA
		_ = crClient.Delete(ctx, &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: vaName, Namespace: cfg.LLMDNamespace},
		})

		// Delete service
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx,
			modelServiceName+"-service", metav1.DeleteOptions{})

		// Delete deployment
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx,
			modelServiceName+"-decode", metav1.DeleteOptions{})
	})

	It("should verify scale-to-zero is disabled in ConfigMap", func() {
		By("Checking ConfigMap has scale-to-zero disabled")
		cm, err := k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Get(ctx, configMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should be able to get ConfigMap")

		Expect(cm.Data["default"]).To(ContainSubstring("enable_scale_to_zero: false"),
			"ConfigMap default should have scale-to-zero disabled")

		// Use sanitized model ID key (same as used when creating ConfigMap)
		sanitizedModelID := strings.ReplaceAll(cfg.ModelID, "/", "-")
		Expect(cm.Data[sanitizedModelID]).To(ContainSubstring("enable_scale_to_zero: false"),
			"ConfigMap model entry should have scale-to-zero disabled")

		GinkgoWriter.Println("Verified scale-to-zero is DISABLED in ConfigMap")
	})

	It("should maintain minimum replicas when scale-to-zero is disabled", func() {
		By("Waiting for idle period (3 minutes)")
		time.Sleep(3 * time.Minute)

		By("Verifying deployment maintains at least 1 replica")
		Consistently(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx,
				modelServiceName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")
			g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", 1),
				"Deployment should maintain at least 1 replica when scale-to-zero is disabled")
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		GinkgoWriter.Println("Verified: Deployment maintains minimum replicas with scale-to-zero disabled")
	})

	It("should have VA recommend at least 1 replica", func() {
		By("Checking VA desired replicas")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace,
				Name:      vaName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			desiredReplicas := va.Status.DesiredOptimizedAlloc.NumReplicas
			GinkgoWriter.Printf("VA desired replicas: %d (should be >= 1)\n", desiredReplicas)

			// When scale-to-zero is disabled, VA should recommend at least 1 replica
			g.Expect(desiredReplicas).To(BeNumerically(">=", 1),
				"VA should recommend at least 1 replica when scale-to-zero is disabled")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		GinkgoWriter.Println("Verified: VA recommends at least 1 replica with scale-to-zero disabled")
	})
})
