package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	flag "github.com/spf13/pflag"

	interfaces "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// writeTestConfigFile writes a YAML config file to a temp directory and returns its path.
func writeTestConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}
	return path
}

func TestLoad_Defaults(t *testing.T) {
	// Setup: No flags, no env, no config file
	// Set required Prometheus env var
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cfg, err := Load(nil, "")
	if err != nil {
		t.Fatalf("Load() failed with defaults: %v", err)
	}

	// Verify defaults
	if cfg.MetricsAddr() != "0" {
		t.Errorf("Expected MetricsAddr default '0', got %q", cfg.MetricsAddr())
	}
	if cfg.ProbeAddr() != ":8081" {
		t.Errorf("Expected ProbeAddr default ':8081', got %q", cfg.ProbeAddr())
	}
	if cfg.EnableLeaderElection() != false {
		t.Errorf("Expected EnableLeaderElection default false, got %v", cfg.EnableLeaderElection())
	}
	if cfg.OptimizationInterval() != 60*time.Second {
		t.Errorf("Expected OptimizationInterval default 60s, got %v", cfg.OptimizationInterval())
	}
}

func TestLoad_FlagsPrecedence(t *testing.T) {
	// Set env var (should be overridden by flags)
	_ = os.Setenv("METRICS_BIND_ADDRESS", "env-value")
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() {
		_ = os.Unsetenv("METRICS_BIND_ADDRESS")
		_ = os.Unsetenv("PROMETHEUS_BASE_URL")
	}()

	// Config file value (should be overridden by flags)
	configFile := writeTestConfigFile(t, `METRICS_BIND_ADDRESS: "file-value"`)

	// Create a flagset with metrics-bind-address explicitly set
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("metrics-bind-address", "0", "")
	_ = fs.Set("metrics-bind-address", "flag-value")

	cfg, err := Load(fs, configFile)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Flag should take precedence
	if cfg.MetricsAddr() != "flag-value" {
		t.Errorf("Expected MetricsAddr 'flag-value' (from flag), got %q", cfg.MetricsAddr())
	}
}

func TestLoad_EnvPrecedence(t *testing.T) {
	_ = os.Setenv("METRICS_BIND_ADDRESS", "env-value")
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() {
		_ = os.Unsetenv("METRICS_BIND_ADDRESS")
		_ = os.Unsetenv("PROMETHEUS_BASE_URL")
	}()

	// Config file value (should be overridden by env)
	configFile := writeTestConfigFile(t, `METRICS_BIND_ADDRESS: "file-value"`)

	cfg, err := Load(nil, configFile)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Env should take precedence over config file
	if cfg.MetricsAddr() != "env-value" {
		t.Errorf("Expected MetricsAddr 'env-value' (from env), got %q", cfg.MetricsAddr())
	}
}

func TestLoad_FilePrecedence(t *testing.T) {
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	configFile := writeTestConfigFile(t, `METRICS_BIND_ADDRESS: "file-value"`)

	cfg, err := Load(nil, configFile)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Config file should be used
	if cfg.MetricsAddr() != "file-value" {
		t.Errorf("Expected MetricsAddr 'file-value' (from file), got %q", cfg.MetricsAddr())
	}
}

func TestLoad_PrometheusConfigRequired(t *testing.T) {
	// No Prometheus config set
	cfg, err := Load(nil, "")
	if err == nil {
		t.Fatal("Expected Load() to fail when Prometheus config is missing, but it succeeded")
	}
	if cfg != nil {
		t.Error("Expected Load() to return nil Config when validation fails")
	}
}

func TestLoad_PrometheusConfigFromEnv(t *testing.T) {
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus-env:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cfg, err := Load(nil, "")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.PrometheusBaseURL() == "" {
		t.Fatal("Expected Prometheus config to be loaded")
	}
	if cfg.PrometheusBaseURL() != "https://prometheus-env:9090" {
		t.Errorf("Expected Prometheus BaseURL from env, got %q", cfg.PrometheusBaseURL())
	}
}

func TestLoad_PrometheusConfigFromFile(t *testing.T) {
	configFile := writeTestConfigFile(t, `PROMETHEUS_BASE_URL: "https://prometheus-file:9090"`)

	cfg, err := Load(nil, configFile)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.PrometheusBaseURL() == "" {
		t.Fatal("Expected Prometheus config to be loaded")
	}
	if cfg.PrometheusBaseURL() != "https://prometheus-file:9090" {
		t.Errorf("Expected Prometheus BaseURL from file, got %q", cfg.PrometheusBaseURL())
	}
}

func TestLoad_OptimizationIntervalFromFile(t *testing.T) {
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	configFile := writeTestConfigFile(t, `GLOBAL_OPT_INTERVAL: "30s"`)

	cfg, err := Load(nil, configFile)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.OptimizationInterval() != 30*time.Second {
		t.Errorf("Expected OptimizationInterval 30s, got %v", cfg.OptimizationInterval())
	}
}

func TestLoad_FeatureFlagsFromFile(t *testing.T) {
	configFile := writeTestConfigFile(t, `
PROMETHEUS_BASE_URL: "https://prometheus:9090"
WVA_SCALE_TO_ZERO: "true"
WVA_LIMITED_MODE: "false"
SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY: "5"
`)

	cfg, err := Load(nil, configFile)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if !cfg.ScaleToZeroEnabled() {
		t.Error("Expected ScaleToZeroEnabled to be true")
	}
	if cfg.LimitedModeEnabled() {
		t.Error("Expected LimitedModeEnabled to be false")
	}
	if cfg.ScaleFromZeroMaxConcurrency() != 5 {
		t.Errorf("Expected ScaleFromZeroMaxConcurrency 5, got %d", cfg.ScaleFromZeroMaxConcurrency())
	}
}

func TestLoad_PrometheusCacheConfigFromFile(t *testing.T) {
	configFile := writeTestConfigFile(t, `
PROMETHEUS_BASE_URL: "https://prometheus:9090"
PROMETHEUS_METRICS_CACHE_TTL: "60s"
`)

	cfg, err := Load(nil, configFile)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	cacheConfig := cfg.PrometheusCacheConfig()
	if cacheConfig == nil {
		t.Fatal("Expected Prometheus cache config to be loaded")
	}
	if cacheConfig.TTL != 60*time.Second {
		t.Errorf("Expected cache TTL 60s, got %v", cacheConfig.TTL)
	}
}

func TestConfig_ThreadSafety(t *testing.T) {
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cfg, err := Load(nil, "")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Test concurrent reads
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- true }()
			_ = cfg.OptimizationInterval()
			_ = cfg.SaturationConfig()
			_ = cfg.ScaleToZeroConfig()
			_ = cfg.PrometheusCacheConfig()
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestConfig_UpdateDynamicConfig(t *testing.T) {
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cfg, err := Load(nil, "")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Update saturation config
	satConfig := map[string]interfaces.SaturationScalingConfig{
		"test": {
			KvCacheThreshold:     0.9,
			QueueLengthThreshold: 10,
			KvSpareTrigger:       0.2,
			QueueSpareTrigger:    5,
		},
	}
	cfg.UpdateSaturationConfig(satConfig)

	updatedSatConfig := cfg.SaturationConfig()
	if len(updatedSatConfig) != 1 {
		t.Fatalf("Expected 1 saturation config entry after update, got %d", len(updatedSatConfig))
	}
}

func TestLoad_Validation_OptimizationInterval(t *testing.T) {
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cfg, err := Load(nil, "")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Default should be valid (> 0)
	if cfg.OptimizationInterval() <= 0 {
		t.Errorf("Expected positive optimization interval, got %v", cfg.OptimizationInterval())
	}
}

func TestLoad_NoConfigFile(t *testing.T) {
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cfg, err := Load(nil, "")
	if err != nil {
		t.Fatalf("Load() should succeed with defaults when no config file: %v", err)
	}

	// Should use defaults
	if cfg.OptimizationInterval() != 60*time.Second {
		t.Errorf("Expected default OptimizationInterval 60s, got %v", cfg.OptimizationInterval())
	}
}

func TestLoad_MissingConfigFile(t *testing.T) {
	// Pointing to a non-existent file should fail
	_, err := Load(nil, "/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("Expected Load() to fail when config file doesn't exist")
	}
}

func TestLoad_ConfigFromFile(t *testing.T) {
	configFile := writeTestConfigFile(t, `
PROMETHEUS_BASE_URL: "https://prometheus-file:9090"
GLOBAL_OPT_INTERVAL: "120s"
WVA_SCALE_TO_ZERO: "true"
PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "true"
PROMETHEUS_CA_CERT_PATH: "/custom/ca.crt"
`)

	cfg, err := Load(nil, configFile)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.PrometheusBaseURL() != "https://prometheus-file:9090" {
		t.Errorf("Expected Prometheus BaseURL from file, got %q", cfg.PrometheusBaseURL())
	}
	if cfg.OptimizationInterval() != 120*time.Second {
		t.Errorf("Expected OptimizationInterval 120s, got %v", cfg.OptimizationInterval())
	}
	if !cfg.ScaleToZeroEnabled() {
		t.Error("Expected ScaleToZeroEnabled to be true from file")
	}
	if !cfg.PrometheusInsecureSkipVerify() {
		t.Error("Expected PrometheusInsecureSkipVerify to be true from file")
	}
	if cfg.PrometheusCACertPath() != "/custom/ca.crt" {
		t.Errorf("Expected Prometheus CA cert path '/custom/ca.crt', got %q", cfg.PrometheusCACertPath())
	}
}

// TestLoad_BoolPrecedence tests that boolean flag precedence is correct: flag > env > file
func TestLoad_BoolPrecedence(t *testing.T) {
	// Set required Prometheus env var
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	t.Run("flag=false should take precedence over file=true", func(t *testing.T) {
		configFile := writeTestConfigFile(t, `LEADER_ELECT: "true"`)

		// Flag explicitly set to false
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.Bool("leader-elect", false, "")
		_ = fs.Set("leader-elect", "false")

		cfg, err := Load(fs, configFile)
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if cfg.EnableLeaderElection() {
			t.Errorf("Expected EnableLeaderElection=false (from flag), got true")
		}
	})

	t.Run("flag=true should take precedence over file=false", func(t *testing.T) {
		configFile := writeTestConfigFile(t, `LEADER_ELECT: "false"`)

		// Flag explicitly set to true
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.Bool("leader-elect", false, "")
		_ = fs.Set("leader-elect", "true")

		cfg, err := Load(fs, configFile)
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if !cfg.EnableLeaderElection() {
			t.Errorf("Expected EnableLeaderElection=true (from flag), got false")
		}
	})

	t.Run("env should take precedence over file when flag is unset", func(t *testing.T) {
		configFile := writeTestConfigFile(t, `LEADER_ELECT: "false"`)

		_ = os.Setenv("LEADER_ELECT", "true")
		defer func() { _ = os.Unsetenv("LEADER_ELECT") }()

		cfg, err := Load(nil, configFile)
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if !cfg.EnableLeaderElection() {
			t.Errorf("Expected EnableLeaderElection=true (from env), got false")
		}
	})

	t.Run("file should be used when flag and env are unset", func(t *testing.T) {
		configFile := writeTestConfigFile(t, `LEADER_ELECT: "true"`)

		_ = os.Unsetenv("LEADER_ELECT")

		cfg, err := Load(nil, configFile)
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if !cfg.EnableLeaderElection() {
			t.Errorf("Expected EnableLeaderElection=true (from file), got false")
		}
	})
}

// TestLoad_DurationPrecedence tests that duration flag precedence is correct: flag > env > file > defaults
func TestLoad_DurationPrecedence(t *testing.T) {
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	t.Run("flag should take precedence over file", func(t *testing.T) {
		configFile := writeTestConfigFile(t, `LEADER_ELECTION_LEASE_DURATION: "30s"`)

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.Duration("leader-election-lease-duration", 60*time.Second, "")
		_ = fs.Set("leader-election-lease-duration", "45s")

		cfg, err := Load(fs, configFile)
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if cfg.LeaseDuration() != 45*time.Second {
			t.Errorf("Expected LeaseDuration=45s (from flag), got %v", cfg.LeaseDuration())
		}
	})

	t.Run("env should take precedence over file when flag is unset", func(t *testing.T) {
		configFile := writeTestConfigFile(t, `LEADER_ELECTION_LEASE_DURATION: "30s"`)

		_ = os.Setenv("LEADER_ELECTION_LEASE_DURATION", "45s")
		defer func() { _ = os.Unsetenv("LEADER_ELECTION_LEASE_DURATION") }()

		cfg, err := Load(nil, configFile)
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if cfg.LeaseDuration() != 45*time.Second {
			t.Errorf("Expected LeaseDuration=45s (from env), got %v", cfg.LeaseDuration())
		}
	})

	t.Run("file should be used when flag and env are unset", func(t *testing.T) {
		configFile := writeTestConfigFile(t, `LEADER_ELECTION_LEASE_DURATION: "30s"`)

		_ = os.Unsetenv("LEADER_ELECTION_LEASE_DURATION")

		cfg, err := Load(nil, configFile)
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if cfg.LeaseDuration() != 30*time.Second {
			t.Errorf("Expected LeaseDuration=30s (from file), got %v", cfg.LeaseDuration())
		}
	})

	t.Run("default should be used when flag, env, and file are all unset", func(t *testing.T) {
		_ = os.Unsetenv("LEADER_ELECTION_LEASE_DURATION")

		cfg, err := Load(nil, "")
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if cfg.LeaseDuration() != 60*time.Second {
			t.Errorf("Expected LeaseDuration=60s (from default), got %v", cfg.LeaseDuration())
		}
	})
}
