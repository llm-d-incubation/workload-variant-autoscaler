package benchmark

import "math"

// BenchmarkResults holds the collected metrics from a benchmark scenario.
// This struct is shared across all benchmark scenarios.
type BenchmarkResults struct {
	ScaleUpTimeSec     float64 `json:"scaleUpTimeSec"`
	ScaleDownTimeSec   float64 `json:"scaleDownTimeSec"`
	MaxReplicas        int32   `json:"maxReplicas"`
	AvgKVCacheUsage    float64 `json:"avgKVCacheUsage"`
	AvgQueueDepth      float64 `json:"avgQueueDepth"`
	ReplicaOscillation float64 `json:"replicaOscillation"`
	TotalDurationSec   float64 `json:"totalDurationSec"`
	GrafanaSnapshotURL string  `json:"grafanaSnapshotUrl,omitempty"`
}

// stddev computes the standard deviation of a float64 slice.
func stddev(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))

	var variance float64
	for _, v := range values {
		diff := v - mean
		variance += diff * diff
	}
	variance /= float64(len(values))

	return math.Sqrt(variance)
}
