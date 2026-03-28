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

	// Timeouts
	PodReadyTimeout int // Seconds to wait for pods to be ready
	ScaleUpTimeout  int // Seconds to wait for scale-up
}

// LoadConfigFromEnv reads e2e test configuration from environment variables.
func LoadConfigFromEnv() E2EConfig {
	cfg := E2EConfig{
		SharedConfig: testconfig.LoadSharedConfig(),

		ScaleToZeroEnabled: testconfig.GetEnvBool("SCALE_TO_ZERO_ENABLED", false),

		PodReadyTimeout: testconfig.GetEnvInt("POD_READY_TIMEOUT", 300), // 5 minutes
		ScaleUpTimeout:  testconfig.GetEnvInt("SCALE_UP_TIMEOUT", 600),  // 10 minutes
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
