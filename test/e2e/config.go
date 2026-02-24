package e2e

import (
	"os"
	"strconv"
	"strings"
)

// E2EConfig holds configuration for e2e tests loaded from environment variables
type E2EConfig struct {
	// Cluster info
	Environment string // "kind", "openshift", "kubernetes"
	Kubeconfig  string

	// Namespaces
	WVANamespace  string // WVA controller namespace
	LLMDNamespace string // llm-d infrastructure namespace
	MonitoringNS  string // Prometheus namespace

	// Infrastructure mode
	UseSimulator bool   // true for emulated GPUs, false for real vLLM
	GPUType      string // "nvidia-mix", "amd-mix", "real"

	// Feature gates
	ScaleToZeroEnabled bool // HPAScaleToZero feature gate

	// EPP configuration
	EPPMode          string            // "poolName" or "endpointSelector"
	PoolName         string            // InferencePool name (if using poolName mode)
	EndpointSelector map[string]string // Pod selector (if using endpointSelector)

	// Model configuration
	ModelID         string // e.g., "unsloth/Meta-Llama-3.1-8B"
	AcceleratorType string // e.g., "H100", "A100" (must be valid Kubernetes label value)
	MaxNumSeqs      int    // vLLM batch size (lower = easier to saturate)

	// Load generation
	LoadStrategy string // "synthetic", "sharegpt"
	RequestRate  int    // Requests per second
	NumPrompts   int    // Total number of requests
	InputTokens  int    // Average input tokens
	OutputTokens int    // Average output tokens

	// Timeouts
	PodReadyTimeout int // Seconds to wait for pods to be ready
	ScaleUpTimeout  int // Seconds to wait for scale-up
}

// LoadConfigFromEnv reads e2e test configuration from environment variables
func LoadConfigFromEnv() E2EConfig {
	cfg := E2EConfig{
		// Cluster defaults
		Environment: getEnv("ENVIRONMENT", "kind-emulator"),
		Kubeconfig:  getEnv("KUBECONFIG", os.Getenv("HOME")+"/.kube/config"),

		// Namespace defaults
		WVANamespace:  getEnv("WVA_NAMESPACE", "workload-variant-autoscaler-system"),
		LLMDNamespace: getEnv("LLMD_NAMESPACE", "llm-d-sim"),
		MonitoringNS:  getEnv("MONITORING_NAMESPACE", "workload-variant-autoscaler-monitoring"),

		// Infrastructure defaults
		UseSimulator: getEnvBool("USE_SIMULATOR", true),
		GPUType:      getEnv("GPU_TYPE", "nvidia-mix"),

		// Feature gate defaults
		ScaleToZeroEnabled: getEnvBool("SCALE_TO_ZERO_ENABLED", false),

		// EPP defaults
		EPPMode:          getEnv("EPP_MODE", "poolName"),
		PoolName:         getEnv("POOL_NAME", ""),
		EndpointSelector: parseEndpointSelector(getEnv("ENDPOINT_SELECTOR", "")),

		// Model defaults
		ModelID:         getEnv("MODEL_ID", "unsloth/Meta-Llama-3.1-8B"),
		AcceleratorType: getEnv("ACCELERATOR_TYPE", "H100"),
		MaxNumSeqs:      getEnvInt("MAX_NUM_SEQS", 5),

		// Load generation defaults
		LoadStrategy: getEnv("LOAD_STRATEGY", "synthetic"),
		RequestRate:  getEnvInt("REQUEST_RATE", 8),
		NumPrompts:   getEnvInt("NUM_PROMPTS", 1000),
		InputTokens:  getEnvInt("INPUT_TOKENS", 100),
		OutputTokens: getEnvInt("OUTPUT_TOKENS", 50),

		// Timeout defaults
		PodReadyTimeout: getEnvInt("POD_READY_TIMEOUT", 300), // 5 minutes
		ScaleUpTimeout:  getEnvInt("SCALE_UP_TIMEOUT", 600),  // 10 minutes
	}

	// OpenShift clusters typically don't have the HPAScaleToZero feature gate
	// enabled, so attempting to create HPAs with minReplicas=0 will fail with:
	//   "spec.minReplicas: Invalid value: 0: must be greater than or equal to 1"
	// Override the env var to prevent test failures on OpenShift.
	if cfg.Environment == "openshift" && cfg.ScaleToZeroEnabled {
		cfg.ScaleToZeroEnabled = false
	}

	return cfg
}

// Helper functions for environment variable parsing

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.Atoi(value)
		if err == nil {
			return parsed
		}
	}
	return defaultValue
}

func parseEndpointSelector(value string) map[string]string {
	if value == "" {
		return nil
	}

	selector := make(map[string]string)
	pairs := strings.Split(value, ",")
	for _, pair := range pairs {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) == 2 {
			selector[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return selector
}
