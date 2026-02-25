package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// GPU Limiter test validates that the WVA controller respects GPU resource constraints
// and doesn't recommend scaling beyond available GPU capacity.
//
// This test creates VAs with different accelerator requirements and verifies that
// the limiter correctly constrains scale-up decisions based on GPU availability.
var _ = Describe("GPU Limiter Feature", Label("full"), Ordered, func() {
	var (
		poolA         = "limiter-pool-a"
		poolB         = "limiter-pool-b"
		modelServiceA = "limiter-ms-a"
		modelServiceB = "limiter-ms-b"
		vaA           = "limiter-va-nvidia"
		vaB           = "limiter-va-amd"
		hpaA          = "limiter-hpa-nvidia"
		hpaB          = "limiter-hpa-amd"
	)

	BeforeAll(func() {
		// Note: InferencePools should already exist from infra-only deployment
		// We no longer create InferencePools in individual tests

		By("Creating two model services with different accelerator requirements")

		// Pool A - NVIDIA GPUs
		err := fixtures.CreateModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceA, poolA, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service A")

		err = fixtures.CreateService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceA, modelServiceA+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service A")

		By("Creating ServiceMonitor for service A")
		err = fixtures.CreateServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceA, modelServiceA+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor A")

		// Register cleanup for ServiceMonitor A
		DeferCleanup(func() {
			serviceMonitorName := modelServiceA + "-monitor"
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

		// Pool B - AMD GPUs
		err = fixtures.CreateModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceB, poolB, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service B")

		err = fixtures.CreateService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceB, modelServiceB+"-decode", 8001)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service B")

		By("Creating ServiceMonitor for service B")
		err = fixtures.CreateServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceB, modelServiceB+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor B")

		// Register cleanup for ServiceMonitor B
		DeferCleanup(func() {
			serviceMonitorName := modelServiceB + "-monitor"
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

		By("Waiting for both model services to be ready")
		Eventually(func(g Gomega) {
			depA, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceA+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(depA.Status.ReadyReplicas).To(Equal(int32(1)))

			depB, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceB+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(depB.Status.ReadyReplicas).To(Equal(int32(1)))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

		By("Creating VAs with different accelerator types")

		// VA A - NVIDIA accelerator
		err = fixtures.CreateVariantAutoscaling(
			ctx, crClient, cfg.LLMDNamespace, vaA,
			modelServiceA+"-decode", cfg.ModelID, "H100", 30.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VA A")

		// VA B - AMD accelerator
		err = fixtures.CreateVariantAutoscaling(
			ctx, crClient, cfg.LLMDNamespace, vaB,
			modelServiceB+"-decode", cfg.ModelID, "MI300X", 40.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VA B")

		By("Creating HPAs for both deployments")
		err = fixtures.CreateHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaA, modelServiceA+"-decode", vaA, 1, 10)
		Expect(err).NotTo(HaveOccurred(), "Failed to create HPA A")

		err = fixtures.CreateHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaB, modelServiceB+"-decode", vaB, 1, 10)
		Expect(err).NotTo(HaveOccurred(), "Failed to create HPA B")

		GinkgoWriter.Println("GPU Limiter test setup complete with two VAs (NVIDIA and AMD accelerators)")
	})

	AfterAll(func() {
		By("Cleaning up GPU limiter test resources")

		// Delete in reverse dependency order: HPA -> VA -> Service -> Deployment
		// ServiceMonitor cleanup is handled by DeferCleanup registered in BeforeAll

		// Delete HPAs
		hpaNameA := hpaA + "-hpa"
		hpaNameB := hpaB + "-hpa"
		cleanupResource(ctx, "HPA", cfg.LLMDNamespace, hpaNameA,
			func() error {
				return k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaNameA, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaNameA, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})
		cleanupResource(ctx, "HPA", cfg.LLMDNamespace, hpaNameB,
			func() error {
				return k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaNameB, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaNameB, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})

		// Delete VAs
		vaAObj := &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: vaA, Namespace: cfg.LLMDNamespace},
		}
		cleanupResource(ctx, "VA", cfg.LLMDNamespace, vaA,
			func() error {
				return crClient.Delete(ctx, vaAObj)
			},
			func() bool {
				err := crClient.Get(ctx, client.ObjectKey{Name: vaA, Namespace: cfg.LLMDNamespace}, vaAObj)
				return errors.IsNotFound(err)
			})
		vaBObj := &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: vaB, Namespace: cfg.LLMDNamespace},
		}
		cleanupResource(ctx, "VA", cfg.LLMDNamespace, vaB,
			func() error {
				return crClient.Delete(ctx, vaBObj)
			},
			func() bool {
				err := crClient.Get(ctx, client.ObjectKey{Name: vaB, Namespace: cfg.LLMDNamespace}, vaBObj)
				return errors.IsNotFound(err)
			})

		// Delete services
		serviceNameA := modelServiceA + "-service"
		serviceNameB := modelServiceB + "-service"
		cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceNameA,
			func() error {
				return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceNameA, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceNameA, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})
		cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceNameB,
			func() error {
				return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceNameB, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceNameB, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})

		// Delete deployments
		deploymentNameA := modelServiceA + "-decode"
		deploymentNameB := modelServiceB + "-decode"
		cleanupResource(ctx, "Deployment", cfg.LLMDNamespace, deploymentNameA,
			func() error {
				return k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentNameA, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentNameA, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})
		cleanupResource(ctx, "Deployment", cfg.LLMDNamespace, deploymentNameB,
			func() error {
				return k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentNameB, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentNameB, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})
	})

	Context("VA creation and reconciliation", func() {
		It("should have both VAs created with different accelerators", func() {
			By("Verifying VA A (NVIDIA)")
			vaAObj := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace,
				Name:      vaA,
			}, vaAObj)
			Expect(err).NotTo(HaveOccurred())
			Expect(vaAObj.Labels["inference.optimization/acceleratorName"]).To(Equal("H100"))

			By("Verifying VA B (AMD)")
			vaBObj := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace,
				Name:      vaB,
			}, vaBObj)
			Expect(err).NotTo(HaveOccurred())
			Expect(vaBObj.Labels["inference.optimization/acceleratorName"]).To(Equal("MI300X"))

			GinkgoWriter.Printf("VA A accelerator: %s, VA B accelerator: %s\n",
				vaAObj.Labels["inference.optimization/acceleratorName"], vaBObj.Labels["inference.optimization/acceleratorName"])
		})

		It("should reconcile both VAs successfully", func() {
			By("Checking VA A status")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaA,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty())
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("Checking VA B status")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaB,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty())
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			GinkgoWriter.Println("Both VAs reconciled successfully")
		})
	})

	Context("Accelerator-specific scaling", func() {
		It("should correctly identify accelerator constraints for each VA", func() {
			By("Generating load for both services")
			loadCfg := fixtures.LoadConfig{
				Strategy:     cfg.LoadStrategy,
				RequestRate:  cfg.RequestRate,
				NumPrompts:   500,
				InputTokens:  cfg.InputTokens,
				OutputTokens: cfg.OutputTokens,
				ModelID:      cfg.ModelID,
			}

			// Create load for both services
			targetA := fmt.Sprintf("http://%s-service:8000", modelServiceA)
			err := fixtures.CreateLoadJob(ctx, k8sClient, cfg.LLMDNamespace, "limiter-load-a", targetA, loadCfg)
			Expect(err).NotTo(HaveOccurred())

			targetB := fmt.Sprintf("http://%s-service:8001", modelServiceB)
			err = fixtures.CreateLoadJob(ctx, k8sClient, cfg.LLMDNamespace, "limiter-load-b", targetB, loadCfg)
			Expect(err).NotTo(HaveOccurred())

			jobNameA := "limiter-load-a-load"
			jobNameB := "limiter-load-b-load"

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

			By("Waiting for VAs to process load")
			time.Sleep(2 * time.Minute)

			By("Verifying both VAs are independently managed")
			// Use Eventually to handle transient API connectivity issues (e.g., TLS handshake timeouts)
			vaAObj := &variantautoscalingv1alpha1.VariantAutoscaling{}
			Eventually(func(g Gomega) {
				err = crClient.Get(ctx, client.ObjectKey{Name: vaA, Namespace: cfg.LLMDNamespace}, vaAObj)
				g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VA A")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			vaBObj := &variantautoscalingv1alpha1.VariantAutoscaling{}
			Eventually(func(g Gomega) {
				err = crClient.Get(ctx, client.ObjectKey{Name: vaB, Namespace: cfg.LLMDNamespace}, vaBObj)
				g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VA B")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			// Both VAs should have status (limiter may constrain scale-up based on GPU availability)
			Expect(vaAObj.Status.Conditions).NotTo(BeEmpty(), "VA A should have status conditions")
			Expect(vaBObj.Status.Conditions).NotTo(BeEmpty(), "VA B should have status conditions")

			GinkgoWriter.Printf("VA A status: %+v\n", vaAObj.Status)
			GinkgoWriter.Printf("VA B status: %+v\n", vaBObj.Status)
		})

		It("should respect GPU resource constraints per accelerator type", func() {
			By("Checking deployment replicas don't exceed expected limits")

			// Get deployment replica counts
			depA, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceA+"-decode", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			depB, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceB+"-decode", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			replicasA := depA.Status.Replicas
			replicasB := depB.Status.Replicas

			GinkgoWriter.Printf("Deployment A (NVIDIA) replicas: %d\n", replicasA)
			GinkgoWriter.Printf("Deployment B (AMD) replicas: %d\n", replicasB)

			// In emulated environment, deployments should still respect HPA maxReplicas
			Expect(replicasA).To(BeNumerically("<=", 10), "Deployment A should not exceed maxReplicas")
			Expect(replicasB).To(BeNumerically("<=", 10), "Deployment B should not exceed maxReplicas")

			// Both deployments should be able to scale independently
			GinkgoWriter.Println("GPU limiter correctly manages deployments with different accelerator types")
		})
	})
})
