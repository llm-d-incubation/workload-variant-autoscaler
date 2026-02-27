package e2e

import (
	"fmt"
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
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

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
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

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
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
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
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
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
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

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
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

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
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
				GinkgoWriter.Printf("DesiredOptimizedAlloc is populated: accelerator=%s, replicas=%d\n",
					va.Status.DesiredOptimizedAlloc.Accelerator, va.Status.DesiredOptimizedAlloc.NumReplicas)
			} else {
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})

		It("should scale up under load", func() {
			// HPA name: with Prometheus Adapter we create HPA named <hpaName>-hpa; with KEDA, KEDA creates HPA named keda-hpa-<scaledobject-name>
			effectiveHpaName := hpaName + "-hpa"
			if cfg.ScalerBackend == "keda" {
				effectiveHpaName = "keda-hpa-" + hpaName + "-so"
			}

			// wait for VA to stabilize at minReplicas before starting load
			// This ensures we're measuring scale-up from load, not residual scale from prior activity
			By("Waiting for VA to stabilize at minReplicas")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				optimized := int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
				GinkgoWriter.Printf("Waiting for VA to be ready: optimized=%d, minReplicas=%d\n", optimized, minReplicas)
				// Wait for optimized >= minReplicas (allows for initial 0 during engine startup)
				// accepts any value >= minReplicas as initial state
				g.Expect(optimized).To(BeNumerically(">=", minReplicas), "VA should have optimized >= minReplicas")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			// wait for deployment to be fully stable (no pods in transition)
			// This prevents starting load while pods are terminating from scale-down
			// Note: We don't wait for HPA to scale deployment to match VA recommendation because:
			// 1. HPA may take time to read external metrics and scale
			// 2. The test checks for scale-up from initial state, not absolute values
			// 3. record initialOptimized (whatever VA recommends) and check for increase from there
			By("Waiting for deployment to stabilize (no pods in transition)")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				specReplicas := *deployment.Spec.Replicas
				statusReplicas := deployment.Status.Replicas
				readyReplicas := deployment.Status.ReadyReplicas
				GinkgoWriter.Printf("Waiting for deployment stability: spec=%d, status=%d, ready=%d\n",
					specReplicas, statusReplicas, readyReplicas)
				// All replica counts must match - no pods starting or terminating
				g.Expect(statusReplicas).To(Equal(specReplicas), "Status replicas should match spec")
				g.Expect(readyReplicas).To(Equal(specReplicas), "Ready replicas should match spec")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			// Prefer starting from minReplicas so we reliably detect scale-up (1 -> 2+). Wait for VA to
			// settle at minReplicas when possible; otherwise use current value.
			By("Waiting for VA to settle at minReplicas before recording initial state (best-effort)")
			settled := false
			for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				if err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va); err != nil {
					break
				}
				if int32(va.Status.DesiredOptimizedAlloc.NumReplicas) == minReplicas {
					settled = true
					break
				}
				GinkgoWriter.Printf("Waiting for VA to settle: optimized=%d, minReplicas=%d\n", va.Status.DesiredOptimizedAlloc.NumReplicas, minReplicas)
				time.Sleep(10 * time.Second)
			}
			if !settled {
				GinkgoWriter.Printf("VA did not settle at minReplicas within 5m; will use current value as initial\n")
			}

			// Record initial state after stabilization
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred())
			initialOptimized := int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
			GinkgoWriter.Printf("Initial optimized replicas (after stabilization): %d (settled=%v)\n", initialOptimized, settled)

			By("Starting burst load generation to trigger scale-up")
			// Use burst load pattern
			// Burst pattern: sends 10 requests in parallel, then sleeps 0.5s, repeats
			// This creates queue spikes that are more likely to trigger saturation detection.
			// Use at least 2400 prompts so load lasts long enough for Engine to detect saturation
			// (Engine polls every 30s; we need several cycles). When running full suite,
			// use cfg.NumPrompts (e.g. 3000) so load is consistent and sufficient.
			scaleUpPrompts := 2400
			if cfg.NumPrompts > scaleUpPrompts {
				scaleUpPrompts = cfg.NumPrompts
			}
			loadCfg := fixtures.LoadConfig{
				Strategy:     cfg.LoadStrategy,
				RequestRate:  0,              // Not used for burst pattern
				NumPrompts:   scaleUpPrompts, // Enough prompts to trigger saturation
				InputTokens:  cfg.InputTokens,
				OutputTokens: 400,
				ModelID:      cfg.ModelID,
			}

			// Use burst load pattern
			// Burst pattern creates queue spikes that are more likely to trigger saturation detection
			targetURL := fmt.Sprintf("http://%s-service.%s.svc.cluster.local:8000/v1/completions", modelServiceName, cfg.LLMDNamespace)
			err = fixtures.EnsureBurstLoadJob(ctx, k8sClient, cfg.LLMDNamespace, "smoke-scaleup-load", targetURL, loadCfg)
			Expect(err).NotTo(HaveOccurred(), "Failed to create burst load generation job")

			jobName := "smoke-scaleup-load-load"

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

			loadStartTime := time.Now()

			By("Verifying load job was created")
			Eventually(func(g Gomega) {
				job, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "Load job should exist")
				GinkgoWriter.Printf("Load job status: Active=%d, Succeeded=%d, Failed=%d\n",
					job.Status.Active, job.Status.Succeeded, job.Status.Failed)
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("Waiting for load job pod to start")
			// With pre-built image, pod should start quickly (no pip install needed)
			Eventually(func(g Gomega) {
				podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("job-name=%s", jobName),
				})
				g.Expect(err).NotTo(HaveOccurred())
				if len(podList.Items) == 0 {
					// If no pods, check Job status for errors
					job, jobErr := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
					if jobErr == nil {
						GinkgoWriter.Printf("Job exists but no pods yet. Job status: Active=%d, Succeeded=%d, Failed=%d\n",
							job.Status.Active, job.Status.Succeeded, job.Status.Failed)
					}
					g.Expect(podList.Items).NotTo(BeEmpty(), "Load job pod should exist")
				}

				pod := podList.Items[0]
				// Log pod status with detailed reason extraction
				if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded {
					reason := pod.Status.Reason
					var messages []string

					// For Pending pods, check PodScheduled condition first (most common)
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
							if reason == "" {
								reason = condition.Reason
							}
							if condition.Message != "" {
								messages = append(messages, fmt.Sprintf("PodScheduled: %s", condition.Message))
							}
						} else if condition.Status == corev1.ConditionFalse {
							// Log all False conditions for context
							messages = append(messages, fmt.Sprintf("%s: %s", condition.Type, condition.Reason))
							if condition.Message != "" {
								messages = append(messages, fmt.Sprintf("  %s", condition.Message))
							}
						}
					}

					// Check container waiting states
					for _, containerStatus := range pod.Status.ContainerStatuses {
						if containerStatus.State.Waiting != nil {
							if reason == "" {
								reason = containerStatus.State.Waiting.Reason
							}
							if containerStatus.State.Waiting.Message != "" {
								messages = append(messages, fmt.Sprintf("Container %s: %s", containerStatus.Name, containerStatus.State.Waiting.Message))
							}
						}
					}

					// If still no reason, try to get events for the pod
					if reason == "" {
						events, err := k8sClient.CoreV1().Events(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
							FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", pod.Name),
							Limit:         5, // Only get recent events
						})
						if err == nil {
							// Get most recent event
							for i := len(events.Items) - 1; i >= 0; i-- {
								event := events.Items[i]
								if event.Reason != "" {
									reason = event.Reason
									if event.Message != "" {
										messages = append(messages, fmt.Sprintf("Event: %s", event.Message))
									}
									break
								}
							}
						}
					}

					// Only show "Unknown" if we truly couldn't determine a reason
					if reason == "" {
						reason = "Unknown (check pod events for details)"
					}

					GinkgoWriter.Printf("Load job pod status: Phase=%s, Reason=%s\n", pod.Status.Phase, reason)
					if len(messages) > 0 {
						for _, msg := range messages {
							GinkgoWriter.Printf("  %s\n", msg)
						}
					}
				}
				g.Expect(pod.Status.Phase).To(Or(
					Equal(corev1.PodRunning),
					Equal(corev1.PodSucceeded),
				), fmt.Sprintf("Load job pod should be running or succeeded (current: %s)", pod.Status.Phase))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			GinkgoWriter.Println("Load generation job is running")

			// wait for load generation to ramp up (30 seconds)
			// This gives the load time to build up before checking for scale-up
			By("Waiting for load generation to ramp up (30 seconds)")
			time.Sleep(30 * time.Second)

			By("Waiting for VA to detect saturation and recommend scale-up")
			var desiredReplicas int
			checkCount := 0
			scaleUpTimeout := 7 * time.Minute // Allow time for metrics propagation (e.g. KEDA/Prometheus)
			// Store loadCfg in closure for progress logging
			loadConfig := loadCfg
			Eventually(func(g Gomega) {
				checkCount++
				elapsed := time.Since(loadStartTime)
				remaining := scaleUpTimeout - elapsed

				// Get VA status
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				desiredReplicas = va.Status.DesiredOptimizedAlloc.NumReplicas
				metricsAvailable := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
				metricsStatus := "Unknown"
				metricsReason := ""
				if metricsAvailable != nil {
					metricsStatus = string(metricsAvailable.Status)
					metricsReason = metricsAvailable.Reason
				}

				// Get HPA status (name differs by backend: we create hpaName-hpa, KEDA creates keda-hpa-<so-name>)
				hpa, hpaErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, effectiveHpaName, metav1.GetOptions{})
				hpaDesired := int32(0)
				hpaCurrent := int32(0)
				if hpaErr == nil {
					hpaDesired = hpa.Status.DesiredReplicas
					hpaCurrent = hpa.Status.CurrentReplicas
				}

				// Get deployment status
				deployment, deployErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
				deploySpec := int32(0)
				deployReady := int32(0)
				if deployErr == nil {
					if deployment.Spec.Replicas != nil {
						deploySpec = *deployment.Spec.Replicas
					}
					deployReady = deployment.Status.ReadyReplicas
				}

				// Get load job status with better pod status reporting
				job, jobErr := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
				jobSucceeded := int32(0)
				jobFailed := int32(0)
				jobActive := int32(0)
				loadStatus := "Unknown"
				loadReason := ""
				if jobErr == nil {
					jobSucceeded = job.Status.Succeeded
					jobFailed = job.Status.Failed
					jobActive = job.Status.Active
				}

				podList, podErr := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("job-name=%s", jobName),
				})
				if podErr == nil && len(podList.Items) > 0 {
					pod := podList.Items[0]
					loadStatus = string(pod.Status.Phase)
					// Get actual reason from pod conditions or container states
					if pod.Status.Phase == corev1.PodPending {
						// Check PodScheduled condition first
						for _, condition := range pod.Status.Conditions {
							if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
								loadReason = condition.Reason
								break
							}
						}
						// If no PodScheduled reason, check container waiting states
						if loadReason == "" {
							for _, containerStatus := range pod.Status.ContainerStatuses {
								if containerStatus.State.Waiting != nil {
									loadReason = containerStatus.State.Waiting.Reason
									break
								}
							}
						}
					}
					if loadReason == "" {
						loadReason = "Running" // Pod is running or succeeded
					}
				}

				// Calculate expected load duration and progress
				var expectedDuration time.Duration
				if loadConfig.RequestRate > 0 {
					expectedDuration = time.Duration(loadConfig.NumPrompts/loadConfig.RequestRate+10) * time.Second
				} else {
					numBatches := (loadConfig.NumPrompts + 9) / 10
					expectedDuration = time.Duration(numBatches*1+10) * time.Second
				}
				loadProgress := ""
				if expectedDuration.Seconds() > 0 && elapsed < expectedDuration {
					progressPct := int((elapsed.Seconds() / expectedDuration.Seconds()) * 100)
					if progressPct > 100 {
						progressPct = 100
					}
					loadProgress = fmt.Sprintf(" (~%d%% of expected %v)", progressPct, expectedDuration.Round(time.Second))
				} else if elapsed >= expectedDuration {
					loadProgress = " (expected duration exceeded)"
				}

				// Compact, structured progress logging
				scaleUpDetected := desiredReplicas > int(initialOptimized)
				statusIndicator := "⏳"
				if scaleUpDetected {
					statusIndicator = "✓"
				}
				GinkgoWriter.Printf("[%s Progress %d] %v elapsed | %v remaining\n", statusIndicator, checkCount, elapsed.Round(time.Second), remaining.Round(time.Second))
				GinkgoWriter.Printf("  VA: %d replicas (initial: %d) | Metrics: %s/%s | LastRun: %v\n",
					desiredReplicas, initialOptimized, metricsStatus, metricsReason, va.Status.DesiredOptimizedAlloc.LastRunTime.Format("15:04:05"))
				GinkgoWriter.Printf("  HPA: Desired=%d | Current=%d | Deployment: Spec=%d | Ready=%d\n",
					hpaDesired, hpaCurrent, deploySpec, deployReady)
				// Format load config display - show "burst" pattern if RequestRate is 0
				loadConfigDisplay := ""
				if loadConfig.RequestRate > 0 {
					loadConfigDisplay = fmt.Sprintf("%d req/s", loadConfig.RequestRate)
				} else {
					loadConfigDisplay = "burst pattern"
				}
				GinkgoWriter.Printf("  Load: Phase=%s", loadStatus)
				if loadReason != "" && loadReason != "Running" {
					GinkgoWriter.Printf(" (Reason: %s)", loadReason)
				}
				GinkgoWriter.Printf(" | Config: %s, %d prompts | Active=%d | Succeeded=%d | Failed=%d%s\n",
					loadConfigDisplay, loadConfig.NumPrompts, jobActive, jobSucceeded, jobFailed, loadProgress)

				// Log detailed status every 30 seconds (every 3rd check) or when scale-up detected
				if checkCount%3 == 0 || scaleUpDetected {
					if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
						GinkgoWriter.Printf("  └─ Accelerator: %s", va.Status.DesiredOptimizedAlloc.Accelerator)
					}
					if metricsAvailable != nil && metricsAvailable.Message != "" {
						GinkgoWriter.Printf(" | Metrics: %s", metricsAvailable.Message)
					}
					if hpaErr == nil && len(hpa.Status.Conditions) > 0 {
						for _, cond := range hpa.Status.Conditions {
							if cond.Type == autoscalingv2.AbleToScale {
								GinkgoWriter.Printf(" | HPA: %s/%s", cond.Status, cond.Reason)
								break
							}
						}
					}
					GinkgoWriter.Println()
				}

				// VA should detect saturation and recommend more replicas than initial, or (if we didn't settle at minReplicas)
				// at least recommend a scaled state (>= 2) under load.
				if settled {
					g.Expect(desiredReplicas).To(BeNumerically(">", int(initialOptimized)),
						fmt.Sprintf("VA should recommend more replicas than initial under load (current: %d, initial: %d, elapsed: %v)", desiredReplicas, initialOptimized, elapsed))
				} else {
					// Initial was already above minReplicas; accept that VA recommends at least 2 under load
					g.Expect(desiredReplicas).To(BeNumerically(">=", 2),
						fmt.Sprintf("VA should recommend at least 2 replicas under load when initial was %d (current: %d, elapsed: %v)", initialOptimized, desiredReplicas, elapsed))
					g.Expect(desiredReplicas).To(BeNumerically(">=", int(minReplicas)),
						fmt.Sprintf("VA should recommend at least minReplicas under load (current: %d, minReplicas: %d)", desiredReplicas, minReplicas))
				}
			}, scaleUpTimeout, 10*time.Second).Should(Succeed())

			GinkgoWriter.Printf("✓ VA detected saturation and recommended %d replicas (took %v)\n", desiredReplicas, time.Since(loadStartTime))
			GinkgoWriter.Printf("  → VA scale-up detected! Now verifying HPA and deployment scaling...\n")

			if cfg.ScalerBackend == "keda" {
				// With KEDA, the ScaledObject's Prometheus metric may not be read by the KEDA-created HPA
				// in time in the test env (e.g. metric format or polling). Smoke test verifies VA recommended
				// scale-up and that the KEDA HPA exists and tracks the deployment.
				By("Verifying KEDA HPA exists and has valid status (skipping desired-replicas check)")
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, effectiveHpaName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(hpa.Status.CurrentReplicas).To(BeNumerically(">=", minReplicas),
					"KEDA HPA should report current replicas >= minReplicas")
				GinkgoWriter.Printf("✓ KEDA HPA exists: Desired=%d, Current=%d (VA recommended %d)\n",
					hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas, desiredReplicas)
			} else {
				By("Verifying HPA reads the metric and updates desired replicas")
				hpaCheckStart := time.Now()
				Eventually(func(g Gomega) {
					hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, effectiveHpaName, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					elapsed := time.Since(hpaCheckStart)
					GinkgoWriter.Printf("  HPA check: Desired=%d | Current=%d (elapsed: %v)\n",
						hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas, elapsed.Round(time.Second))
					g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">", 1),
						"HPA should have desired replicas > 1 after reading scale-up metric")
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
				GinkgoWriter.Printf("✓ HPA updated desired replicas to > 1 (took %v)\n", time.Since(hpaCheckStart))

				// verify deployment actually scales up to match VA recommendation
				By("Waiting for deployment to scale up and new pods to be ready")
				deployCheckStart := time.Now()
				Eventually(func(g Gomega) {
					deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					elapsed := time.Since(deployCheckStart)
					var specReplicas int32
					if deployment.Spec.Replicas != nil {
						specReplicas = *deployment.Spec.Replicas
					}
					GinkgoWriter.Printf("  Deployment check: Spec=%d | Replicas=%d | Ready=%d | VA recommended=%d (elapsed: %v)\n",
						specReplicas, deployment.Status.Replicas, deployment.Status.ReadyReplicas, desiredReplicas, elapsed.Round(time.Second))

					g.Expect(deployment.Status.Replicas).To(BeNumerically(">", minReplicas),
						fmt.Sprintf("Deployment should have more total replicas than minReplicas under load (current: %d, min: %d)", deployment.Status.Replicas, minReplicas))
					g.Expect(int32(deployment.Status.ReadyReplicas)).To(BeNumerically(">=", int32(desiredReplicas)),
						fmt.Sprintf("Deployment should have at least %d ready replicas to match VA recommendation (current: %d)", desiredReplicas, deployment.Status.ReadyReplicas))
				}, 10*time.Minute, 10*time.Second).Should(Succeed())
				deployment, _ := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
				GinkgoWriter.Printf("✓ Deployment successfully scaled up under load (took %v)\n", time.Since(deployCheckStart))
				GinkgoWriter.Printf("  Final state: VA recommended %d replicas, deployment has %d ready pods\n", desiredReplicas, deployment.Status.ReadyReplicas)

				By("Verifying at least one additional pod becomes ready")
				Eventually(func(g Gomega) {
					pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
						LabelSelector: "app=" + modelServiceName + "-decode",
					})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(len(pods.Items)).To(BeNumerically(">", 1), "Should have more than 1 pod after scale-up")

					readyCount := 0
					for _, pod := range pods.Items {
						for _, condition := range pod.Status.Conditions {
							if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
								readyCount++
								break
							}
						}
					}
					g.Expect(readyCount).To(BeNumerically(">", 1), "At least 2 pods should be ready after scale-up")
				}, 5*time.Minute, 10*time.Second).Should(Succeed())
			}

			GinkgoWriter.Printf("Deployment successfully scaled up under load\n")
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
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

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
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("Verifying VA continues to exist after deployment deletion")
			// The VA should continue to exist even when the deployment is deleted
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Name:      errorTestVAName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred(), "VA should continue to exist after deployment deletion")

			// Note: The controller may not immediately detect deployment deletion due to caching.
			// The TargetResolved=False functionality is already verified in target_condition_test.go
			// which creates a VA with a non-existent deployment. For smoke tests, we verify that:
			// 1. Deployment can be deleted
			// 2. VA continues to exist
			// 3. VA can resume operation when deployment is recreated

			By("Recreating the deployment")
			err = fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, errorTestModelServiceName, errorTestPoolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to recreate model service")

			By("Waiting for deployment to be created and progressing")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "Deployment should be created")
				// Verify deployment exists and is progressing (may not be ready yet)
				g.Expect(deployment.Status.Replicas).To(BeNumerically(">=", 0), "Deployment should have replica status")
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("Waiting for deployment to be ready (with extended timeout for recreation)")
			// When recreating, pods may take longer to start (image pull, etc.)
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)),
					"Model service should have 1 ready replica after recreation")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

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
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

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
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
			} else {
				// If not populated, that's okay - Engine may not have run yet
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run due to missing metrics)\n")
			}
		})
	})
})
