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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// PodInfo records pod placement and startup details for the report.
type PodInfo struct {
	Name       string  `json:"name"`
	Node       string  `json:"node"`
	GPU        string  `json:"gpu"`
	StartupSec float64 `json:"startup_sec"`
}

// PrefillResult holds results for one prefill benchmark run (HPA or WVA).
type PrefillResult struct {
	AutoscalerType   string          `json:"autoscaler_type"`
	ModelID          string          `json:"model_id"`
	VAConfig         string          `json:"va_config"`
	HPAConfig        string          `json:"hpa_config"`
	Pods             []PodInfo       `json:"pods,omitempty"`
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
}

// ReplicaSnap records replica count at a point in time.
type ReplicaSnap struct {
	ElapsedSec    float64 `json:"elapsed_sec"`
	SpecReplicas  int32   `json:"spec_replicas"`
	ReadyReplicas int32   `json:"ready_replicas"`
}

// MetricSnap records KV cache and queue depth at a point in time.
type MetricSnap struct {
	ElapsedSec    float64 `json:"elapsed_sec"`
	QueueDepth    float64 `json:"queue_depth"`
	EPPQueueDepth float64 `json:"epp_queue_depth"`
	KVCache       float64 `json:"kv_cache"`
}

var prefillResults []PrefillResult

const prefillResultsFile = "/tmp/prefill-benchmark-results.json"

var _ = Describe("Prefill Heavy Workload Benchmark", Ordered, Label("benchmark", "phase3a"), func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
		res    ScenarioResources
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background()) //nolint:fatcontext // Ginkgo BeforeEach requires reassigning outer ctx
		res = ScenarioResources{
			PoolName:     benchCfg.PoolName,
			ModelService: "prefill-ms",
			VAName:       "prefill-va",
			HPAName:      "prefill-hpa",
			JobBaseName:  "prefill-ms",
		}
	})

	AfterEach(func() {
		cancel()
	})

	// cleanupAutoscalers removes leftover HPAs and VAs from previous tests to avoid conflicts.
	cleanupAutoscalers := func() {
		GinkgoWriter.Println("Cleaning up existing autoscalers...")
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(ctx, res.HPAName+"-standard-hpa", metav1.DeleteOptions{})
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(ctx, res.HPAName+"-hpa", metav1.DeleteOptions{})
		_ = fixtures.DeleteVariantAutoscaling(ctx, crClient, benchCfg.LLMDNamespace, res.VAName)
		time.Sleep(3 * time.Second)
	}

	// findInfraDecodeDeployment discovers the Helm-deployed decode deployment.
	// We reuse this deployment instead of creating a new one because the Gateway/EPP
	// routing is configured to match its labels (InferencePool selector).
	findInfraDecodeDeployment := func() string {
		By("Finding Helm-deployed decode deployment for Gateway-compatible routing")
		deployments, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to list deployments")
		for i := range deployments.Items {
			d := &deployments.Items[i]
			if strings.HasSuffix(d.Name, "-decode") && strings.Contains(d.Name, "modelservice") {
				GinkgoWriter.Printf("  Found infra decode deployment: %s\n", d.Name)
				return d.Name
			}
		}
		Fail("No Helm-deployed decode deployment found in namespace " + benchCfg.LLMDNamespace)
		return ""
	}

	// ensureInfraDeploymentReady scales the Helm-deployed model service to 1 replica and waits for readiness.
	ensureInfraDeploymentReady := func() {
		By("Ensuring infra decode deployment is scaled to 1 and ready")
		one := int32(1)
		deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
			deployment.Spec.Replicas = &one
			_, err = k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to scale infra deployment to 1")
			GinkgoWriter.Printf("  Scaled %s to 1 replica\n", res.DeploymentName)
		}

		By("Waiting for infra deployment to have at least 1 ready replica")
		Eventually(func(g Gomega) {
			d, getErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			spec := int32(0)
			if d.Spec.Replicas != nil {
				spec = *d.Spec.Replicas
			}
			GinkgoWriter.Printf("  %s: spec=%d, ready=%d\n", res.DeploymentName, spec, d.Status.ReadyReplicas)
			g.Expect(d.Status.ReadyReplicas).To(BeNumerically(">=", 1), "Deployment should have at least 1 ready replica")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	}

	// dumpExternalMetricsDiagnostics logs the state of pods serving the external metrics API.
	dumpExternalMetricsDiagnostics := func() {
		GinkgoWriter.Println("--- External Metrics API Diagnostics ---")

		// Check APIService health
		result, err := k8sClient.RESTClient().
			Get().
			AbsPath("/apis/external.metrics.k8s.io/v1beta1").
			DoRaw(ctx)
		if err != nil {
			GinkgoWriter.Printf("  external.metrics.k8s.io/v1beta1 discovery: ERROR %v\n", err)
		} else {
			GinkgoWriter.Printf("  external.metrics.k8s.io/v1beta1 discovery: OK (%d bytes)\n", len(result))
		}

		// Check prometheus-adapter pods across common namespaces
		for _, ns := range []string{benchCfg.WVANamespace, benchCfg.LLMDNamespace, "kube-system", "monitoring", "custom-metrics"} {
			pods, podErr := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			if podErr != nil {
				continue
			}
			for i := range pods.Items {
				p := &pods.Items[i]
				if strings.Contains(p.Name, "prometheus-adapter") || strings.Contains(p.Name, "custom-metrics") || strings.Contains(p.Name, "metrics-server") {
					phase := string(p.Status.Phase)
					ready := false
					restarts := int32(0)
					for _, c := range p.Status.ContainerStatuses {
						if c.Ready {
							ready = true
						}
						restarts += c.RestartCount
					}
					GinkgoWriter.Printf("  [%s] %s: phase=%s ready=%v restarts=%d\n", ns, p.Name, phase, ready, restarts)
				}
			}
		}
		GinkgoWriter.Println("--- End External Metrics Diagnostics ---")
	}

	// waitForVAAndMetrics waits for the VA to stabilize. External metrics and Prometheus
	// checks are best-effort warnings — the benchmark proceeds even if they fail, since
	// the prometheus-adapter may be transiently unavailable.
	waitForVAAndMetrics := func() {
		By("Waiting for VA to stabilize (NumReplicas set)")
		Eventually(func(g Gomega) {
			currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: benchCfg.LLMDNamespace,
				Name:      res.VAName,
			}, currentVA)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(currentVA.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(), "NumReplicas should be set")
			g.Expect(*currentVA.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1), "VA should have optimized >= 1")
			GinkgoWriter.Printf("VA status: desired replicas = %d\n", *currentVA.Status.DesiredOptimizedAlloc.NumReplicas)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("Checking external metrics API (best-effort, non-blocking)")
		externalMetricsOK := false
		Eventually(func() bool {
			result, err := k8sClient.RESTClient().
				Get().
				AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + benchCfg.LLMDNamespace + "/wva_desired_replicas").
				DoRaw(ctx)
			if err != nil {
				GinkgoWriter.Printf("  External metrics API check: %v\n", err)
				return false
			}
			s := string(result)
			if strings.Contains(s, "wva_desired_replicas") && strings.Contains(s, res.VAName) {
				GinkgoWriter.Printf("External metrics API confirmed: wva_desired_replicas available for %s\n", res.VAName)
				externalMetricsOK = true
				return true
			}
			GinkgoWriter.Printf("  External metrics API responded but metric not found for %s\n", res.VAName)
			return false
		}, 3*time.Minute, 10*time.Second).Should(Or(BeTrue(), Not(BeTrue())))
		if !externalMetricsOK {
			GinkgoWriter.Println("WARNING: External metrics API not available — HPA may not scale. Proceeding with benchmark.")
			dumpExternalMetricsDiagnostics()
		}

		By("Checking Prometheus vLLM metrics (best-effort, non-blocking)")
		promOK := false
		Eventually(func() bool {
			_, err := promClient.QueryWithRetry(ctx, `vllm:kv_cache_usage_perc`)
			if err == nil {
				GinkgoWriter.Println("Prometheus confirmed: vllm:kv_cache_usage_perc is available")
				promOK = true
				return true
			}
			return false
		}, 2*time.Minute, 15*time.Second).Should(Or(BeTrue(), Not(BeTrue())))
		if !promOK {
			GinkgoWriter.Println("WARNING: Prometheus vLLM metrics not yet available — KV cache data may be incomplete.")
		}
	}

	// dumpInfrastructureDiagnostics captures EPP, InferencePool, InferenceModel, HTTPRoute
	// state for debugging Gateway 500 errors.
	dumpInfrastructureDiagnostics := func() {
		By("Dumping infrastructure diagnostics for Gateway debugging")

		GinkgoWriter.Println("--- EPP Pod Status ---")
		pods, err := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if strings.Contains(p.Name, "epp") || strings.Contains(p.Name, "inference-scheduler") {
					phase := string(p.Status.Phase)
					ready := false
					for _, c := range p.Status.ContainerStatuses {
						if c.Ready {
							ready = true
						}
					}
					GinkgoWriter.Printf("  %s: phase=%s ready=%v restarts=%d\n", p.Name, phase, ready, func() int32 {
						for _, c := range p.Status.ContainerStatuses {
							return c.RestartCount
						}
						return 0
					}())
				}
			}
		}

		GinkgoWriter.Println("--- All Services (all ports) ---")
		svcs, svcErr := k8sClient.CoreV1().Services(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		if svcErr == nil {
			for i := range svcs.Items {
				s := &svcs.Items[i]
				ports := make([]string, 0, len(s.Spec.Ports))
				for _, port := range s.Spec.Ports {
					ports = append(ports, fmt.Sprintf("%s:%d→%s", port.Name, port.Port, port.TargetPort.String()))
				}
				GinkgoWriter.Printf("  svc/%s  type=%s  ports=[%s]  selector=%v\n",
					s.Name, s.Spec.Type, strings.Join(ports, ", "), s.Spec.Selector)
			}
		}

		GinkgoWriter.Println("--- All Deployments ---")
		deps, depErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		if depErr == nil {
			for i := range deps.Items {
				d := &deps.Items[i]
				spec := int32(0)
				if d.Spec.Replicas != nil {
					spec = *d.Spec.Replicas
				}
				GinkgoWriter.Printf("  deploy/%s  spec=%d  ready=%d  selector=%v\n",
					d.Name, spec, d.Status.ReadyReplicas, d.Spec.Selector.MatchLabels)
			}
		}

		GinkgoWriter.Println("--- EPP Pod Logs (last 50 lines) ---")
		if pods, pErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{}); pErr == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if strings.Contains(p.Name, "epp") || strings.Contains(p.Name, "inference-scheduler") {
					tailLines := int64(50)
					logOpts := &corev1.PodLogOptions{TailLines: &tailLines}
					logReq := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).GetLogs(p.Name, logOpts)
					logBytes, logErr := logReq.DoRaw(ctx)
					if logErr != nil {
						GinkgoWriter.Printf("  [%s] failed to get logs: %v\n", p.Name, logErr)
					} else {
						GinkgoWriter.Printf("  [%s] logs:\n%s\n", p.Name, string(logBytes))
					}
				}
			}
		}

		GinkgoWriter.Println("--- Gateway Pod Logs (last 30 lines) ---")
		if pods, pErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{}); pErr == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if strings.Contains(p.Name, "gateway") {
					tailLines := int64(30)
					logOpts := &corev1.PodLogOptions{TailLines: &tailLines}
					logReq := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).GetLogs(p.Name, logOpts)
					logBytes, logErr := logReq.DoRaw(ctx)
					if logErr != nil {
						GinkgoWriter.Printf("  [%s] failed to get logs: %v\n", p.Name, logErr)
					} else {
						GinkgoWriter.Printf("  [%s] logs:\n%s\n", p.Name, string(logBytes))
					}
				}
			}
		}

		GinkgoWriter.Println("--- InferencePool / InferenceModel (via unstructured) ---")
		if crClient != nil {
			poolList := &unstructured.UnstructuredList{}
			poolList.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "inference.networking.k8s.io", Version: "v1", Kind: "InferencePoolList",
			})
			if err := crClient.List(ctx, poolList, client.InNamespace(benchCfg.LLMDNamespace)); err == nil {
				for _, item := range poolList.Items {
					data, _ := json.MarshalIndent(item.Object, "  ", "  ")
					GinkgoWriter.Printf("  InferencePool/%s (v1):\n  %s\n", item.GetName(), string(data))
				}
			} else {
				GinkgoWriter.Printf("  Failed to list InferencePool (v1): %v\n", err)
				poolList.SetGroupVersionKind(schema.GroupVersionKind{
					Group: "inference.networking.x-k8s.io", Version: "v1alpha2", Kind: "InferencePoolList",
				})
				if err2 := crClient.List(ctx, poolList, client.InNamespace(benchCfg.LLMDNamespace)); err2 == nil {
					for _, item := range poolList.Items {
						data, _ := json.MarshalIndent(item.Object, "  ", "  ")
						GinkgoWriter.Printf("  InferencePool/%s (v1alpha2):\n  %s\n", item.GetName(), string(data))
					}
				} else {
					GinkgoWriter.Printf("  Failed to list InferencePool (v1alpha2): %v\n", err2)
				}
			}

			modelList := &unstructured.UnstructuredList{}
			modelList.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "inference.networking.x-k8s.io", Version: "v1alpha2", Kind: "InferenceModelList",
			})
			if err := crClient.List(ctx, modelList, client.InNamespace(benchCfg.LLMDNamespace)); err == nil {
				for _, item := range modelList.Items {
					data, _ := json.MarshalIndent(item.Object, "  ", "  ")
					GinkgoWriter.Printf("  InferenceModel/%s:\n  %s\n", item.GetName(), string(data))
				}
			} else {
				GinkgoWriter.Printf("  Failed to list InferenceModel (v1alpha2): %v\n", err)
			}
		}

		GinkgoWriter.Println("--- End Diagnostics ---")
	}

	eppPatched := false
	ensureEPPConfig := func() {
		if eppPatched {
			return
		}
		By("Patching EPP ConfigMap with flowControl + scorer weights (queue=2, kv-cache=2, prefix-cache=3)")
		eppDeployName, findErr := FindEPPDeployment(ctx, k8sClient, benchCfg.LLMDNamespace)
		Expect(findErr).NotTo(HaveOccurred(), "Failed to find EPP deployment")
		patchErr := PatchEPPConfigMap(ctx, k8sClient, benchCfg.LLMDNamespace, eppDeployName)
		if patchErr != nil {
			GinkgoWriter.Printf("WARNING: EPP ConfigMap patch failed (non-fatal): %v\n", patchErr)
		} else {
			GinkgoWriter.Println("EPP ConfigMap patched successfully — flowControl enabled, weights 2/2/3")
			eppPatched = true
		}
	}

	verifyEPPConfig := func() {
		By("Discovering EPP deployment")
		eppDeployName, findErr := FindEPPDeployment(ctx, k8sClient, benchCfg.LLMDNamespace)
		Expect(findErr).NotTo(HaveOccurred(), "Failed to find EPP deployment")
		GinkgoWriter.Printf("  Found EPP deployment: %s\n", eppDeployName)

		dep, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, eppDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to get EPP deployment")

		c := dep.Spec.Template.Spec.Containers[0]
		GinkgoWriter.Printf("  EPP image: %s\n", c.Image)
		GinkgoWriter.Printf("  EPP args: %v\n", c.Args)

		flowControlEnabled := false
		for _, e := range c.Env {
			if e.Name == "ENABLE_EXPERIMENTAL_FLOW_CONTROL_LAYER" && e.Value == "true" {
				flowControlEnabled = true
			}
			GinkgoWriter.Printf("  EPP env: %s=%s\n", e.Name, e.Value)
		}

		if flowControlEnabled {
			GinkgoWriter.Println("  Flow control: ENABLED (via env var)")
		} else {
			GinkgoWriter.Println("  WARNING: Flow control env var not found — EPP queue metrics may be zero")
		}

		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.ConfigMap != nil {
				cm, cmErr := k8sClient.CoreV1().ConfigMaps(benchCfg.LLMDNamespace).Get(ctx, v.ConfigMap.Name, metav1.GetOptions{})
				if cmErr == nil {
					for key, val := range cm.Data {
						GinkgoWriter.Printf("  EPP ConfigMap %s/%s:\n%s\n", v.ConfigMap.Name, key, val)
					}
				}
			}
		}
	}

	runPrefillBenchmark := func(autoscalerType string) {
		ensureEPPConfig()
		ensureInfraDeploymentReady()
		verifyEPPConfig()
		dumpInfrastructureDiagnostics()

		gatewayURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d",
			benchCfg.GatewayServiceName, benchCfg.LLMDNamespace, benchCfg.GatewayServicePort)

		By("Verifying Gateway connectivity (hard requirement — traffic must flow through EPP)")
		Eventually(func(g Gomega) {
			err := VerifyGatewayConnectivity(ctx, k8sClient, benchCfg.LLMDNamespace, gatewayURL, benchCfg.ModelID)
			g.Expect(err).NotTo(HaveOccurred(), "Gateway not ready yet")
		}, 5*time.Minute, 15*time.Second).Should(Succeed(), "Gateway connectivity check failed — EPP must be reachable via Gateway")
		GinkgoWriter.Println("  Gateway connectivity verified")

		targetURL := gatewayURL
		GinkgoWriter.Printf("  Using Gateway URL (traffic flows through EPP): %s\n", targetURL)

		By("Checking Prometheus metric availability before load")
		for _, q := range []string{
			fmt.Sprintf(`vllm:kv_cache_usage_perc{namespace="%s"}`, benchCfg.LLMDNamespace),
			fmt.Sprintf(`vllm:num_requests_waiting{namespace="%s"}`, benchCfg.LLMDNamespace),
			fmt.Sprintf(`inference_extension_flow_control_queue_size{namespace="%s"}`, benchCfg.LLMDNamespace),
			fmt.Sprintf(`kube_deployment_status_replicas{deployment="%s",namespace="%s"}`, res.DeploymentName, benchCfg.LLMDNamespace),
		} {
			val, err := QueryRangeAvg(promClient.API(), q, time.Now().Add(-2*time.Minute), time.Now(), 30*time.Second)
			if err != nil {
				GinkgoWriter.Printf("  Metric check: %s → NOT FOUND (%v)\n", q, err)
			} else {
				GinkgoWriter.Printf("  Metric check: %s → %.4f\n", q, val)
			}
		}

		By("Checking HPA status before load")
		hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range hpaList.Items {
				hpa := &hpaList.Items[i]
				GinkgoWriter.Printf("  HPA %s: currentReplicas=%d desiredReplicas=%d\n", hpa.Name, hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas)
				for _, cond := range hpa.Status.Conditions {
					GinkgoWriter.Printf("    condition %s: %s (%s)\n", cond.Type, cond.Status, cond.Message)
				}
			}
		}

		By("Launching GuideLLM Load Generator")

		err = CreateGuideLLMJobWithArgs(
			ctx, k8sClient, benchCfg.LLMDNamespace, res.ModelService,
			targetURL, benchCfg.ModelID,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create GuideLLM load job")

		loadStart := time.Now()
		jobName := res.ModelService + "-load"

		By("Monitoring replicas and HPA status while GuideLLM runs (~10 min)")
		var timeline []ReplicaSnap
		var metricsTimeline []MetricSnap
		var maxReplicas int32 = 1
		done := make(chan error, 1)

		go func() {
			done <- WaitForJobCompletion(ctx, k8sClient, benchCfg.LLMDNamespace, jobName, 25*time.Minute)
		}()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

	monitorLoop:
		for {
			select {
			case jobErr := <-done:
				if jobErr != nil {
					logs, logErr := GetJobPodLogs(ctx, k8sClient, benchCfg.LLMDNamespace, jobName)
					if logErr == nil {
						GinkgoWriter.Printf("\n--- GuideLLM Job Failed. Pod Logs ---\n%s\n---------------------------\n", logs)
					}
				}
				Expect(jobErr).NotTo(HaveOccurred(), "GuideLLM job failed or timed out")
				break monitorLoop
			case <-ticker.C:
				elapsed := time.Since(loadStart).Seconds()
				var spec, ready int32
				deployment, depErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
				if depErr == nil {
					spec = *deployment.Spec.Replicas
					ready = deployment.Status.ReadyReplicas
					if spec > maxReplicas {
						maxReplicas = spec
					}
					timeline = append(timeline, ReplicaSnap{ElapsedSec: elapsed, SpecReplicas: spec, ReadyReplicas: ready})
				}

				// Sample KV cache, vLLM queue depth, and EPP queue depth from Prometheus
				qdQuery := fmt.Sprintf(`avg(vllm:num_requests_waiting{namespace="%s"})`, benchCfg.LLMDNamespace)
				kvQuery := fmt.Sprintf(`avg(vllm:kv_cache_usage_perc{namespace="%s"})`, benchCfg.LLMDNamespace)
				eppQDQuery := fmt.Sprintf(`sum(inference_extension_flow_control_queue_size{namespace="%s"})`, benchCfg.LLMDNamespace)
				snap := MetricSnap{ElapsedSec: elapsed}
				if qdResult, _, qdErr := promClient.API().Query(ctx, qdQuery, time.Now()); qdErr == nil {
					if vec, ok := qdResult.(model.Vector); ok && len(vec) > 0 {
						snap.QueueDepth = float64(vec[0].Value)
					}
				}
				if kvResult, _, kvErr := promClient.API().Query(ctx, kvQuery, time.Now()); kvErr == nil {
					if vec, ok := kvResult.(model.Vector); ok && len(vec) > 0 {
						snap.KVCache = float64(vec[0].Value)
					}
				}
				if eppResult, _, eppErr := promClient.API().Query(ctx, eppQDQuery, time.Now()); eppErr == nil {
					if vec, ok := eppResult.(model.Vector); ok && len(vec) > 0 {
						snap.EPPQueueDepth = float64(vec[0].Value)
					}
				}
				metricsTimeline = append(metricsTimeline, snap)

				// VA desired replicas
				vaDesired := "?"
				currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				if vaErr := crClient.Get(ctx, client.ObjectKey{Namespace: benchCfg.LLMDNamespace, Name: res.VAName}, currentVA); vaErr == nil {
					if currentVA.Status.DesiredOptimizedAlloc.NumReplicas != nil {
						vaDesired = strconv.FormatInt(int64(*currentVA.Status.DesiredOptimizedAlloc.NumReplicas), 10)
					}
				}

				// HPA status
				hpaCurrent, hpaDesired := "?", "?"
				hpaList, hpaErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
				if hpaErr == nil {
					for i := range hpaList.Items {
						hpa := &hpaList.Items[i]
						hpaCurrent = strconv.FormatInt(int64(hpa.Status.CurrentReplicas), 10)
						hpaDesired = strconv.FormatInt(int64(hpa.Status.DesiredReplicas), 10)
					}
				}

				// Consolidated single-line output matching test-multi-model-scaling format
				GinkgoWriter.Printf("  [%s] replicas: spec=%d ready=%d | va=%s hpa=%s→%s | kv=%.4f qd=%.1f epp_qd=%.1f\n",
					fmt.Sprintf("%.0fs", elapsed), spec, ready, vaDesired, hpaCurrent, hpaDesired,
					snap.KVCache, snap.QueueDepth, snap.EPPQueueDepth)

				// Pod-level health check for crash detection
				pods, podErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + res.DeploymentName,
				})
				if podErr == nil {
					for i := range pods.Items {
						p := &pods.Items[i]
						for _, cs := range p.Status.ContainerStatuses {
							if cs.RestartCount > 0 {
								reason := "running"
								if cs.State.Waiting != nil {
									reason = cs.State.Waiting.Reason
								} else if cs.State.Terminated != nil {
									reason = fmt.Sprintf("terminated(%s,exit=%d)", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
								}
								GinkgoWriter.Printf("  [%.0fs] Pod %s: restarts=%d state=%s\n", elapsed, p.Name, cs.RestartCount, reason)
							}
						}
					}
				}
			}
		}
		loadEnd := time.Now()
		loadDuration := loadEnd.Sub(loadStart).Seconds()

		By("Extracting GuideLLM results from pod logs")
		logs, err := GetJobPodLogs(ctx, k8sClient, benchCfg.LLMDNamespace, jobName)
		Expect(err).NotTo(HaveOccurred(), "Failed to get GuideLLM pod logs")

		var guidellmRaw json.RawMessage
		var ttftJSON, itlJSON, throughputJSON json.RawMessage

		if idx := strings.Index(logs, "=== BENCHMARK JSON ==="); idx != -1 {
			jsonStr := strings.TrimSpace(logs[idx+len("=== BENCHMARK JSON ==="):])
			guidellmRaw = json.RawMessage(jsonStr)

			var parsed map[string]interface{}
			if jsonErr := json.Unmarshal([]byte(jsonStr), &parsed); jsonErr == nil {
				// GuideLLM stores metrics at benchmarks[0].metrics.<metric_name>.successful
				extractGuideLLMMetric(&parsed, "time_to_first_token_ms", &ttftJSON)
				extractGuideLLMMetric(&parsed, "inter_token_latency_ms", &itlJSON)
				extractGuideLLMMetric(&parsed, "output_tokens_per_second", &throughputJSON)

				// Also extract request totals for diagnostics
				var requestTotals json.RawMessage
				extractGuideLLMMetric(&parsed, "request_totals", &requestTotals)
				if requestTotals != nil {
					GinkgoWriter.Printf("  Request Totals: %s\n", string(requestTotals))
				}
			} else {
				GinkgoWriter.Printf("Warning: failed to parse GuideLLM JSON: %v\n", jsonErr)
				GinkgoWriter.Printf("Raw JSON (first 1000 chars): %s\n", truncateHead(jsonStr, 1000))
			}
		} else {
			GinkgoWriter.Println("Warning: '=== BENCHMARK JSON ===' marker not found in pod logs")
			GinkgoWriter.Printf("Pod log tail (last 500 chars): %s\n", truncateTail(logs, 500))
		}

		By("Querying Prometheus for Replicas, Queue Depth, and KV Cache")
		replicaAvg, err := QueryRangeAvg(
			promClient.API(),
			fmt.Sprintf(`avg(kube_deployment_status_replicas{deployment="%s", namespace="%s"})`, res.DeploymentName, benchCfg.LLMDNamespace),
			loadStart, loadEnd, 30*time.Second,
		)
		if err != nil {
			GinkgoWriter.Printf("Warning: failed to query replica avg: %v\n", err)
		}

		qdAvg, err := QueryRangeAvg(
			promClient.API(),
			fmt.Sprintf(`avg(vllm:num_requests_waiting{namespace="%s"})`, benchCfg.LLMDNamespace),
			loadStart, loadEnd, 30*time.Second,
		)
		if err != nil {
			GinkgoWriter.Printf("Warning: failed to query queue depth avg: %v\n", err)
		}

		kvAvg, err := QueryRangeAvg(
			promClient.API(),
			fmt.Sprintf(`avg(vllm:kv_cache_usage_perc{namespace="%s"})`, benchCfg.LLMDNamespace),
			loadStart, loadEnd, 30*time.Second,
		)
		if err != nil {
			GinkgoWriter.Printf("Warning: failed to query KV cache avg: %v\n", err)
		}

		eppQDAvg, err := QueryRangeAvg(
			promClient.API(),
			fmt.Sprintf(`sum(inference_extension_flow_control_queue_size{namespace="%s"})`, benchCfg.LLMDNamespace),
			loadStart, loadEnd, 30*time.Second,
		)
		if err != nil {
			GinkgoWriter.Printf("Warning: failed to query EPP queue depth avg: %v\n", err)
		}

		// Collect pod placement info (node, GPU, startup time)
		By("Collecting pod placement details for report")
		var podInfos []PodInfo
		// Try multiple label selectors: Helm chart uses llm-d.ai/role=decode,
		// fall back to app=<deployment> for custom deployments
		var decodePods *corev1.PodList
		var podListErr error
		for _, sel := range []string{
			"llm-d.ai/role=decode",
			"app=" + res.DeploymentName,
		} {
			decodePods, podListErr = k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: sel,
			})
			if podListErr == nil && len(decodePods.Items) > 0 {
				GinkgoWriter.Printf("  Found %d pods with selector %s\n", len(decodePods.Items), sel)
				break
			}
		}
		if podListErr == nil {
			for i := range decodePods.Items {
				p := &decodePods.Items[i]
				gpu := "Unknown"
				if p.Spec.NodeName != "" {
					node, nodeErr := k8sClient.CoreV1().Nodes().Get(ctx, p.Spec.NodeName, metav1.GetOptions{})
					if nodeErr == nil {
						if g, ok := node.Labels["nvidia.com/gpu.product"]; ok {
							gpu = g
						} else if g, ok := node.Labels["accelerator"]; ok {
							gpu = g
						}
					}
				}
				var startupSec float64
				for _, cond := range p.Status.Conditions {
					if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
						startupSec = cond.LastTransitionTime.Sub(p.CreationTimestamp.Time).Seconds()
						break
					}
				}
				podInfos = append(podInfos, PodInfo{
					Name:       p.Name,
					Node:       p.Spec.NodeName,
					GPU:        gpu,
					StartupSec: startupSec,
				})
				GinkgoWriter.Printf("  Pod: %s  Node: %s  GPU: %s  Startup: %.0fs\n", p.Name, p.Spec.NodeName, gpu, startupSec)
			}
		}

		vaConfig := "Min Replicas: 1, Max Replicas: 10, Cost Factor: 10.0"
		hpaConfig := "Min Replicas: 1, Max Replicas: 10 | Scale Up: stabilizationWindow=0s, policy=10 Pods/150s | Scale Down: stabilizationWindow=240s, policy=10 Pods/150s"

		// Extract error counts and achieved RPS from GuideLLM output
		var errorCount, incompleteCount, completedCount int
		var achievedRPS float64
		if guidellmRaw != nil {
			var parsed map[string]interface{}
			if jsonErr := json.Unmarshal(guidellmRaw, &parsed); jsonErr == nil {
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
		if achievedRPS == 0 && completedCount > 0 && loadDuration > 0 {
			achievedRPS = float64(completedCount) / loadDuration
		}

		result := PrefillResult{
			AutoscalerType:   autoscalerType,
			ModelID:          benchCfg.ModelID,
			VAConfig:         vaConfig,
			HPAConfig:        hpaConfig,
			Pods:             podInfos,
			ReplicaTimeline:  timeline,
			MetricsTimeline:  metricsTimeline,
			AvgReplicas:      replicaAvg,
			MaxReplicas:      maxReplicas,
			AvgQueueDepth:    qdAvg,
			AvgEPPQueueDepth: eppQDAvg,
			AvgKVCache:       kvAvg,
			AchievedRPS:      achievedRPS,
			ErrorCount:       errorCount,
			IncompleteCount:  incompleteCount,
			TTFT:             ttftJSON,
			ITL:              itlJSON,
			Throughput:       throughputJSON,
			GuideLLMRaw:      guidellmRaw,
			DurationSec:      loadDuration,
		}
		prefillResults = append(prefillResults, result)

		// Format TTFT/ITL/Throughput as p50/p90/p99 strings
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

		GinkgoWriter.Printf("\n  ┌────────────────────────────────────────────────────────────\n")
		GinkgoWriter.Printf("  │ %s PREFILL BENCHMARK RESULTS\n", autoscalerType)
		GinkgoWriter.Printf("  │ Model: %s\n", benchCfg.ModelID)
		GinkgoWriter.Printf("  ├────────────────────────────────────────────────────────────\n")
		GinkgoWriter.Printf("  │ Duration:        %.0fs\n", loadDuration)
		GinkgoWriter.Printf("  │ Final Replicas:  spec=%d\n", func() int32 {
			d, e := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
			if e != nil {
				return 0
			}
			return *d.Spec.Replicas
		}())
		GinkgoWriter.Printf("  │ Max Replicas:    %d\n", maxReplicas)
		GinkgoWriter.Printf("  │ Avg Replicas:    %.2f\n", replicaAvg)
		GinkgoWriter.Printf("  ├── Prometheus Metrics ──────────────────────────────────────\n")
		GinkgoWriter.Printf("  │ Avg KV Cache:    %.4f\n", kvAvg)
		GinkgoWriter.Printf("  │ Avg Queue Depth: %.2f\n", qdAvg)
		GinkgoWriter.Printf("  │ Avg EPP Queue:   %.2f\n", eppQDAvg)
		GinkgoWriter.Printf("  ├── GuideLLM Results ────────────────────────────────────────\n")
		GinkgoWriter.Printf("  │ Achieved RPS:    %.2f\n", achievedRPS)
		GinkgoWriter.Printf("  │ TTFT (ms):       %s\n", formatPercentiles(ttftJSON))
		GinkgoWriter.Printf("  │ ITL (ms):        %s\n", formatPercentiles(itlJSON))
		GinkgoWriter.Printf("  │ Throughput:      %s\n", formatPercentiles(throughputJSON))
		GinkgoWriter.Printf("  │ Errors:          %d\n", errorCount)
		GinkgoWriter.Printf("  │ Incomplete:      %d\n", incompleteCount)
		GinkgoWriter.Printf("  ├── Replica Timeline (%d snapshots) ─────────────────────────\n", len(timeline))
		for _, s := range timeline {
			GinkgoWriter.Printf("  │   t=%.0fs  spec=%d  ready=%d\n", s.ElapsedSec, s.SpecReplicas, s.ReadyReplicas)
		}
		GinkgoWriter.Printf("  └────────────────────────────────────────────────────────────\n\n")

		By("Saving prefill benchmark results to file")
		data, _ := json.MarshalIndent(prefillResults, "", "  ")
		_ = os.WriteFile(prefillResultsFile, data, 0644)
	}

	Context("WVA", func() {
		It("should run the prefill heavy workload against WVA", func() {
			cleanupAutoscalers()
			res.DeploymentName = findInfraDecodeDeployment()
			ensureInfraDeploymentReady()

			By("Creating VariantAutoscaling resource (max=10, cost=10)")
			err := fixtures.EnsureVariantAutoscaling(
				ctx, crClient, benchCfg.LLMDNamespace, res.VAName, res.DeploymentName,
				benchCfg.ModelID, benchCfg.AcceleratorType, 10.0, benchCfg.ControllerInstance,
				fixtures.WithMinReplicas(1),
				fixtures.WithMaxReplicas(10),
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VA")

			By("Creating HPA (Scale Up: 0s/Pods/10/150, Scale Down: 240s/Pods/10/150)")
			behavior := &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(0)),
					Policies:                   []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 10, PeriodSeconds: 150}},
				},
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(240)),
					Policies:                   []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 10, PeriodSeconds: 150}},
				},
			}
			err = fixtures.EnsureHPA(ctx, k8sClient, benchCfg.LLMDNamespace, res.HPAName, res.DeploymentName, res.VAName, 1, 10, WithBehavior(behavior))
			Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")

			waitForVAAndMetrics()

			runPrefillBenchmark("WVA")
		})
	})

	AfterAll(func() {
		GinkgoWriter.Println("Prefill benchmark complete — cleaning up autoscalers and scaling to 1 for next test suite")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()

		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, "prefill-hpa", metav1.DeleteOptions{})
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, "prefill-hpa-standard-hpa", metav1.DeleteOptions{})
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(cleanupCtx, "prefill-hpa-hpa", metav1.DeleteOptions{})
		_ = fixtures.DeleteVariantAutoscaling(cleanupCtx, crClient, benchCfg.LLMDNamespace, "prefill-va")

		deployments, listErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).List(cleanupCtx, metav1.ListOptions{})
		if listErr == nil {
			for i := range deployments.Items {
				d := &deployments.Items[i]
				if strings.HasSuffix(d.Name, "-decode") && strings.Contains(d.Name, "modelservice") {
					scale, scaleErr := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).GetScale(cleanupCtx, d.Name, metav1.GetOptions{})
					if scaleErr == nil && scale.Spec.Replicas > 1 {
						scale.Spec.Replicas = 1
						_, _ = k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).UpdateScale(cleanupCtx, d.Name, scale, metav1.UpdateOptions{})
						GinkgoWriter.Printf("Scaled %s back to 1 for next test suite\n", d.Name)
					}
				}
			}
		}
	})
})

// extractGuideLLMMetric extracts a metric from the GuideLLM JSON structure.
// The structure is: benchmarks[0].metrics.<key>.successful (for successful request stats).
// Falls back to benchmarks[0].metrics.<key> if "successful" sub-key doesn't exist.
func extractGuideLLMMetric(parsed *map[string]interface{}, key string, out *json.RawMessage) {
	benchmarks, ok := (*parsed)["benchmarks"].([]interface{})
	if !ok || len(benchmarks) == 0 {
		return
	}
	bm, ok := benchmarks[0].(map[string]interface{})
	if !ok {
		return
	}
	metrics, ok := bm["metrics"].(map[string]interface{})
	if !ok {
		return
	}
	metricVal, ok := metrics[key]
	if !ok {
		return
	}
	metricMap, ok := metricVal.(map[string]interface{})
	if ok {
		if successful, ok := metricMap["successful"]; ok {
			raw, _ := json.Marshal(successful)
			*out = raw
			return
		}
	}
	raw, _ := json.Marshal(metricVal)
	*out = raw
}

func truncateTail(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return "..." + s[len(s)-maxLen:]
}

func truncateHead(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
