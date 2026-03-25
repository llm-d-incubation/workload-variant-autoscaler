package utils

import (
	"os"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	// Initialize logger for tests
	logging.NewTestLogger()
}

func testConfigFromEnv(t *testing.T, env map[string]string) *config.Config {
	t.Helper()

	keys := []string{
		"PROMETHEUS_BASE_URL",
		"PROMETHEUS_BEARER_TOKEN",
		"PROMETHEUS_TOKEN_PATH",
		"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY",
		"PROMETHEUS_CA_CERT_PATH",
		"PROMETHEUS_CLIENT_CERT_PATH",
		"PROMETHEUS_CLIENT_KEY_PATH",
		"PROMETHEUS_SERVER_NAME",
	}

	originalValues := make(map[string]string, len(keys))
	originalSet := make(map[string]bool, len(keys))
	for _, key := range keys {
		value, ok := os.LookupEnv(key)
		if ok {
			originalValues[key] = value
			originalSet[key] = true
		}
		_ = os.Unsetenv(key)
	}

	for key, value := range env {
		require.NoError(t, os.Setenv(key, value))
	}

	t.Cleanup(func() {
		for _, key := range keys {
			if originalSet[key] {
				_ = os.Setenv(key, originalValues[key])
			} else {
				_ = os.Unsetenv(key)
			}
		}
	})

	cfg, err := config.Load(nil, "")
	require.NoError(t, err)
	return cfg
}

func TestCreateTLSConfig(t *testing.T) {
	tests := []struct {
		name        string
		promConfig  *config.Config
		expectError bool
	}{
		{
			name:        "nil config",
			promConfig:  nil,
			expectError: false,
		},
		{
			name: "TLS with insecure skip verify",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":                 "https://prometheus:9090",
				"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
			}),
			expectError: false,
		},
		{
			name: "TLS with server name",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":    "https://prometheus:9090",
				"PROMETHEUS_SERVER_NAME": "prometheus.example.com",
			}),
			expectError: false,
		},
		{
			name: "insecure skip verify with invalid CA cert path should not error",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":                 "https://prometheus:9090",
				"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
				"PROMETHEUS_CA_CERT_PATH":             "/nonexistent/path/ca.crt",
			}),
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsCfg, err := CreateTLSConfig(tt.promConfig)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			if tt.promConfig != nil {
				assert.NotNil(t, tlsCfg)
			} else {
				assert.Nil(t, tlsCfg)
			}
		})
	}
}

func TestCreateTLSConfig_InsecureSkipVerifySkipsCertLoading(t *testing.T) {
	invalidCertFile, err := os.CreateTemp(t.TempDir(), "invalid-cert-*.crt")
	require.NoError(t, err)
	_, err = invalidCertFile.WriteString("# CA certificate not provided - using system CA bundle")
	require.NoError(t, err)
	require.NoError(t, invalidCertFile.Close())

	cfg := testConfigFromEnv(t, map[string]string{
		"PROMETHEUS_BASE_URL":                 "https://prometheus:9090",
		"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
		"PROMETHEUS_CA_CERT_PATH":             invalidCertFile.Name(),
	})

	tlsCfg, err := CreateTLSConfig(cfg)
	assert.NoError(t, err)
	assert.NotNil(t, tlsCfg)
	assert.True(t, tlsCfg.InsecureSkipVerify)
	assert.Nil(t, tlsCfg.RootCAs, "RootCAs should be nil when insecureSkipVerify is true")
}

func TestValidateTLSConfig(t *testing.T) {
	tests := []struct {
		name        string
		promConfig  *config.Config
		expectError bool
	}{
		{
			name:        "nil config - should fail",
			promConfig:  nil,
			expectError: true,
		},
		{
			name: "HTTP URL - should fail",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL": "http://prometheus:9090",
			}),
			expectError: true,
		},
		{
			name: "TLS with insecure skip verify",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":                 "https://prometheus:9090",
				"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
			}),
			expectError: false,
		},
		{
			name: "TLS with server name",
			promConfig: testConfigFromEnv(t, map[string]string{
				"PROMETHEUS_BASE_URL":    "https://prometheus:9090",
				"PROMETHEUS_SERVER_NAME": "prometheus.example.com",
			}),
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTLSConfig(tt.promConfig)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
