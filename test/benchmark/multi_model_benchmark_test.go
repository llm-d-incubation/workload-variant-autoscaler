package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/testconfig"
)

// MultiModelResult holds per-model results for the multi-model benchmark.
type MultiModelResult struct {
	ModelID          string          `json:"model_id"`
	Slug             string          `json:"slug"`
	DeploymentName   string          `json:"deployment_name"`
	AutoscalerType   string          `json:"autoscaler_type"`
	VAName           string          `json:"va_name"`
	HPAName          string          `json:"hpa_name"`
	ReplicaTimeline  []ReplicaSnap   `json:"replica_timeline"`
	MetricsTimeline  []MetricSnap    `json:"metrics_timeline"`
	AvgReplicas      float64         `json:"avg_replicas"`
	MaxReplicas      int32           `json:"max_replicas"`
	AvgQueueDepth    float64         `json:"avg_queue_depth"`
	AvgEPPQueueDepth float64         `json:"avg_epp_queue_depth"`
	AvgKVCache       float64         `json:"avg_kv_cache"`
	AchievedRPS      float64         `json:"achieved_rps"`
	ErrorCount       int             `json:"error_count"`
	IncompleteCount  int             `json:"incomplete_count"`
	TTFT             json.RawMessage `json:"ttft,omitempty"`
	ITL              json.RawMessage `json:"itl,omitempty"`
	Throughput       json.RawMessage `json:"throughput,omitempty"`
	GuideLLMRaw      json.RawMessage `json:"guidellm_raw,omitempty"`
	DurationSec      float64         `json:"duration_sec"`
	JobStatus        string          `json:"job_status"`
}

const multiModelResultsFile = "/tmp/multi-model-benchmark-results.json"

// modelToSlug converts a model ID to a DNS-safe slug, matching the deploy script convention.
func modelToSlug(modelID string) string {
	s := strings.ToLower(modelID)
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}

// modelInfo groups per-model resources discovered at runtime.
type modelInfo struct {
	ModelID        string
	Slug           string
	DeploymentName string
	VAName         string
	HPAName        string
	JobName        string
}

var _ = Describe("Multi-Model Scaling Benchmark", Ordered, Label("benchmark", "multi-model"), func() {
	var (
		testCtx    context.Context
		testCancel context.CancelFunc
		models     []modelInfo
	)

	BeforeAll(func() {
		testCtx, testCancel = context.WithCancel(context.Background()) //nolint:fatcontext // top-level BeforeAll, not nested

		// Parse MODELS env var (comma-separated list of model IDs)
		modelsEnv := testconfig.GetEnv("MODELS", "")
		Expect(modelsEnv).NotTo(BeEmpty(), "MODELS env var is required (comma-separated model IDs)")

		modelIDs := strings.Split(modelsEnv, ",")
		for _, m := range modelIDs {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			slug := modelToSlug(m)
			models = append(models, modelInfo{
				ModelID: m,
				Slug:    slug,
				VAName:  "va-" + slug,
				HPAName: "hpa-" + slug,
				JobName: "load-" + slug,
			})
		}
		Expect(models).NotTo(BeEmpty(), "At least one model must be specified in MODELS")

		GinkgoWriter.Printf("Multi-model benchmark configured for %d models:\n", len(models))
		for i, m := range models {
			GinkgoWriter.Printf("  [%d] %s (slug: %s)\n", i+1, m.ModelID, m.Slug)
		}
	})

	AfterAll(func() {
		testCancel()
	})

	// discoverDeployments finds the Helm-deployed decode deployment for each model.
	discoverDeployments := func() {
		By("Discovering decode deployments for each model")
		deployments, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).List(testCtx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to list deployments")

		for i := range models {
			slug := models[i].Slug
			for j := range deployments.Items {
				d := &deployments.Items[j]
				if strings.Contains(d.Name, "ms-"+slug) && strings.HasSuffix(d.Name, "-decode") {
					models[i].DeploymentName = d.Name
					GinkgoWriter.Printf("  %s → %s\n", slug, d.Name)
					break
				}
			}
			Expect(models[i].DeploymentName).NotTo(BeEmpty(),
				"No decode deployment found for model slug "+slug)
		}
	}

	// patchAllEPPConfigs patches every EPP deployment's ConfigMap with flowControl
	// enabled and scorer weights (queue=2, kv-cache=2, prefix-cache=3),
	// matching the single-model test-benchmark behavior.
	patchAllEPPConfigs := func() {
		By("Patching all EPP ConfigMaps with flowControl + scorer weights (queue=2, kv-cache=2, prefix-cache=3)")
		deployments, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).List(testCtx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())

		for i := range deployments.Items {
			d := &deployments.Items[i]
			if !strings.Contains(d.Name, "epp") {
				continue
			}
			GinkgoWriter.Printf("  Patching EPP: %s\n", d.Name)
			patchErr := PatchEPPConfigMap(testCtx, k8sClient, benchCfg.LLMDNamespace, d.Name)
			if patchErr != nil {
				GinkgoWriter.Printf("  WARNING: EPP patch failed for %s (non-fatal): %v\n", d.Name, patchErr)
			} else {
				GinkgoWriter.Printf("  EPP %s patched — flowControl enabled, weights 2/2/3\n", d.Name)
			}
		}
	}

	// ensureDeploymentsReady scales each model deployment to 1 and waits for readiness.
	ensureDeploymentsReady := func() {
		By("Ensuring all decode deployments are ready")
		for _, m := range models {
			dep, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(testCtx, m.DeploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			if dep.Spec.Replicas == nil || *dep.Spec.Replicas < 1 {
				one := int32(1)
				dep.Spec.Replicas = &one
				_, err = k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Update(testCtx, dep, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		for _, m := range models {
			Eventually(func(g Gomega) {
				d, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(testCtx, m.DeploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(d.Status.ReadyReplicas).To(BeNumerically(">=", 1))
			}, 5*time.Minute, 10*time.Second).Should(Succeed(),
				fmt.Sprintf("Deployment %s should have at least 1 ready replica", m.DeploymentName))
			GinkgoWriter.Printf("  %s: ready\n", m.DeploymentName)
		}
	}

	// createVAsAndHPAs creates a VA + HPA per model.
	createVAsAndHPAs := func() {
		By("Creating VariantAutoscaling and HPA resources for each model")
		maxReplicas := int32(testconfig.GetEnvInt("MM_MAX_REPLICAS", 5))
		minReplicas := int32(testconfig.GetEnvInt("MM_MIN_REPLICAS", 1))

		for _, m := range models {
			GinkgoWriter.Printf("  Creating VA %s → %s (model=%s)\n", m.VAName, m.DeploymentName, m.ModelID)
			err := fixtures.EnsureVariantAutoscaling(
				testCtx, crClient, benchCfg.LLMDNamespace,
				m.VAName, m.DeploymentName, m.ModelID,
				benchCfg.AcceleratorType, 10.0, benchCfg.ControllerInstance,
				fixtures.WithMinReplicas(minReplicas),
				fixtures.WithMaxReplicas(maxReplicas),
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VA "+m.VAName)

			GinkgoWriter.Printf("  Creating HPA %s → %s\n", m.HPAName, m.DeploymentName)
			behavior := &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(0)),
					Policies: []autoscalingv2.HPAScalingPolicy{
						{Type: autoscalingv2.PodsScalingPolicy, Value: 10, PeriodSeconds: 150},
					},
				},
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(240)),
					Policies: []autoscalingv2.HPAScalingPolicy{
						{Type: autoscalingv2.PodsScalingPolicy, Value: 10, PeriodSeconds: 150},
					},
				},
			}
			err = fixtures.EnsureHPA(testCtx, k8sClient, benchCfg.LLMDNamespace,
				m.HPAName, m.DeploymentName, m.VAName,
				minReplicas, maxReplicas, WithBehavior(behavior))
			Expect(err).NotTo(HaveOccurred(), "Failed to create HPA "+m.HPAName)
		}
	}

	// waitForVASync waits for all VAs to have desiredOptimizedAlloc.numReplicas set.
	waitForVASync := func() {
		By("Waiting for all VariantAutoscaling resources to sync")
		for _, m := range models {
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(testCtx, client.ObjectKey{
					Namespace: benchCfg.LLMDNamespace,
					Name:      m.VAName,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil())
				g.Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1))
				GinkgoWriter.Printf("  %s: desiredReplicas=%d\n", m.VAName, *va.Status.DesiredOptimizedAlloc.NumReplicas)
			}, 5*time.Minute, 10*time.Second).Should(Succeed(),
				fmt.Sprintf("VA %s should have NumReplicas set", m.VAName))
		}
	}

	// launchLoadJobs creates a GuideLLM job for each model targeting the shared Gateway.
	launchLoadJobs := func() {
		By("Launching GuideLLM load jobs for each model")
		gatewayName := testconfig.GetEnv("GATEWAY_SERVICE_NAME", "multi-model-inference-gateway-istio")
		gwHost := fmt.Sprintf("%s.%s.svc.cluster.local", gatewayName, benchCfg.LLMDNamespace)

		for _, m := range models {
			targetURL := fmt.Sprintf("http://%s/%s", gwHost, m.Slug)
			GinkgoWriter.Printf("  %s → %s\n", m.JobName, targetURL)

			err := CreateGuideLLMJobWithArgs(
				testCtx, k8sClient, benchCfg.LLMDNamespace,
				m.JobName, targetURL, m.ModelID,
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create load job "+m.JobName)
		}
	}

	// monitorAndCollect monitors scaling metrics and collects results for all models.
	monitorAndCollect := func() []MultiModelResult {
		// Wait for all load job pods to be Running before starting the monitor clock.
		// The GuideLLM container sleeps 30s + pod scheduling/image pull can take time.
		By("Waiting for all load job pods to enter Running state")
		for _, m := range models {
			jobName := m.JobName + "-load"
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(testCtx, metav1.ListOptions{
					LabelSelector: "job-name=" + jobName,
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "no pods for job "+jobName)
				g.Expect(string(pods.Items[0].Status.Phase)).To(Equal("Running"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed(),
				fmt.Sprintf("job pod for %s should be Running", jobName))
			GinkgoWriter.Printf("  %s pod is Running\n", jobName)
		}

		// Add 30s grace for the GuideLLM 'sleep 30' startup inside the container
		GinkgoWriter.Println("Waiting 35s for GuideLLM in-container startup delay...")
		time.Sleep(35 * time.Second)

		By("Starting scaling monitor")
		loadStart := time.Now()

		// Per-model accumulators
		type accumulator struct {
			replicaSum, kvSum, qdSum, eppQDSum         float64
			replicaCount, kvCount, qdCount, eppQDCount int
			maxReplicas                                int32
			timeline                                   []ReplicaSnap
			metrics                                    []MetricSnap
		}
		accums := make(map[string]*accumulator, len(models))
		for _, m := range models {
			accums[m.Slug] = &accumulator{maxReplicas: 1}
		}

		// The GuideLLM job uses --max-seconds=600 (hardcoded in workload.go).
		// Monitor timeout must exceed this to capture full run + allow job completion.
		monitorTimeout := 780 * time.Second // 600s job + 180s buffer (startup + cooldown)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		done := make(chan struct{})
		go func() {
			defer close(done)
			// Wait for all jobs to complete
			for _, m := range models {
				_ = WaitForJobCompletion(testCtx, k8sClient, benchCfg.LLMDNamespace, m.JobName+"-load", 25*time.Minute)
			}
		}()

		deadline := time.After(monitorTimeout)
	monitorLoop:
		for {
			select {
			case <-done:
				GinkgoWriter.Println("All load jobs completed.")
				break monitorLoop
			case <-deadline:
				GinkgoWriter.Println("Monitor timeout reached.")
				break monitorLoop
			case <-ticker.C:
				elapsed := time.Since(loadStart).Seconds()
				for _, m := range models {
					acc := accums[m.Slug]

					// Deployment replicas
					var spec, ready int32
					dep, depErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(testCtx, m.DeploymentName, metav1.GetOptions{})
					if depErr == nil && dep.Spec.Replicas != nil {
						spec = *dep.Spec.Replicas
						ready = dep.Status.ReadyReplicas
						acc.replicaSum += float64(spec)
						acc.replicaCount++
						if spec > acc.maxReplicas {
							acc.maxReplicas = spec
						}
						acc.timeline = append(acc.timeline, ReplicaSnap{ElapsedSec: elapsed, SpecReplicas: spec, ReadyReplicas: ready})
					}

					// Prometheus metrics — filter per model
					snap := MetricSnap{ElapsedSec: elapsed}
					qdQuery := fmt.Sprintf(`avg(vllm:num_requests_waiting{namespace="%s",model_name="%s"})`, benchCfg.LLMDNamespace, m.ModelID)
					kvQuery := fmt.Sprintf(`avg(vllm:kv_cache_usage_perc{namespace="%s",model_name="%s"})`, benchCfg.LLMDNamespace, m.ModelID)
					eppPoolName := "gaie-" + m.Slug
					eppQDQuery := fmt.Sprintf(`sum(inference_extension_flow_control_queue_size{namespace="%s",inference_pool="%s"})`, benchCfg.LLMDNamespace, eppPoolName)

					if qdResult, _, qdErr := promClient.API().Query(testCtx, qdQuery, time.Now()); qdErr == nil {
						if vec, ok := qdResult.(model.Vector); ok && len(vec) > 0 {
							snap.QueueDepth = float64(vec[0].Value)
							acc.qdSum += snap.QueueDepth
							acc.qdCount++
						}
					}
					if kvResult, _, kvErr := promClient.API().Query(testCtx, kvQuery, time.Now()); kvErr == nil {
						if vec, ok := kvResult.(model.Vector); ok && len(vec) > 0 {
							snap.KVCache = float64(vec[0].Value)
							acc.kvSum += snap.KVCache
							acc.kvCount++
						}
					}
					if eppResult, _, eppErr := promClient.API().Query(testCtx, eppQDQuery, time.Now()); eppErr == nil {
						if vec, ok := eppResult.(model.Vector); ok && len(vec) > 0 {
							snap.EPPQueueDepth = float64(vec[0].Value)
							acc.eppQDSum += snap.EPPQueueDepth
							acc.eppQDCount++
						}
					}
					acc.metrics = append(acc.metrics, snap)

					// VA/HPA status
					vaDesired := "?"
					va := &variantautoscalingv1alpha1.VariantAutoscaling{}
					if vaErr := crClient.Get(testCtx, client.ObjectKey{Namespace: benchCfg.LLMDNamespace, Name: m.VAName}, va); vaErr == nil {
						if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
							vaDesired = strconv.FormatInt(int64(*va.Status.DesiredOptimizedAlloc.NumReplicas), 10)
						}
					}
					hpaCurrent, hpaDesired := "?", "?"
					hpa, hpaErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Get(testCtx, m.HPAName+"-hpa", metav1.GetOptions{})
					if hpaErr == nil {
						hpaCurrent = strconv.FormatInt(int64(hpa.Status.CurrentReplicas), 10)
						hpaDesired = strconv.FormatInt(int64(hpa.Status.DesiredReplicas), 10)
					}

					GinkgoWriter.Printf("  [%s %.0fs] replicas: spec=%d ready=%d | va=%s hpa=%s→%s | kv=%.4f qd=%.1f epp_qd=%.1f\n",
						m.Slug, elapsed, spec, ready, vaDesired, hpaCurrent, hpaDesired,
						snap.KVCache, snap.QueueDepth, snap.EPPQueueDepth)
				}
			}
		}

		loadEnd := time.Now()
		totalDuration := loadEnd.Sub(loadStart).Seconds()

		// Ensure all jobs have completed before collecting results
		By("Waiting for any remaining load jobs to complete")
		for _, m := range models {
			jobName := m.JobName + "-load"
			if waitErr := WaitForJobCompletion(testCtx, k8sClient, benchCfg.LLMDNamespace, jobName, 5*time.Minute); waitErr != nil {
				GinkgoWriter.Printf("  WARNING: job %s did not complete cleanly: %v\n", jobName, waitErr)
			} else {
				GinkgoWriter.Printf("  Job %s completed\n", jobName)
			}
		}

		// Collect results per model
		results := make([]MultiModelResult, 0, len(models))
		for _, m := range models {
			acc := accums[m.Slug]
			jobName := m.JobName + "-load"

			// Get GuideLLM output from pod logs
			var guidellmRaw json.RawMessage
			var ttftJSON, itlJSON, throughputJSON json.RawMessage
			var errorCount, incompleteCount, completedCount int
			var achievedRPS float64

			logs, logErr := GetJobPodLogs(testCtx, k8sClient, benchCfg.LLMDNamespace, jobName)
			if logErr == nil {
				if idx := strings.Index(logs, "=== BENCHMARK JSON ==="); idx != -1 {
					jsonStr := strings.TrimSpace(logs[idx+len("=== BENCHMARK JSON ==="):])
					guidellmRaw = json.RawMessage(jsonStr)

					var parsed map[string]interface{}
					if jsonErr := json.Unmarshal([]byte(jsonStr), &parsed); jsonErr == nil {
						extractGuideLLMMetric(&parsed, "time_to_first_token_ms", &ttftJSON)
						extractGuideLLMMetric(&parsed, "inter_token_latency_ms", &itlJSON)
						extractGuideLLMMetric(&parsed, "output_tokens_per_second", &throughputJSON)

						if benchmarks, ok := parsed["benchmarks"].([]interface{}); ok && len(benchmarks) > 0 {
							if bm, ok := benchmarks[0].(map[string]interface{}); ok {
								if metrics, ok := bm["metrics"].(map[string]interface{}); ok {
									if rt, ok := metrics["request_totals"].(map[string]interface{}); ok {
										if f, ok := rt["errored"].(float64); ok {
											errorCount = int(f)
										}
										if f, ok := rt["incomplete"].(float64); ok {
											incompleteCount = int(f)
										}
										if f, ok := rt["successful"].(float64); ok {
											completedCount = int(f)
										}
									}
								}
								if rateObj, ok := bm["rate"].(map[string]interface{}); ok {
									if f, ok := rateObj["completed_rate"].(float64); ok {
										achievedRPS = f
									}
								}
							}
						}
					}
				}
			}
			if achievedRPS == 0 && completedCount > 0 && totalDuration > 0 {
				achievedRPS = float64(completedCount) / totalDuration
			}

			// Compute averages
			avgReplicas := float64(0)
			if acc.replicaCount > 0 {
				avgReplicas = acc.replicaSum / float64(acc.replicaCount)
			}
			avgKV := float64(0)
			if acc.kvCount > 0 {
				avgKV = acc.kvSum / float64(acc.kvCount)
			}
			avgQD := float64(0)
			if acc.qdCount > 0 {
				avgQD = acc.qdSum / float64(acc.qdCount)
			}
			avgEPPQD := float64(0)
			if acc.eppQDCount > 0 {
				avgEPPQD = acc.eppQDSum / float64(acc.eppQDCount)
			}

			// Job status
			jobStatus := "Unknown"
			job, jobErr := k8sClient.BatchV1().Jobs(benchCfg.LLMDNamespace).Get(testCtx, jobName, metav1.GetOptions{})
			if jobErr == nil {
				for _, cond := range job.Status.Conditions {
					if cond.Status == conditionStatusTrue {
						jobStatus = string(cond.Type)
						break
					}
				}
			}

			results = append(results, MultiModelResult{
				ModelID:          m.ModelID,
				Slug:             m.Slug,
				DeploymentName:   m.DeploymentName,
				AutoscalerType:   "WVA",
				VAName:           m.VAName,
				HPAName:          m.HPAName,
				ReplicaTimeline:  acc.timeline,
				MetricsTimeline:  acc.metrics,
				AvgReplicas:      avgReplicas,
				MaxReplicas:      acc.maxReplicas,
				AvgQueueDepth:    avgQD,
				AvgEPPQueueDepth: avgEPPQD,
				AvgKVCache:       avgKV,
				AchievedRPS:      achievedRPS,
				ErrorCount:       errorCount,
				IncompleteCount:  incompleteCount,
				TTFT:             ttftJSON,
				ITL:              itlJSON,
				Throughput:       throughputJSON,
				GuideLLMRaw:      guidellmRaw,
				DurationSec:      totalDuration,
				JobStatus:        jobStatus,
			})
		}

		return results
	}

	// printResults outputs the results in the unified box-drawing format.
	printResults := func(results []MultiModelResult) {
		formatPercentiles := func(raw json.RawMessage) string {
			if raw == nil {
				return "n/a"
			}
			var m map[string]interface{}
			if err := json.Unmarshal(raw, &m); err != nil {
				return string(raw)
			}
			p50, _ := m["p50"].(float64)
			p90, _ := m["p90"].(float64)
			p99, _ := m["p99"].(float64)
			if p50 == 0 && p90 == 0 && p99 == 0 {
				return string(raw)
			}
			return fmt.Sprintf("p50=%.1f p90=%.1f p99=%.1f", p50, p90, p99)
		}

		GinkgoWriter.Printf("\n══════════════════════════════════════════════════════════════════\n")
		GinkgoWriter.Printf("  MULTI-MODEL SCALING BENCHMARK RESULTS\n")
		GinkgoWriter.Printf("  Models: %d\n", len(results))
		GinkgoWriter.Printf("══════════════════════════════════════════════════════════════════\n\n")

		for _, r := range results {
			// Final deployment state
			finalSpec := int32(0)
			finalReady := int32(0)
			dep, depErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(testCtx, r.DeploymentName, metav1.GetOptions{})
			if depErr == nil && dep.Spec.Replicas != nil {
				finalSpec = *dep.Spec.Replicas
				finalReady = dep.Status.ReadyReplicas
			}

			GinkgoWriter.Printf("  ┌────────────────────────────────────────────────────────────\n")
			GinkgoWriter.Printf("  │ MODEL: %s\n", r.ModelID)
			GinkgoWriter.Printf("  │ Slug:  %s\n", r.Slug)
			GinkgoWriter.Printf("  ├────────────────────────────────────────────────────────────\n")
			GinkgoWriter.Printf("  │ Load Job:        %s\n", r.JobStatus)
			GinkgoWriter.Printf("  │ Duration:        %.0fs\n", r.DurationSec)
			GinkgoWriter.Printf("  │ Final Replicas:  spec=%d ready=%d\n", finalSpec, finalReady)
			GinkgoWriter.Printf("  │ Max Replicas:    %d\n", r.MaxReplicas)
			GinkgoWriter.Printf("  │ Avg Replicas:    %.2f\n", r.AvgReplicas)
			GinkgoWriter.Printf("  ├── Prometheus Metrics ──────────────────────────────────────\n")
			GinkgoWriter.Printf("  │ Avg KV Cache:    %.4f\n", r.AvgKVCache)
			GinkgoWriter.Printf("  │ Avg Queue Depth: %.2f\n", r.AvgQueueDepth)
			GinkgoWriter.Printf("  │ Avg EPP Queue:   %.2f\n", r.AvgEPPQueueDepth)
			GinkgoWriter.Printf("  ├── GuideLLM Results ────────────────────────────────────────\n")
			GinkgoWriter.Printf("  │ Achieved RPS:    %.2f\n", r.AchievedRPS)
			GinkgoWriter.Printf("  │ TTFT (ms):       %s\n", formatPercentiles(r.TTFT))
			GinkgoWriter.Printf("  │ ITL (ms):        %s\n", formatPercentiles(r.ITL))
			GinkgoWriter.Printf("  │ Throughput:      %s\n", formatPercentiles(r.Throughput))
			GinkgoWriter.Printf("  │ Errors:          %d\n", r.ErrorCount)
			GinkgoWriter.Printf("  │ Incomplete:      %d\n", r.IncompleteCount)
			GinkgoWriter.Printf("  ├── Replica Timeline (%d snapshots) ─────────────────────────\n", len(r.ReplicaTimeline))
			for _, s := range r.ReplicaTimeline {
				GinkgoWriter.Printf("  │   t=%.0fs  spec=%d  ready=%d\n", s.ElapsedSec, s.SpecReplicas, s.ReadyReplicas)
			}
			GinkgoWriter.Printf("  └────────────────────────────────────────────────────────────\n\n")
		}
	}

	Context("WVA Multi-Model", func() {
		It("should scale multiple models independently under prefill-heavy load", func() {
			discoverDeployments()
			patchAllEPPConfigs()
			ensureDeploymentsReady()
			createVAsAndHPAs()
			waitForVASync()
			launchLoadJobs()

			results := monitorAndCollect()
			printResults(results)

			By("Saving multi-model benchmark results to file")
			data, err := json.MarshalIndent(results, "", "  ")
			Expect(err).NotTo(HaveOccurred())
			err = os.WriteFile(multiModelResultsFile, data, 0644)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Results saved to %s\n", multiModelResultsFile)
		})
	})

	AfterAll(func() {
		GinkgoWriter.Println("Multi-model benchmark complete — cleaning up")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()

		for _, m := range models {
			_ = k8sClient.BatchV1().Jobs(benchCfg.LLMDNamespace).Delete(cleanupCtx, m.JobName+"-load", metav1.DeleteOptions{
				PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
			})
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, m.HPAName+"-hpa", metav1.DeleteOptions{})
			_ = fixtures.DeleteVariantAutoscaling(cleanupCtx, crClient, benchCfg.LLMDNamespace, m.VAName)

			// Scale back decode deployments to 1
			if m.DeploymentName != "" {
				scale, scaleErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).GetScale(cleanupCtx, m.DeploymentName, metav1.GetOptions{})
				if scaleErr == nil && scale.Spec.Replicas > 1 {
					scale.Spec.Replicas = 1
					_, _ = k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).UpdateScale(cleanupCtx, m.DeploymentName, scale, metav1.UpdateOptions{})
					GinkgoWriter.Printf("  Scaled %s back to 1\n", m.DeploymentName)
				}
			}
		}
	})
})
