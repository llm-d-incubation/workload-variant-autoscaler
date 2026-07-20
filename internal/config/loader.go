package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
	ctrl "sigs.k8s.io/controller-runtime"
)

// flagBindings maps viper keys (= env var names = config file keys) to pflag names.
var flagBindings = map[string]string{
	"METRICS_BIND_ADDRESS":           "metrics-bind-address",
	"HEALTH_PROBE_BIND_ADDRESS":      "health-probe-bind-address",
	"LEADER_ELECT":                   "leader-elect",
	"LEADER_ELECTION_LEASE_DURATION": "leader-election-lease-duration",
	"LEADER_ELECTION_RENEW_DEADLINE": "leader-election-renew-deadline",
	"LEADER_ELECTION_RETRY_PERIOD":   "leader-election-retry-period",
	"REST_CLIENT_TIMEOUT":            "rest-client-timeout",
	"METRICS_SECURE":                 "metrics-secure",
	"WATCH_NAMESPACE":                "watch-namespace",
	"V":                              "v",
	"WEBHOOK_CERT_PATH":              "webhook-cert-path",
	"WEBHOOK_CERT_NAME":              "webhook-cert-name",
	"WEBHOOK_CERT_KEY":               "webhook-cert-key",
	"METRICS_CERT_PATH":              "metrics-cert-path",
	"METRICS_CERT_NAME":              "metrics-cert-name",
	"METRICS_CERT_KEY":               "metrics-cert-key",
	"LIMITER_TYPE":                   "limiter-type",
	"QUOTA_CONFIG_FILE":              "quota-config-file",
}

// Load loads and validates the unified configuration.
// Precedence: flags > env > config file > defaults
// The main configuration is read from a mounted YAML file, but can be overridden
// by environment variables or command-line flags.
// Returns the loaded Config object.
// Returns error if required configuration is missing or invalid (fail-fast).
// flagSet may be nil (e.g. in tests that don't set CLI flags).
func Load(flagSet *flag.FlagSet, configFilePath string) (*Config, error) {
	cfg := &Config{}

	// Load configuration (flags > env > config file > defaults)
	if err := loadConfig(cfg, flagSet, configFilePath); err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Validate required configuration (fail-fast)
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	ctrl.Log.Info("Configuration loaded successfully")
	return cfg, nil
}

// loadConfig loads configuration with precedence: flags > env > config file > defaults
func loadConfig(cfg *Config, flagSet *flag.FlagSet, configFilePath string) error {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	v := viper.New()

	// Set defaults
	v.SetDefault("METRICS_BIND_ADDRESS", "0")
	v.SetDefault("HEALTH_PROBE_BIND_ADDRESS", ":8081")
	v.SetDefault("LEADER_ELECT", false)
	v.SetDefault("LEADER_ELECTION_ID", "72dd1cf1.llm-d.ai")
	v.SetDefault("LEADER_ELECTION_LEASE_DURATION", 60*time.Second)
	v.SetDefault("LEADER_ELECTION_RENEW_DEADLINE", 50*time.Second)
	v.SetDefault("LEADER_ELECTION_RETRY_PERIOD", 10*time.Second)
	v.SetDefault("REST_CLIENT_TIMEOUT", 60*time.Second)
	v.SetDefault("METRICS_SECURE", true)
	v.SetDefault("WATCH_NAMESPACE", "")
	v.SetDefault("V", 0)
	v.SetDefault("WEBHOOK_CERT_PATH", "")
	v.SetDefault("WEBHOOK_CERT_NAME", "tls.crt")
	v.SetDefault("WEBHOOK_CERT_KEY", "tls.key")
	v.SetDefault("METRICS_CERT_PATH", "")
	v.SetDefault("METRICS_CERT_NAME", "tls.crt")
	v.SetDefault("METRICS_CERT_KEY", "tls.key")
	v.SetDefault("WVA_SCALE_TO_ZERO", false)
	v.SetDefault("WVA_LIMITED_MODE", false)
	v.SetDefault("SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY", 10)
	v.SetDefault("GLOBAL_OPT_INTERVAL", "60s")
	v.SetDefault("EXPERIMENTAL_COORDINATOR_ENABLED", false)
	v.SetDefault("COORDINATOR_INTERVAL", "15s")
	v.SetDefault("LIMITER_TYPE", "inventory")
	v.SetDefault("QUOTA_CONFIG_FILE", "")

	// Load from config file (mounted in the container) — sits between env and defaults in precedence
	if configFilePath != "" {
		v.SetConfigFile(configFilePath)
		if err := v.ReadInConfig(); err != nil {
			return fmt.Errorf("failed to read config file %s: %w", configFilePath, err)
		}
		ctrl.Log.Info("Loaded config from file", "path", configFilePath)
	}

	// Bind environment variables (precedence above config file, below flags)
	v.AutomaticEnv()

	// Bind pflag flags (highest precedence for explicitly-set flags)
	if flagSet != nil {
		for viperKey, flagName := range flagBindings {
			if f := flagSet.Lookup(flagName); f != nil {
				_ = v.BindPFlag(viperKey, f)
			}
		}
	}

	// Read resolved values into Config
	cfg.infrastructure = infrastructureConfig{
		metricsAddr:          v.GetString("METRICS_BIND_ADDRESS"),
		probeAddr:            v.GetString("HEALTH_PROBE_BIND_ADDRESS"),
		enableLeaderElection: v.GetBool("LEADER_ELECT"),
		leaderElectionID:     v.GetString("LEADER_ELECTION_ID"),
		leaseDuration:        v.GetDuration("LEADER_ELECTION_LEASE_DURATION"),
		renewDeadline:        v.GetDuration("LEADER_ELECTION_RENEW_DEADLINE"),
		retryPeriod:          v.GetDuration("LEADER_ELECTION_RETRY_PERIOD"),
		restTimeout:          v.GetDuration("REST_CLIENT_TIMEOUT"),
		secureMetrics:        v.GetBool("METRICS_SECURE"),
		watchNamespace:       v.GetString("WATCH_NAMESPACE"),
		loggerVerbosity:      v.GetInt("V"),
		optimizationInterval: v.GetDuration("GLOBAL_OPT_INTERVAL"),
	}

	cfg.tls = tlsConfig{
		webhookCertPath: v.GetString("WEBHOOK_CERT_PATH"),
		webhookCertName: v.GetString("WEBHOOK_CERT_NAME"),
		webhookCertKey:  v.GetString("WEBHOOK_CERT_KEY"),
		metricsCertPath: v.GetString("METRICS_CERT_PATH"),
		metricsCertName: v.GetString("METRICS_CERT_NAME"),
		metricsCertKey:  v.GetString("METRICS_CERT_KEY"),
	}

	cfg.features = featureFlagsConfig{
		scaleToZeroEnabled:          v.GetBool("WVA_SCALE_TO_ZERO"),
		limitedModeEnabled:          v.GetBool("WVA_LIMITED_MODE"),
		scaleFromZeroMaxConcurrency: v.GetInt("SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY"),
	}

	cfg.saturation = saturationConfig{
		global:           make(SaturationScalingConfigPerModel),
		namespaceConfigs: make(map[string]SaturationScalingConfigPerModel),
	}

	cfg.scaleToZero = scaleToZeroConfig{
		global:           make(ScaleToZeroConfigData),
		namespaceConfigs: make(map[string]ScaleToZeroConfigData),
	}

	cfg.coordinator = coordinatorConfig{
		enabled:  v.GetBool("EXPERIMENTAL_COORDINATOR_ENABLED"),
		interval: v.GetDuration("COORDINATOR_INTERVAL"),
	}

	// Prometheus cache config from config file / env / defaults
	cfg.prometheus.cache = parsePrometheusCacheConfigFromViper(v)

	// Prometheus connection config from config file / env
	promBaseURL := v.GetString("PROMETHEUS_BASE_URL")
	if promBaseURL == "" {
		return errors.New("prometheus configuration is required but not found. " +
			"set PROMETHEUS_BASE_URL in config file or environment variable")
	}
	cfg.prometheus.baseURL = promBaseURL
	cfg.prometheus.bearerToken = v.GetString("PROMETHEUS_BEARER_TOKEN")
	cfg.prometheus.tokenPath = v.GetString("PROMETHEUS_TOKEN_PATH")
	cfg.prometheus.insecureSkipVerify = v.GetBool("PROMETHEUS_TLS_INSECURE_SKIP_VERIFY")
	cfg.prometheus.allowHTTP = v.GetBool("PROMETHEUS_ALLOW_HTTP")
	cfg.prometheus.caCertPath = v.GetString("PROMETHEUS_CA_CERT_PATH")
	cfg.prometheus.clientCertPath = v.GetString("PROMETHEUS_CLIENT_CERT_PATH")
	cfg.prometheus.clientKeyPath = v.GetString("PROMETHEUS_CLIENT_KEY_PATH")
	cfg.prometheus.serverName = v.GetString("PROMETHEUS_SERVER_NAME")

	cfg.limiter = limiterConfig{
		limiterType:     LimiterType(v.GetString("LIMITER_TYPE")),
		quotaConfigFile: v.GetString("QUOTA_CONFIG_FILE"),
	}
	if err := loadQuotaLimiterEntries(&cfg.limiter); err != nil {
		return fmt.Errorf("failed to load quota limiter entries: %w", err)
	}
	return nil
}

// loadQuotaLimiterEntries reads, parses, and validates the YAML file at
// limiter.quotaConfigFile when limiterType == LimiterTypeQuota. The file
// shape is QuotaLimiterEntries. Warnings are emitted to the controller
// log; only hard errors abort startup.
//
// No-op when limiterType != LimiterTypeQuota or quotaConfigFile is empty —
// validation that QUOTA_CONFIG_FILE is set when LIMITER_TYPE=quota lives
// in Validate() so an operator gets a single coherent error message.
func loadQuotaLimiterEntries(limiter *limiterConfig) error {
	if limiter.limiterType != LimiterTypeQuota || limiter.quotaConfigFile == "" {
		return nil
	}
	data, err := os.ReadFile(limiter.quotaConfigFile)
	if err != nil {
		return fmt.Errorf("read %q: %w", limiter.quotaConfigFile, err)
	}
	var entries QuotaLimiterEntries
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse %q as quota limiter YAML: %w", limiter.quotaConfigFile, err)
	}
	warnings, err := entries.Validate()
	if err != nil {
		return fmt.Errorf("validate %q: %w", limiter.quotaConfigFile, err)
	}
	for _, w := range warnings {
		ctrl.Log.Info("quota limiter config warning", "warning", w, "file", limiter.quotaConfigFile)
	}
	limiter.quotaEntries = entries.Limiters
	return nil
}

// parsePrometheusCacheConfigFromViper reads Prometheus cache configuration from
// a viper instance (which may have loaded values from file, env, or defaults).
func parsePrometheusCacheConfigFromViper(v *viper.Viper) *CacheConfig {
	defaults := defaultPrometheusCacheConfig()

	config := &CacheConfig{
		TTL:                 parseDurationOrDefault(v.GetString("PROMETHEUS_METRICS_CACHE_TTL"), defaults.TTL),
		CleanupInterval:     parseDurationOrDefault(v.GetString("PROMETHEUS_METRICS_CACHE_CLEANUP_INTERVAL"), defaults.CleanupInterval),
		FetchInterval:       parseDurationOrDefault(v.GetString("PROMETHEUS_METRICS_CACHE_FETCH_INTERVAL"), defaults.FetchInterval),
		FreshnessThresholds: DefaultFreshnessThresholds(),
	}

	if t := v.GetString("PROMETHEUS_METRICS_CACHE_FRESH_THRESHOLD"); t != "" {
		config.FreshnessThresholds.FreshThreshold = parseDurationOrDefault(t, defaults.FreshnessThresholds.FreshThreshold)
	}
	if t := v.GetString("PROMETHEUS_METRICS_CACHE_STALE_THRESHOLD"); t != "" {
		config.FreshnessThresholds.StaleThreshold = parseDurationOrDefault(t, defaults.FreshnessThresholds.StaleThreshold)
	}
	if t := v.GetString("PROMETHEUS_METRICS_CACHE_UNAVAILABLE_THRESHOLD"); t != "" {
		config.FreshnessThresholds.UnavailableThreshold = parseDurationOrDefault(t, defaults.FreshnessThresholds.UnavailableThreshold)
	}

	return config
}

// parseDurationOrDefault parses a duration string and returns the default if parsing fails.
func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// defaultPrometheusCacheConfig returns default Prometheus cache configuration
func defaultPrometheusCacheConfig() *CacheConfig {
	return &CacheConfig{
		TTL:                 30 * time.Second,
		CleanupInterval:     1 * time.Minute,
		FetchInterval:       30 * time.Second,
		FreshnessThresholds: DefaultFreshnessThresholds(),
	}
}
