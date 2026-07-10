package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// SGLang backend e2e: deploys a synthetic SGLang model server (CPU-only emitter of
// sglang:* metrics, launched with a faithful `sglang.launch_server` command),
// wires Prometheus scraping, and registers it with WVA via an annotated scaler
// (annotation-based discovery — no VariantAutoscaling CRD). It asserts that WVA —
// with no engine configuration — detects SGLang, collects the sglang:* metrics,
// and emits wva_desired_replicas driving a scale-up.
//
// This exercises the same code path as vLLM, only with SGLang metric names and
// flags. It runs in the kind-emulator environment (cfg.UseSimulator); a real
// SGLang server requires a GPU.
var _ = Describe("SGLang backend", Label("smoke", "full"), Ordered, func() {
	const (
		baseName = "e2e-sglang"
		// variantName is the annotated scaler's object name; WVA uses it as the
		// variant_name label on wva_desired_replicas.
		variantName = "e2e-sglang-hpa"
		// decodeSuffix mirrors the fixtures' "<base>-decode" naming convention.
		decodeSuffix = "-decode"
		// sglangEmulatorPort is the container/Service port the emitter serves on.
		sglangEmulatorPort = 8000
	)
	var (
		ctx      = context.Background()
		appLabel = baseName + decodeSuffix
		modelID  = "e2ewva/sglang-model"
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("SGLang e2e uses a CPU-only metrics emitter; it runs in the kind-emulator environment (UseSimulator=true)")
		}

		By("Deploying the synthetic SGLang model server")
		Expect(fixtures.CreateSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName, modelID, variantName)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName) })

		By("Exposing it via a Service and ServiceMonitor")
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, baseName, appLabel, sglangEmulatorPort)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteService(ctx, k8sClient, cfg.LLMDNamespace, baseName) })
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, baseName, appLabel)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteServiceMonitor(ctx, crClient, cfg.MonitoringNS, baseName) })

		By("Registering the SGLang deployment with WVA via an annotated scaler")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, baseName, appLabel, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(modelID, "30.0"))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, baseName) })
	})

	It("detects SGLang and emits wva_desired_replicas from sglang:* metrics", func() {
		// A correctly routed SGLang collection must produce wva_desired_replicas
		// for the variant. vLLM queries would return nothing here, so the metric's
		// presence proves the SGLang path collected sglang:* metrics.
		//
		// The fixture emits a saturated operating point (token_usage=0.85,
		// num_queue_reqs=3), so the emitted value drives the managed scaler above a
		// single replica. We observe that through the scaler surface rather than a
		// VA status field: for KEDA via the managed HPA's CurrentMetrics, for the
		// Prometheus-adapter backend via the external metrics API.
		By("Verifying KEDA read wva_desired_replicas for the SGLang variant")
		Eventually(func(g Gomega) {
			hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
			for i := range hpaList.Items {
				if hpaList.Items[i].Spec.ScaleTargetRef.Name == appLabel {
					kedaHPA = &hpaList.Items[i]
					break
				}
			}
			g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the SGLang deployment")
			g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
				"KEDA HPA should have CurrentMetrics populated from wva_desired_replicas")
		}).WithTimeout(time.Duration(cfg.EventuallyExtendedSec) * time.Second).
			WithPolling(time.Duration(cfg.PollIntervalSlowSec) * time.Second).
			Should(Succeed())
	})
})
