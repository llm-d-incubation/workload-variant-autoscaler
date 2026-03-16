package interfaces

import (
	"context"
	"time"
)

// Analyzer is the common interface for all scaling analyzers.
// Each analyzer observes workload metrics and produces capacity signals
// (required_capacity, spare_capacity) that the engine combines into
// scaling decisions. Analyzers do NOT build scaling plans — the engine does.
//
// Saturation Analyzer V2 is the first implementation of this interface.
// Future analyzers (throughput, SLO) will implement the same interface.
type Analyzer interface {
	// Name returns the analyzer's identifier (e.g., "saturation", "throughput", "slo").
	Name() string

	// Analyze computes capacity signals for a model across all its variants.
	// Returns per-variant capacity breakdown and model-level scaling signals.
	Analyze(ctx context.Context, input AnalyzerInput) (*AnalyzerResult, error)
}

// AnalyzerConfig is the interface for analyzer-specific configuration.
// Each analyzer defines its own config type that implements this interface.
type AnalyzerConfig interface {
	// GetAnalyzerName returns the name of the analyzer this config is for.
	GetAnalyzerName() string
}

// AnalyzerInput is the common input provided to all analyzers.
type AnalyzerInput struct {
	ModelID        string
	Namespace      string
	ReplicaMetrics []ReplicaMetrics
	VariantStates  []VariantReplicaState
	Config         AnalyzerConfig

	// SchedulerQueue holds model-level queue metrics from the llm-d inference
	// scheduler flow control layer. These represent requests queued upstream
	// before reaching any vLLM pod and contribute to demand estimation.
	// Nil when flow control is disabled or metrics are unavailable.
	SchedulerQueue *SchedulerQueueMetrics
}

// SchedulerQueueMetrics holds model-level queue metrics from the llm-d
// inference scheduler flow control layer (inference_extension_flow_control_*).
// These are model-scoped, not per-pod, since the scheduler queues requests
// before routing them to a specific backend pod.
//
// TODO(#2309): The upstream metrics lack a namespace label. If the same model
// name exists in different namespaces, these values may include cross-namespace
// data. Once the upstream adds a namespace label, queries should filter by it.
type SchedulerQueueMetrics struct {
	// QueueSize is the number of requests currently queued in the
	// scheduler's flow control layer for this model.
	// Sourced from inference_extension_flow_control_queue_size.
	QueueSize int64

	// QueueBytes is the total bytes of request bodies currently queued
	// in the scheduler's flow control layer for this model.
	// Sourced from inference_extension_flow_control_queue_bytes.
	// Approximate token count: QueueBytes / BytesPerToken.
	QueueBytes int64
}

// RoleCapacity holds per-role capacity aggregation for P/D disaggregated models.
type RoleCapacity struct {
	Role             string
	TotalSupply      float64
	TotalDemand      float64
	RequiredCapacity float64
	SpareCapacity    float64
}

// AnalyzerResult is the common output produced by all analyzers.
// The engine consumes these results to build scaling plans.
type AnalyzerResult struct {
	// AnalyzerName identifies which analyzer produced this result.
	AnalyzerName string

	ModelID    string
	Namespace  string
	AnalyzedAt time.Time

	// Per-variant capacity breakdown (in analyzer-specific units).
	VariantCapacities []VariantCapacity

	// Model-level aggregates (in analyzer-specific units).
	TotalSupply float64 // Sum of all variant TotalCapacity
	TotalDemand float64 // Sum of all variant TotalDemand
	Utilization float64 // TotalDemand / TotalSupply (0.0-1.0)

	// Scaling signals — the core output consumed by the engine.
	// These are the primary inputs for the engine's signal combination logic.
	RequiredCapacity float64 // >0 means scale-up needed (demand/threshold - supply)
	SpareCapacity    float64 // >0 means scale-down possible (supply - demand/boundary)

	// Score is the composite priority score for this model.
	// Computed as: priority * sum(requiredCapacity_i * analyzerScore_i)
	// Used by GreedyByScoreOptimizer for fair-share ordering.
	Score float64

	// RoleCapacities holds per-role capacity aggregation for P/D disaggregated models.
	// nil when no disaggregation is active (all variants are role "both").
	RoleCapacities map[string]RoleCapacity
}

// VariantCapacity holds per-variant capacity data in analyzer-specific units.
// For saturation: units are tokens. For throughput: tokens/sec. For SLO: latency-constrained capacity.
type VariantCapacity struct {
	VariantName     string
	AcceleratorName string
	Cost            float64
	Role            string // "prefill", "decode", "both", "" (empty = non-disaggregated)

	ReplicaCount    int
	PendingReplicas int

	// PerReplicaCapacity is the representative capacity per replica.
	// For saturation V2: median(effectiveCapacity) in tokens across ready replicas.
	PerReplicaCapacity float64

	// TotalCapacity is ReplicaCount × PerReplicaCapacity.
	TotalCapacity float64

	// TotalDemand is the aggregate demand on this variant.
	TotalDemand float64

	// Utilization is TotalDemand / TotalCapacity (0.0-1.0).
	Utilization float64
}
