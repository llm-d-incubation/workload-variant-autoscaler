package collector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

func TestCollectAggregateMetricsWithCache_NilCache(t *testing.T) {
	ctx := context.Background()

	// When cache is nil, should behave like CollectAggregateMetrics
	load, ttft, itl, err := CollectAggregateMetricsWithCache(ctx, "test-model", "test-ns", &utils.MockPromAPI{}, nil)

	// Should not error (mock returns valid data)
	if err != nil {
		t.Errorf("Expected no error with nil cache, got %v", err)
	}

	// Should return metrics
	if load.ArrivalRate == "" {
		t.Error("Expected load metrics to be populated")
	}

	if ttft == "" {
		t.Error("Expected TTFT to be populated")
	}

	if itl == "" {
		t.Error("Expected ITL to be populated")
	}
}

func TestCollectAggregateMetricsWithCache_CacheMiss(t *testing.T) {
	ctx := context.Background()
	cache := NewModelMetricsCache(10 * time.Second)

	// First call - cache miss
	load1, ttft1, itl1, err := CollectAggregateMetricsWithCache(ctx, "test-model", "test-ns", &utils.MockPromAPI{}, cache)

	if err != nil {
		t.Errorf("Expected no error on cache miss, got %v", err)
	}

	// Verify cache was populated
	if cache.Size() != 1 {
		t.Errorf("Expected cache size 1 after miss, got %d", cache.Size())
	}

	cached, found := cache.Get("test-model", "test-ns")
	if !found {
		t.Fatal("Expected metrics to be cached after miss")
	}

	if cached.Load.ArrivalRate != load1.ArrivalRate {
		t.Error("Cached load doesn't match returned load")
	}

	if cached.TTFTAverage != ttft1 {
		t.Error("Cached TTFT doesn't match returned TTFT")
	}

	if cached.ITLAverage != itl1 {
		t.Error("Cached ITL doesn't match returned ITL")
	}
}

func TestCollectAggregateMetricsWithCache_CacheHit(t *testing.T) {
	ctx := context.Background()
	cache := NewModelMetricsCache(10 * time.Second)

	// First call - cache miss (populates cache)
	_, ttft1, _, err := CollectAggregateMetricsWithCache(ctx, "test-model", "test-ns", &utils.MockPromAPI{}, cache)
	if err != nil {
		t.Fatalf("First call failed: %v", err)
	}

	// Second call - cache hit (should return cached data)
	_, ttft2, _, err := CollectAggregateMetricsWithCache(ctx, "test-model", "test-ns", &utils.MockPromAPI{}, cache)
	if err != nil {
		t.Errorf("Expected no error on cache hit, got %v", err)
	}

	// Should return same data from cache
	if ttft2 != ttft1 {
		t.Error("Cache hit should return same data as cache miss")
	}

	// Cache size should still be 1
	if cache.Size() != 1 {
		t.Errorf("Expected cache size to remain 1, got %d", cache.Size())
	}
}

func TestCollectAggregateMetricsWithCache_MultipleModels(t *testing.T) {
	ctx := context.Background()
	cache := NewModelMetricsCache(10 * time.Second)

	// Query different models
	_, _, _, err1 := CollectAggregateMetricsWithCache(ctx, "model1", "ns1", &utils.MockPromAPI{}, cache)
	_, _, _, err2 := CollectAggregateMetricsWithCache(ctx, "model2", "ns1", &utils.MockPromAPI{}, cache)
	_, _, _, err3 := CollectAggregateMetricsWithCache(ctx, "model1", "ns2", &utils.MockPromAPI{}, cache)

	if err1 != nil || err2 != nil || err3 != nil {
		t.Error("Expected no errors when querying different models")
	}

	// Should have 3 entries (different model/namespace combinations)
	if cache.Size() != 3 {
		t.Errorf("Expected cache size 3 for different models, got %d", cache.Size())
	}

	// Verify each is cached separately
	_, found1 := cache.Get("model1", "ns1")
	_, found2 := cache.Get("model2", "ns1")
	_, found3 := cache.Get("model1", "ns2")

	if !found1 || !found2 || !found3 {
		t.Error("Expected all model/namespace combinations to be cached separately")
	}
}

func TestCollectAggregateMetricsWithCache_CacheExpiration(t *testing.T) {
	ctx := context.Background()
	cache := NewModelMetricsCache(100 * time.Millisecond) // Very short TTL

	// First call - cache miss
	_, _, _, err := CollectAggregateMetricsWithCache(ctx, "test-model", "test-ns", &utils.MockPromAPI{}, cache)
	if err != nil {
		t.Fatalf("First call failed: %v", err)
	}

	// Verify it's cached
	_, found := cache.Get("test-model", "test-ns")
	if !found {
		t.Fatal("Expected metrics to be cached")
	}

	// Wait for expiration
	time.Sleep(150 * time.Millisecond)

	// Should be expired now
	_, found = cache.Get("test-model", "test-ns")
	if found {
		t.Error("Expected cache entry to be expired")
	}

	// Second call after expiration - should query Prometheus again
	_, _, _, err = CollectAggregateMetricsWithCache(ctx, "test-model", "test-ns", &utils.MockPromAPI{}, cache)
	if err != nil {
		t.Errorf("Expected no error after expiration, got %v", err)
	}

	// Should be cached again
	_, found = cache.Get("test-model", "test-ns")
	if !found {
		t.Error("Expected metrics to be re-cached after expiration")
	}
}

func TestCollectAggregateMetricsWithCache_ErrorHandling(t *testing.T) {
	ctx := context.Background()
	cache := NewModelMetricsCache(10 * time.Second)

	// Build the actual arrival rate query that will fail
	// This matches the first query in CollectAggregateMetrics which returns error immediately
	arrivalQuery := utils.CreateArrivalQuery("test-model", "test-ns")

	// Use mock that returns error for arrival rate query (first query that blocks)
	mockAPI := &utils.MockPromAPI{
		QueryErrors: map[string]error{
			arrivalQuery: fmt.Errorf("prometheus unavailable"),
		},
	}

	load, ttft, itl, err := CollectAggregateMetricsWithCache(ctx, "test-model", "test-ns", mockAPI, cache)

	// Should return error
	if err == nil {
		t.Error("Expected error when Prometheus query fails")
	}

	// Should still cache the failed result (to prevent thundering herd)
	if cache.Size() != 1 {
		t.Errorf("Expected cache to store failed query result, size is %d", cache.Size())
	}

	cached, found := cache.Get("test-model", "test-ns")
	if !found {
		t.Error("Expected failed query to be cached")
	}
	if found && cached.Valid {
		t.Error("Cached failed query should be marked as invalid")
	}

	// Verify returned values are defaults for error case
	if load.ArrivalRate != "" || ttft != "" || itl != "" {
		t.Error("Expected empty values when error occurs")
	}
}

func TestCollectAggregateMetricsWithCache_NamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	cache := NewModelMetricsCache(10 * time.Second)

	// Same model ID in different namespaces
	_, _, _, _ = CollectAggregateMetricsWithCache(ctx, "model1", "namespace1", &utils.MockPromAPI{}, cache)
	_, ttft2, _, _ := CollectAggregateMetricsWithCache(ctx, "model1", "namespace2", &utils.MockPromAPI{}, cache)

	// Should be cached separately
	if cache.Size() != 2 {
		t.Errorf("Expected 2 separate cache entries, got %d", cache.Size())
	}

	// Verify they're independent
	cached1, _ := cache.Get("model1", "namespace1")
	cached2, _ := cache.Get("model1", "namespace2")

	if cached1.Namespace == cached2.Namespace {
		t.Error("Cache entries should have different namespaces")
	}

	// Modifying one namespace's data shouldn't affect the other
	cache.Invalidate("model1", "namespace1")

	_, found1 := cache.Get("model1", "namespace1")
	_, found2 := cache.Get("model1", "namespace2")

	if found1 {
		t.Error("Invalidated entry should not be found")
	}

	if !found2 {
		t.Error("Other namespace entry should still exist")
	}

	// Verify the data is still correct
	if cached2.TTFTAverage != ttft2 {
		t.Error("Namespace2 data should be unchanged after invalidating namespace1")
	}
}

func TestNewModelMetricsCache_InvalidTTL(t *testing.T) {
	// Test with zero TTL
	cache1 := NewModelMetricsCache(0)
	if cache1.ttl != 30*time.Second {
		t.Errorf("Zero TTL should default to 30s, got %v", cache1.ttl)
	}

	// Test with negative TTL
	cache2 := NewModelMetricsCache(-5 * time.Second)
	if cache2.ttl != 30*time.Second {
		t.Errorf("Negative TTL should default to 30s, got %v", cache2.ttl)
	}

	// Test with valid TTL
	cache3 := NewModelMetricsCache(1 * time.Minute)
	if cache3.ttl != 1*time.Minute {
		t.Errorf("Valid TTL should be preserved, got %v", cache3.ttl)
	}
}
