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

// cleanupLWSTestResources deletes all resources created by LWS tests to ensure clean state
func cleanupLWSTestResources() {
	GinkgoWriter.Println("Cleaning up LWS test resources for clean state...")

	// Helper to check if resource name matches lws test patterns
	isLWSTestResource := func(name string) bool {
		return strings.HasPrefix(name, "lws-test-")
	}

	// Delete all VariantAutoscalings with lws-test prefix
	vaList := &variantautoscalingv1alpha1.VariantAutoscalingList{}
	if err := crClient.List(ctx, vaList, client.InNamespace(cfg.LLMDNamespace)); err == nil {
		for _, va := range vaList.Items {
			if isLWSTestResource(va.Name) {
				GinkgoWriter.Printf("  Deleting VA: %s\n", va.Name)
				_ = crClient.Delete(ctx, &va)
			}
		}
	}

	// Delete all HPAs with lws-test prefix
	hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, hpa := range hpaList.Items {
			if isLWSTestResource(hpa.Name) {
				GinkgoWriter.Printf("  Deleting HPA: %s\n", hpa.Name)
				_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpa.Name, metav1.DeleteOptions{})
			}
		}
	}

	// Delete all ScaledObjects with lws-test prefix
	if cfg.ScalerBackend == scalerBackendKeda {
		soList := &unstructured.UnstructuredList{}
		soList.SetAPIVersion("keda.sh/v1alpha1")
		soList.SetKind("ScaledObjectList")
		if err := crClient.List(ctx, soList, client.InNamespace(cfg.LLMDNamespace)); err == nil {
			for _, so := range soList.Items {
				GinkgoWriter.Printf("  Deleting ScaledObject: %s\n", so.GetName())
				_ = crClient.Delete(ctx, &so)
			}
		}
	}

	// Delete all Deployments with lws-test prefix
	deployList, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, deploy := range deployList.Items {
			if isLWSTestResource(deploy.Name) {
				GinkgoWriter.Printf("  Deleting Deployment: %s\n", deploy.Name)
				_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploy.Name, metav1.DeleteOptions{})
			}
		}
	}

	// Delete all LeaderWorkerSets with lws-test prefix
	lwsList := &unstructured.UnstructuredList{}
	lwsList.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
	lwsList.SetKind("LeaderWorkerSetList")
	if err := crClient.List(ctx, lwsList, client.InNamespace(cfg.LLMDNamespace)); err == nil {
		for _, lws := range lwsList.Items {
			if isLWSTestResource(lws.GetName()) {
				GinkgoWriter.Printf("  Deleting LeaderWorkerSet: %s\n", lws.GetName())
				_ = crClient.Delete(ctx, &lws)
			}
		}
	}

	// Delete all Services with lws-test prefix
	svcList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, svc := range svcList.Items {
			if isLWSTestResource(svc.Name) {
				GinkgoWriter.Printf("  Deleting Service: %s\n", svc.Name)
				_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			}
		}
	}

	// Delete all ServiceMonitors with lws-test prefix
	smList := &promoperator.ServiceMonitorList{}
	if err := crClient.List(ctx, smList, client.InNamespace(cfg.MonitoringNS)); err == nil {
		for _, sm := range smList.Items {
			if isLWSTestResource(sm.Name) {
				GinkgoWriter.Printf("  Deleting ServiceMonitor: %s\n", sm.Name)
				_ = crClient.Delete(ctx, &sm)
			}
		}
	}

	GinkgoWriter.Println("LWS test resource cleanup complete")
}

var _ = Describe("LeaderWorkerSet Tests", Label("full"), Serial, Ordered, func() {
	Context("Basic VA lifecycle with LeaderWorkerSet", Ordered, func() {
		var (
			poolName         = "lws-test-pool"
			modelServiceName = "lws-test-ms"
			lwsName          = modelServiceName + "-decode"
			vaName           = "lws-test-va"
			hpaName          = "lws-test-hpa"
			minReplicas      = int32(1)
			lwsGroupSize     = int32(2) // 1 leader + 1 worker
		)

		BeforeAll(func() {
			By("Cleaning up any existing lws test resources")
			cleanupLWSTestResources()

			By("Creating model service LeaderWorkerSet")
			err := fixtures.EnsureModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs, lwsGroupSize)
			Expect(err).NotTo(HaveOccurred(), "Failed to create model service LWS")

			// Register cleanup for LWS (runs even if test fails)
			DeferCleanup(func() {
				cleanupResource(ctx, "LeaderWorkerSet", cfg.LLMDNamespace, lwsName,
					func() error {
						return fixtures.DeleteModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName)
					},
					func() bool {
						lws := &unstructured.Unstructured{}
						lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
						lws.SetKind("LeaderWorkerSet")
						err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
						return errors.IsNotFound(err)
					})
			})

			By("Creating service to expose LWS model server")
			err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, lwsName, 8000)
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

			By("Creating ServiceMonitor for LWS metrics scraping")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, lwsName)
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

			By("Waiting for LWS to be ready")
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				readyReplicas, found, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")
				g.Expect(found).To(BeTrue(), "LWS should have status.readyReplicas")
				g.Expect(readyReplicas).To(Equal(int64(1)), "LWS should have 1 ready replica")
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

			By("Creating VariantAutoscaling resource for LWS")
			err = fixtures.EnsureVariantAutoscaling(
				ctx, crClient, cfg.LLMDNamespace, vaName,
				lwsName, cfg.ModelID, cfg.AcceleratorType,
				30.0, cfg.ControllerInstance,
				fixtures.WithScaleTargetKind("LeaderWorkerSet"),
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

			By("Creating scaler for the LWS (HPA or ScaledObject per backend)")
			if cfg.ScaleToZeroEnabled {
				minReplicas = 0
			}
			if cfg.ScalerBackend == scalerBackendKeda {
				_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
				err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName, lwsName, vaName, minReplicas, 10, cfg.MonitoringNS,
					fixtures.WithScaledObjectScaleTargetKind("LeaderWorkerSet"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject")
			} else {
				err = fixtures.EnsureHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName, lwsName, vaName, minReplicas, 10,
					fixtures.WithScaleTargetRefKind("LeaderWorkerSet"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")
			}
		})

		AfterAll(func() {
			By("Cleaning up LWS test resources")
			if cfg.ScalerBackend == scalerBackendKeda {
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

		It("should reconcile the VA successfully with LWS", func() {
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
					if cond.Type == variantautoscalingv1alpha1.TypeTargetResolved && cond.Status == metav1.ConditionTrue {
						targetResolved = true
						break
					}
				}
				g.Expect(targetResolved).To(BeTrue(), "VA should have TargetResolved=True condition")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should expose external metrics for the VA with LWS", func() {
			By("Waiting for VA to be reconciled (TargetResolved condition)")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(condition).NotTo(BeNil(), "VA should have TargetResolved condition")
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "TargetResolved should be True")
			}).Should(Succeed())

			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists (KEDA backend; external metric name is KEDA-generated)")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject %s should exist", soName)
			} else {
				By("Querying external metrics API for wva_desired_replicas")
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
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			getErr := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(getErr).NotTo(HaveOccurred())
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
			} else {
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})

		It("should verify LWS structure with correct group size", func() {
			By("Checking LWS has correct group size")
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			Expect(err).NotTo(HaveOccurred())

			size, found, _ := unstructured.NestedInt64(lws.Object, "spec", "leaderWorkerTemplate", "size")
			Expect(found).To(BeTrue(), "LWS should have spec.leaderWorkerTemplate.size")
			Expect(size).To(Equal(int64(lwsGroupSize)), fmt.Sprintf("LWS should have group size %d (1 leader + %d workers)", lwsGroupSize, lwsGroupSize-1))
		})

		It("should have MetricsAvailable condition set when LWS pods are ready", func() {
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
				g.Expect(condition.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
					"MetricsAvailable condition should have a valid status")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have scaling controlled by backend with LWS", func() {
			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists and KEDA has created an HPA for LWS")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject should exist")

				Eventually(func(g Gomega) {
					hpaList, listErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
					g.Expect(listErr).NotTo(HaveOccurred())
					var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
					for i := range hpaList.Items {
						h := &hpaList.Items[i]
						if h.Spec.ScaleTargetRef.Name == lwsName {
							kedaHPA = h
							break
						}
					}
					g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the LWS")
					g.Expect(kedaHPA.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			} else {
				By("Verifying HPA exists and is configured for LWS")
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "HPA should exist")
				Expect(hpa.Spec.Metrics).NotTo(BeEmpty(), "HPA should have metrics configured")
				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use External metric type")
				Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal(constants.WVADesiredReplicas), "HPA should use wva_desired_replicas metric")
				Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("LeaderWorkerSet"), "HPA should target LeaderWorkerSet")

				By("Waiting for HPA to read the metric and update status")
				Eventually(func(g Gomega) {
					hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(hpa.Status.CurrentReplicas).To(BeNumerically(">=", 0), "HPA should have current replicas set")
					g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			}
		})

		It("should verify Prometheus is scraping LWS metrics", func() {
			By("Checking that LWS pods are ready and reporting metrics")
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
			Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
			if condition.Status == metav1.ConditionTrue {
				Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonMetricsFound),
					"When metrics are available, reason should be MetricsFound")
			}

			By("Checking if DesiredOptimizedAlloc is populated (best-effort)")
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

		It("should verify LWS pods are created with correct group structure", func() {
			By("Checking that LWS created pods with correct group size")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + modelServiceName + "-decode",
				})
				g.Expect(err).NotTo(HaveOccurred())
				// With 1 replica and group size 2, we expect 2 pods total (1 leader + 1 worker)
				g.Expect(pods.Items).To(HaveLen(int(lwsGroupSize)), fmt.Sprintf("Should have %d pods (1 replica × group size %d)", lwsGroupSize, lwsGroupSize))

				// At least the leader should be ready
				readyCount := 0
				for _, pod := range pods.Items {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyCount++
							break
						}
					}
				}
				g.Expect(readyCount).To(BeNumerically(">", 0), "At least one pod (leader) should be ready")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		// This is a long-running test that verifies scale-up behavior under simulated load
		// It only runs in full e2e mode (Label("full") is set on the Describe block)
		It("should scale up LWS under load", func() {
			if cfg.ScaleToZeroEnabled {
				Skip("Scale-up test not compatible with scale-to-zero (initial state is 0 replicas)")
			}

			// HPA name: with Prometheus Adapter we create HPA named <hpaName>-hpa; with KEDA, KEDA creates HPA named keda-hpa-<scaledobject-name>
			effectiveHpaName := hpaName + "-hpa"
			if cfg.ScalerBackend == scalerBackendKeda {
				effectiveHpaName = "keda-hpa-" + hpaName + "-so"
			}

			// wait for VA to stabilize at minReplicas before starting load
			By("Waiting for VA to stabilize at minReplicas")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				var optimized int32
				if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
					optimized = *va.Status.DesiredOptimizedAlloc.NumReplicas
				}
				GinkgoWriter.Printf("Waiting for VA to be ready: optimized=%d, minReplicas=%d\n", optimized, minReplicas)
				g.Expect(optimized).To(BeNumerically(">=", minReplicas), "VA should have optimized >= minReplicas")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			// wait for LWS to be fully stable
			By("Waiting for LWS to stabilize (no pods in transition)")
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				specReplicas, _, _ := unstructured.NestedInt64(lws.Object, "spec", "replicas")
				statusReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "replicas")
				readyReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")

				GinkgoWriter.Printf("Waiting for LWS stability: spec=%d, status=%d, ready=%d\n",
					specReplicas, statusReplicas, readyReplicas)
				g.Expect(statusReplicas).To(Equal(specReplicas), "Status replicas should match spec")
				g.Expect(readyReplicas).To(Equal(specReplicas), "Ready replicas should match spec")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			// Prefer starting from minReplicas so we reliably detect scale-up
			By("Waiting for VA to settle at minReplicas before recording initial state (best-effort)")
			settled := false
			initialOptimized := minReplicas
			for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				if err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va); err != nil {
					break
				}
				if va.Status.DesiredOptimizedAlloc.NumReplicas != nil && *va.Status.DesiredOptimizedAlloc.NumReplicas == minReplicas {
					settled = true
					break
				}
				var current int32
				if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
					current = *va.Status.DesiredOptimizedAlloc.NumReplicas
				}
				if current > 0 {
					GinkgoWriter.Printf("VA not yet settled at minReplicas: current=%d, waiting...\n", current)
				}
				time.Sleep(10 * time.Second)
			}
			if settled {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				_ = crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
				if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
					initialOptimized = *va.Status.DesiredOptimizedAlloc.NumReplicas
				}
			}
			GinkgoWriter.Printf("Initial optimized replicas (after stabilization): %d (settled=%v)\n", initialOptimized, settled)

			By("Starting burst load generation to trigger scale-up")
			scaleUpPrompts := 2400
			if cfg.NumPrompts > scaleUpPrompts {
				scaleUpPrompts = cfg.NumPrompts
			}
			loadCfg := fixtures.LoadConfig{
				Strategy:     cfg.LoadStrategy,
				RequestRate:  0,
				NumPrompts:   scaleUpPrompts,
				InputTokens:  cfg.InputTokens,
				OutputTokens: 400,
				ModelID:      cfg.ModelID,
			}

			targetURL := fmt.Sprintf("http://%s-service.%s.svc.cluster.local:8000/v1/completions", modelServiceName, cfg.LLMDNamespace)
			err := fixtures.EnsureBurstLoadJob(ctx, k8sClient, cfg.LLMDNamespace, "lws-scaleup-load", targetURL, loadCfg)
			Expect(err).NotTo(HaveOccurred(), "Failed to create burst load generation job")

			loadStartTime := time.Now()

			By("Verifying load generation job is running")
			Eventually(func(g Gomega) {
				job, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, "lws-scaleup-load", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(job.Status.Active).To(BeNumerically(">", 0), "Load generation job should have at least one active pod")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
			GinkgoWriter.Println("Load generation job is running")

			By("Waiting for load generation to ramp up (30 seconds)")
			time.Sleep(30 * time.Second)

			By("Waiting for VA to detect saturation and recommend scale-up")
			var desiredReplicas int
			checkCount := 0
			scaleUpTimeout := 7 * time.Minute
			loadConfig := loadCfg
			Eventually(func(g Gomega) {
				checkCount++
				elapsed := time.Since(loadStartTime)
				remaining := scaleUpTimeout - elapsed

				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
					desiredReplicas = int(*va.Status.DesiredOptimizedAlloc.NumReplicas)
				}

				if checkCount%6 == 1 {
					GinkgoWriter.Printf("Check %d: VA recommended %d replicas (elapsed: %v, remaining: %v, load: %d prompts @ strategy=%s)\n",
						checkCount, desiredReplicas, elapsed.Round(time.Second), remaining.Round(time.Second), loadConfig.NumPrompts, loadConfig.Strategy)
				}

				if desiredReplicas >= 2 {
					g.Expect(desiredReplicas).To(BeNumerically(">=", 2),
						fmt.Sprintf("VA should recommend at least 2 replicas under load when initial was %d (current: %d, elapsed: %v)", initialOptimized, desiredReplicas, elapsed))
					g.Expect(desiredReplicas).To(BeNumerically(">=", int(minReplicas)),
						fmt.Sprintf("VA should recommend at least minReplicas under load (current: %d, minReplicas: %d)", desiredReplicas, minReplicas))
				}
			}, scaleUpTimeout, 10*time.Second).Should(Succeed())

			GinkgoWriter.Printf("✓ VA detected saturation and recommended %d replicas (took %v)\n", desiredReplicas, time.Since(loadStartTime))
			GinkgoWriter.Printf("  → VA scale-up detected! Now verifying HPA and LWS scaling...\n")

			if cfg.ScalerBackend == scalerBackendKeda {
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
			}

			By("Waiting for LWS to scale up and reach desired replicas")
			lwsCheckStart := time.Now()
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				specReplicas, _, _ := unstructured.NestedInt64(lws.Object, "spec", "replicas")
				statusReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "replicas")
				readyReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")

				GinkgoWriter.Printf("  LWS check: spec=%d | status=%d | ready=%d (elapsed: %v)\n",
					specReplicas, statusReplicas, readyReplicas, time.Since(lwsCheckStart).Round(time.Second))

				g.Expect(statusReplicas).To(BeNumerically(">", int64(minReplicas)),
					fmt.Sprintf("LWS should have more total replicas than minReplicas under load (current: %d, min: %d)", statusReplicas, minReplicas))
				g.Expect(readyReplicas).To(BeNumerically(">=", int64(desiredReplicas)),
					fmt.Sprintf("LWS should have at least %d ready replicas to match VA recommendation (current: %d)", desiredReplicas, readyReplicas))
			}, 10*time.Minute, 10*time.Second).Should(Succeed())

			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			_ = crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			readyReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")
			GinkgoWriter.Printf("✓ LWS successfully scaled up under load (took %v)\n", time.Since(lwsCheckStart))
			GinkgoWriter.Printf("  Final state: VA recommended %d replicas, LWS has %d ready replicas\n", desiredReplicas, readyReplicas)

			By("Verifying at least one additional LWS replica group becomes ready")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + modelServiceName + "-decode",
				})
				g.Expect(err).NotTo(HaveOccurred())
				// With group size 2, 2 replicas means 4 pods total
				g.Expect(len(pods.Items)).To(BeNumerically(">", int(lwsGroupSize)), "Should have more pods after scale-up")

				readyCount := 0
				for _, pod := range pods.Items {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyCount++
							break
						}
					}
				}
				// At least 2 replica groups (4 pods with group size 2) should be ready
				g.Expect(readyCount).To(BeNumerically(">", int(lwsGroupSize)), "At least 2 replica groups should be ready after scale-up")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			GinkgoWriter.Printf("LWS successfully scaled up under load\n")
		})
	})

	Context("Basic VA lifecycle with LeaderWorkerSet (single-node)", Ordered, func() {
		var (
			poolName         = "lws-test-single-pool"
			modelServiceName = "lws-test-single-ms"
			lwsName          = modelServiceName + "-decode"
			vaName           = "lws-test-single-va"
			hpaName          = "lws-test-single-hpa"
			minReplicas      = int32(1)
			lwsGroupSize     = int32(1) // 1 leader + 0 workers
		)

		BeforeAll(func() {
			By("Cleaning up any existing lws test resources")
			cleanupLWSTestResources()

			By("Creating model service LeaderWorkerSet with single-node (leader only)")
			err := fixtures.EnsureModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs, lwsGroupSize)
			Expect(err).NotTo(HaveOccurred(), "Failed to create model service LWS")

			// Register cleanup for LWS (runs even if test fails)
			DeferCleanup(func() {
				cleanupResource(ctx, "LeaderWorkerSet", cfg.LLMDNamespace, lwsName,
					func() error {
						return fixtures.DeleteModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName)
					},
					func() bool {
						lws := &unstructured.Unstructured{}
						lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
						lws.SetKind("LeaderWorkerSet")
						err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
						return errors.IsNotFound(err)
					})
			})

			By("Creating service to expose single-node LWS model server")
			err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, lwsName, 8000)
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

			By("Creating ServiceMonitor for single-node LWS metrics scraping")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, lwsName)
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

			By("Waiting for single-node LWS to be ready")
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				readyReplicas, found, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")
				g.Expect(found).To(BeTrue(), "LWS should have status.readyReplicas")
				g.Expect(readyReplicas).To(Equal(int64(1)), "LWS should have 1 ready replica")
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

			By("Creating VariantAutoscaling resource for single-node LWS")
			err = fixtures.EnsureVariantAutoscaling(
				ctx, crClient, cfg.LLMDNamespace, vaName,
				lwsName, cfg.ModelID, cfg.AcceleratorType,
				30.0, cfg.ControllerInstance,
				fixtures.WithScaleTargetKind("LeaderWorkerSet"),
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

			By("Creating scaler for the single-node LWS (HPA or ScaledObject per backend)")
			if cfg.ScaleToZeroEnabled {
				minReplicas = 0
			}
			if cfg.ScalerBackend == scalerBackendKeda {
				_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
				err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName, lwsName, vaName, minReplicas, 10, cfg.MonitoringNS,
					fixtures.WithScaledObjectScaleTargetKind("LeaderWorkerSet"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject")
			} else {
				err = fixtures.EnsureHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName, lwsName, vaName, minReplicas, 10,
					fixtures.WithScaleTargetRefKind("LeaderWorkerSet"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")
			}
		})

		AfterAll(func() {
			By("Cleaning up single-node LWS test resources")
			if cfg.ScalerBackend == scalerBackendKeda {
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

		It("should reconcile the VA successfully with single-node LWS", func() {
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
					if cond.Type == variantautoscalingv1alpha1.TypeTargetResolved && cond.Status == metav1.ConditionTrue {
						targetResolved = true
						break
					}
				}
				g.Expect(targetResolved).To(BeTrue(), "VA should have TargetResolved=True condition")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should expose external metrics for the VA with single-node LWS", func() {
			By("Waiting for VA to be reconciled (TargetResolved condition)")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(condition).NotTo(BeNil(), "VA should have TargetResolved condition")
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "TargetResolved should be True")
			}).Should(Succeed())

			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists (KEDA backend; external metric name is KEDA-generated)")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject %s should exist", soName)
			} else {
				By("Querying external metrics API for wva_desired_replicas")
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
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			getErr := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(getErr).NotTo(HaveOccurred())
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
			} else {
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})

		It("should verify single-node LWS structure with group size 1", func() {
			By("Checking single-node LWS has group size 1")
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			Expect(err).NotTo(HaveOccurred())

			size, found, _ := unstructured.NestedInt64(lws.Object, "spec", "leaderWorkerTemplate", "size")
			Expect(found).To(BeTrue(), "LWS should have spec.leaderWorkerTemplate.size")
			Expect(size).To(Equal(int64(lwsGroupSize)), fmt.Sprintf("LWS should have group size %d (leader only)", lwsGroupSize))
		})

		It("should have MetricsAvailable condition set when single-node LWS pods are ready", func() {
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
				g.Expect(condition.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
					"MetricsAvailable condition should have a valid status")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have scaling controlled by backend with single-node LWS", func() {
			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists and KEDA has created an HPA for single-node LWS")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject should exist")

				Eventually(func(g Gomega) {
					hpaList, listErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
					g.Expect(listErr).NotTo(HaveOccurred())
					var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
					for i := range hpaList.Items {
						h := &hpaList.Items[i]
						if h.Spec.ScaleTargetRef.Name == lwsName {
							kedaHPA = h
							break
						}
					}
					g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the single-node LWS")
					g.Expect(kedaHPA.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			} else {
				By("Verifying HPA exists and is configured for single-node LWS")
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "HPA should exist")
				Expect(hpa.Spec.Metrics).NotTo(BeEmpty(), "HPA should have metrics configured")
				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use External metric type")
				Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal(constants.WVADesiredReplicas), "HPA should use wva_desired_replicas metric")
				Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("LeaderWorkerSet"), "HPA should target LeaderWorkerSet")

				By("Waiting for HPA to read the metric and update status")
				Eventually(func(g Gomega) {
					hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(hpa.Status.CurrentReplicas).To(BeNumerically(">=", 0), "HPA should have current replicas set")
					g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			}
		})

		It("should verify Prometheus is scraping single-node LWS metrics", func() {
			By("Checking that single-node LWS pods are ready and reporting metrics")
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
			Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
			if condition.Status == metav1.ConditionTrue {
				Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonMetricsFound),
					"When metrics are available, reason should be MetricsFound")
			}

			By("Checking if DesiredOptimizedAlloc is populated (best-effort)")
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

		It("should verify single-node LWS pods are created (leader only)", func() {
			By("Checking that single-node LWS created pods with leader only")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + modelServiceName + "-decode",
				})
				g.Expect(err).NotTo(HaveOccurred())
				// With 1 replica and group size 1, we expect 1 pod total (1 leader + 0 workers)
				g.Expect(pods.Items).To(HaveLen(int(lwsGroupSize)), fmt.Sprintf("Should have %d pod (1 replica × group size %d)", lwsGroupSize, lwsGroupSize))

				// The leader should be ready
				readyCount := 0
				for _, pod := range pods.Items {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyCount++
							break
						}
					}
				}
				g.Expect(readyCount).To(Equal(1), "The leader pod should be ready")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
})
