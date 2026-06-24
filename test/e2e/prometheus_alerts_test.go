package e2e

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	prometheusRuleName     = "controller-manager-alerts"
	prometheusRuleYAMLPath = "config/components/prometheus-alerts/prometheusrule.yaml"
)

// createWVAPrometheusRule loads PrometheusRule from the actual config YAML file
func createWVAPrometheusRule(namespace string) *promoperator.PrometheusRule {
	// Get the path to the YAML file (relative to the project root)
	yamlPath := filepath.Join("..", "..", prometheusRuleYAMLPath)

	// Read the YAML file
	yamlBytes, err := os.ReadFile(yamlPath)
	Expect(err).NotTo(HaveOccurred(), "Should be able to read PrometheusRule YAML file")

	// Unmarshal into PrometheusRule
	prometheusRule := &promoperator.PrometheusRule{}
	err = yaml.Unmarshal(yamlBytes, prometheusRule)
	Expect(err).NotTo(HaveOccurred(), "Should be able to unmarshal PrometheusRule YAML")

	// Override the namespace to use the test namespace
	prometheusRule.Namespace = namespace

	return prometheusRule
}

var _ = Describe("PrometheusAlerts", Label("full"), Label("prometheus-alerts"), Ordered, func() {
	var prometheusRuleCreated bool

	BeforeAll(func() {
		// Check if PrometheusRule CRD is available
		By("Checking if PrometheusRule CRD is available")
		_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("monitoring.coreos.com/v1")
		if err != nil {
			Skip("PrometheusRule CRD not available - skipping Prometheus alerts tests")
		}
		GinkgoWriter.Println("✓ PrometheusRule CRD is available")
	})

	AfterAll(func() {
		if prometheusRuleCreated {
			By("Cleaning up PrometheusRule")
			prometheusRule := &promoperator.PrometheusRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      prometheusRuleName,
					Namespace: cfg.WVANamespace,
				},
			}
			err := crClient.Delete(ctx, prometheusRule)
			if err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: Failed to delete PrometheusRule: %v\n", err)
			} else {
				GinkgoWriter.Println("✓ PrometheusRule cleaned up")
			}
		}
	})

	It("should create PrometheusRule with WVA alert rules", func() {
		By("Checking if PrometheusRule already exists")
		existingRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.WVANamespace,
		}, existingRule)

		if err == nil {
			By("Deleting existing PrometheusRule from previous deployment")
			err = crClient.Delete(ctx, existingRule)
			Expect(err).NotTo(HaveOccurred(), "Should be able to delete existing PrometheusRule")
			GinkgoWriter.Printf("✓ Deleted existing PrometheusRule '%s'\n", prometheusRuleName)

			// Wait for deletion to complete
			Eventually(func(g Gomega) {
				checkRule := &promoperator.PrometheusRule{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      prometheusRuleName,
					Namespace: cfg.WVANamespace,
				}, checkRule)
				g.Expect(errors.IsNotFound(err)).To(BeTrue(), "PrometheusRule should be deleted")
			}, time.Duration(cfg.EventuallyShortSec)*time.Second,
				time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
			GinkgoWriter.Println("✓ PrometheusRule deletion confirmed")
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "Unexpected error checking for existing PrometheusRule")
		}

		By("Creating PrometheusRule")
		prometheusRule := createWVAPrometheusRule(cfg.WVANamespace)

		err = crClient.Create(ctx, prometheusRule)
		Expect(err).NotTo(HaveOccurred(), "Should be able to create PrometheusRule")
		prometheusRuleCreated = true
		GinkgoWriter.Printf("✓ Created PrometheusRule '%s' in namespace '%s'\n", prometheusRuleName, cfg.WVANamespace)

		By("Verifying PrometheusRule was created successfully")
		Eventually(func(g Gomega) {
			createdRule := &promoperator.PrometheusRule{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      prometheusRuleName,
				Namespace: cfg.WVANamespace,
			}, createdRule)
			g.Expect(err).NotTo(HaveOccurred(), "PrometheusRule should exist")
			g.Expect(createdRule.Spec.Groups).To(HaveLen(1), "Should have 1 rule group")
			g.Expect(createdRule.Spec.Groups[0].Name).To(Equal("wva.rules"))
			g.Expect(createdRule.Spec.Groups[0].Rules).To(HaveLen(5), "Should have 5 alert rules")
		}, time.Duration(cfg.EventuallyShortSec)*time.Second,
			time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		GinkgoWriter.Println("✓ PrometheusRule verified")
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

	It("should delete PrometheusRule and verify removal", func() {
		By("Verifying PrometheusRule exists before deletion")
		prometheusRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.WVANamespace,
		}, prometheusRule)
		Expect(err).NotTo(HaveOccurred(), "PrometheusRule should exist before deletion")
		GinkgoWriter.Printf("✓ PrometheusRule '%s' exists\n", prometheusRuleName)

		By("Deleting PrometheusRule")
		err = crClient.Delete(ctx, prometheusRule)
		Expect(err).NotTo(HaveOccurred(), "Should be able to delete PrometheusRule")
		GinkgoWriter.Printf("✓ Deletion request sent for PrometheusRule '%s'\n", prometheusRuleName)

		By("Verifying PrometheusRule has been removed")
		Eventually(func(g Gomega) {
			deletedRule := &promoperator.PrometheusRule{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      prometheusRuleName,
				Namespace: cfg.WVANamespace,
			}, deletedRule)
			g.Expect(errors.IsNotFound(err)).To(BeTrue(), "PrometheusRule should be deleted")
		}, time.Duration(cfg.EventuallyShortSec)*time.Second,
			time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		GinkgoWriter.Printf("✓ PrometheusRule '%s' successfully removed\n", prometheusRuleName)

		// Mark as not created so AfterAll doesn't try to delete again
		prometheusRuleCreated = false
	})
})
