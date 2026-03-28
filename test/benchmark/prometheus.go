package benchmark

import (
	"context"
	"fmt"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// QueryRangeAvg executes a Prometheus range query and returns the average value
// across all samples in the result matrix.
func QueryRangeAvg(api promv1.API, query string, start, end time.Time, step time.Duration) (float64, error) {
	r := promv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, warnings, err := api.QueryRange(ctx, query, r)
	if err != nil {
		return 0, fmt.Errorf("range query %q failed: %w", query, err)
	}
	if len(warnings) > 0 {
		fmt.Printf("Prometheus warnings for %q: %v\n", query, warnings)
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return 0, fmt.Errorf("unexpected result type %T for %q", result, query)
	}

	var sum float64
	var count int
	for _, stream := range matrix {
		for _, sample := range stream.Values {
			sum += float64(sample.Value)
			count++
		}
	}

	if count == 0 {
		return 0, fmt.Errorf("no samples returned for %q", query)
	}

	return sum / float64(count), nil
}
