package e2e

import (
	"os"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

const (
	prometheusRuleName = "controller-manager-alerts"
)

// wvaMetricNames contains all WVA output metrics that should be referenced in alerts.
// This list is derived from internal/constants/metrics.go (WVA Output Metrics section).
var wvaMetricNames = []string{
	constants.WVAReplicaScalingTotal,
	constants.WVADesiredReplicas,
	constants.WVACurrentReplicas,
	constants.WVADesiredRatio,
	constants.WVAOptimizationDurationSeconds,
	constants.WVAModelsProcessed,
	constants.WVADecisionsLimitedTotal,
	constants.WVAAvailableGpus,
	constants.WVAEnforcerModificationsTotal,
	constants.WVAOptimizerActive,
	constants.WVAErrorsTotal,
	constants.WVAConfigInfo,
	constants.WVAConfigKvSpareThreshold,
	constants.WVAConfigQueueSpareThreshold,
	constants.WVAConfigOptimizationIntervalSeconds,
	constants.WVAMetricsCollectionDurationSeconds,
	constants.WVAMetricsCollectionErrorsTotal,
	constants.WVAMetricsPodsDiscovered,
	constants.WVAMetricsFreshnessStatus,
	constants.WVASaturationUtilization,
	constants.WVASpareCapacity,
	constants.WVARequiredCapacity,
	constants.WVAKvCacheTokensUsed,
	constants.WVAKvCacheTokensCapacity,
	constants.WVAPodMappingMissTotal,
}

// extractMetricNames extracts metric names from a PromQL expression.
// It uses a simple regex to find metric identifiers (word characters, colons, underscores).
func extractMetricNames(expr string) []string {
	// Match metric names: alphanumeric, underscores, colons (for vllm:* metrics if any)
	// This pattern matches Prometheus metric naming conventions
	metricPattern := regexp.MustCompile(`\b([a-zA-Z_:][a-zA-Z0-9_:]*)\b`)
	matches := metricPattern.FindAllString(expr, -1)

	// Filter out PromQL keywords, functions, and common label names
	promqlKeywords := map[string]bool{
		// Functions
		"rate": true, "irate": true, "sum": true, "avg": true, "min": true, "max": true,
		"count": true, "stddev": true, "stdvar": true,
		"max_over_time": true, "min_over_time": true, "avg_over_time": true,
		"absent": true, "absent_over_time": true,
		// Keywords
		"by": true, "without": true, "and": true, "or": true, "unless": true,
		"on": true, "ignoring": true, "group_left": true, "group_right": true,
		"bool": true, "offset": true,
		// Common label names (not metrics)
		"namespace": true, "variant_name": true, "model_name": true,
		"component": true, "error_type": true, "status": true,
	}

	var metrics []string
	seen := make(map[string]bool)
	for _, match := range matches {
		lower := strings.ToLower(match)
		if !promqlKeywords[lower] && !seen[match] {
			metrics = append(metrics, match)
			seen[match] = true
		}
	}
	return metrics
}

// isValidWVAMetric checks if a metric name is a valid WVA metric, accounting for
// Prometheus auto-generated suffixes (_total, _count, _sum, _bucket).
func isValidWVAMetric(metricName string, validMetrics map[string]bool) bool {
	// Check exact match first
	if validMetrics[metricName] {
		return true
	}

	// Check with common Prometheus suffixes removed
	// Counters: _total (auto-added by client library)
	// Histograms: _count, _sum, _bucket
	// Summaries: _count, _sum
	suffixes := []string{"_total", "_count", "_sum", "_bucket"}
	for _, suffix := range suffixes {
		if baseMetric, found := strings.CutSuffix(metricName, suffix); found {
			if validMetrics[baseMetric] {
				return true
			}
		}
	}

	return false
}

// PrometheusAlerts test suite validates the deployed PrometheusRule resource.
// This test:
// - Verifies PrometheusRule exists (deployed via install.sh)
// - Validates all expected alert rules are present with correct structure
// - Validates alert expressions reference only known WVA metrics
//
// This test does NOT:
// - Create or delete PrometheusRule resources (expects them to be deployed)
// - Validate that alerts actually fire when conditions are met (would require metric injection)
var _ = Describe("PrometheusAlerts", Label("smoke"), Label("prometheus-alerts"), Ordered, func() {
	BeforeAll(func() {
		// Skip if DEPLOY_ALERTING_RULES is not set to true
		deployAlertingRules := os.Getenv("DEPLOY_ALERTING_RULES")
		if deployAlertingRules != "true" {
			Skip("DEPLOY_ALERTING_RULES not set to 'true' - skipping Prometheus alerts tests. " +
				"Set DEPLOY_ALERTING_RULES=true when running 'make deploy-e2e-infra' to enable these tests.")
		}
		GinkgoWriter.Println("✓ DEPLOY_ALERTING_RULES is set to 'true'")

		// Check if PrometheusRule CRD is available
		By("Checking if PrometheusRule CRD is available")
		_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("monitoring.coreos.com/v1")
		Expect(err).NotTo(HaveOccurred(),
			"PrometheusRule CRD must be available. "+
				"Ensure Prometheus Operator is installed in the cluster.")
		GinkgoWriter.Println("✓ PrometheusRule CRD is available")
	})

	It("should have PrometheusRule deployed", func() {
		By("Verifying PrometheusRule exists")
		prometheusRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.WVANamespace,
		}, prometheusRule)
		Expect(err).NotTo(HaveOccurred(),
			"PrometheusRule must be deployed. "+
				"Ensure 'make deploy-e2e-infra' was run with DEPLOY_ALERTING_RULES=true.")

		GinkgoWriter.Printf("✓ PrometheusRule '%s' exists in namespace '%s'\n",
			prometheusRuleName, cfg.WVANamespace)

		By("Verifying PrometheusRule has correct structure")
		Expect(prometheusRule.Spec.Groups).To(HaveLen(1), "Should have 1 rule group")
		Expect(prometheusRule.Spec.Groups[0].Name).To(Equal("wva.rules"))
		Expect(prometheusRule.Spec.Groups[0].Rules).To(HaveLen(5), "Should have 5 alert rules")
		GinkgoWriter.Println("✓ PrometheusRule structure is valid")
	})

	It("should have all expected alert rules defined", func() {
		By("Retrieving PrometheusRule")
		prometheusRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.WVANamespace,
		}, prometheusRule)
		Expect(err).NotTo(HaveOccurred(), "PrometheusRule should exist")

		By("Verifying all 5 alert rules are present")
		expectedAlerts := []string{
			"WVAHighErrorRate",
			"WVAOptimizationLoopStalled",
			"WVAMetricsCollectionFailing",
			"WVAGPUResourceExhausted",
			"WVAReplicaScalingThrashing",
		}

		rules := prometheusRule.Spec.Groups[0].Rules
		foundAlerts := make(map[string]bool)
		for _, rule := range rules {
			if rule.Alert != "" {
				foundAlerts[rule.Alert] = true
				GinkgoWriter.Printf("  ✓ Found alert: %s\n", rule.Alert)
			}
		}

		for _, expectedAlert := range expectedAlerts {
			Expect(foundAlerts).To(HaveKey(expectedAlert),
				"PrometheusRule should contain alert: "+expectedAlert)
		}
		GinkgoWriter.Printf("✓ All %d expected alert rules are present\n", len(expectedAlerts))
	})

	It("should have valid alert rule structure", func() {
		By("Retrieving PrometheusRule")
		prometheusRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.WVANamespace,
		}, prometheusRule)
		Expect(err).NotTo(HaveOccurred())

		By("Validating each alert rule has required fields")
		rules := prometheusRule.Spec.Groups[0].Rules
		for _, rule := range rules {
			if rule.Alert == "" {
				continue
			}

			// Verify alert name
			Expect(rule.Alert).NotTo(BeEmpty(), "Alert should have a name")

			// Verify expression
			Expect(rule.Expr.String()).NotTo(BeEmpty(), "Alert should have an expression")

			// Verify severity label
			Expect(rule.Labels).To(HaveKey("severity"), "Alert should have severity label")

			// Verify annotations
			Expect(rule.Annotations).To(HaveKey("summary"), "Alert should have summary annotation")
			Expect(rule.Annotations).To(HaveKey("description"), "Alert should have description annotation")

			GinkgoWriter.Printf("  ✓ Alert '%s' has valid structure\n", rule.Alert)
		}
		GinkgoWriter.Println("✓ All alert rules have valid structure")
	})

	It("should only reference known WVA metrics in alert expressions", func() {
		By("Retrieving PrometheusRule")
		prometheusRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.WVANamespace,
		}, prometheusRule)
		Expect(err).NotTo(HaveOccurred())

		By("Building a map of valid WVA metric names")
		validMetrics := make(map[string]bool)
		for _, metric := range wvaMetricNames {
			validMetrics[metric] = true
		}

		By("Validating each alert expression references only known metrics")
		rules := prometheusRule.Spec.Groups[0].Rules
		for _, rule := range rules {
			if rule.Alert == "" {
				continue
			}

			expr := rule.Expr.String()
			referencedMetrics := extractMetricNames(expr)

			GinkgoWriter.Printf("  Checking alert '%s':\n", rule.Alert)
			GinkgoWriter.Printf("    Expression: %s\n", expr)
			GinkgoWriter.Printf("    Referenced metrics: %v\n", referencedMetrics)

			for _, metric := range referencedMetrics {
				Expect(isValidWVAMetric(metric, validMetrics)).To(BeTrue(),
					"Alert '%s' references unknown metric '%s'. "+
						"If this is a new WVA metric, add it to internal/constants/metrics.go and wvaMetricNames in this test. "+
						"If this is a typo or renamed metric, update the alert expression. "+
						"Note: Prometheus auto-generates _total/_count/_sum/_bucket suffixes for counters/histograms.",
					rule.Alert, metric)
			}

			GinkgoWriter.Printf("  ✓ Alert '%s' references only known metrics\n", rule.Alert)
		}
		GinkgoWriter.Println("✓ All alert expressions reference known WVA metrics")
	})

})
