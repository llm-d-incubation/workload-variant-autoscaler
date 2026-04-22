package pipeline

import (
	"context"
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// DefaultLimiter combines an Inventory with an AllocationAlgorithm to constrain
// scaling decisions based on resource availability.
//
// The limiter follows the pipeline pattern:
//  1. Refresh inventory to get latest resource limits from cluster
//  2. Calculate current GPU usage from decisions
//  3. Create allocator with available resources
//  4. Run allocation algorithm to distribute resources
//  5. Update decision metadata (WasLimited, LimitedBy, DecisionSteps)
type DefaultLimiter struct {
	name           string
	inventory      Inventory
	algorithm      AllocationAlgorithm
	metricsEmitter *metrics.MetricsEmitter
}

// NewDefaultLimiter creates a limiter that combines inventory tracking with
// an allocation algorithm.
func NewDefaultLimiter(name string, inventory Inventory, algorithm AllocationAlgorithm) *DefaultLimiter {
	return &DefaultLimiter{
		name:           name,
		inventory:      inventory,
		algorithm:      algorithm,
		metricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// Name returns the limiter identifier for logging/metrics.
func (l *DefaultLimiter) Name() string {
	return l.name
}

// Limit applies resource constraints to scaling decisions.
// Modifies decisions in place - may reduce TargetReplicas based on available resources.
func (l *DefaultLimiter) Limit(ctx context.Context, decisions []*interfaces.VariantDecision) error {
	if len(decisions) == 0 {
		return nil
	}

	// Step 1: Refresh inventory to get latest limits from cluster
	if err := l.inventory.Refresh(ctx); err != nil {
		return fmt.Errorf("failed to refresh inventory: %w", err)
	}

	// Step 1b: Resolve empty/unknown accelerator names using inventory.
	// In homogeneous clusters (single GPU type), map to the real type so
	// usage counting and allocation both use the correct pool. Decisions
	// are mutated in place — the resolved type flows to status and metrics.
	l.resolveUnknownAccelerators(decisions)

	// Step 2: Calculate current GPU usage from decisions
	usedByType := l.calculateUsedGPUs(decisions)
	l.inventory.SetUsed(usedByType)

	// Step 3: Create allocator with available resources
	allocator := l.inventory.CreateAllocator(ctx)

	// Step 4: Run allocation algorithm to distribute resources
	if err := l.algorithm.Allocate(ctx, decisions, allocator); err != nil {
		return fmt.Errorf("allocation algorithm failed: %w", err)
	}

	// Step 5: Update decision metadata
	l.updateDecisionMetadata(decisions)

	return nil
}

// resolveUnknownAccelerators maps empty or "unknown" accelerator names to the
// real GPU type when the inventory has exactly one type (homogeneous cluster).
// This must run before calculateUsedGPUs so existing replicas are counted
// against the correct pool, and before TryAllocate so new allocations use it.
func (l *DefaultLimiter) resolveUnknownAccelerators(decisions []*interfaces.VariantDecision) {
	pools := l.inventory.GetResourcePools()
	if len(pools) != 1 {
		return // heterogeneous or empty — can't resolve
	}
	var realType string
	for t := range pools {
		realType = t
	}
	for _, d := range decisions {
		if !constants.IsAcceleratorResolved(d.AcceleratorName) {
			d.AcceleratorName = realType
		}
	}
}

// calculateUsedGPUs computes current GPU usage per accelerator type.
// Uses CurrentReplicas * GPUsPerReplica for each decision.
func (l *DefaultLimiter) calculateUsedGPUs(decisions []*interfaces.VariantDecision) map[string]int {
	usedByType := make(map[string]int)
	for _, d := range decisions {
		if d.AcceleratorName == "" {
			continue
		}
		usedByType[d.AcceleratorName] += d.CurrentReplicas * d.GPUsPerReplica
	}
	return usedByType
}

// updateDecisionMetadata sets LimitedBy and adds DecisionSteps.
// Note: WasLimited is set by the algorithm during allocation.
func (l *DefaultLimiter) updateDecisionMetadata(decisions []*interfaces.VariantDecision) {
	for _, d := range decisions {
		// If the algorithm marked the decision as limited, set LimitedBy
		if d.WasLimited {
			d.LimitedBy = l.name
			l.metricsEmitter.RecordDecisionsLimitedTotalMetric(d.VariantName, d.Namespace, d.LimitedBy)
		}

		// Add decision step for observability
		reason := l.buildStepReason(d)
		d.AddDecisionStep(l.name, reason, d.WasLimited)
	}
}

// buildStepReason creates a human-readable reason for the decision step.
func (l *DefaultLimiter) buildStepReason(d *interfaces.VariantDecision) string {
	replicaChange := d.TargetReplicas - d.CurrentReplicas

	if replicaChange <= 0 {
		return fmt.Sprintf("no scale-up (target=%d, current=%d)", d.TargetReplicas, d.CurrentReplicas)
	}
	if d.WasLimited {
		return fmt.Sprintf("limited: allocated %d GPUs for +%d replicas", d.GPUsAllocated, replicaChange)
	}
	return fmt.Sprintf("allocated %d GPUs for +%d replicas", d.GPUsAllocated, replicaChange)
}

// ComputeConstraints refreshes the inventory and returns per-type resource availability.
// This is the V2 path: expose constraints for the optimizer instead of modifying
// decisions directly (which is what Limit() does for the V1 path).
func (l *DefaultLimiter) ComputeConstraints(ctx context.Context, currentUsage map[string]int) (*ResourceConstraints, error) {
	// Step 1: Refresh inventory (same as Limit step 1)
	if err := l.inventory.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("failed to refresh inventory: %w", err)
	}

	// Step 2: Set current usage (same as Limit step 2)
	l.inventory.SetUsed(currentUsage)

	// Step 3: Expose per-type availability
	pools := l.inventory.GetResourcePools()

	rc := &ResourceConstraints{
		ProviderName: l.name,
		Pools:        pools,
		TotalLimit:   l.inventory.TotalLimit(),
		TotalUsed:    l.inventory.TotalUsed(),
		TotalAvail:   l.inventory.TotalAvailable(),
	}
	return rc, nil
}

// Ensure DefaultLimiter implements Limiter interface
var _ Limiter = (*DefaultLimiter)(nil)

// Ensure DefaultLimiter implements ConstraintProvider interface
var _ ConstraintProvider = (*DefaultLimiter)(nil)
