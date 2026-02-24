package e2e

import (
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

// Target Condition tests verify that VariantAutoscaling correctly sets the TargetResolved
// condition based on whether the target deployment exists or not.
var _ = Describe("VariantAutoscaling Target Condition", Label("smoke", "full"), Ordered, func() {
	var (
		poolName         = "target-condition-pool"
		modelServiceName = "target-condition-ms"
		validVAName      = "target-condition-valid-va"
		invalidVAName    = "target-condition-invalid-va"
		deployName       string
	)

	BeforeAll(func() {
		// Get the actual deployment name (fixtures append "-decode")
		deployName = modelServiceName + "-decode"
		serviceName := modelServiceName + "-service"

		By("Creating model service deployment")
		err := fixtures.CreateModelService(ctx, k8sClient, cfg.LLMDNamespace,
			modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		// Register cleanup for deployment (runs even if test fails)
		DeferCleanup(func() {
			cleanupResource(ctx, "Deployment", cfg.LLMDNamespace, deployName,
				func() error {
					return k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deployName, metav1.DeleteOptions{})
				},
				func() bool {
					_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deployName, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
		})

		By("Creating service to expose model server")
		err = fixtures.CreateService(ctx, k8sClient, cfg.LLMDNamespace,
			modelServiceName, deployName, 8000)
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
		err = fixtures.CreateServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, deployName)
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
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx,
				deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", 1),
				"Model service should have at least 1 ready replica")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("Cleaning up test resources")

		// Delete VAs (Service and Deployment cleanup handled by DeferCleanup)
		vaValid := &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: validVAName, Namespace: cfg.LLMDNamespace},
		}
		cleanupResource(ctx, "VA", cfg.LLMDNamespace, validVAName,
			func() error {
				return crClient.Delete(ctx, vaValid)
			},
			func() bool {
				err := crClient.Get(ctx, client.ObjectKey{Name: validVAName, Namespace: cfg.LLMDNamespace}, vaValid)
				return errors.IsNotFound(err)
			})

		vaInvalid := &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: invalidVAName, Namespace: cfg.LLMDNamespace},
		}
		cleanupResource(ctx, "VA", cfg.LLMDNamespace, invalidVAName,
			func() error {
				return crClient.Delete(ctx, vaInvalid)
			},
			func() bool {
				err := crClient.Get(ctx, client.ObjectKey{Name: invalidVAName, Namespace: cfg.LLMDNamespace}, vaInvalid)
				return errors.IsNotFound(err)
			})
	})

	It("should set TargetResolved=True when target deployment exists", func() {
		By("Creating VariantAutoscaling with valid deployment reference")
		err := fixtures.CreateVariantAutoscaling(
			ctx, crClient, cfg.LLMDNamespace, validVAName,
			deployName, cfg.ModelID, cfg.AcceleratorType, 10.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

		By("Waiting for TargetResolved=True condition")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace,
				Name:      validVAName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			condition := variantautoscalingv1alpha1.GetCondition(va,
				variantautoscalingv1alpha1.TypeTargetResolved)
			g.Expect(condition).NotTo(BeNil(), "TargetResolved condition should exist")
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue),
				"TargetResolved should be True when deployment exists")
			g.Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonTargetFound),
				"Reason should be TargetFound")
		}, 1*time.Minute, 1*time.Second).Should(Succeed())
	})

	It("should set TargetResolved=False when target deployment does not exist", func() {
		By("Creating VariantAutoscaling with non-existent deployment reference")
		nonExistentDeployName := "non-existent-deployment"
		err := fixtures.CreateVariantAutoscaling(
			ctx, crClient, cfg.LLMDNamespace, invalidVAName,
			nonExistentDeployName, cfg.ModelID, cfg.AcceleratorType, 10.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

		By("Waiting for TargetResolved=False condition")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace,
				Name:      invalidVAName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			condition := variantautoscalingv1alpha1.GetCondition(va,
				variantautoscalingv1alpha1.TypeTargetResolved)
			g.Expect(condition).NotTo(BeNil(), "TargetResolved condition should exist")
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse),
				"TargetResolved should be False when deployment does not exist")
			g.Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonTargetNotFound),
				"Reason should be TargetNotFound")
		}, 1*time.Minute, 1*time.Second).Should(Succeed())
	})
})
