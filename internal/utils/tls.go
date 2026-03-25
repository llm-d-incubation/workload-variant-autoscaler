package utils

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// CreateTLSConfig creates a TLS configuration from getter-based Prometheus config.
// TLS is always enabled for HTTPS-only support. The configuration supports:
// - Server certificate validation via CA certificate
// - Mutual TLS authentication via client certificates
// - Insecure certificate verification (development/testing only)
func CreateTLSConfig(cfg *config.Config) (*tls.Config, error) {
	if cfg == nil {
		return nil, nil
	}

	insecureSkipVerify := cfg.PrometheusInsecureSkipVerify()
	serverName := cfg.PrometheusServerName()
	caCertPath := cfg.PrometheusCACertPath()
	clientCertPath := cfg.PrometheusClientCertPath()
	clientKeyPath := cfg.PrometheusClientKeyPath()

	config := &tls.Config{
		InsecureSkipVerify: insecureSkipVerify,
		ServerName:         serverName,
		MinVersion:         tls.VersionTLS12, // Enforce minimum TLS version - https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/security_and_compliance/tls-security-profiles#:~:text=requires%20a%20minimum-,TLS%20version%20of%201.2,-.
	}

	if insecureSkipVerify {
		ctrl.Log.V(logging.VERBOSE).Info("TLS certificate verification is disabled, skipping certificate loading")
		return config, nil
	}

	// Load CA certificate if provided
	if caCertPath != "" {
		caCert, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate from %s: %w", caCertPath, err)
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate from %s", caCertPath)
		}
		config.RootCAs = caCertPool
		ctrl.Log.V(logging.VERBOSE).Info("CA certificate loaded successfully", "path", caCertPath)
	}

	// Load client certificate and key if provided
	if clientCertPath != "" && clientKeyPath != "" {
		cert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate from %s and key from %s: %w",
				clientCertPath, clientKeyPath, err)
		}
		config.Certificates = []tls.Certificate{cert}
		ctrl.Log.V(logging.VERBOSE).Info("Client certificate loaded successfully",
			"cert_path", clientCertPath, "key_path", clientKeyPath)
	}

	return config, nil
}

// ValidateTLSConfig validates TLS configuration.
// Ensures HTTPS is used and certificate files exist when verification is enabled.
func ValidateTLSConfig(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	baseURL := cfg.PrometheusBaseURL()
	insecureSkipVerify := cfg.PrometheusInsecureSkipVerify()
	caCertPath := cfg.PrometheusCACertPath()
	clientCertPath := cfg.PrometheusClientCertPath()
	clientKeyPath := cfg.PrometheusClientKeyPath()

	// Validate that the URL uses HTTPS (TLS is always required)
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("HTTPS is required - URL must use https:// scheme: %s", baseURL)
	}

	// If InsecureSkipVerify is true, we don't need to validate certificate files
	// since we're intentionally skipping certificate verification
	if insecureSkipVerify {
		ctrl.Log.V(logging.VERBOSE).Info("TLS certificate verification is disabled - this is not recommended for production")
		return nil
	}

	// Check if certificate files exist (only when not skipping verification)
	if caCertPath != "" {
		if _, err := os.Stat(caCertPath); os.IsNotExist(err) {
			return fmt.Errorf("CA certificate file not found: %s", caCertPath)
		}
	}

	if clientCertPath != "" {
		if _, err := os.Stat(clientCertPath); os.IsNotExist(err) {
			return fmt.Errorf("client certificate file not found: %s", clientCertPath)
		}
	}

	if clientKeyPath != "" {
		if _, err := os.Stat(clientKeyPath); os.IsNotExist(err) {
			return fmt.Errorf("client key file not found: %s", clientKeyPath)
		}
	}

	return nil
}
