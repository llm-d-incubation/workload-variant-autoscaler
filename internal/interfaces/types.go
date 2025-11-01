package controller

import (
	"fmt"
	"math"
	"strconv"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	inferno "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/core"
)

// Captures response from ModelAnalyzer(s) per model
type ModelAnalyzeResponse struct {
	// feasible allocations for all accelerators
	Allocations map[string]*ModelAcceleratorAllocation // accelerator name -> allocation
}

// Allocation details of an accelerator to a variant
type ModelAcceleratorAllocation struct {
	Allocation *inferno.Allocation // allocation result of model analyzer

	RequiredPrefillQPS float64
	RequiredDecodeQPS  float64
	Reason             string
}

type ServiceClassEntry struct {
	Model   string `yaml:"model"`
	SLOTPOT int    `yaml:"slo-tpot"`
	SLOTTFT int    `yaml:"slo-ttft"`
}

type ServiceClass struct {
	Name     string              `yaml:"name"`
	Priority int                 `yaml:"priority"`
	Data     []ServiceClassEntry `yaml:"data"`
}

// VariantMetrics represents the internal structure for load, TTFT, and ITL metrics
// extracted from VariantAutoscaling CRD. All values are parsed and validated.
type VariantMetrics struct {
	// Load metrics
	Load LoadMetrics

	// Performance metrics
	TTFTAverage float32 // Time to first token average in milliseconds
	ITLAverage  float32 // Inter-token latency average in milliseconds
}

// LoadMetrics represents workload characteristics with typed values
type LoadMetrics struct {
	ArrivalRate     float32 // Rate of incoming requests (requests per minute)
	AvgInputTokens  int     // Average number of input (prefill) tokens per request
	AvgOutputTokens int     // Average number of output (decode) tokens per request
}

// LoadProfile represents workload characteristics as collected from Prometheus.
// This is an internal type used for metrics collection and parsing.
type LoadProfile struct {
	// ArrivalRate is the rate of incoming requests in inference server.
	ArrivalRate string `json:"arrivalRate"`

	// AvgInputTokens is the average number of input(prefill) tokens per request in inference server.
	AvgInputTokens string `json:"avgInputTokens"`

	// AvgOutputTokens is the average number of output(decode) tokens per request in inference server.
	AvgOutputTokens string `json:"avgOutputTokens"`
}

// NewVariantMetrics creates a new VariantMetrics from collected metrics (load, TTFT, ITL)
// All metrics are collected from Prometheus and passed separately, not stored in VA status
func NewVariantMetrics(load LoadProfile, ttftAverage, itlAverage string) (*VariantMetrics, error) {
	metrics := &VariantMetrics{}

	// Parse load metrics
	loadMetrics, err := ParseLoadProfile(load)
	if err != nil {
		return nil, fmt.Errorf("failed to parse load profile: %w", err)
	}
	metrics.Load = loadMetrics

	// Parse TTFT average
	ttft, err := parseFloat32(ttftAverage, "TTFTAverage")
	if err != nil {
		return nil, err
	}
	// Validate non-negative (latency cannot be negative)
	if ttft < 0 {
		ttft = 0
	}
	metrics.TTFTAverage = ttft

	// Parse ITL average
	itl, err := parseFloat32(itlAverage, "ITLAverage")
	if err != nil {
		return nil, err
	}
	// Validate non-negative (latency cannot be negative)
	if itl < 0 {
		itl = 0
	}
	metrics.ITLAverage = itl

	return metrics, nil
}

// ParseLoadProfile converts LoadProfile (string values) to internal LoadMetrics (typed values)
func ParseLoadProfile(load LoadProfile) (LoadMetrics, error) {
	metrics := LoadMetrics{}

	// Parse arrival rate
	arrivalRate, err := parseFloat32(load.ArrivalRate, "ArrivalRate")
	if err != nil {
		return metrics, err
	}
	// Validate non-negative (arrival rate cannot be negative)
	if arrivalRate < 0 {
		arrivalRate = 0
	}
	metrics.ArrivalRate = arrivalRate

	// Parse average input tokens
	avgInputTokens, err := parseFloat64(load.AvgInputTokens, "AvgInputTokens")
	if err != nil {
		return metrics, err
	}
	// Validate non-negative and within int bounds
	if avgInputTokens < 0 {
		avgInputTokens = 0
	}
	if avgInputTokens > float64(math.MaxInt32) {
		avgInputTokens = float64(math.MaxInt32)
	}
	metrics.AvgInputTokens = int(avgInputTokens)

	// Parse average output tokens
	avgOutputTokens, err := parseFloat64(load.AvgOutputTokens, "AvgOutputTokens")
	if err != nil {
		return metrics, err
	}
	// Validate non-negative and within int bounds
	if avgOutputTokens < 0 {
		avgOutputTokens = 0
	}
	if avgOutputTokens > float64(math.MaxInt32) {
		avgOutputTokens = float64(math.MaxInt32)
	}
	metrics.AvgOutputTokens = int(avgOutputTokens)

	return metrics, nil
}

// Helper function to parse float32 from string with validation
// Returns 0 for empty strings, invalid values, NaN, or Inf (resilient behavior)
func parseFloat32(value, fieldName string) (float32, error) {
	// Handle empty string
	if value == "" {
		return 0, nil
	}

	f, err := strconv.ParseFloat(value, 32)
	if err != nil {
		// Be resilient - log and default to 0 instead of failing
		logger.Log.Debug("Failed to parse metric, defaulting to 0", "field", fieldName, "value", value, "error", err)
		return 0, nil
	}

	// Check for NaN or Inf (matches original CheckValue behavior)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		logger.Log.Debug("Invalid float value (NaN/Inf), defaulting to 0", "field", fieldName, "value", value)
		return 0, nil
	}

	return float32(f), nil
}

// Helper function to parse float64 from string with validation
// Returns 0 for empty strings, invalid values, NaN, or Inf (resilient behavior)
func parseFloat64(value, fieldName string) (float64, error) {
	// Handle empty string
	if value == "" {
		return 0, nil
	}

	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		// Be resilient - log and default to 0 instead of failing
		logger.Log.Debug("Failed to parse metric, defaulting to 0", "field", fieldName, "value", value, "error", err)
		return 0, nil
	}

	// Check for NaN or Inf (matches original CheckValue behavior)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		logger.Log.Debug("Invalid float value (NaN/Inf), defaulting to 0", "field", fieldName, "value", value)
		return 0, nil
	}

	return f, nil
}

// PrometheusConfig holds complete Prometheus client configuration including TLS settings
type PrometheusConfig struct {
	// BaseURL is the Prometheus server URL (must use https:// scheme)
	BaseURL string `json:"baseURL"`

	// TLS configuration fields (TLS is always enabled for HTTPS-only support)
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"` // Skip certificate verification (development/testing only)
	CACertPath         string `json:"caCertPath,omitempty"`         // Path to CA certificate for server validation
	ClientCertPath     string `json:"clientCertPath,omitempty"`     // Path to client certificate for mutual TLS authentication
	ClientKeyPath      string `json:"clientKeyPath,omitempty"`      // Path to client private key for mutual TLS authentication
	ServerName         string `json:"serverName,omitempty"`         // Expected server name for SNI (Server Name Indication)

	// Authentication fields (BearerToken takes precedence over TokenPath)
	BearerToken string `json:"bearerToken,omitempty"` // Direct bearer token string (development/testing)
	TokenPath   string `json:"tokenPath,omitempty"`   // Path to file containing bearer token (production with mounted secrets)
}
