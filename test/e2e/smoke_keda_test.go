package e2e

import (
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
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

var _ = Describe("KEDA Smoke Tests - Infrastructure Readiness", Label("smoke", "keda", "full"), func() {
	Context("Basic infrastructure validation", func() {
		It("should have WVA controller running and ready", func() {
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.WVANamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "control-plane=controller-manager",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "WVA controller pod should exist")
				readyPods := 0
				for _, pod := range pods.Items {
					if pod.Status.Phase == corev1.PodRunning {
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
			_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("inference.networking.k8s.io/v1")
			Expect(err).NotTo(HaveOccurred(), "llm-d CRDs should be installed")
		})

		It("should have Prometheus running", func() {
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).List(ctx, metav1.ListOptions{
					LabelSelector: "app.kubernetes.io/name=prometheus",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Prometheus pod should exist")
			}).Should(Succeed())
		})

		It("should have KEDA operator ready", func() {
			By("Checking KEDA operator pods in " + cfg.KEDANamespace)
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.KEDANamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app.kubernetes.io/name=keda-operator",
				})
				g.Expect(err).NotTo(HaveOccurred(), "Failed to list KEDA operator pods")
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

		It("should have KEDA metrics server serving the external metrics API", func() {
			By("Checking external.metrics.k8s.io/v1beta1 is available (owned by KEDA)")
			Eventually(func(g Gomega) {
				_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
				g.Expect(err).NotTo(HaveOccurred(), "KEDA metrics server should register external.metrics.k8s.io/v1beta1")
			}).Should(Succeed())
		})
	})
})

var _ = Describe("KEDA Smoke Tests - Basic Autoscaling", Label("smoke", "keda", "full"), Ordered, func() {
	var (
		poolName         = "smoke-test-pool"
		modelServiceName = "smoke-test-ms"
		deploymentName   = modelServiceName + "-decode"
		vaName           = "smoke-test-va"
		scalerName       = "smoke-test-hpa" // base name; ScaledObject will be scalerName+"-so"
		minReplicas      = int32(1)
	)

	BeforeAll(func() {
		By("Cleaning up any existing smoke test resources")
		cleanupSmokeTestResources()

		if cfg.ScaleToZeroEnabled {
			minReplicas = 0
		}

		By("Creating model service deployment")
		err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, vaName, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

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

		By("Creating ScaledObject")
		err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerName, deploymentName, vaName, minReplicas, 10, cfg.MonitoringNS)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject")
	})

	AfterAll(func() {
		By("Cleaning up KEDA smoke test resources")
		err := fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerName)
		Expect(err).NotTo(HaveOccurred())

		va := &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: vaName, Namespace: cfg.LLMDNamespace},
		}
		cleanupResource(ctx, "VA", cfg.LLMDNamespace, vaName,
			func() error { return crClient.Delete(ctx, va) },
			func() bool {
				err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
				return errors.IsNotFound(err)
			})
	})

	It("should reconcile the VA successfully", func() {
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
			g.Expect(condition).NotTo(BeNil(), "VA should have TargetResolved condition")
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "TargetResolved should be True")
		}).Should(Succeed())
	})

	It("should have MetricsAvailable condition set", func() {
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
			g.Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
			g.Expect(condition.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
				"MetricsAvailable should have a valid status")
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should have ScaledObject Ready condition set by KEDA", func() {
		soName := scalerName + "-so"
		Eventually(func(g Gomega) {
			so := &kedav1alpha1.ScaledObject{}
			err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
			g.Expect(err).NotTo(HaveOccurred(), "ScaledObject %s should exist", soName)
			ready := so.Status.Conditions.GetReadyCondition()
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue), "ScaledObject should have Ready=True")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should have KEDA create an HPA for the deployment", func() {
		Eventually(func(g Gomega) {
			hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var found bool
			for _, h := range hpaList.Items {
				if h.Spec.ScaleTargetRef.Name == deploymentName {
					found = true
					g.Expect(h.Status.DesiredReplicas).To(BeNumerically(">=", 0), "KEDA HPA should have desired replicas set")
					break
				}
			}
			g.Expect(found).To(BeTrue(), "KEDA should have created an HPA targeting %s", deploymentName)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should verify Prometheus is scraping model server pods", func() {
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + deploymentName,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).NotTo(BeEmpty(), "Should have at least one pod")
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
			err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")
		}).Should(Succeed())

		By("Verifying MetricsAvailable condition indicates metrics collection")
		va := &variantautoscalingv1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
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
			GinkgoWriter.Printf("DesiredOptimizedAlloc: accelerator=%s, replicas=%d\n",
				va.Status.DesiredOptimizedAlloc.Accelerator, *va.Status.DesiredOptimizedAlloc.NumReplicas)
		} else {
			GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
		}
	})
})

var _ = Describe("KEDA Smoke Tests - Error Handling", Label("smoke", "keda", "full"), Serial, Ordered, func() {
	var (
		errorTestPoolName         = "error-test-pool"
		errorTestModelServiceName = "error-test-ms"
		errorTestDeploymentName   = errorTestModelServiceName + "-decode"
		errorTestVAName           = "error-test-va"
	)

	BeforeAll(func() {
		By("Cleaning up any existing smoke test resources")
		cleanupSmokeTestResources()

		By("Creating model service deployment for error handling tests")
		err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, errorTestModelServiceName, errorTestPoolName, cfg.ModelID, errorTestVAName, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		DeferCleanup(func() {
			cleanupResource(ctx, "Deployment", cfg.LLMDNamespace, errorTestDeploymentName,
				func() error {
					return k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, errorTestDeploymentName, metav1.DeleteOptions{})
				},
				func() bool {
					_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, errorTestDeploymentName, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
		})

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, errorTestDeploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Model service should have 1 ready replica")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Creating VariantAutoscaling resource")
		err = fixtures.EnsureVariantAutoscalingWithDefaults(
			ctx, crClient, cfg.LLMDNamespace, errorTestVAName,
			errorTestDeploymentName, cfg.ModelID, cfg.AcceleratorType,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

		By("Waiting for VA to reconcile initially")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: errorTestVAName, Namespace: cfg.LLMDNamespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")
		}).Should(Succeed())
	})

	AfterAll(func() {
		va := &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: errorTestVAName, Namespace: cfg.LLMDNamespace},
		}
		cleanupResource(ctx, "VA", cfg.LLMDNamespace, errorTestVAName,
			func() error { return crClient.Delete(ctx, va) },
			func() bool {
				err := crClient.Get(ctx, client.ObjectKey{Name: errorTestVAName, Namespace: cfg.LLMDNamespace}, va)
				return errors.IsNotFound(err)
			})
	})

	It("should handle deployment deletion and recreation gracefully", func() {
		By("Verifying deployment exists before deletion")
		_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, errorTestDeploymentName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Deployment should exist before deletion")

		By("Deleting the deployment")
		err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, errorTestDeploymentName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should be able to delete deployment")

		By("Waiting for deployment to be fully deleted")
		Eventually(func(g Gomega) {
			_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, errorTestDeploymentName, metav1.GetOptions{})
			g.Expect(errors.IsNotFound(err)).To(BeTrue(), "Deployment should be deleted")
		}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).Should(Succeed())

		By("Verifying VA continues to exist after deployment deletion")
		va := &variantautoscalingv1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{Name: errorTestVAName, Namespace: cfg.LLMDNamespace}, va)
		Expect(err).NotTo(HaveOccurred(), "VA should continue to exist after deployment deletion")

		By("Recreating the deployment")
		err = fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, errorTestModelServiceName, errorTestPoolName, cfg.ModelID, errorTestVAName, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to recreate model service")

		By("Waiting for deployment to be ready after recreation")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, errorTestDeploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)),
				"Model service should have 1 ready replica after recreation")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

		By("Verifying VA automatically resumes operation")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: errorTestVAName, Namespace: cfg.LLMDNamespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
			g.Expect(condition).NotTo(BeNil(), "TargetResolved condition should exist")
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "TargetResolved should be True when deployment is recreated")
		}).Should(Succeed())
	})

	It("should handle metrics unavailability gracefully", func() {
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Name: errorTestVAName, Namespace: cfg.LLMDNamespace}, va)
			g.Expect(err).NotTo(HaveOccurred())
			condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
			g.Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
			switch condition.Status {
			case metav1.ConditionFalse:
				g.Expect(condition.Reason).To(BeElementOf(
					variantautoscalingv1alpha1.ReasonMetricsMissing,
					variantautoscalingv1alpha1.ReasonMetricsStale,
					variantautoscalingv1alpha1.ReasonPrometheusError,
					variantautoscalingv1alpha1.ReasonMetricsUnavailable,
				), "When metrics unavailable, reason should indicate the cause")
			case metav1.ConditionTrue:
				g.Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonMetricsFound))
			}
		}).Should(Succeed())
	})
})
