# Model Metrics Caching

## Overview

The Workload-Variant-Autoscaler implements a thread-safe, TTL-based cache for model-level metrics to reduce redundant Prometheus queries and improve controller performance. This is particularly beneficial in multi-variant scenarios where multiple `VariantAutoscaling` resources reference the same model.

## Architecture

### Cache Design

The `ModelMetricsCache` provides:
- **Thread-safe operations** using `sync.RWMutex` for concurrent access
- **TTL-based expiration** (default 30 seconds, configurable)
- **Namespace isolation** - same model in different namespaces is cached separately
- **Failed query caching** - prevents thundering herd on Prometheus failures
- **Automatic cleanup** - expired entries can be removed on-demand

### Components

```
┌─────────────────────────────────────────────────────────────┐
│                  Controller Reconciliation Loop             │
└───────────────────────────┬─────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│         CollectAggregateMetricsWithCache()                  │
│                                                             │
│  1. Check cache for modelID:namespace                       │
│  2. If found & valid: return cached metrics                 │
│  3. If not found: query Prometheus                          │
│  4. Cache result (even if query failed)                     │
└───────────────────────────┬─────────────────────────────────┘
                            │
         ┌──────────────────┴──────────────────┐
         │                                     │
         ▼                                     ▼
┌─────────────────┐                 ┌──────────────────────┐
│ ModelMetrics    │                 │  Prometheus API      │
│ Cache           │                 │                      │
│                 │                 │  - Load metrics      │
│ - Get()         │                 │  - TTFT average      │
│ - Set()         │                 │  - ITL average       │
│ - Invalidate()  │                 │                      │
│ - Cleanup()     │                 └──────────────────────┘
└─────────────────┘
```

## Implementation Details

### Cache Structure

```go
type ModelMetricsCache struct {
    metrics map[string]*ModelMetrics // key: "modelID:namespace"
    mu      sync.RWMutex             // protects metrics map
    ttl     time.Duration            // time-to-live for cached entries
}

type ModelMetrics struct {
    ModelID     string          // Unique identifier for the model
    Namespace   string          // Kubernetes namespace
    Load        LoadProfile     // Workload characteristics
    TTFTAverage string          // Time to first token (ms)
    ITLAverage  string          // Inter-token latency (ms)
    LastUpdated time.Time       // When metrics were cached
    Valid       bool            // Whether query succeeded
}
```

### Cache Key Format

Cache keys follow the pattern: `modelID:namespace`

**Examples:**
- `meta-llama/Llama-2-7b:production`
- `meta-llama/Llama-2-7b:staging`
- `ibm/granite-13b:default`

This ensures namespace isolation - the same model in different namespaces is cached separately.

### TTL and Expiration

**Dynamic TTL:** Calculated as half of the reconciliation interval (minimum 5 seconds)

The cache TTL is dynamically calculated at controller startup based on the configured reconciliation interval:
- **Formula:** `cacheTTL = reconciliationInterval / 2`
- **Minimum:** 5 seconds (prevents excessive queries if interval is very short)
- **Example:** If `GLOBAL_OPT_INTERVAL=60s`, then `cacheTTL=30s`
- **Example:** If `GLOBAL_OPT_INTERVAL=20s`, then `cacheTTL=10s`
- **Example:** If `GLOBAL_OPT_INTERVAL=8s`, then `cacheTTL=5s` (minimum applied)

This dynamic approach ensures:
- **Fresh data** - Cache expires between reconciliation loops, guaranteeing fresh Prometheus data at each reconciliation
- **Performance** - Cache still provides benefit within same reconciliation batch (multiple VAs for same model)
- **Adaptability** - TTL automatically adjusts to user-configured reconciliation intervals

**Expiration Logic:**
```go
func (c *ModelMetricsCache) Get(modelID, namespace string) (*ModelMetrics, bool) {
    c.mu.RLock()
    defer c.mu.RUnlock()

    metrics, exists := c.metrics[key]
    if !exists {
        return nil, false // Cache miss
    }

    if time.Since(metrics.LastUpdated) > c.ttl {
        return nil, false // Expired
    }

    return metrics, true // Cache hit
}
```

### Failed Query Caching

When Prometheus queries fail, the cache stores the failed result with `Valid: false`:

```go
// Update cache even on error (mark as invalid) to prevent thundering herd
if cache != nil {
    cache.Set(modelName, namespace, load, ttftAvg, itlAvg, err == nil)
}
```

**Benefits:**
- Prevents retry storms when Prometheus is temporarily unavailable
- Reduces controller reconciliation failures
- Failed queries respect the same TTL, allowing retry after expiration

## Usage

### Controller Integration

The cache is initialized in the controller's `SetupWithManager()` with dynamic TTL:

```go
func (r *VariantAutoscalingReconciler) SetupWithManager(mgr ctrl.Manager) error {
    // ... Prometheus client setup ...

    // Read reconciliation interval from ConfigMap to calculate optimal cache TTL
    intervalStr, err := r.readOptimizationConfig(context.Background())
    if err != nil {
        logger.Log.Warn("Failed to read optimization config, using default reconciliation interval",
            "error", err.Error())
        intervalStr = "" // Will default to 60s below
    }

    // Parse reconciliation interval (default 60s if not set)
    reconciliationInterval := 60 * time.Second
    if intervalStr != "" {
        if parsedInterval, parseErr := time.ParseDuration(intervalStr); parseErr != nil {
            logger.Log.Warn("Failed to parse reconciliation interval, using default",
                "configuredInterval", intervalStr,
                "error", parseErr.Error(),
                "default", reconciliationInterval.String())
        } else {
            reconciliationInterval = parsedInterval
        }
    }

    // Calculate cache TTL as half of reconciliation interval
    // This guarantees cache expires between reconciliation loops
    cacheTTL := reconciliationInterval / 2

    // Apply minimum TTL of 5 seconds
    minCacheTTL := 5 * time.Second
    if cacheTTL < minCacheTTL {
        logger.Log.Warn("Calculated cache TTL too short, using minimum",
            "calculated", cacheTTL.String(),
            "minimum", minCacheTTL.String())
        cacheTTL = minCacheTTL
    }

    r.MetricsCache = collector.NewModelMetricsCache(cacheTTL)
    logger.Log.Info("Model metrics cache initialized with dynamic TTL",
        "cacheTTL", cacheTTL.String(),
        "reconciliationInterval", reconciliationInterval.String())

    return ctrl.NewControllerManagedBy(mgr).
        For(&llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}).
        Complete(r)
}
```

### Collecting Metrics with Cache

```go
// In reconciliation loop
load, ttftAvg, itlAvg, err := collector.CollectAggregateMetricsWithCache(
    ctx,
    modelName,
    deploy.Namespace,
    r.PromAPI,
    r.MetricsCache,
)
```

## Performance Impact

### Metrics Query Reduction

**Scenario:** 10 VariantAutoscalings referencing the same model with 60s reconciliation interval (cacheTTL=30s)

**Without cache:**
- 10 Prometheus queries per reconciliation cycle
- Each query executes 4 separate Prometheus API calls
- Total: 40 API calls per cycle

**With dynamic cache (TTL = interval / 2):**
- First VA in reconciliation batch: 1 Prometheus query (cache miss or expired) → 4 API calls
- Next 9 VAs in same batch: Return from cache → 0 API calls
- Total: 4 API calls per cycle
- Next reconciliation (60s later): Cache expired, fresh query for first VA

**Reduction:** 90% fewer Prometheus API calls **within same reconciliation batch**

**Fresh Data Guarantee:** Cache TTL = half of reconciliation interval ensures cache always expires between reconciliation loops

### Memory Overhead

Each cached entry: ~200 bytes

**Example:** 100 unique model-namespace combinations = ~20 KB

## Configuration

### TTL Configuration

The cache TTL is **automatically calculated** from the reconciliation interval. To adjust the cache TTL, change the reconciliation interval:

1. Update the reconciliation interval via ConfigMap:
   ```bash
   kubectl patch configmap workload-variant-autoscaler-variantautoscaling-config \
     -n workload-variant-autoscaler-system \
     --type merge \
     -p '{"data":{"GLOBAL_OPT_INTERVAL":"30s"}}'
   ```

2. Restart the controller to apply changes:
   ```bash
   kubectl rollout restart deployment workload-variant-autoscaler-controller-manager \
     -n workload-variant-autoscaler-system
   ```

3. Verify the new TTL in controller logs:
   ```
   Model metrics cache initialized with dynamic TTL
     cacheTTL=15s reconciliationInterval=30s ratio="TTL = interval / 2"
   ```

**Cache TTL Examples:**
| Reconciliation Interval | Cache TTL | Notes |
|------------------------|-----------|-------|
| 60s (default) | 30s | Balanced performance |
| 30s | 15s | Fast response |
| 20s | 10s | Very fast response |
| 10s | 5s | Minimum TTL applied |
| 5s | 5s | Minimum TTL (prevents excessive queries) |

### Disabling the Cache

To disable caching (for debugging):

```go
// Pass nil cache to bypass caching
load, ttftAvg, itlAvg, err := collector.CollectAggregateMetricsWithCache(
    ctx, modelName, namespace, promAPI, nil,
)
```

## Monitoring and Debugging

### Cache Hit/Miss Logging

Debug logs show cache behavior:

```
DEBUG Using cached metrics for model  model=meta-llama/Llama-2-7b namespace=production
DEBUG Querying Prometheus for model metrics  model=ibm/granite-13b namespace=staging
```

Enable debug logging:
```bash
kubectl set env deployment/wva-controller \
  -n workload-variant-autoscaler-system \
  LOG_LEVEL=debug
```

### Cache Statistics

Programmatically access cache stats:

```go
size := r.MetricsCache.Size()
all := r.MetricsCache.GetAll()
logger.Log.Info("Cache stats", "entries", size)
```

### Manual Cache Invalidation

Force cache refresh for a specific model:

```go
r.MetricsCache.Invalidate(modelID, namespace)
```

Clear entire cache:

```go
r.MetricsCache.Clear()
```

## Thread Safety

All cache operations are thread-safe:

- **Read operations** (`Get`, `GetAll`, `Size`): Use `RLock()` - multiple concurrent reads allowed
- **Write operations** (`Set`, `Invalidate`, `Clear`, `Cleanup`): Use `Lock()` - exclusive access

**Tested:** Concurrent access with 100 goroutines in `model_metrics_cache_test.go`

## Testing

### Unit Tests

**Cache functionality:** `internal/collector/model_metrics_cache_test.go`
- Set/Get operations
- TTL expiration
- Namespace isolation
- Concurrent access
- Invalid TTL handling
- Cleanup operations

**Integration tests:** `internal/collector/collector_cache_test.go`
- Cache miss/hit scenarios
- Multiple models
- Failed query caching
- Nil cache behavior

### Running Tests

```bash
# All collector tests (includes cache tests)
go test ./internal/collector/... -v

# Specific cache tests
go test ./internal/collector/... -v -run TestModelMetricsCache
go test ./internal/collector/... -v -run TestCollectAggregateMetricsWithCache
```

## Limitations and Future Improvements

### Current Limitations

1. **Startup-time TTL calculation** - TTL is calculated at controller startup, not dynamically during runtime. Changing `GLOBAL_OPT_INTERVAL` requires controller restart.
2. **No eviction policy** - Cache grows unbounded (though TTL provides implicit bounds)
3. **No metrics** - Cache hit/miss rates not exposed as Prometheus metrics

### Planned Improvements

1. **Runtime TTL updates** - Detect ConfigMap changes and recalculate cache TTL without controller restart
2. **Cache metrics** - Expose cache hit/miss rates, size, eviction count as Prometheus metrics
3. **LRU eviction** - Limit cache size with LRU policy for long-running controllers
4. **Prometheus query batching** - Batch multiple model queries into single API call
5. **Cache warming** - Pre-populate cache on controller startup

## API Reference

### ModelMetricsCache Methods

```go
// NewModelMetricsCache creates a cache with specified TTL
func NewModelMetricsCache(ttl time.Duration) *ModelMetricsCache

// Get retrieves cached metrics (returns nil if expired or not found)
func (c *ModelMetricsCache) Get(modelID, namespace string) (*ModelMetrics, bool)

// Set stores metrics in cache with current timestamp
func (c *ModelMetricsCache) Set(modelID, namespace string, load LoadProfile,
    ttftAvg, itlAvg string, valid bool)

// Invalidate removes specific model metrics from cache
func (c *ModelMetricsCache) Invalidate(modelID, namespace string)

// Clear removes all cached metrics
func (c *ModelMetricsCache) Clear()

// Size returns number of cached entries
func (c *ModelMetricsCache) Size() int

// GetAll returns snapshot of all cached metrics
func (c *ModelMetricsCache) GetAll() map[string]*ModelMetrics

// Cleanup removes expired entries, returns count removed
func (c *ModelMetricsCache) Cleanup() int
```

### Collector Functions

```go
// CollectAggregateMetricsWithCache queries with cache support
func CollectAggregateMetricsWithCache(ctx context.Context,
    modelName string, namespace string,
    promAPI promv1.API, cache *ModelMetricsCache) (LoadProfile, string, string, error)

// CollectAggregateMetrics queries without cache (legacy)
func CollectAggregateMetrics(ctx context.Context,
    modelName string, namespace string,
    promAPI promv1.API) (LoadProfile, string, string, error)
```

## See Also

- [Prometheus Integration](../integrations/prometheus.md)
- [Controller Architecture](development.md#project-structure)
- [Performance Testing](../tutorials/performance-testing.md)
