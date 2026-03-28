// Scenario: scale-up-latency
//
// Measures how quickly WVA scales from min to target replicas under sudden load.
//
//	phases:
//	  - baseline (2m):  no load, establish baseline metrics in Prometheus
//	  - spike (5m):     burst load via parallel curl workers targeting /v1/completions,
//	                    measure time to first scale-up (VA → HPA → deployment)
//	  - sustained (3m): load continues, collect replica stability (stddev),
//	                    avg KV cache usage and queue depth from Prometheus
//	  - cooldown (5m):  remove load, measure time to scale back down to 1 replica
//
//	metrics:
//	  - scale_up_time_seconds:   time from load start to VA recommending >1 replicas
//	  - scale_down_time_seconds: time from load removal to deployment returning to 1 replica
//	  - max_replicas_reached:    peak replica count during spike+sustained
//	  - replica_oscillation:     stddev of replica samples during sustained phase
//	  - avg_kv_cache_usage:      mean KV cache utilization during sustained phase
//	  - avg_queue_depth:         mean queue depth during sustained phase
package benchmark

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Benchmark load generation constants.
// Match e2e's maxSingleReplicaWorkers=1 — single-replica deployments need only 1 worker
// to avoid overwhelming the simulator's max-num-seqs queue and causing request failures.
const (
	benchLoadWorkers       = 1
	benchRequestsPerWorker = 1100
	benchMaxTokens         = 400
)

var _ = Describe("Scale-Up Latency Benchmark", Label("benchmark"), Ordered, func() {
	var (
		res = ScenarioResources{
			PoolName:       "bench-pool",
			ModelService:   "bench-ms",
			DeploymentName: "bench-ms-decode",
			ServiceName:    "bench-ms-service",
			VAName:         "bench-va",
			HPAName:        "bench-hpa",
			JobBaseName:    "bench-ms",
		}

		results       BenchmarkResults
		scenarioStart time.Time
	)

	BeforeAll(func() {
		setupBenchmarkScenario(res)
		scenarioStart = time.Now()
		GinkgoWriter.Println("Benchmark scenario starting")
	})

	AfterAll(func() {
		captureResultsAndGrafana(&results, scenarioStart)
	})

	It("Phase 1: Baseline — establish baseline metrics", func() {
		baselineDuration := time.Duration(benchCfg.BaselineDurationSec) * time.Second
		GinkgoWriter.Printf("Running baseline phase for %v (Prometheus collects replica metrics)\n", baselineDuration)

		time.Sleep(baselineDuration)

		GinkgoWriter.Println("Baseline phase complete")
	})

	It("Phase 2: Spike — launch load and observe scale-up", func() {
		spikeDuration := time.Duration(benchCfg.SpikeDurationSec) * time.Second
		GinkgoWriter.Printf("Running spike phase for %v\n", spikeDuration)

		spikeStart := time.Now()

		targetURL := gatewayTargetURL()
		GinkgoWriter.Printf("Load target URL (via Gateway): %s\n", targetURL)

		By("Cleaning up any existing load jobs")
		fixtures.DeleteParallelLoadJobs(ctx, k8sClient, res.JobBaseName, benchCfg.LLMDNamespace, benchLoadWorkers)

		By("Launching parallel load generation jobs")
		loadCfg := fixtures.LoadConfig{
			Strategy:     benchCfg.LoadStrategy,
			NumPrompts:   benchRequestsPerWorker,
			InputTokens:  benchCfg.InputTokens,
			OutputTokens: benchMaxTokens,
			ModelID:      benchCfg.ModelID,
		}
		err := fixtures.EnsureParallelLoadJobs(ctx, k8sClient, res.JobBaseName, benchCfg.LLMDNamespace, targetURL, benchLoadWorkers, loadCfg)
		Expect(err).NotTo(HaveOccurred(), "Failed to create load generation jobs")

		DeferCleanup(func() {
			fixtures.DeleteParallelLoadJobs(ctx, k8sClient, res.JobBaseName, benchCfg.LLMDNamespace, benchLoadWorkers)
		})

		By("Observing scale-up during spike phase (no assertions — Prometheus captures replica metrics)")
		var maxReplicas int32 = 1
		deadline := time.Now().Add(spikeDuration)

		for time.Now().Before(deadline) {
			// Observe VA status
			currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
			if vaErr := crClient.Get(ctx, client.ObjectKey{Namespace: benchCfg.LLMDNamespace, Name: res.VAName}, currentVA); vaErr == nil {
				var optimized int32
				if currentVA.Status.DesiredOptimizedAlloc.NumReplicas != nil {
					optimized = *currentVA.Status.DesiredOptimizedAlloc.NumReplicas
				}
				if optimized > 1 && results.ScaleUpTimeSec == 0 {
					results.ScaleUpTimeSec = time.Since(spikeStart).Seconds()
					GinkgoWriter.Printf("VA scale-up detected at %.1fs (optimized=%d)\n", results.ScaleUpTimeSec, optimized)
				}
			}

			// Observe deployment replicas
			deployment, deployErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
			if deployErr == nil {
				replicas := *deployment.Spec.Replicas
				if replicas > maxReplicas {
					maxReplicas = replicas
				}
				GinkgoWriter.Printf("Spike: deployment replicas=%d, ready=%d\n",
					replicas, deployment.Status.ReadyReplicas)
			}

			time.Sleep(15 * time.Second)
		}

		results.MaxReplicas = maxReplicas
		GinkgoWriter.Printf("Spike phase complete: maxReplicas=%d, scaleUpTime=%.1fs\n", maxReplicas, results.ScaleUpTimeSec)
	})

	It("Phase 3: Sustained — collect stability metrics", func() {
		sustainedDuration := time.Duration(benchCfg.SustainedDurationSec) * time.Second
		GinkgoWriter.Printf("Running sustained phase for %v\n", sustainedDuration)

		sustainedStart := time.Now()

		var replicaSamples []float64
		deadline := time.Now().Add(sustainedDuration)

		for time.Now().Before(deadline) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
			if err != nil {
				GinkgoWriter.Printf("Warning: failed to get deployment: %v\n", err)
				time.Sleep(15 * time.Second)
				continue
			}

			replicas := float64(*deployment.Spec.Replicas)
			replicaSamples = append(replicaSamples, replicas)

			if replicas > float64(results.MaxReplicas) {
				results.MaxReplicas = int32(replicas)
			}

			GinkgoWriter.Printf("Sustained: replicas=%.0f\n", replicas)
			time.Sleep(15 * time.Second)
		}

		if len(replicaSamples) > 1 {
			results.ReplicaOscillation = stddev(replicaSamples)
		}

		sustainedEnd := time.Now()
		kvAvg, err := QueryRangeAvg(
			promClient.API(),
			`avg(vllm:kv_cache_usage_perc)`,
			sustainedStart, sustainedEnd,
			30*time.Second,
		)
		if err != nil {
			GinkgoWriter.Printf("Warning: failed to query KV cache avg: %v\n", err)
		} else {
			results.AvgKVCacheUsage = kvAvg
		}

		qdAvg, err := QueryRangeAvg(
			promClient.API(),
			`avg(vllm:num_requests_waiting)`,
			sustainedStart, sustainedEnd,
			30*time.Second,
		)
		if err != nil {
			GinkgoWriter.Printf("Warning: failed to query queue depth avg: %v\n", err)
		} else {
			results.AvgQueueDepth = qdAvg
		}

		GinkgoWriter.Printf("Sustained phase complete: oscillation=%.2f, kvCache=%.3f, queueDepth=%.2f\n",
			results.ReplicaOscillation, results.AvgKVCacheUsage, results.AvgQueueDepth)
	})

	It("Phase 4: Cooldown — delete load and measure scale-down time", func() {
		cooldownDuration := time.Duration(benchCfg.CooldownDurationSec) * time.Second
		GinkgoWriter.Printf("Running cooldown phase for %v\n", cooldownDuration)

		By("Deleting load generation jobs")
		fixtures.DeleteParallelLoadJobs(ctx, k8sClient, res.JobBaseName, benchCfg.LLMDNamespace, benchLoadWorkers)

		cooldownStart := time.Now()
		scaleDownDetected := false
		deadline := time.Now().Add(cooldownDuration)

		for time.Now().Before(deadline) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
			if err != nil {
				GinkgoWriter.Printf("Warning: failed to get deployment: %v\n", err)
				time.Sleep(15 * time.Second)
				continue
			}

			replicas := *deployment.Spec.Replicas
			GinkgoWriter.Printf("Cooldown: spec replicas=%d (elapsed %v)\n",
				replicas, time.Since(cooldownStart).Round(time.Second))

			if replicas <= 1 && !scaleDownDetected {
				results.ScaleDownTimeSec = time.Since(cooldownStart).Seconds()
				scaleDownDetected = true
				GinkgoWriter.Printf("Scale-down detected at %.1fs\n", results.ScaleDownTimeSec)
				break
			}

			time.Sleep(15 * time.Second)
		}

		if !scaleDownDetected {
			GinkgoWriter.Println("WARNING: Scale-down was NOT detected during cooldown phase")
			results.ScaleDownTimeSec = -1
		}
	})
})
