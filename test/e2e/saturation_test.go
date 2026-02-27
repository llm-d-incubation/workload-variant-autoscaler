package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Saturation-based mode workload test constants
const (
	MinimumReplicas = 2 // Minimum replicas for smoke test baseline
)

var _ = Describe("Saturation Mode - Single VariantAutoscaling", Label("full"), Ordered, func() {
	var (
		poolName         = "saturation-single-pool"
		modelServiceName = "saturation-single-ms"
		vaName           = "saturation-single-va"
		hpaName          = "saturation-single-hpa"
	)

	BeforeAll(func() {
		// Note: InferencePool should already exist from infra-only deployment
		// We no longer create InferencePools in individual tests

		serviceName := modelServiceName + "-service"
		deploymentName := modelServiceName + "-decode"

		By("Creating model service deployment")
		err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
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
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, deploymentName, 8000)
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
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, deploymentName)
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

			// Log deployment status for debugging
			if deployment.Status.ReadyReplicas < 1 {
				GinkgoWriter.Printf("Deployment status: Replicas=%d, ReadyReplicas=%d, AvailableReplicas=%d, UpdatedReplicas=%d\n",
					deployment.Status.Replicas, deployment.Status.ReadyReplicas,
					deployment.Status.AvailableReplicas, deployment.Status.UpdatedReplicas)

				// Check pod status for more details
				podList, listErr := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("app=%s", deploymentName),
				})
				if listErr == nil {
					for _, pod := range podList.Items {
						GinkgoWriter.Printf("Pod %s: Phase=%s, Ready=%v\n", pod.Name, pod.Status.Phase, pod.Status.Phase == corev1.PodRunning)
						if pod.Status.Phase != corev1.PodRunning && len(pod.Status.ContainerStatuses) > 0 {
							for _, cs := range pod.Status.ContainerStatuses {
								if cs.State.Waiting != nil {
									GinkgoWriter.Printf("  Container %s waiting: Reason=%s, Message=%s\n",
										cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
								}
							}
						}
					}
				}
			}

			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Model service should have 1 ready replica")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

		By("Creating VariantAutoscaling resource")
		err = fixtures.EnsureVariantAutoscaling(
			ctx, crClient, cfg.LLMDNamespace, vaName,
			deploymentName, cfg.ModelID, cfg.AcceleratorType, 30.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

		By("Creating scaler for the deployment (HPA or ScaledObject per backend)")
		minReplicas := int32(1)
		if cfg.ScaleToZeroEnabled {
			minReplicas = 0
		}
		if cfg.ScalerBackend == "keda" {
			// Remove any existing HPA that might manage this deployment (e.g. from a prior run with prometheus-adapter)
			// so KEDA's webhook does not reject the ScaledObject with "workload already managed by HPA"
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
			err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName, deploymentName, vaName, minReplicas, 10, cfg.MonitoringNS)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject")
		} else {
			err = fixtures.EnsureHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName, deploymentName, vaName, minReplicas, 10)
			Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")
		}
	})

	AfterAll(func() {
		By("Cleaning up test resources")

		// Delete in reverse dependency order: scaler (HPA or ScaledObject) -> VA
		// Service and Deployment cleanup is handled by DeferCleanup registered in BeforeAll
		if cfg.ScalerBackend == "keda" {
			Expect(fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName)).NotTo(HaveOccurred())
		} else {
			hpaNameFull := hpaName + "-hpa"
			cleanupResource(ctx, "HPA", cfg.LLMDNamespace, hpaNameFull,
				func() error {
					return k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaNameFull, metav1.DeleteOptions{})
				},
				func() bool {
					_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaNameFull, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
		}

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

		// Note: InferencePool cleanup requires llm-d CRD client, handled by AfterSuite
	})

	// Smoke test: Verify VA reconciliation and basic readiness
	Context("VA reconciliation and status", Label("smoke", "full"), func() {
		It("should have VA reconciled with correct status conditions", func() {
			By("Checking VA status conditions")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")

				// Check for TargetResolved condition
				targetResolved := false
				for _, cond := range va.Status.Conditions {
					if cond.Type == "TargetResolved" && cond.Status == metav1.ConditionTrue {
						targetResolved = true
						break
					}
				}
				g.Expect(targetResolved).To(BeTrue(), "VA should have TargetResolved=True condition")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("Logging VA status after reconciliation")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("VA Status: %+v\n", va.Status)
		})

		It("should have scaler created and tracking VA metrics", func() {
			deploymentName := modelServiceName + "-decode"
			if cfg.ScalerBackend == "keda" {
				By("Verifying ScaledObject exists")
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: hpaName + "-so"}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject should exist")
				Eventually(func(g Gomega) {
					hpaList, listErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
					g.Expect(listErr).NotTo(HaveOccurred())
					for i := range hpaList.Items {
						if hpaList.Items[i].Spec.ScaleTargetRef.Name == deploymentName {
							return
						}
					}
					g.Expect(false).To(BeTrue(), "KEDA should have created an HPA for the deployment")
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			} else {
				By("Verifying HPA exists")
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "HPA should exist")
				By("Verifying HPA is configured for external metrics")
				Expect(hpa.Spec.Metrics).NotTo(BeEmpty(), "HPA should have metrics configured")
				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use External metric type")
				Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal("wva_desired_replicas"))
			}
		})
	})

	// Full test: Replica stability under constant load
	Context("Replica stability under constant load", Label("full"), func() {
		It("should maintain stable replica count under constant load", func() {
			// Skip when using simulator: the simulator produces metrics that the saturation
			// engine interprets as saturation, causing unpredictable scaling (1→3 replicas)
			// instead of the stable count this test expects. Stability testing requires
			// real vLLM with predictable GPU saturation behavior under constant load.
			if cfg.UseSimulator {
				Skip("Saturation stability test requires real vLLM — simulator metrics cause unpredictable scaling")
			}

			By("Starting constant load generation")
			loadCfg := fixtures.LoadConfig{
				Strategy:     cfg.LoadStrategy,
				RequestRate:  cfg.RequestRate / 2, // Lower rate for stability test
				NumPrompts:   cfg.NumPrompts,
				InputTokens:  cfg.InputTokens,
				OutputTokens: cfg.OutputTokens,
				ModelID:      cfg.ModelID,
			}

			targetURL := fmt.Sprintf("http://%s-service:8000", modelServiceName)
			err := fixtures.CreateLoadJob(ctx, k8sClient, cfg.LLMDNamespace, "stability-load", targetURL, loadCfg)
			Expect(err).NotTo(HaveOccurred(), "Failed to create stability load job")

			jobName := "stability-load-load"

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

			By("Waiting for load to stabilize")
			time.Sleep(2 * time.Minute)

			By("Verifying replica count remains stable")
			var replicaCounts []int32
			for i := 0; i < 6; i++ {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				replicaCounts = append(replicaCounts, deployment.Status.Replicas)
				time.Sleep(30 * time.Second)
			}

			// All replica counts should be the same (within ±1 for transient states)
			baseline := replicaCounts[0]
			for _, count := range replicaCounts {
				Expect(count).To(BeNumerically("~", baseline, 1),
					"Replica count should remain stable under constant load")
			}

			GinkgoWriter.Printf("Replica count remained stable at %d under constant load\n", baseline)
		})
	})
})

// Multi-variant saturation test (cost-based scaling)
var _ = Describe("Saturation Mode - Multiple VariantAutoscalings", Label("full"), Ordered, func() {
	var (
		poolA         = "saturation-multi-pool-a"
		poolB         = "saturation-multi-pool-b"
		modelServiceA = "saturation-multi-ms-a"
		modelServiceB = "saturation-multi-ms-b"
		vaA           = "saturation-multi-va-a"
		vaB           = "saturation-multi-va-b"
		hpaA          = "saturation-multi-hpa-a"
		hpaB          = "saturation-multi-hpa-b"
	)

	BeforeAll(func() {
		// Note: InferencePools should already exist from infra-only deployment
		// We no longer create InferencePools in individual tests

		By("Creating two model services with different configurations")

		// Pool A (cheaper)
		err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceA, poolA, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred())

		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceA, modelServiceA+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred())

		By("Creating ServiceMonitor for service A")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceA, modelServiceA+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor A")

		// Pool B (more expensive)
		err = fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceB, poolB, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred())

		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceB, modelServiceB+"-decode", 8001)
		Expect(err).NotTo(HaveOccurred())

		By("Creating ServiceMonitor for service B")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceB, modelServiceB+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor B")

		By("Waiting for both model services to be ready")
		Eventually(func(g Gomega) {
			depA, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceA+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(depA.Status.ReadyReplicas).To(Equal(int32(1)))

			depB, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceB+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(depB.Status.ReadyReplicas).To(Equal(int32(1)))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

		By("Creating two VAs with different costs")
		// VA A: Lower cost (should be preferred)
		err = fixtures.EnsureVariantAutoscaling(ctx, crClient, cfg.LLMDNamespace, vaA, modelServiceA+"-decode", cfg.ModelID, "A100", 30.0, cfg.ControllerInstance)
		Expect(err).NotTo(HaveOccurred())

		// VA B: Higher cost
		err = fixtures.EnsureVariantAutoscaling(ctx, crClient, cfg.LLMDNamespace, vaB, modelServiceB+"-decode", cfg.ModelID, "H100", 50.0, cfg.ControllerInstance)
		Expect(err).NotTo(HaveOccurred())

		By("Creating scalers for both deployments (HPA or ScaledObject per backend)")
		if cfg.ScalerBackend == "keda" {
			// Remove any existing HPAs that might manage these deployments so KEDA's webhook accepts the ScaledObjects
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaA+"-hpa", metav1.DeleteOptions{})
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaB+"-hpa", metav1.DeleteOptions{})
			err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaA, modelServiceA+"-decode", vaA, 1, 10, cfg.MonitoringNS)
			Expect(err).NotTo(HaveOccurred())
			err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaB, modelServiceB+"-decode", vaB, 1, 10, cfg.MonitoringNS)
			Expect(err).NotTo(HaveOccurred())
		} else {
			err = fixtures.EnsureHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaA, modelServiceA+"-decode", vaA, 1, 10)
			Expect(err).NotTo(HaveOccurred())
			err = fixtures.EnsureHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaB, modelServiceB+"-decode", vaB, 1, 10)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterAll(func() {
		By("Cleaning up multi-variant test resources")

		if cfg.ScalerBackend == "keda" {
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaA)
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaB)
		} else {
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaA+"-hpa", metav1.DeleteOptions{})
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaB+"-hpa", metav1.DeleteOptions{})
		}

		// Delete VAs
		_ = crClient.Delete(ctx, &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: vaA, Namespace: cfg.LLMDNamespace},
		})
		_ = crClient.Delete(ctx, &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: vaB, Namespace: cfg.LLMDNamespace},
		})

		// Delete services
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, modelServiceA+"-service", metav1.DeleteOptions{})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, modelServiceB+"-service", metav1.DeleteOptions{})

		// Delete deployments
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelServiceA+"-decode", metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelServiceB+"-decode", metav1.DeleteOptions{})
	})

	It("should prefer cheaper variant (VA A) for scale-up when both variants are available", func() {
		By("Generating load to both services")
		loadCfg := fixtures.LoadConfig{
			Strategy:     cfg.LoadStrategy,
			RequestRate:  cfg.RequestRate,
			NumPrompts:   cfg.NumPrompts,
			InputTokens:  cfg.InputTokens,
			OutputTokens: cfg.OutputTokens,
			ModelID:      cfg.ModelID,
		}

		// Create load jobs for both services
		targetA := fmt.Sprintf("http://%s-service:8000", modelServiceA)
		err := fixtures.CreateLoadJob(ctx, k8sClient, cfg.LLMDNamespace, "multi-load-a", targetA, loadCfg)
		Expect(err).NotTo(HaveOccurred())

		targetB := fmt.Sprintf("http://%s-service:8001", modelServiceB)
		err = fixtures.CreateLoadJob(ctx, k8sClient, cfg.LLMDNamespace, "multi-load-b", targetB, loadCfg)
		Expect(err).NotTo(HaveOccurred())

		jobNameA := "multi-load-a-load"
		jobNameB := "multi-load-b-load"

		// Register cleanup for load jobs (runs even if test fails)
		DeferCleanup(func() {
			cleanupResource(ctx, "Job", cfg.LLMDNamespace, jobNameA,
				func() error {
					propagation := metav1.DeletePropagationBackground
					return k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, jobNameA, metav1.DeleteOptions{PropagationPolicy: &propagation})
				},
				func() bool {
					_, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobNameA, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
			cleanupResource(ctx, "Job", cfg.LLMDNamespace, jobNameB,
				func() error {
					propagation := metav1.DeletePropagationBackground
					return k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, jobNameB, metav1.DeleteOptions{PropagationPolicy: &propagation})
				},
				func() bool {
					_, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobNameB, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
		})

		By("Waiting for both VAs to detect saturation")
		time.Sleep(2 * time.Minute)

		By("Verifying VA A (cheaper) scaled up more than VA B")
		vaAObj := &variantautoscalingv1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{Name: vaA, Namespace: cfg.LLMDNamespace}, vaAObj)
		Expect(err).NotTo(HaveOccurred())

		vaBObj := &variantautoscalingv1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{Name: vaB, Namespace: cfg.LLMDNamespace}, vaBObj)
		Expect(err).NotTo(HaveOccurred())

		replicasA := vaAObj.Status.DesiredOptimizedAlloc.NumReplicas
		replicasB := vaBObj.Status.DesiredOptimizedAlloc.NumReplicas

		GinkgoWriter.Printf("VA A (cheaper, cost=30.0) replicas: %d\n", replicasA)
		GinkgoWriter.Printf("VA B (expensive, cost=50.0) replicas: %d\n", replicasB)

		// Cheaper variant should be preferred (or at least equal)
		Expect(replicasA).To(BeNumerically(">=", replicasB),
			"Cheaper variant (VA A) should be preferred or equal to expensive variant (VA B)")
	})
})
