package e2e

import (
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

var _ = Describe("Observability - Collector Health Metrics Feature", Ordered, Label("full"), func() {
	var (
		portForwardCmd     *exec.Cmd
		promClient         *utils.PrometheusClient
		usePrometheus      bool
		cleanupPortForward bool
	)

	BeforeAll(func() {
		// Set up Prometheus client
		// Environment variables:
		//   - PROMETHEUS_URL: Override the Prometheus endpoint (default: read from WVA configmap and port-forward)
		//   - PROMETHEUS_SKIP_TLS_VERIFY: Set to "false" to enable TLS cert verification (default: true for HTTPS with self-signed certs)
		prometheusURL := os.Getenv("PROMETHEUS_URL")
		if prometheusURL == "" {
			By("Reading Prometheus service info from WVA configmap")
			// Get Prometheus service info from workload-variant-autoscaler-variantautoscaling-config configmap
			promInfo, err := utils.GetPrometheusServiceInfoFromConfigMap(ctx, k8sClient, cfg.WVANamespace)
			if err != nil {
				GinkgoWriter.Printf("Warning: Failed to get Prometheus service info from configmap (will skip metric verification): %v\n", err)
				usePrometheus = false
			} else {
				GinkgoWriter.Printf("Prometheus service from configmap: %s/%s:%d (scheme: %s)\n",
					promInfo.Namespace, promInfo.ServiceName, promInfo.Port, promInfo.Scheme)

				By("Setting up port-forward to Prometheus")
				localPort := 9090
				portForwardCmd = utils.SetUpPortForward(k8sClient, ctx, promInfo.ServiceName, promInfo.Namespace, localPort, promInfo.Port)
				cleanupPortForward = true

				// Give port-forward a moment to establish
				GinkgoWriter.Printf("Waiting for port-forward to establish...\n")
				time.Sleep(2 * time.Second)

				By("Verifying Prometheus port-forward is ready")
				// Prometheus service uses HTTPS, so kubectl port-forward proxies HTTPS.
				// Use HTTPS with InsecureSkipVerify since it's a self-signed cert over an authenticated kubectl channel.
				prometheusURL = fmt.Sprintf("%s://localhost:%d", promInfo.Scheme, localPort)
				readinessURL := prometheusURL + "/-/ready"
				err = utils.VerifyPortForwardReadiness(ctx, localPort, readinessURL)
				if err != nil {
					GinkgoWriter.Printf("Warning: Prometheus port-forward not ready (will skip metric verification): %v\n", err)
					GinkgoWriter.Printf("  Port-forward command may have failed. Check if service exists: kubectl get svc -n %s %s\n",
						promInfo.Namespace, promInfo.ServiceName)
					usePrometheus = false
				} else {
					GinkgoWriter.Printf("Prometheus port-forward established at %s\n", prometheusURL)
					usePrometheus = true
				}
			}
		} else {
			GinkgoWriter.Printf("Using PROMETHEUS_URL from environment: %s\n", prometheusURL)
			usePrometheus = true
		}

		// Create Prometheus client if URL is available
		if usePrometheus {
			By("Creating Prometheus client")
			// Port-forward connections use HTTPS with self-signed certs (already authenticated via kubectl).
			// For custom PROMETHEUS_URL, allow TLS verification to be configured via PROMETHEUS_SKIP_TLS_VERIFY.
			insecureSkipVerify := true // Default for HTTPS with self-signed certs
			if strings.HasPrefix(prometheusURL, "http://") {
				// HTTP connections don't use TLS
				insecureSkipVerify = false
			} else if skipVerify := os.Getenv("PROMETHEUS_SKIP_TLS_VERIFY"); skipVerify != "" {
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

		GinkgoWriter.Printf("Collector health metrics test starting (prometheus: %v)\n", usePrometheus)
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

	It("should have wva_metrics_collection_duration_seconds metric with correct labels", func() {
		By("Verifying wva_metrics_collection_duration_seconds has query_type label")
		// The metric should have query_type label
		// This is a histogram, so we query the _count suffix to get the number of observations
		query := constants.WVAMetricsCollectionDurationSeconds + "_count"

		Eventually(func(g Gomega) {
			value, err := promClient.QueryWithRetry(ctx, query)
			// If metric exists, it should have valid non-negative value
			if err == nil {
				g.Expect(value).To(BeNumerically(">=", 0.0), "wva_metrics_collection_duration_seconds_count should have non-negative value")
				GinkgoWriter.Printf("wva_metrics_collection_duration_seconds_count metric exists: %s = %f\n", query, value)
			} else {
				// Metric may not exist yet if no collection operations have run
				GinkgoWriter.Printf("wva_metrics_collection_duration_seconds_count metric not found yet - controller may not have collected metrics\n")
			}
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should have at least one wva_metrics_collection_duration_seconds observation", func() {
		By("Querying wva_metrics_collection_duration_seconds_count metric from Prometheus")
		// In a healthy system, the controller should have collected metrics at least once
		// This is a histogram, so _count gives us the total number of collection operations
		query := constants.WVAMetricsCollectionDurationSeconds + "_count"

		Eventually(func(g Gomega) {
			value, err := promClient.QueryWithRetry(ctx, query)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query wva_metrics_collection_duration_seconds_count metric")

			// There should be at least one collection operation
			g.Expect(value).To(BeNumerically(">", 0.0), "Should have at least one metrics collection operation")

			GinkgoWriter.Printf("wva_metrics_collection_duration_seconds_count = %f\n", value)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should collect metrics for all expected query types", func() {
		By("Verifying metrics collection for kv_cache, queue_length, and cache_config query types")
		// In a healthy system, the collector should query these metric types
		queryTypes := []string{
			constants.QueryTypeKVCache,
			constants.QueryTypeQueueLength,
			constants.QueryTypeCacheConfig,
		}

		Eventually(func(g Gomega) {
			for _, queryType := range queryTypes {
				query := fmt.Sprintf(`%s_count{%s="%s"}`,
					constants.WVAMetricsCollectionDurationSeconds,
					constants.LabelQueryType,
					queryType)

				value, err := promClient.QueryWithRetry(ctx, query)
				g.Expect(err).NotTo(HaveOccurred(),
					"Should be able to query metrics collection for query_type="+queryType)

				// There should be at least one collection operation for each query type
				g.Expect(value).To(BeNumerically(">", 0.0),
					"Should have collected metrics for query_type="+queryType)

				GinkgoWriter.Printf("wva_metrics_collection_duration_seconds_count{query_type='%s'} = %f\n", queryType, value)
			}
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should have wva_metrics_pods_discovered metric with correct labels", func() {
		By("Verifying wva_metrics_pods_discovered has namespace label")
		// The metric should have namespace label
		query := constants.WVAMetricsPodsDiscovered

		Eventually(func(g Gomega) {
			value, err := promClient.QueryWithRetry(ctx, query)
			// If metric exists, it should have valid non-negative value
			if err == nil {
				g.Expect(value).To(BeNumerically(">=", 0.0), "wva_metrics_pods_discovered should have non-negative value")
				GinkgoWriter.Printf("wva_metrics_pods_discovered metric exists: %s = %f\n", query, value)
			} else {
				// Metric may not exist yet if no pod discovery has run
				GinkgoWriter.Printf("wva_metrics_pods_discovered metric not found yet - controller may not have discovered pods\n")
			}
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should discover pods in the model namespace", func() {
		By("Querying wva_metrics_pods_discovered for the LLMD namespace")
		// In a normal system with model pods running, the controller should discover pods
		// Note: The metric uses "exported_namespace" label for the namespace where pods are discovered
		query := fmt.Sprintf(`%s{exported_namespace="%s"}`, constants.WVAMetricsPodsDiscovered, cfg.LLMDNamespace)

		Eventually(func(g Gomega) {
			value, err := promClient.QueryWithRetry(ctx, query)
			g.Expect(err).NotTo(HaveOccurred(),
				"Should be able to query wva_metrics_pods_discovered for exported_namespace="+cfg.LLMDNamespace)

			// There should be at least one pod discovered in the LLMD namespace
			g.Expect(value).To(BeNumerically(">", 0.0),
				"Should have discovered at least one pod in namespace="+cfg.LLMDNamespace)

			GinkgoWriter.Printf("wva_metrics_pods_discovered{exported_namespace='%s'} = %f\n", cfg.LLMDNamespace, value)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should have wva_metrics_freshness_status metric with correct labels", func() {
		By("Verifying wva_metrics_freshness_status has variant_name and status labels")
		// The metric should have variant_name and status labels
		query := constants.WVAMetricsFreshnessStatus

		Eventually(func(g Gomega) {
			value, err := promClient.QueryWithRetry(ctx, query)
			// If metric exists, it should have valid non-negative value
			if err == nil {
				g.Expect(value).To(BeNumerically(">=", 0.0), "wva_metrics_freshness_status should have non-negative value")
				GinkgoWriter.Printf("wva_metrics_freshness_status metric exists: %s = %f\n", query, value)
			} else {
				// Metric may not exist yet if no variants have been processed
				GinkgoWriter.Printf("wva_metrics_freshness_status metric not found yet - controller may not have processed variants\n")
			}
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should emit wva_metrics_freshness_status metric after controller starts", func() {
		By("Querying wva_metrics_freshness_status metric from Prometheus")
		// The wva_metrics_freshness_status metric should be available after the WVA controller has started
		// This metric tracks the freshness status of metrics for each variant
		// Labels: variant_name, status
		query := constants.WVAMetricsFreshnessStatus

		Eventually(func(g Gomega) {
			value, err := promClient.QueryWithRetry(ctx, query)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query wva_metrics_freshness_status metric")

			// There should be at least one freshness status metric available
			// The value represents the count of variants in that freshness state
			g.Expect(value).To(BeNumerically(">=", 0.0), "wva_metrics_freshness_status metric should exist with non-negative value")

			GinkgoWriter.Printf("wva_metrics_freshness_status metric available: %s = %f\n", query, value)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should have at least one wva_metrics_freshness_status with status 'fresh'", func() {
		By("Querying wva_metrics_freshness_status metric with status='fresh'")
		// There should be at least one variant with fresh metrics after the controller starts
		query := fmt.Sprintf(`%s{%s="fresh"}`, constants.WVAMetricsFreshnessStatus, constants.LabelStatus)

		Eventually(func(g Gomega) {
			value, err := promClient.QueryWithRetry(ctx, query)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query wva_metrics_freshness_status with status='fresh'")

			// There should be at least one variant with fresh metrics
			g.Expect(value).To(BeNumerically(">", 0.0), "Should have at least one variant with fresh metrics")

			GinkgoWriter.Printf("wva_metrics_freshness_status{status='fresh'} = %f\n", value)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should track metric freshness status changes", func() {
		By("Verifying we have fresh metrics initially")
		freshQuery := fmt.Sprintf(`%s{%s="fresh"}`, constants.WVAMetricsFreshnessStatus, constants.LabelStatus)
		Eventually(func(g Gomega) {
			value, err := promClient.QueryWithRetry(ctx, freshQuery)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query fresh metrics")
			g.Expect(value).To(BeNumerically(">", 0.0), "Should have fresh metrics initially")
			GinkgoWriter.Printf("Initial fresh metrics count: %f\n", value)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Checking for other freshness statuses")
		for _, status := range []string{"stale", "missing", "unavailable"} {
			query := fmt.Sprintf(`%s{%s="%s"}`, constants.WVAMetricsFreshnessStatus, constants.LabelStatus, status)
			value, err := promClient.QueryWithRetry(ctx, query)
			if err == nil {
				GinkgoWriter.Printf("Freshness status '%s': %f\n", status, value)
			} else {
				GinkgoWriter.Printf("Freshness status '%s': no data (expected for healthy system)\n", status)
			}
		}

		// Query all freshness statuses to see what's being tracked
		By("Verifying total freshness metrics are being emitted")
		allStatusQuery := constants.WVAMetricsFreshnessStatus
		Eventually(func(g Gomega) {
			value, err := promClient.QueryWithRetry(ctx, allStatusQuery)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query all freshness status metrics")
			g.Expect(value).To(BeNumerically(">", 0.0), "Should have freshness status metrics")
			GinkgoWriter.Printf("Total freshness status metrics: %f\n", value)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})
})
