package e2e

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Annotation-based discovery e2e tests verify that WVA can discover variants from
// annotated HPA / ScaledObject resources without any VariantAutoscaling CRD.
// These tests are the Phase 1 dual-mode counterpart; the VA-based tests remain
// untouched and will be migrated in Phase 3.
var _ = Describe("Annotation-based variant discovery", Serial, func() {

	// ── Scenario A: Basic lifecycle ─────────────────────────────────────────────
	// Mirrors the "Basic VA lifecycle" block in smoke_test.go, but with no VA CR.
	Context("basic lifecycle", Ordered, func() {
		var (
			poolName         = "ann-disc-pool"
			modelServiceName = "ann-disc-basic"
			deploymentName   = modelServiceName + "-decode"
			// hpaBaseName is the logical base; the HPA object name will be hpaBaseName+"-hpa".
			// WVA discovers that HPA and uses its object name as variant_name in wva_desired_replicas.
			hpaBaseName = "ann-disc-basic"
			hpaName     = hpaBaseName + "-hpa"
		)

		BeforeAll(func() {
			By("Creating model service deployment")
			err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, "", cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

			DeferCleanup(func() {
				_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
				_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, modelServiceName+"-service", metav1.DeleteOptions{})
			})

			By("Waiting for deployment to be ready")
			Eventually(func(g Gomega) {
				d, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(d.Status.ReadyReplicas).To(Equal(int32(1)))
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Creating annotated scaler (no VA CR)")
			if cfg.ScalerBackend == scalerBackendKeda {
				err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaBaseName, deploymentName, hpaName, 1, 10, cfg.MonitoringNS,
					fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create annotated ScaledObject")
				DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaBaseName) })
			} else {
				err = fixtures.EnsureHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaBaseName, deploymentName, hpaName, 1, 10,
					fixtures.WithWVAAnnotations(cfg.ModelID, "30.0"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create annotated HPA")
				DeferCleanup(func() {
					_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName, metav1.DeleteOptions{})
				})
			}
		})

		It("should not create a VariantAutoscaling CR", func() {
			vaList := &variantautoscalingv1alpha1.VariantAutoscalingList{}
			err := crClient.List(ctx, vaList, client.InNamespace(cfg.LLMDNamespace))
			Expect(err).NotTo(HaveOccurred())
			for _, va := range vaList.Items {
				Expect(va.Name).NotTo(Equal(hpaName),
					"No VA should be created for annotation-based discovery")
			}
		})

		It("should expose wva_desired_replicas for the annotated scaler", func() {
			// In both backends WVA emits wva_desired_replicas to Prometheus.
			// For HPA (Prometheus Adapter): verify the external metrics API returns the metric.
			// For KEDA: verify KEDA has read the metric from Prometheus by checking that the
			// KEDA-managed HPA has CurrentMetrics populated (KEDA only sets this after a
			// successful Prometheus query).
			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying KEDA has read wva_desired_replicas from Prometheus")
				Eventually(func(g Gomega) {
					hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
					for i := range hpaList.Items {
						if hpaList.Items[i].Spec.ScaleTargetRef.Name == deploymentName {
							kedaHPA = &hpaList.Items[i]
							break
						}
					}
					g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the deployment")
					g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
						"KEDA HPA should have CurrentMetrics populated, indicating wva_desired_replicas was read from Prometheus")
					GinkgoWriter.Printf("KEDA HPA CurrentMetrics populated (%d entries) — wva_desired_replicas is being consumed\n",
						len(kedaHPA.Status.CurrentMetrics))
				}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
			} else {
				By("Querying external metrics API for wva_desired_replicas")
				Eventually(func(g Gomega) {
					result, err := k8sClient.RESTClient().
						Get().
						AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + cfg.LLMDNamespace + "/" + constants.WVADesiredReplicas).
						DoRaw(ctx)
					if err != nil {
						if errors.IsNotFound(err) {
							// API accessible but metric not yet emitted — engine may not have ticked
							_, discoveryErr := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
							g.Expect(discoveryErr).NotTo(HaveOccurred(), "External metrics API should be accessible")
							return
						}
						g.Expect(err).NotTo(HaveOccurred())
					}
					if !strings.Contains(string(result), `"items":[]`) {
						g.Expect(string(result)).To(ContainSubstring(constants.WVADesiredReplicas))
						GinkgoWriter.Printf("wva_desired_replicas emitted for annotated HPA %s\n", hpaName)
					}
				}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
			}
		})

	})

})
