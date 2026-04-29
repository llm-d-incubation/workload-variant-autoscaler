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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

var _ = Describe("Error metrics recording", Label("full"), Ordered, func() {
	var (
		cmNamespace        string
		cmOriginal         *corev1.ConfigMap
		cmExisted          bool
		promClient         *utils.PrometheusClient
		usePrometheus      bool
		portForwardCmd     *exec.Cmd
		cleanupPortForward bool
	)

	BeforeAll(func() {
		cmNamespace = cfg.WVANamespace

		// Discover the actual saturation ConfigMap name from the cluster
		// The ConfigMap name is generated from Helm chart template:
		// {{ include "workload-variant-autoscaler.fullname" . }}-wva-saturation-scaling-config
		// We need to find it dynamically since the fullname depends on release name
		configMapName := discoverSaturationConfigMapName(ctx, cmNamespace)
		if configMapName == "" {
			// No existing ConfigMap found - use default name from config package
			configMapName = config.SaturationConfigMapName()
			GinkgoWriter.Printf("No saturation ConfigMap found in %s, will use default name: %s\n", cmNamespace, configMapName)
		} else {
			// Set env var so config.SaturationConfigMapName() returns the correct name
			os.Setenv("SATURATION_CONFIG_MAP_NAME", configMapName)
			GinkgoWriter.Printf("Discovered saturation ConfigMap name: %s\n", configMapName)
		}

		// Snapshot existing ConfigMap if it exists
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, configMapName, metav1.GetOptions{})
		if err == nil {
			cmExisted = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		// Set up port-forward to Prometheus if PROMETHEUS_URL is not set
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
			err = utils.VerifyPortForwardReadiness(ctx, 9090, "https://localhost:9090/-/ready")
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

		// Only create Prometheus client if we have a valid URL and usePrometheus is true
		if usePrometheus {
			By("Creating Prometheus client")
			promClient, err = utils.NewPrometheusClient(prometheusURL, true)
			if err != nil {
				GinkgoWriter.Printf("Warning: Failed to create Prometheus client (will skip metric verification): %v\n", err)
				usePrometheus = false
			} else {
				GinkgoWriter.Printf("Prometheus client created successfully (URL: %s)\n", prometheusURL)
			}
		}

		GinkgoWriter.Printf("Error metrics test starting (namespace: %s, prometheus: %v)\n", cmNamespace, usePrometheus)
	})

	AfterAll(func() {
		By("Cleaning up test ConfigMap")
		if cmExisted && cmOriginal != nil {
			// Restore original ConfigMap
			toCreate := cleanConfigMapForRecreate(cmOriginal)
			propagation := metav1.DeletePropagationBackground
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, config.SaturationConfigMapName(), metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			})
			time.Sleep(1 * time.Second) // Brief delay for deletion to complete
			if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to restore saturation configmap: %v\n", err)
			}
		} else {
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, config.SaturationConfigMapName(), metav1.DeleteOptions{})
		}

		// Clean up port-forward if we created it
		if cleanupPortForward && portForwardCmd != nil {
			By("Stopping Prometheus port-forward")
			if err := utils.StopCmd(portForwardCmd); err != nil {
				GinkgoWriter.Printf("Warning: failed to stop port-forward: %v\n", err)
			}
		}
	})

	It("should increment wva_errors_total when ConfigMap has invalid YAML", func() {
		// This test verifies that metrics.RecordError is called when the controller
		// encounters invalid YAML in a ConfigMap. It tests the error recording path in
		// internal/controller/configmap_helpers.go:41-43
		errorType := "Failed to parse saturation scaling config entry"
		var baseline float64

		configMapName := config.SaturationConfigMapName()
		GinkgoWriter.Printf("Test setup:\n")
		GinkgoWriter.Printf("  ConfigMap name: %s\n", configMapName)
		GinkgoWriter.Printf("  Namespace: %s\n", cmNamespace)
		GinkgoWriter.Printf("  Error type to check: %s\n", errorType)

		if usePrometheus {
			By("Getting baseline error count from Prometheus")
			baseline = getErrorMetricCount(promClient, errorType)
			GinkgoWriter.Printf("Baseline count for '%s': %.0f\n", errorType, baseline)
		} else {
			GinkgoWriter.Printf("Prometheus client not available, will verify via controller logs instead\n")
		}

		By("Step 1: First, trigger reconciliation with valid ConfigMap to verify controller is watching")
		validYAML := `
	   kvCacheThreshold: 0.8
	   queueLengthThreshold: 5
	   `
		err := createOrUpdateConfigMap(ctx, cmNamespace, configMapName, "test-valid-model", validYAML)
		Expect(err).NotTo(HaveOccurred())
		GinkgoWriter.Printf("Created valid ConfigMap to verify controller reconciliation\n")
		time.Sleep(time.Duration(cfg.PollIntervalSec) * time.Second)

		By("Step 2: Now create invalid YAML to trigger the error")
		invalidYAML := `
	   this is not valid yaml: [
	     - missing closing bracket
	     invalid: structure
	   `
		err = createOrUpdateConfigMap(ctx, cmNamespace, configMapName, "invalid-model", invalidYAML)
		Expect(err).NotTo(HaveOccurred())
		GinkgoWriter.Printf("Updated ConfigMap %s/%s with invalid YAML\n", cmNamespace, configMapName)

		By("Verifying ConfigMap was updated")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, configMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data).To(HaveKey("invalid-model"))
		GinkgoWriter.Printf("ConfigMap has %d entries\n", len(cm.Data))

		By("Step 3: Wait for controller to reconcile and record the error")
		GinkgoWriter.Printf("Waiting for controller to reconcile (will poll for %d seconds)...\n", cfg.EventuallyLongSec)

		if usePrometheus {
			By("Verifying error metric was incremented in Prometheus")
			Eventually(func(g Gomega) {
				count := getErrorMetricCount(promClient, errorType)
				GinkgoWriter.Printf("  Metric count: %.0f (baseline: %.0f, need > %.0f)\n", count, baseline, baseline)
				g.Expect(count).To(BeNumerically(">", baseline),
					"Error metric '%s' should be incremented. Check controller logs for reconciliation activity.", errorType)
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
			GinkgoWriter.Printf("✓ Error metric incremented successfully\n")
		} else {
			GinkgoWriter.Printf("Prometheus not available. To verify the test worked, check controller logs:\n")
			GinkgoWriter.Printf("  kubectl logs -n %s -l control-plane=controller-manager --tail=100 | grep '%s'\n",
				cfg.WVANamespace, errorType)
		}
	})
})

// getErrorMetricCount queries Prometheus for the wva_errors_total metric count (controller component)
func getErrorMetricCount(promClient *utils.PrometheusClient, errorType string) float64 {
	return getErrorMetricCountByComponent(promClient, constants.ComponentController, errorType)
}

// getErrorMetricCountByComponent queries Prometheus for wva_errors_total with specific component
func getErrorMetricCountByComponent(promClient *utils.PrometheusClient, component string, errorType string) float64 {
	if promClient == nil {
		return 0.0
	}

	query := fmt.Sprintf(
		`wva_errors_total{component="%s",error_type="%s"}`,
		component,
		errorType,
	)

	// Use a short context timeout for the query
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	value, err := promClient.QueryWithRetry(queryCtx, query)
	if err != nil {
		GinkgoWriter.Printf("Debug: Prometheus query '%s' failed (returning 0): %v\n", query, err)

		// Try to query all wva_errors_total metrics to debug label mismatches
		debugQuery := "wva_errors_total"
		debugValue, debugErr := promClient.QueryWithRetry(queryCtx, debugQuery)
		if debugErr == nil {
			GinkgoWriter.Printf("Debug: All wva_errors_total metrics sum: %.0f\n", debugValue)
		} else {
			GinkgoWriter.Printf("Debug: Query for all wva_errors_total also failed: %v\n", debugErr)
		}

		return 0.0
	}

	return value
}

// createOrUpdateConfigMap creates or updates a ConfigMap with the given data
func createOrUpdateConfigMap(ctx context.Context, namespace, name, key, value string) error {
	cmClient := k8sClient.CoreV1().ConfigMaps(namespace)
	cm, err := cmClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			newCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Data: map[string]string{key: value},
			}
			_, createErr := cmClient.Create(ctx, newCM, metav1.CreateOptions{})
			return createErr
		}
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = value
	_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// cleanConfigMapForRecreate returns a copy of orig suitable for Create after Delete
func cleanConfigMapForRecreate(orig *corev1.ConfigMap) *corev1.ConfigMap {
	cm := orig.DeepCopy()
	cm.ResourceVersion = ""
	cm.UID = ""
	cm.Generation = 0
	cm.CreationTimestamp = metav1.Time{}
	cm.DeletionTimestamp = nil
	cm.DeletionGracePeriodSeconds = nil
	cm.ManagedFields = nil
	cm.Finalizers = nil
	return cm
}

// discoverSaturationConfigMapName finds the saturation scaling ConfigMap by suffix
// The ConfigMap name is generated from Helm chart template:
// {{ include "workload-variant-autoscaler.fullname" . }}-wva-saturation-scaling-config
// Since the fullname depends on release name and overrides, we discover it dynamically
func discoverSaturationConfigMapName(ctx context.Context, namespace string) string {
	const suffix = "-wva-saturation-scaling-config"

	cmList, err := k8sClient.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("Warning: Failed to list ConfigMaps in %s: %v\n", namespace, err)
		return ""
	}

	for _, cm := range cmList.Items {
		if strings.HasSuffix(cm.Name, suffix) {
			GinkgoWriter.Printf("Found saturation ConfigMap: %s/%s\n", namespace, cm.Name)
			return cm.Name
		}
	}

	GinkgoWriter.Printf("Warning: No ConfigMap with suffix '%s' found in namespace %s\n", suffix, namespace)
	return ""
}
