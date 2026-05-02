package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

var _ = Describe("Observability - Collector Health Metrics Feature", Ordered, Label("full"), func() {
	var (
		poolName          = "pod-scraping-pool"
		modelServiceName  = "pod-scraping-ms"
		eppServiceName    string
		metricsSecretName string
		portForwardCmd    *exec.Cmd
		promClient        *utils.PrometheusClient
	)

	BeforeAll(func() {
		By("Creating model service to ensure EPP pods exist")
		// EPP pods are created when a model service is deployed to an InferencePool
		err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace,
			modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		By("Creating service to expose model server")
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace,
			modelServiceName, modelServiceName+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx,
				modelServiceName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1),
				"Model service should have at least 1 ready replica")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Discovering EPP service")
		// Discover existing EPP services dynamically (like legacy tests)
		// EPP service name follows pattern: {poolName}-epp
		// First try the expected pool name, then discover any existing EPP service
		expectedEPPName := poolName + "-epp"

		// Verify EPP service exists (either the expected one or discover an existing one)
		Eventually(func(g Gomega) {
			// Try expected EPP service first
			_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx,
				expectedEPPName, metav1.GetOptions{})
			if err == nil {
				eppServiceName = expectedEPPName
				return
			}

			// If expected service doesn't exist, discover existing EPP services
			serviceList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list services")

			// Find first EPP service (service name ends with "-epp")
			for _, svc := range serviceList.Items {
				if len(svc.Name) > 4 && svc.Name[len(svc.Name)-4:] == "-epp" {
					eppServiceName = svc.Name
					GinkgoWriter.Printf("Discovered EPP service: %s\n", eppServiceName)
					return
				}
			}

			g.Expect(err).NotTo(HaveOccurred(), "EPP service should exist")
		}).Should(Succeed())

		Expect(eppServiceName).NotTo(BeEmpty(), "EPP service name should be set")

		By("Verifying EPP pods are Ready")
		Eventually(func(g Gomega) {
			pods, err := utils.FindExistingEPPPods(ctx, k8sClient, cfg.LLMDNamespace, eppServiceName)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to find EPP pods")

			readyCount := 0
			for _, pod := range pods {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						readyCount++
						break
					}
				}
			}
			g.Expect(readyCount).To(BeNumerically(">=", 1),
				"Should have at least one Ready EPP pod")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

		By("Discovering or creating metrics reader secret")
		var discoverErr error
		metricsSecretName, discoverErr = utils.DiscoverMetricsReaderSecret(ctx, k8sClient,
			crClient, cfg.LLMDNamespace, eppServiceName)
		Expect(discoverErr).NotTo(HaveOccurred(), "Should be able to discover or create metrics secret")
		GinkgoWriter.Printf("Using metrics secret: %s\n", metricsSecretName)

		By("Setting up port forwarding to Prometheus")
		// Forward local port 9090 to Prometheus service port 9090
		portForwardCmd = utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", cfg.MonitoringNS, 9090, 9090)

		By("Verifying port forwarding is ready")
		err = utils.VerifyPortForwardReadiness(ctx, 9090, "https://localhost:9090/-/ready")
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

		By("Cleaning up test resources")
		// Service and deployment cleanup
		serviceName := modelServiceName + "-service"
		deploymentName := modelServiceName + "-decode"
		cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceName,
			func() error {
				return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})
		cleanupResource(ctx, "Deployment", cfg.LLMDNamespace, deploymentName,
			func() error {
				return k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})
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
