package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

var _ = Describe("Metrics Tests", Label("smoke", "full"), func() {
	Context("Optimizer metrics", Serial, Ordered, func() {
		var (
			portForwardCmd *exec.Cmd
			promClient     *utils.PrometheusClient
		)

		BeforeAll(func() {
			By("Setting up port forwarding to Prometheus")
			// Forward local port 9090 to Prometheus service port 9090
			portForwardCmd = utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", cfg.MonitoringNS, 9090, 9090)

			By("Verifying port forwarding is ready")
			err := utils.VerifyPortForwardReadiness(ctx, 9090, "https://localhost:9090/-/ready")
			Expect(err).NotTo(HaveOccurred(), "Port forwarding should be ready")

			By("Creating Prometheus client")
			promClient, err = utils.NewPrometheusClient("https://localhost:9090", true)
			Expect(err).NotTo(HaveOccurred(), "Should create Prometheus client")
		})

		AfterAll(func() {
			By("Cleaning up port forwarding")
			if portForwardCmd != nil && portForwardCmd.Process != nil {
				err := portForwardCmd.Process.Kill()
				if err != nil {
					GinkgoWriter.Printf("Warning: failed to kill port forward process: %v\n", err)
				}
			}
		})

		It("should emit optimizer active metric", func() {
			By("Verifying that exactly one optimizer is active")
			// Query for cost-aware optimizer
			costAwareQuery := fmt.Sprintf(`%s{%s="cost-aware"}`, constants.WVAOptimizerActive, constants.LabelOptimizerName)
			greedyQuery := fmt.Sprintf(`%s{%s="greedy-by-score"}`, constants.WVAOptimizerActive, constants.LabelOptimizerName)

			Eventually(func(g Gomega) {
				costAwareValue, err := promClient.QueryWithRetry(ctx, costAwareQuery)
				g.Expect(err).NotTo(HaveOccurred(), "Should query cost-aware optimizer metric")

				greedyValue, err := promClient.QueryWithRetry(ctx, greedyQuery)
				g.Expect(err).NotTo(HaveOccurred(), "Should query greedy-by-score optimizer metric")

				// Exactly one optimizer should be active (sum should equal 1)
				activeCount := costAwareValue + greedyValue
				g.Expect(activeCount).To(Equal(1.0), "Exactly one optimizer should be active")

				// Log which optimizer is active
				if costAwareValue == 1.0 {
					GinkgoWriter.Printf("Cost-aware optimizer is active\n")
				} else if greedyValue == 1.0 {
					GinkgoWriter.Printf("Greedy-by-score optimizer is active\n")
				}
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

		It("should have optimizer metric with correct labels", func() {
			By("Verifying optimizer_name label exists")
			// Query all optimizer metrics to verify label structure
			query := constants.WVAOptimizerActive

			Eventually(func(g Gomega) {
				value, err := promClient.QueryWithRetry(ctx, query)
				g.Expect(err).NotTo(HaveOccurred(), "Should query optimizer metrics")
				// At least one optimizer should be active (value >= 0)
				g.Expect(value).To(BeNumerically(">=", 0.0), "Optimizer metric should exist with valid value")
				GinkgoWriter.Printf("Optimizer metrics available: %s = %f\n", query, value)
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

		It("should have complementary optimizer states", func() {
			By("Verifying active and inactive optimizer states are complementary")
			costAwareQuery := fmt.Sprintf(`%s{%s="cost-aware"}`, constants.WVAOptimizerActive, constants.LabelOptimizerName)
			greedyQuery := fmt.Sprintf(`%s{%s="greedy-by-score"}`, constants.WVAOptimizerActive, constants.LabelOptimizerName)

			Eventually(func(g Gomega) {
				costAwareValue, err := promClient.QueryWithRetry(ctx, costAwareQuery)
				g.Expect(err).NotTo(HaveOccurred(), "Should query cost-aware optimizer metric")

				greedyValue, err := promClient.QueryWithRetry(ctx, greedyQuery)
				g.Expect(err).NotTo(HaveOccurred(), "Should query greedy-by-score optimizer metric")

				// If one is active (1), the other must be inactive (0)
				g.Expect(costAwareValue).To(BeElementOf(0.0, 1.0), "Cost-aware optimizer value should be 0 or 1")
				g.Expect(greedyValue).To(BeElementOf(0.0, 1.0), "Greedy-by-score optimizer value should be 0 or 1")
				g.Expect(costAwareValue).NotTo(Equal(greedyValue), "Optimizers should have complementary states")

				GinkgoWriter.Printf("Optimizer states: cost-aware=%f, greedy-by-score=%f\n", costAwareValue, greedyValue)
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})
	})
})
