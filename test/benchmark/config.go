package benchmark

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/testconfig"
)

// BenchmarkConfig holds configuration for benchmark tests loaded from environment variables.
// Common fields are inherited from testconfig.SharedConfig.
type BenchmarkConfig struct {
	testconfig.SharedConfig

	// Gateway configuration (benchmark routes through the full llm-d stack)
	GatewayServiceName string
	GatewayServicePort int

	// Benchmark-specific
	BenchmarkResultsFile string

	// Grafana
	GrafanaEnabled          bool   // Deploy ephemeral Grafana and capture snapshot
	GrafanaSnapshotFile     string // Path to write snapshot URL
	GrafanaSnapshotJSONFile string // Path to export full snapshot JSON (re-importable)
	GrafanaPanelDir         string // Directory to write rendered panel PNGs

	// Phase durations (seconds, overridable via env for tuning)
	BaselineDurationSec  int
	SpikeDurationSec     int
	SustainedDurationSec int
	CooldownDurationSec  int
}

// LoadConfigFromEnv reads benchmark configuration from environment variables.
func LoadConfigFromEnv() BenchmarkConfig {
	shared := testconfig.LoadSharedConfig()

	gatewayServiceDefault := "infra-inference-scheduling-inference-gateway-istio"
	if shared.Environment == "kind-emulator" {
		gatewayServiceDefault = "infra-sim-inference-gateway-istio"
	}

	if shared.PoolName == "" {
		shared.PoolName = "gaie-inference-scheduling"
		if shared.Environment == "kind-emulator" {
			shared.PoolName = "gaie-sim"
		}
	}

	return BenchmarkConfig{
		SharedConfig: shared,

		GatewayServiceName: testconfig.GetEnv("GATEWAY_SERVICE_NAME", gatewayServiceDefault),
		GatewayServicePort: testconfig.GetEnvInt("GATEWAY_SERVICE_PORT", 80),

		BenchmarkResultsFile: testconfig.GetEnv("BENCHMARK_RESULTS_FILE", "/tmp/benchmark-results.json"),

		GrafanaEnabled:          testconfig.GetEnvBool("BENCHMARK_GRAFANA_ENABLED", true),
		GrafanaSnapshotFile:     testconfig.GetEnv("BENCHMARK_GRAFANA_SNAPSHOT_FILE", "/tmp/benchmark-grafana-snapshot.txt"),
		GrafanaSnapshotJSONFile: testconfig.GetEnv("BENCHMARK_GRAFANA_SNAPSHOT_JSON", "/tmp/benchmark-grafana-snapshot.json"),
		GrafanaPanelDir:         testconfig.GetEnv("BENCHMARK_GRAFANA_PANEL_DIR", "/tmp/benchmark-panels"),

		BaselineDurationSec:  testconfig.GetEnvInt("BENCHMARK_BASELINE_DURATION", 120),
		SpikeDurationSec:     testconfig.GetEnvInt("BENCHMARK_SPIKE_DURATION", 300),
		SustainedDurationSec: testconfig.GetEnvInt("BENCHMARK_SUSTAINED_DURATION", 180),
		CooldownDurationSec:  testconfig.GetEnvInt("BENCHMARK_COOLDOWN_DURATION", 300),
	}
}
