package e2e

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/testconfig"
)

// E2EConfig holds configuration for e2e tests loaded from environment variables.
// Common fields are inherited from testconfig.SharedConfig.
type E2EConfig struct {
	testconfig.SharedConfig

	// Feature gates
	ScaleToZeroEnabled bool // HPAScaleToZero feature gate

	// Timeouts (seconds unless noted)
	PodReadyTimeout int // Wait for deployment/model pods ready
	ScaleUpTimeout  int // Long reconcile / scale-from-zero / job completion

	// Gomega Eventually timeouts and poll intervals (seconds)
	EventuallyShortSec      int // Quick checks (e.g. CRD list, delete verification)
	EventuallyMediumSec     int // Single-step medium waits (e.g. 60s)
	EventuallyStandardSec   int // Most status / reconcile waits (default 120s)
	EventuallyLongSec       int // Metrics / adapter stabilization (e.g. 180s)
	EventuallyExtendedSec   int // Long engine/HPA steps (e.g. 300s / 5m)
	PollIntervalSec         int // Default polling interval for Eventually
	PollIntervalQuickSec    int // Faster polling for short waits
	PollIntervalSlowSec     int // Slower polling for long-running conditions
	PollIntervalVerySlowSec int // e.g. job completion probes

	// Prometheus Adapter BeforeSuite: probe this long before optional pod restart (seconds)
	PrometheusAdapterProbeSec int
}

// LoadConfigFromEnv reads e2e test configuration from environment variables.
func LoadConfigFromEnv() E2EConfig {
	cfg := E2EConfig{
		SharedConfig: testconfig.LoadSharedConfig(),

		ScaleToZeroEnabled: testconfig.GetEnvBool("SCALE_TO_ZERO_ENABLED", false),

		PodReadyTimeout: testconfig.GetEnvInt("POD_READY_TIMEOUT", 300), // 5 minutes
		ScaleUpTimeout:  testconfig.GetEnvInt("SCALE_UP_TIMEOUT", 600),  // 10 minutes

		EventuallyShortSec:        testconfig.GetEnvInt("E2E_EVENTUALLY_SHORT", 30),
		EventuallyMediumSec:       testconfig.GetEnvInt("E2E_EVENTUALLY_MEDIUM", 60),
		EventuallyStandardSec:     testconfig.GetEnvInt("E2E_EVENTUALLY_STANDARD", 120),
		EventuallyLongSec:         testconfig.GetEnvInt("E2E_EVENTUALLY_LONG", 180),
		EventuallyExtendedSec:     testconfig.GetEnvInt("E2E_EVENTUALLY_EXTENDED", 300),
		PollIntervalSec:           testconfig.GetEnvInt("E2E_EVENTUALLY_POLL", 5),
		PollIntervalQuickSec:      testconfig.GetEnvInt("E2E_EVENTUALLY_POLL_QUICK", 2),
		PollIntervalSlowSec:       testconfig.GetEnvInt("E2E_EVENTUALLY_POLL_SLOW", 10),
		PollIntervalVerySlowSec:   testconfig.GetEnvInt("E2E_EVENTUALLY_POLL_VERY_SLOW", 15),
		PrometheusAdapterProbeSec: testconfig.GetEnvInt("E2E_PROM_ADAPTER_PROBE_SEC", 90),
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
