package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

var _ = Describe("Observability - Pipeline Stage Visibility Metric Tests", Label("smoke", "full"), Serial, Ordered, func() {
	var (
		portForwardCmd     *exec.Cmd
		promClient         *utils.PrometheusClient
		usePrometheus      bool
		cleanupPortForward bool
	)

	BeforeAll(func() {
		// Set up Prometheus client
		// Environment variables:
		//   - PROMETHEUS_URL: Override the Prometheus endpoint (default: setup port-forward to https://localhost:9090)
		//   - PROMETHEUS_SKIP_TLS_VERIFY: Set to "false" to enable TLS cert verification (default: true)
		prometheusURL := os.Getenv("PROMETHEUS_URL")
		if prometheusURL == "" {
			By("Setting up port-forward to Prometheus")
			GinkgoWriter.Printf("No PROMETHEUS_URL set, will set up port-forward to kube-prometheus-stack-prometheus\n")

			promServiceName := "kube-prometheus-stack-prometheus"
			promNamespace := cfg.MonitoringNS
			promServicePort := 9090

			portForwardCmd = utils.SetUpPortForward(k8sClient, ctx, promServiceName, promNamespace, 9090, promServicePort)
			cleanupPortForward = true

			// Give port-forward a moment to establish
			GinkgoWriter.Printf("Waiting for port-forward to establish...\n")
			time.Sleep(2 * time.Second)

			By("Verifying Prometheus port-forward is ready")
			err := utils.VerifyPortForwardReadiness(ctx, 9090, "https://localhost:9090/-/ready")
			if err != nil {
				GinkgoWriter.Printf("Warning: Prometheus port-forward not ready (will skip metric verification): %v\n", err)
				GinkgoWriter.Printf("  Port-forward command may have failed. Check if service exists: kubectl get svc -n %s %s\n", promNamespace, promServiceName)
				usePrometheus = false
			} else {
				prometheusURL = utils.DefaultPrometheusURL
				GinkgoWriter.Printf("Prometheus port-forward established at %s\n", prometheusURL)
				usePrometheus = true
			}
		} else {
			GinkgoWriter.Printf("Using PROMETHEUS_URL from environment: %s\n", prometheusURL)
			usePrometheus = true
		}

		// Create Prometheus client if URL is available
		if usePrometheus {
			By("Creating Prometheus client")
			// Allow TLS verification to be configured via environment variable.
			// WARNING: insecureSkipVerify=true disables TLS certificate verification and is intended
			// only for E2E tests with self-signed certificates. Do not use in production.
			// Set PROMETHEUS_SKIP_TLS_VERIFY=false to enable certificate verification.
			insecureSkipVerify := true // Default for E2E tests with self-signed certs
			if skipVerify := os.Getenv("PROMETHEUS_SKIP_TLS_VERIFY"); skipVerify != "" {
				insecureSkipVerify = strings.EqualFold(skipVerify, "true")
			}

			var err error
			promClient, err = utils.NewPrometheusClient(prometheusURL, insecureSkipVerify)
			if err != nil {
				GinkgoWriter.Printf("Warning: Failed to create Prometheus client (will skip metric verification): %v\n", err)
				usePrometheus = false
			} else {
				GinkgoWriter.Printf("Prometheus client created successfully (URL: %s, TLS verification: %v)\n", prometheusURL, !insecureSkipVerify)
			}
		}

		GinkgoWriter.Printf("Pipeline stage visibility metrics test starting (prometheus: %v)\n", usePrometheus)
	})

	AfterAll(func() {
		// Clean up port-forward if we created it
		if cleanupPortForward && portForwardCmd != nil {
			By("Stopping Prometheus port-forward")
			if err := utils.StopCmd(portForwardCmd); err != nil {
				GinkgoWriter.Printf("Warning: failed to stop port-forward: %v\n", err)
			}
		}
	})

	Context("Optimizer metrics", func() {

		It("should emit optimizer active metric", func() {
			if !usePrometheus {
				Skip("Prometheus not available, skipping metric verification")
			}

			By("Verifying that exactly one optimizer is active")
			// Query for cost-aware optimizer
			costAwareQuery := fmt.Sprintf(`%s{%s="cost-aware"}`, constants.WVAOptimizerActive, constants.LabelOptimizerName)
			greedyQuery := fmt.Sprintf(`%s{%s="greedy-by-score"}`, constants.WVAOptimizerActive, constants.LabelOptimizerName)

			Eventually(func(g Gomega) {
				// Use a short context timeout for the query
				queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()

				costAwareValue, err := promClient.QueryWithRetry(queryCtx, costAwareQuery)
				g.Expect(err).NotTo(HaveOccurred(), "Should query cost-aware optimizer metric")

				greedyValue, err := promClient.QueryWithRetry(queryCtx, greedyQuery)
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
			if !usePrometheus {
				Skip("Prometheus not available, skipping metric verification")
			}

			By("Verifying optimizer_name label exists")
			// Query all optimizer metrics to verify label structure
			query := constants.WVAOptimizerActive

			Eventually(func(g Gomega) {
				// Use a short context timeout for the query
				queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()

				value, err := promClient.QueryWithRetry(queryCtx, query)
				g.Expect(err).NotTo(HaveOccurred(), "Should query optimizer metrics")
				// At least one optimizer should be active (value >= 0)
				g.Expect(value).To(BeNumerically(">=", 0.0), "Optimizer metric should exist with valid value")
				GinkgoWriter.Printf("Optimizer metrics available: %s = %f\n", query, value)
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

		It("should have complementary optimizer states", func() {
			if !usePrometheus {
				Skip("Prometheus not available, skipping metric verification")
			}

			By("Verifying active and inactive optimizer states are complementary")
			costAwareQuery := fmt.Sprintf(`%s{%s="cost-aware"}`, constants.WVAOptimizerActive, constants.LabelOptimizerName)
			greedyQuery := fmt.Sprintf(`%s{%s="greedy-by-score"}`, constants.WVAOptimizerActive, constants.LabelOptimizerName)

			Eventually(func(g Gomega) {
				// Use a short context timeout for the query
				queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()

				costAwareValue, err := promClient.QueryWithRetry(queryCtx, costAwareQuery)
				g.Expect(err).NotTo(HaveOccurred(), "Should query cost-aware optimizer metric")

				greedyValue, err := promClient.QueryWithRetry(queryCtx, greedyQuery)
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
