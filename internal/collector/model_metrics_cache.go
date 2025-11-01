package collector

import (
	"sync"
	"time"

	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
)

// ModelMetrics holds aggregated metrics for a specific model.
// These metrics are collected from Prometheus and represent model-level
// aggregations across all deployments serving that model.
type ModelMetrics struct {
	ModelID     string                 // Unique identifier for the model
	Namespace   string                 // Kubernetes namespace
	Load        interfaces.LoadProfile // Workload characteristics (arrival rate, tokens)
	TTFTAverage string                 // Average time to first token (milliseconds)
	ITLAverage  string                 // Average inter-token latency (milliseconds)
	LastUpdated time.Time              // Timestamp when metrics were last collected
	Valid       bool                   // Whether metrics are valid (query succeeded)
}

// ModelMetricsCache provides thread-safe caching of model-level metrics.
// This prevents redundant Prometheus queries when multiple VariantAutoscalings
// reference the same model.
type ModelMetricsCache struct {
	metrics map[string]*ModelMetrics // key: "modelID:namespace" -> metrics
	mu      sync.RWMutex             // protects metrics map
	ttl     time.Duration            // time-to-live for cached metrics
}

// NewModelMetricsCache creates a new model metrics cache with the specified TTL.
// TTL determines how long cached metrics remain valid before requiring refresh.
// TTL must be positive; if <= 0, defaults to 30 seconds.
//
// Note: Expired entries are not automatically removed from memory but are considered
// invalid on Get(). Call Cleanup() periodically if memory usage is a concern.
func NewModelMetricsCache(ttl time.Duration) *ModelMetricsCache {
	// Validate TTL - prevent negative or zero TTL
	if ttl <= 0 {
		logger.Log.Warn("Invalid TTL provided, using default", "provided", ttl, "default", "30s")
		ttl = 30 * time.Second
	}

	return &ModelMetricsCache{
		metrics: make(map[string]*ModelMetrics),
		ttl:     ttl,
	}
}

// cacheKey generates a unique key for model metrics lookup.
func (c *ModelMetricsCache) cacheKey(modelID, namespace string) string {
	return modelID + ":" + namespace
}

// Get retrieves cached metrics for a model if they exist and are not expired.
// Returns (metrics, true) if valid cached metrics exist, (nil, false) otherwise.
func (c *ModelMetricsCache) Get(modelID, namespace string) (*ModelMetrics, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := c.cacheKey(modelID, namespace)
	metrics, exists := c.metrics[key]

	if !exists {
		return nil, false
	}

	// Check if metrics are still valid (within TTL)
	if time.Since(metrics.LastUpdated) > c.ttl {
		return nil, false
	}

	return metrics, true
}

// Set stores or updates metrics for a model in the cache.
// Automatically sets LastUpdated to current time.
func (c *ModelMetricsCache) Set(modelID, namespace string, load interfaces.LoadProfile, ttftAvg, itlAvg string, valid bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := c.cacheKey(modelID, namespace)
	c.metrics[key] = &ModelMetrics{
		ModelID:     modelID,
		Namespace:   namespace,
		Load:        load,
		TTFTAverage: ttftAvg,
		ITLAverage:  itlAvg,
		LastUpdated: time.Now(),
		Valid:       valid,
	}
}

// Invalidate removes metrics for a specific model from the cache.
// This can be used to force a refresh on the next query.
func (c *ModelMetricsCache) Invalidate(modelID, namespace string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := c.cacheKey(modelID, namespace)
	delete(c.metrics, key)
}

// Clear removes all cached metrics.
// Useful for testing or when a full cache refresh is needed.
func (c *ModelMetricsCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.metrics = make(map[string]*ModelMetrics)
}

// Size returns the current number of cached model metrics.
func (c *ModelMetricsCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.metrics)
}

// GetAll returns a snapshot of all cached metrics.
// The returned map is a copy and safe for concurrent iteration.
func (c *ModelMetricsCache) GetAll() map[string]*ModelMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()

	snapshot := make(map[string]*ModelMetrics, len(c.metrics))
	for k, v := range c.metrics {
		// Create a copy to avoid sharing pointers
		metricsCopy := *v
		snapshot[k] = &metricsCopy
	}

	return snapshot
}

// Cleanup removes expired entries from the cache.
// Should be called periodically to prevent unbounded cache growth.
func (c *ModelMetricsCache) Cleanup() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	now := time.Now()

	for key, metrics := range c.metrics {
		if now.Sub(metrics.LastUpdated) > c.ttl {
			delete(c.metrics, key)
			removed++
		}
	}

	return removed
}

// ================================================================================
// Scale-to-Zero Metrics Cache
// ================================================================================

// ScaleToZeroMetrics holds internal metrics for scale-to-zero decisions.
// These metrics are NOT exposed in the CRD but used internally by the controller.
type ScaleToZeroMetrics struct {
	// TotalRequestsOverRetentionPeriod is the total number of requests received over
	// the scale-to-zero retention period. This is used for scale-to-zero decisions.
	TotalRequestsOverRetentionPeriod float64

	// RetentionPeriod is the configured retention period for this model
	RetentionPeriod time.Duration

	// LastUpdated is the timestamp when these metrics were last updated
	LastUpdated time.Time
}

// ScaleToZeroMetricsCache is a thread-safe cache for storing per-model scale-to-zero metrics.
// This is separate from ModelMetricsCache which stores Prometheus metrics for the optimizer.
type ScaleToZeroMetricsCache struct {
	mu      sync.RWMutex
	metrics map[string]*ScaleToZeroMetrics // modelID -> metrics
}

// NewScaleToZeroMetricsCache creates a new ScaleToZeroMetricsCache
func NewScaleToZeroMetricsCache() *ScaleToZeroMetricsCache {
	return &ScaleToZeroMetricsCache{
		metrics: make(map[string]*ScaleToZeroMetrics),
	}
}

// Set stores or updates scale-to-zero metrics for a model
func (c *ScaleToZeroMetricsCache) Set(modelID string, metrics *ScaleToZeroMetrics) {
	if metrics == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Create a copy to avoid side effects on caller's struct
	metricsCopy := *metrics
	metricsCopy.LastUpdated = time.Now()
	c.metrics[modelID] = &metricsCopy
}

// Get retrieves a copy of scale-to-zero metrics for a model
func (c *ScaleToZeroMetricsCache) Get(modelID string) (*ScaleToZeroMetrics, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	metrics, exists := c.metrics[modelID]
	if !exists {
		return nil, false
	}
	// Return a copy to prevent race conditions
	metricsCopy := *metrics
	return &metricsCopy, true
}

// GetAll returns a copy of all scale-to-zero metrics
func (c *ScaleToZeroMetricsCache) GetAll() map[string]*ScaleToZeroMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make(map[string]*ScaleToZeroMetrics, len(c.metrics))
	for k, v := range c.metrics {
		// Create a copy to avoid race conditions
		metricsCopy := *v
		result[k] = &metricsCopy
	}
	return result
}

// Delete removes scale-to-zero metrics for a model
func (c *ScaleToZeroMetricsCache) Delete(modelID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.metrics, modelID)
}

// Clear removes all cached scale-to-zero metrics
func (c *ScaleToZeroMetricsCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metrics = make(map[string]*ScaleToZeroMetrics)
}
