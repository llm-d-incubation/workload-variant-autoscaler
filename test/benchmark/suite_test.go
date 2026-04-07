// Package benchmark runs cluster-backed scale-up latency scenarios. Parallel
// load Jobs are created via github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures
// (see workload_builder.go there for why load helpers share that package with e2e).
package benchmark

import (
	"context"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

var (
	benchCfg       BenchmarkConfig
	k8sClient      *kubernetes.Clientset
	crClient       client.Client
	restConfig     *rest.Config
	ctx            context.Context
	cancel         context.CancelFunc
	promClient     *utils.PrometheusClient
	portForwardCmd *exec.Cmd
	grafanaClient  *GrafanaClient
)

func TestBenchmark(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Benchmark Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("Loading benchmark configuration from environment")
	benchCfg = LoadConfigFromEnv()

	GinkgoWriter.Printf("=== Benchmark Configuration ===\n")
	GinkgoWriter.Printf("Environment: %s\n", benchCfg.Environment)
	GinkgoWriter.Printf("WVA Namespace: %s\n", benchCfg.WVANamespace)
	GinkgoWriter.Printf("LLMD Namespace: %s\n", benchCfg.LLMDNamespace)
	GinkgoWriter.Printf("Gateway Service: %s:%d\n", benchCfg.GatewayServiceName, benchCfg.GatewayServicePort)
	GinkgoWriter.Printf("EPP Service: %s\n", benchCfg.EPPServiceName)
	GinkgoWriter.Printf("Results File: %s\n", benchCfg.BenchmarkResultsFile)
	GinkgoWriter.Printf("===============================\n\n")

	By("Initializing Kubernetes client")
	var err error
	if _, statErr := os.Stat(benchCfg.Kubeconfig); statErr == nil {
		restConfig, err = clientcmd.BuildConfigFromFlags("", benchCfg.Kubeconfig)
		Expect(err).NotTo(HaveOccurred(), "Failed to load kubeconfig")
	} else {
		restConfig, err = rest.InClusterConfig()
		Expect(err).NotTo(HaveOccurred(), "Failed to load in-cluster config")
	}

	k8sClient, err = kubernetes.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Kubernetes clientset")

	s := runtime.NewScheme()
	err = clientgoscheme.AddToScheme(s)
	Expect(err).NotTo(HaveOccurred())
	err = variantautoscalingv1alpha1.AddToScheme(s)
	Expect(err).NotTo(HaveOccurred())
	err = promoperator.AddToScheme(s)
	Expect(err).NotTo(HaveOccurred())

	crClient, err = client.New(restConfig, client.Options{Scheme: s})
	Expect(err).NotTo(HaveOccurred(), "Failed to create controller-runtime client")

	ctx, cancel = context.WithCancel(context.Background()) //nolint:fatcontext // shared across BeforeSuite/AfterSuite

	By("Setting up port-forward to Prometheus")
	portForwardCmd = utils.SetUpPortForward(k8sClient, ctx, "kube-prometheus-stack-prometheus", benchCfg.MonitoringNS, 9090, 9090)

	By("Verifying Prometheus port-forward is ready")
	err = utils.VerifyPortForwardReadiness(ctx, 9090, "https://localhost:9090/-/ready")
	Expect(err).NotTo(HaveOccurred(), "Prometheus port-forward should be ready")

	By("Creating Prometheus client")
	promClient, err = utils.NewPrometheusClient("https://localhost:9090", true)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Prometheus client")

	if benchCfg.GrafanaEnabled {
		By("Setting up Grafana client with port-forward")
		var clientErr error
		grafanaClient, clientErr = NewGrafanaClient(k8sClient, ctx, benchCfg.MonitoringNS)
		if clientErr != nil {
			GinkgoWriter.Printf("WARNING: Grafana client setup failed (non-fatal): %v\n", clientErr)
			grafanaClient = nil
		} else {
			GinkgoWriter.Println("Grafana client ready for snapshot capture")
		}
	}

	GinkgoWriter.Println("BeforeSuite completed — infrastructure ready for benchmarks")
})

var _ = AfterSuite(func() {
	if grafanaClient != nil {
		By("Closing Grafana port-forward")
		grafanaClient.Close()
	}

	if portForwardCmd != nil && portForwardCmd.Process != nil {
		By("Killing Prometheus port-forward")
		_ = portForwardCmd.Process.Kill()
	}

	if cancel != nil {
		cancel()
	}
})
