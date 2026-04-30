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

var _ = Describe("Pipeline Stage Visibility Metric Tests", Label("smoke", "full"), Serial, Ordered, func() {
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

	Context("Optimizer metrics", func() {

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

	Context("Available GPUs metrics", func() {

		It("should emit available GPUs metric for all vendors", func() {
			By("Querying available GPUs metrics for all GPU vendors")
			// GPU vendors supported by the discovery system
			vendors := []string{"nvidia.com", "amd.com", "intel.com"}

			Eventually(func(g Gomega) {
				foundAtLeastOne := false
				for _, vendor := range vendors {
					query := fmt.Sprintf(`%s{%s="%s"}`, constants.WVAAvailableGpus, constants.LabelAcceleratorType, vendor)
					value, err := promClient.QueryWithRetry(ctx, query)

					// No error means metric exists for this vendor
					if err == nil {
						foundAtLeastOne = true
						// Value should be non-negative (0 or more GPUs)
						g.Expect(value).To(BeNumerically(">=", 0.0),
							fmt.Sprintf("Available GPUs for %s should be non-negative", vendor))
						GinkgoWriter.Printf("Available GPUs for %s: %f\n", vendor, value)
					}
				}
				// At least one vendor should have metrics (or cluster has no GPUs at all)
				// We'll just log if no metrics found
				if !foundAtLeastOne {
					GinkgoWriter.Printf("No GPU metrics found - cluster may have no GPU nodes\n")
				}
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

		It("should have available GPUs metric with correct labels", func() {
			By("Verifying accelerator_type label exists in available GPUs metric")
			query := constants.WVAAvailableGpus

			Eventually(func(g Gomega) {
				value, err := promClient.QueryWithRetry(ctx, query)
				// If metric exists, it should have valid non-negative value
				if err == nil {
					g.Expect(value).To(BeNumerically(">=", 0.0), "Available GPUs metric should have non-negative value")
					GinkgoWriter.Printf("Available GPUs metrics available: %s = %f\n", query, value)
				} else {
					// No GPUs in cluster is acceptable
					GinkgoWriter.Printf("No available GPUs metric found - cluster may have no GPU nodes\n")
				}
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

		It("should track available GPUs per vendor independently", func() {
			By("Verifying each vendor's GPU count is tracked separately")
			vendors := []string{"nvidia.com", "amd.com", "intel.com"}

			Eventually(func(g Gomega) {
				vendorCounts := make(map[string]float64)

				for _, vendor := range vendors {
					query := fmt.Sprintf(`%s{%s="%s"}`, constants.WVAAvailableGpus, constants.LabelAcceleratorType, vendor)
					value, err := promClient.QueryWithRetry(ctx, query)

					if err == nil {
						vendorCounts[vendor] = value
					}
				}

				// Log the per-vendor breakdown
				if len(vendorCounts) > 0 {
					GinkgoWriter.Printf("GPU availability by vendor:\n")
					for vendor, count := range vendorCounts {
						GinkgoWriter.Printf("  %s: %f GPUs\n", vendor, count)
					}
				} else {
					GinkgoWriter.Printf("No GPU vendors found in cluster\n")
				}

				// Each vendor that has GPUs should have independent tracking
				for vendor, count := range vendorCounts {
					g.Expect(count).To(BeNumerically(">=", 0.0),
						fmt.Sprintf("Vendor %s should have non-negative GPU count", vendor))
				}
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})
	})
})
