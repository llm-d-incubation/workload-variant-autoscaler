package benchmark

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// ScenarioResources holds the names of resources created during benchmark setup.
type ScenarioResources struct {
	PoolName       string
	ModelService   string
	DeploymentName string
	ServiceName    string
	VAName         string
	HPAName        string
	JobBaseName    string
}

// setupBenchmarkScenario creates fresh benchmark infrastructure from scratch:
// model service deployment, service, ServiceMonitor, VariantAutoscaling, and HPA.
// It waits for the deployment to be ready, VA to stabilize, external metrics API to
// serve wva_desired_replicas, and Prometheus to scrape simulator metrics.
// Cleanup is registered via DeferCleanup.
func setupBenchmarkScenario(res ScenarioResources) {
	GinkgoHelper()
	By("Creating model service deployment")
	err := fixtures.EnsureModelService(ctx, k8sClient, benchCfg.LLMDNamespace, res.ModelService, res.PoolName, benchCfg.ModelID, benchCfg.UseSimulator, benchCfg.MaxNumSeqs)
	Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

	DeferCleanup(func() {
		_ = k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Delete(ctx, res.DeploymentName, metav1.DeleteOptions{})
	})

	By("Creating service to expose model server")
	err = fixtures.EnsureService(ctx, k8sClient, benchCfg.LLMDNamespace, res.ModelService, res.DeploymentName, 8000)
	Expect(err).NotTo(HaveOccurred(), "Failed to create service")

	DeferCleanup(func() {
		_ = k8sClient.CoreV1().Services(benchCfg.LLMDNamespace).Delete(ctx, res.ServiceName, metav1.DeleteOptions{})
	})

	By("Creating ServiceMonitor for metrics scraping")
	err = fixtures.EnsureServiceMonitor(ctx, crClient, benchCfg.MonitoringNS, benchCfg.LLMDNamespace, res.ModelService, res.DeploymentName)
	Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor")

	DeferCleanup(func() {
		serviceMonitorName := res.ModelService + "-monitor"
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceMonitorName,
				Namespace: benchCfg.MonitoringNS,
			},
		})
	})

	By("Creating VariantAutoscaling resource")
	err = fixtures.EnsureVariantAutoscalingWithDefaults(
		ctx, crClient, benchCfg.LLMDNamespace, res.VAName,
		res.DeploymentName, benchCfg.ModelID, benchCfg.AcceleratorType,
		benchCfg.ControllerInstance,
	)
	Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

	DeferCleanup(func() {
		va := &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      res.VAName,
				Namespace: benchCfg.LLMDNamespace,
			},
		}
		_ = crClient.Delete(ctx, va)
	})

	By("Creating HPA for the deployment")
	err = fixtures.EnsureHPA(ctx, k8sClient, benchCfg.LLMDNamespace, res.HPAName, res.DeploymentName, res.VAName, 1, 10)
	Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")

	DeferCleanup(func() {
		hpaNameFull := res.HPAName + "-hpa"
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(ctx, hpaNameFull, metav1.DeleteOptions{})
	})

	By("Waiting for deployment to be ready at 1 replica")
	Eventually(func(g Gomega) {
		deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1), "Deployment should have at least 1 ready replica")
	}, 5*time.Minute, 5*time.Second).Should(Succeed())

	By("Waiting for VA to stabilize")
	Eventually(func(g Gomega) {
		currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: benchCfg.LLMDNamespace,
			Name:      res.VAName,
		}, currentVA)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(currentVA.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(), "NumReplicas should be set")
		g.Expect(*currentVA.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1), "VA should have optimized >= 1")
	}, 5*time.Minute, 10*time.Second).Should(Succeed())

	By("Verifying external metrics API serves wva_desired_replicas")
	Eventually(func(g Gomega) {
		result, err := k8sClient.RESTClient().
			Get().
			AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + benchCfg.LLMDNamespace + "/wva_desired_replicas").
			DoRaw(ctx)
		g.Expect(err).NotTo(HaveOccurred(), "External metrics API should be accessible")
		g.Expect(string(result)).To(ContainSubstring("wva_desired_replicas"), "Metric should be available")
		g.Expect(string(result)).To(ContainSubstring(res.VAName), "Metric should reference the benchmark VA")
		GinkgoWriter.Printf("External metrics API confirmed: wva_desired_replicas available for %s\n", res.VAName)
	}, 5*time.Minute, 10*time.Second).Should(Succeed())

	By("Waiting for Prometheus to scrape simulator metrics")
	Eventually(func(g Gomega) {
		_, err := promClient.QueryWithRetry(ctx, `vllm:kv_cache_usage_perc`)
		g.Expect(err).NotTo(HaveOccurred(), "Prometheus should have KV cache metrics from simulator")
		GinkgoWriter.Println("Prometheus confirmed: vllm:kv_cache_usage_perc is available")
	}, 5*time.Minute, 15*time.Second).Should(Succeed())

	GinkgoWriter.Println("Scenario setup completed — metrics pipeline verified")
}

// captureResultsAndGrafana captures Grafana snapshots (if enabled) and writes benchmark
// results to the configured output file. Call this from AfterAll.
func captureResultsAndGrafana(results *BenchmarkResults, scenarioStart time.Time) {
	GinkgoHelper()
	results.TotalDurationSec = time.Since(scenarioStart).Seconds()

	if grafanaClient != nil && benchCfg.GrafanaEnabled {
		By("Capturing Grafana snapshot of benchmark dashboard")
		snapResult, snapErr := grafanaClient.CreateSnapshot(scenarioStart)
		if snapErr != nil {
			GinkgoWriter.Printf("Warning: failed to create Grafana snapshot: %v\n", snapErr)
		} else {
			results.GrafanaSnapshotURL = snapResult.URL
			GinkgoWriter.Printf("Grafana snapshot: %s\n", snapResult.URL)

			if benchCfg.GrafanaSnapshotFile != "" {
				if writeErr := os.WriteFile(benchCfg.GrafanaSnapshotFile, []byte(snapResult.URL+"\n"), 0644); writeErr != nil {
					GinkgoWriter.Printf("Warning: failed to write snapshot URL file: %v\n", writeErr)
				}
			}

			if benchCfg.GrafanaSnapshotJSONFile != "" {
				By("Exporting Grafana snapshot JSON")
				if exportErr := grafanaClient.ExportSnapshotJSON(snapResult.Key, benchCfg.GrafanaSnapshotJSONFile); exportErr != nil {
					GinkgoWriter.Printf("Warning: failed to export snapshot JSON: %v\n", exportErr)
				} else {
					GinkgoWriter.Printf("Snapshot JSON exported to %s\n", benchCfg.GrafanaSnapshotJSONFile)
				}
			}
		}

		if benchCfg.GrafanaPanelDir != "" {
			By("Rendering dashboard panels to PNG")
			panelFiles, renderErr := grafanaClient.RenderAllPanels(scenarioStart, time.Now(), benchCfg.GrafanaPanelDir)
			if renderErr != nil {
				GinkgoWriter.Printf("Warning: panel rendering failed: %v\n", renderErr)
			} else {
				GinkgoWriter.Printf("Rendered %d panels to %s\n", len(panelFiles), benchCfg.GrafanaPanelDir)
			}
		}
	}

	By("Writing benchmark results to file")
	data, err := json.MarshalIndent(results, "", "  ")
	Expect(err).NotTo(HaveOccurred(), "Failed to marshal results")

	err = os.WriteFile(benchCfg.BenchmarkResultsFile, data, 0644)
	Expect(err).NotTo(HaveOccurred(), "Failed to write results file")

	GinkgoWriter.Printf("Benchmark results written to %s\n", benchCfg.BenchmarkResultsFile)
	GinkgoWriter.Printf("Results: %s\n", string(data))
}

// gatewayTargetURL returns the full URL for load generation through the Gateway stack.
func gatewayTargetURL() string {
	gwHost := fmt.Sprintf("%s.%s.svc.cluster.local", benchCfg.GatewayServiceName, benchCfg.LLMDNamespace)
	return fmt.Sprintf("http://%s:%d/v1/completions", gwHost, benchCfg.GatewayServicePort)
}
