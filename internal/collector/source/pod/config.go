// Package pod provides the Pod scraping metrics source implementation.
//
// This file contains configuration types and defaults for PodScrapingSource.
package pod

import "time"

// PodScrapingSourceConfig contains configuration for pod scraping.
type PodScrapingSourceConfig struct {
	// Service identification (required)
	ServiceName      string
	ServiceNamespace string

	// Metrics endpoint (provided by client/engine)
	MetricsPort   int32  // provided by client
	MetricsPath   string // provided by client, default: "/metrics"
	MetricsScheme string // provided by client, default: "http"

	// Authentication
	MetricsReaderSecretName      string
	MetricsReaderSecretNamespace string // namespace where the secret is located; defaults to ServiceNamespace if empty
	MetricsReaderSecretKey       string // default: "token"
	BearerToken                  string // optional: explicit token override

	// Scraping behavior
	ScrapeTimeout        time.Duration // default: 5s per pod
	MaxConcurrentScrapes int           // default: 10

	// Cache configuration
	DefaultTTL time.Duration // default: 30s
}

// DefaultPodScrapingSourceConfig returns sensible defaults.
func DefaultPodScrapingSourceConfig() PodScrapingSourceConfig {
	return PodScrapingSourceConfig{
		MetricsPath:            "/metrics",
		MetricsScheme:          "http",
		MetricsReaderSecretKey: "token",
		ScrapeTimeout:          5 * time.Second,
		MaxConcurrentScrapes:   10,
		DefaultTTL:             30 * time.Second,
	}
}
