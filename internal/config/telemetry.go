package config

// otelConfig holds all OpenTelemetry-related configuration
type otelConfig struct {
	// Immutable (set at startup)
	targetEndpointGrpc string
	insecureSkipVerify bool
	caCertPath         string
}

// ============================================================================
// OpenTelemetry Getters (thread-safe)
// ============================================================================

// OtelTargetEndpointGrpc returns the OpenTelemetry target endpoint gRPC URL.
// Thread-safe.
func (c *Config) OtelTargetEndpointGrpc() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.otelConfig.targetEndpointGrpc
}

func (c *Config) OtelCaCertPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.otelConfig.caCertPath
}

func (c *Config) OtelInsecureSkipVerify() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.otelConfig.insecureSkipVerify
}
