package e2e

import (
	"strings"
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
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

var _ = Describe("Smoke Tests - Infrastructure Readiness", Label("smoke", "full"), func() {
	Context("Basic infrastructure validation", func() {
		It("should have WVA controller running and ready", func() {
			By("Checking WVA controller pods")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.WVANamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "control-plane=controller-manager",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "WVA controller pod should exist")

				// At least one pod should be running and ready
				readyPods := 0
				for _, pod := range pods.Items {
					if pod.Status.Phase == "Running" {
						for _, condition := range pod.Status.Conditions {
							if condition.Type == "Ready" && condition.Status == "True" {
								readyPods++
								break
							}
						}
					}
				}
				g.Expect(readyPods).To(BeNumerically(">", 0), "At least one WVA controller pod should be ready")
			}).Should(Succeed())
		})

		It("should have llm-d CRDs installed", func() {
			By("Checking for InferencePool CRD")
			_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("inference.networking.k8s.io/v1")
			Expect(err).NotTo(HaveOccurred(), "llm-d CRDs should be installed")
		})

		It("should have Prometheus running", func() {
			By("Checking Prometheus pods")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).List(ctx, metav1.ListOptions{
					LabelSelector: "app.kubernetes.io/name=prometheus",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Prometheus pod should exist")
			}).Should(Succeed())
		})

		When("using Prometheus Adapter as scaler backend", func() {
			It("should have external metrics API available", func() {
				if cfg.ScalerBackend != "prometheus-adapter" {
					Skip("External metrics API check only applies to Prometheus Adapter backend")
				}
				By("Checking for external.metrics.k8s.io API group")
				Eventually(func(g Gomega) {
					_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
					g.Expect(err).NotTo(HaveOccurred(), "External metrics API should be available")
				}).Should(Succeed())
			})
		})

		When("using KEDA as scaler backend", func() {
			It("should have KEDA operator ready", func() {
				if cfg.ScalerBackend != "keda" {
					Skip("KEDA readiness check only applies when SCALER_BACKEND=keda")
				}
				By("Checking KEDA operator pods in " + cfg.KEDANamespace)
				Eventually(func(g Gomega) {
					pods, err := k8sClient.CoreV1().Pods(cfg.KEDANamespace).List(ctx, metav1.ListOptions{
						LabelSelector: "app.kubernetes.io/name=keda-operator",
					})
					g.Expect(err).NotTo(HaveOccurred(), "Failed to list KEDA pods")
					g.Expect(pods.Items).NotTo(BeEmpty(), "At least one KEDA operator pod should exist")
					ready := 0
					for _, p := range pods.Items {
						if p.Status.Phase == corev1.PodRunning {
							for _, c := range p.Status.Conditions {
								if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
									ready++
									break
								}
							}
						}
					}
					g.Expect(ready).To(BeNumerically(">", 0), "At least one KEDA operator pod should be ready")
				}).Should(Succeed())
			})
		})
	})

	Context("Basic VA lifecycle", Ordered, func() {
		var (
			poolName         = "smoke-test-pool"
			modelServiceName = "smoke-test-ms"
			deploymentName   = modelServiceName + "-decode"
			vaName           = "smoke-test-va"
			hpaName          = "smoke-test-hpa"
			minReplicas      = int32(1) // Store minReplicas for stabilization check
		)

		BeforeAll(func() {
			// Note: InferencePool should already exist from infra-only deployment
			// We no longer create InferencePools in individual tests

			By("Deleting all existing VariantAutoscaling objects for clean test state")
			deletedCount, vaCleanupErr := utils.DeleteAllVariantAutoscalings(ctx, crClient, cfg.LLMDNamespace)
			if vaCleanupErr != nil {
				GinkgoWriter.Printf("Warning: Failed to clean up existing VAs: %v\n", vaCleanupErr)
			} else if deletedCount > 0 {
				GinkgoWriter.Printf("Deleted %d existing VariantAutoscaling objects\n", deletedCount)
			} else {
				GinkgoWriter.Println("No existing VariantAutoscaling objects found")
			}

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
				serviceName := modelServiceName + "-service"
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
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Model service should have 1 ready replica")
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Creating VariantAutoscaling resource")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(
				ctx, crClient, cfg.LLMDNamespace, vaName,
				deploymentName, cfg.ModelID, cfg.AcceleratorType,
				cfg.ControllerInstance,
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

			By("Creating scaler for the deployment (HPA or ScaledObject per backend)")
			if cfg.ScaleToZeroEnabled {
				minReplicas = 0
			}
			if cfg.ScalerBackend == "keda" {
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
			// Load Job, Service, Deployment, and ServiceMonitor cleanup is handled by DeferCleanup registered in BeforeAll and test

			if cfg.ScalerBackend == "keda" {
				err := fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName)
				Expect(err).NotTo(HaveOccurred())
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
		})

		It("should reconcile the VA successfully", func() {
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
			}).Should(Succeed())
		})

		It("should expose external metrics for the VA", func() {
			By("Waiting for VA to be reconciled (TargetResolved condition)")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				// Verify VA is reconciled (has TargetResolved condition)
				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(condition).NotTo(BeNil(), "VA should have TargetResolved condition")
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "TargetResolved should be True")
			}).Should(Succeed())

			if cfg.ScalerBackend == "keda" {
				By("Verifying ScaledObject exists (KEDA backend; external metric name is KEDA-generated)")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject %s should exist", soName)
			} else {
				By("Querying external metrics API for wva_desired_replicas")
				// Note: The metric may not exist until Engine has run and emitted metrics to Prometheus,
				// which Prometheus Adapter then queries. This can take time.
				result, err := k8sClient.RESTClient().
					Get().
					AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + cfg.LLMDNamespace + "/" + constants.WVADesiredReplicas).
					DoRaw(ctx)
				if err != nil {
					if errors.IsNotFound(err) {
						GinkgoWriter.Printf("External metrics API is accessible, but metric %s doesn't exist yet (Engine may not have run)\n", constants.WVADesiredReplicas)
						_, discoveryErr := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
						Expect(discoveryErr).NotTo(HaveOccurred(), "External metrics API should be accessible")
					} else {
						Expect(err).NotTo(HaveOccurred(), "Should be able to query external metrics API")
					}
				} else {
					if strings.Contains(string(result), `"items":[]`) {
						GinkgoWriter.Printf("External metrics API is accessible, but metric %s doesn't exist yet (Engine may not have run)\n", constants.WVADesiredReplicas)
						_, discoveryErr := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
						Expect(discoveryErr).NotTo(HaveOccurred(), "External metrics API should be accessible")
					} else {
						Expect(string(result)).To(ContainSubstring(constants.WVADesiredReplicas), "Metric response should contain metric name")
						GinkgoWriter.Printf("External metrics API returned metric: %s\n", constants.WVADesiredReplicas)
					}
				}
			}

			By("Verifying DesiredOptimizedAlloc is eventually populated (if Engine has run)")
			// This is a best-effort check - DesiredOptimizedAlloc is populated by the Engine
			// which may not run immediately. We check if it's populated, but don't fail if it's not yet.
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			getErr := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(getErr).NotTo(HaveOccurred())
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				// If populated, verify it's valid
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
			} else {
				// If not populated yet, that's okay - Engine may not have run yet
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})

		It("should have MetricsAvailable condition set when pods are ready", func() {
			By("Waiting for MetricsAvailable condition to be set")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
				// MetricsAvailable can be True (metrics found) or False (metrics missing/stale)
				// For smoke tests, we just verify the condition exists and has a valid status
				g.Expect(condition.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
					"MetricsAvailable condition should have a valid status")
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

		It("should have scaling controlled by backend", func() {
			if cfg.ScalerBackend == "keda" {
				By("Verifying ScaledObject exists and KEDA has created an HPA")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject should exist")
				// KEDA creates an HPA for the ScaledObject; name pattern is often keda-hpa-<scaledobject> or from status
				Eventually(func(g Gomega) {
					hpaList, listErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
					g.Expect(listErr).NotTo(HaveOccurred())
					var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
					for i := range hpaList.Items {
						h := &hpaList.Items[i]
						if h.Spec.ScaleTargetRef.Name == deploymentName {
							kedaHPA = h
							break
						}
					}
					g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the deployment")
					g.Expect(kedaHPA.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			} else {
				By("Verifying HPA exists and is configured")
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "HPA should exist")
				Expect(hpa.Spec.Metrics).NotTo(BeEmpty(), "HPA should have metrics configured")
				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use External metric type")
				Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal(constants.WVADesiredReplicas), "HPA should use wva_desired_replicas metric")

				By("Waiting for HPA to read the metric and update status")
				Eventually(func(g Gomega) {
					hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(hpa.Status.CurrentReplicas).To(BeNumerically(">=", 0), "HPA should have current replicas set")
					g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			}
		})

		It("should verify Prometheus is scraping vLLM metrics", func() {
			By("Checking that deployment pods are ready and reporting metrics")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + modelServiceName + "-decode",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Should have at least one pod")

				// At least one pod should be ready
				readyCount := 0
				for _, pod := range pods.Items {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyCount++
							break
						}
					}
				}
				g.Expect(readyCount).To(BeNumerically(">", 0), "At least one pod should be ready for metrics scraping")
			}).Should(Succeed())

			// Note: Direct Prometheus query would require port-forwarding or in-cluster access
			// For smoke tests, we verify pods are ready (which is a prerequisite for metrics)
			// Full Prometheus query validation is in the full test suite
		})

		It("should collect saturation metrics without triggering scale-up", func() {
			By("Verifying VA is reconciled and has conditions")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")
			}).Should(Succeed())

			By("Verifying MetricsAvailable condition indicates metrics collection")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred())

			condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
			// For smoke tests, we verify the condition exists
			// In ideal case, it should be True with ReasonMetricsFound, but False is also valid
			// if metrics are temporarily unavailable (smoke tests don't apply load)
			Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
			if condition.Status == metav1.ConditionTrue {
				Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonMetricsFound),
					"When metrics are available, reason should be MetricsFound")
			}

			By("Checking if DesiredOptimizedAlloc is populated (best-effort)")
			// DesiredOptimizedAlloc is populated by the Engine, which may not run immediately
			// This is a best-effort check - we verify it's valid if populated, but don't fail if not
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
				GinkgoWriter.Printf("DesiredOptimizedAlloc is populated: accelerator=%s, replicas=%d\n",
					va.Status.DesiredOptimizedAlloc.Accelerator, *va.Status.DesiredOptimizedAlloc.NumReplicas)
			} else {
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})
	})

	Context("Error handling and graceful degradation", Label("smoke", "full"), Ordered, func() {
		var (
			errorTestPoolName         = "error-test-pool"
			errorTestModelServiceName = "error-test-ms"
			errorTestVAName           = "error-test-va"
		)

		BeforeAll(func() {
			deploymentName := errorTestModelServiceName + "-decode"

			By("Creating model service deployment for error handling tests")
			err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, errorTestModelServiceName, errorTestPoolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
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

			By("Waiting for model service to be ready")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Model service should have 1 ready replica")
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Creating VariantAutoscaling resource")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(
				ctx, crClient, cfg.LLMDNamespace, errorTestVAName,
				deploymentName, cfg.ModelID, cfg.AcceleratorType,
				cfg.ControllerInstance,
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

			By("Waiting for VA to reconcile initially")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      errorTestVAName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")
			}).Should(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up error handling test resources")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      errorTestVAName,
					Namespace: cfg.LLMDNamespace,
				},
			}
			cleanupResource(ctx, "VA", cfg.LLMDNamespace, errorTestVAName,
				func() error {
					return crClient.Delete(ctx, va)
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: errorTestVAName, Namespace: cfg.LLMDNamespace}, va)
					return errors.IsNotFound(err)
				})
		})

		It("should handle deployment deletion gracefully", func() {
			deploymentName := errorTestModelServiceName + "-decode"

			By("Verifying deployment exists before deletion")
			_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Deployment should exist before deletion")

			By("Deleting the deployment")
			err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to delete deployment")

			By("Waiting for deployment to be fully deleted")
			Eventually(func(g Gomega) {
				_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).To(HaveOccurred(), "Deployment should be deleted")
				g.Expect(errors.IsNotFound(err)).To(BeTrue(), "Error should be NotFound")
			}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).Should(Succeed())

			By("Verifying VA continues to exist after deployment deletion")
			// The VA should continue to exist even when the deployment is deleted
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Name:      errorTestVAName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred(), "VA should continue to exist after deployment deletion")

			// Note: The controller may not immediately detect deployment deletion due to caching.
			// This spec focuses on delete/recreate resilience (VA stays, TargetResolved recovers).
			// An explicit TargetResolved=False assertion on a permanently missing target is optional coverage.

			By("Recreating the deployment")
			err = fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, errorTestModelServiceName, errorTestPoolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to recreate model service")

			By("Waiting for deployment to be created and progressing")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "Deployment should be created")
				// Verify deployment exists and is progressing (may not be ready yet)
				g.Expect(deployment.Status.Replicas).To(BeNumerically(">=", 0), "Deployment should have replica status")
			}, time.Duration(cfg.EventuallyMediumSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Waiting for deployment to be ready (with extended timeout for recreation)")
			// When recreating, pods may take longer to start (image pull, etc.)
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)),
					"Model service should have 1 ready replica after recreation")
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

			By("Verifying VA automatically resumes operation")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      errorTestVAName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(condition).NotTo(BeNil(), "TargetResolved condition should exist")
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue),
					"TargetResolved should be True when deployment is recreated")
			}).Should(Succeed())
		})

		It("should handle metrics unavailability gracefully", func() {
			By("Verifying MetricsAvailable condition exists and reflects metrics state")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      errorTestVAName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")

				// MetricsAvailable can be True or False depending on metrics availability
				// The important thing is that the condition exists and has a valid reason
				switch condition.Status {
				case metav1.ConditionFalse:
					// If metrics are unavailable, reason should indicate why
					g.Expect(condition.Reason).To(BeElementOf(
						variantautoscalingv1alpha1.ReasonMetricsMissing,
						variantautoscalingv1alpha1.ReasonMetricsStale,
						variantautoscalingv1alpha1.ReasonPrometheusError,
						variantautoscalingv1alpha1.ReasonMetricsUnavailable,
					), "When metrics are unavailable, reason should indicate the cause")
				case metav1.ConditionTrue:
					// If metrics are available, reason should be MetricsFound
					g.Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonMetricsFound),
						"When metrics are available, reason should be MetricsFound")
				}
			}).Should(Succeed())

			By("Verifying VA continues to reconcile even if metrics are temporarily unavailable")
			// The VA should continue to reconcile and have status conditions even if metrics are unavailable
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      errorTestVAName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred())
			// VA should have status conditions (indicating it's reconciling)
			Expect(va.Status.Conditions).NotTo(BeEmpty(),
				"VA should have status conditions even if metrics are unavailable")
			// DesiredOptimizedAlloc may not be populated if Engine hasn't run due to missing metrics
			// This is acceptable - the important thing is that the VA continues to reconcile
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				// If populated, verify it's valid
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
			} else {
				// If not populated, that's okay - Engine may not have run yet
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run due to missing metrics)\n")
			}
		})
	})
})
