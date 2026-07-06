package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// SGLang backend e2e: deploys a synthetic SGLang model server (CPU-only emitter of
// sglang:* metrics, launched with a faithful `sglang.launch_server` command),
// wires Prometheus scraping, and creates a VariantAutoscaling for it. It asserts
// that WVA — with no engine configuration — detects SGLang, collects the sglang:*
// metrics, and reconciles the variant to a desired allocation.
//
// This exercises the same code path as vLLM, only with SGLang metric names and
// flags. It runs in the kind-emulator environment (cfg.UseSimulator); a real
// SGLang server requires a GPU.
var _ = Describe("SGLang backend", Label("smoke", "full"), Ordered, func() {
	const (
		baseName = "e2e-sglang"
		vaName   = "e2e-sglang-va"
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
		Expect(fixtures.CreateSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName, modelID, vaName)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName) })

		By("Exposing it via a Service and ServiceMonitor")
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, baseName, appLabel, sglangEmulatorPort)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteService(ctx, k8sClient, cfg.LLMDNamespace, baseName) })
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, baseName, appLabel)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteServiceMonitor(ctx, crClient, cfg.MonitoringNS, baseName) })

		By("Creating a VariantAutoscaling targeting the SGLang deployment")
		Expect(fixtures.CreateVariantAutoscalingWithDefaults(
			ctx, crClient, cfg.LLMDNamespace, vaName, appLabel, modelID, cfg.AcceleratorType, cfg.ControllerInstance,
		)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteVariantAutoscaling(ctx, crClient, cfg.LLMDNamespace, vaName) })
	})

	It("detects SGLang and reconciles the variant from sglang:* metrics", func() {
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			g.Expect(crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: vaName}, va)).To(Succeed())
			// WVA sets status conditions once it has reconciled the variant...
			g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "WVA should set status conditions for the SGLang variant")
			// ...and computes a desired allocation, which is only possible if it
			// collected the SGLang metrics (vLLM queries would return nothing here).
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
				"WVA should compute a desired replica count from sglang:* metrics")
			// Assert an actual scale-up rather than just a non-nil count: static
			// arg-based capacity estimation alone would still produce a value even
			// if engine routing were broken. The fixture emits a saturated operating
			// point (token_usage=0.85, num_queue_reqs=3), so a correctly routed
			// SGLang collection must drive the variant above a single replica.
			g.Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 1),
				"saturated sglang:* metrics should drive a scale-up above 1 replica")
		}).WithTimeout(time.Duration(cfg.EventuallyExtendedSec) * time.Second).
			WithPolling(time.Duration(cfg.PollIntervalSlowSec) * time.Second).
			Should(Succeed())
	})
})
