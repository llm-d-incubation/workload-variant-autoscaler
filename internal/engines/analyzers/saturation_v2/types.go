package saturation_v2

// learnedFromLive indicates a capacity record was derived from live metrics.
const learnedFromLive = "live"

// ReplicaCapacity holds the per-replica capacity breakdown computed by
// the V2 saturation analyzer. It is internal to the analyzer and not
// part of the public interfaces package.
type ReplicaCapacity struct {
	PodName               string
	VariantName           string
	AcceleratorName       string
	TokensInUse           int64
	TotalKvCapacityTokens int64
	MemoryBoundCapacity   int64 // k1: KV-cache-limited capacity
	ComputeBoundCapacity  int64 // k2: compute/scheduling-limited capacity
	EffectiveCapacity     int64 // min(k1, k2)
	IsSaturated           bool
	ReplicaDemand         int64 // tokensInUse + queueLength * avgInputTokens
}

// classifyOutputLength returns a workload bucket name based on average
// output token length. The buckets are used to key compute-capacity (k2)
// history, since k2 depends heavily on generation length.
//
// Buckets:
//
//	"short"  — avgOutput in [0, 100)
//	"medium" — avgOutput in [100, 500)
//	"long"   — avgOutput >= 500
func classifyOutputLength(avgOutputTokens float64) string {
	switch {
	case avgOutputTokens < ShortOutputThreshold:
		return "short"
	case avgOutputTokens < MediumOutputThreshold:
		return "medium"
	default:
		return "long"
	}
}
