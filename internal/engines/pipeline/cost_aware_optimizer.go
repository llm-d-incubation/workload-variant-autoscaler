package pipeline

import (
	"context"
	"fmt"
	"math"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// CostAwareOptimizer is a per-model optimizer that minimizes total cost while
// meeting capacity requirements. It processes each model independently:
//
//   - Scale-up: adds replicas to the most cost-efficient variant (lowest cost / perReplicaCapacity)
//   - Scale-down: removes replicas from the most expensive variant (highest absolute cost)
//   - Only the cheapest variant is protected at >=1 replica; others can scale to 0
//   - Variants with pending replicas are skipped for scale-up
//
// This optimizer ignores ResourceConstraints (unlimited mode). For GPU-limited
// environments, use GreedyBySaturationOptimizer instead.
type CostAwareOptimizer struct{}

// NewCostAwareOptimizer creates a new CostAwareOptimizer.
func NewCostAwareOptimizer() *CostAwareOptimizer {
	return &CostAwareOptimizer{}
}

// Name returns the optimizer identifier.
func (o *CostAwareOptimizer) Name() string {
	return "cost-aware"
}

// Optimize produces VariantDecisions for all models.
// Constraints are ignored in unlimited mode (CostAwareOptimizer).
func (o *CostAwareOptimizer) Optimize(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx).WithName(o.Name())
	var allDecisions []interfaces.VariantDecision

	for _, req := range requests {
		if req.Result == nil {
			continue
		}

		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(req.Result.VariantCapacities)
		targets := initTargets(req.VariantStates)

		if req.Result.RequiredCapacity > 0 {
			costAwareScaleUp(ctx, req.Result, targets)
		} else if req.Result.SpareCapacity > 0 {
			costAwareScaleDown(ctx, req.Result, targets)
		}

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "cost-aware")
		logger.V(logging.DEBUG).Info("Cost-aware optimizer decisions",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// costAwareScaleUp adds replicas to the most cost-efficient variant.
// Sorts by cost-efficiency (cost/perReplicaCapacity) ascending, picks first eligible.
// Pending replicas are not skipped because the analyzer already accounts for their
// capacity in the supply calculation — if RequiredCapacity > 0, demand exceeds total
// supply including pending.
func costAwareScaleUp(
	ctx context.Context,
	result *interfaces.AnalyzerResult,
	targets map[string]int,
) {
	logger := ctrl.LoggerFrom(ctx)

	sorted := sortByCostEfficiencyAsc(result.VariantCapacities)
	remaining := result.RequiredCapacity

	for _, vc := range sorted {
		if remaining <= 0 {
			break
		}
		if vc.PerReplicaCapacity <= 0 {
			continue
		}

		replicasNeeded := int(math.Ceil(remaining / vc.PerReplicaCapacity))
		targets[vc.VariantName] = targets[vc.VariantName] + replicasNeeded
		remaining -= float64(replicasNeeded) * vc.PerReplicaCapacity

		logger.V(logging.DEBUG).Info("Scale-up allocation",
			"variant", vc.VariantName,
			"added", replicasNeeded,
			"costEfficiency", costEfficiency(vc))
	}
}

// costAwareScaleDown removes replicas from the most expensive variant.
// Sorts by absolute cost descending, removes from most expensive first.
// The cheapest variant is protected at min 1 replica only when no other variant
// has replicas — this prevents scale-down deadlocks where the expensive variant's
// per-replica capacity exceeds spare but cheaper replicas could be removed.
func costAwareScaleDown(
	ctx context.Context,
	result *interfaces.AnalyzerResult,
	targets map[string]int,
) {
	logger := ctrl.LoggerFrom(ctx)

	sorted := sortByCostDesc(result.VariantCapacities)
	cheapest := findCheapestVariant(result.VariantCapacities)
	remaining := result.SpareCapacity

	for _, vc := range sorted {
		if remaining <= 0 {
			break
		}
		if vc.PerReplicaCapacity <= 0 {
			continue
		}

		current := targets[vc.VariantName]
		minReplicas := 0
		if vc.VariantName == cheapest {
			// Protect cheapest at 1 only if it's the last variant with replicas
			otherHasReplicas := false
			for name, t := range targets {
				if name != cheapest && t > 0 {
					otherHasReplicas = true
					break
				}
			}
			if !otherHasReplicas {
				minReplicas = 1
			}
		}

		removable := current - minReplicas
		if removable <= 0 {
			continue
		}

		replicasToRemove := int(math.Floor(remaining / vc.PerReplicaCapacity))
		if replicasToRemove > removable {
			replicasToRemove = removable
		}
		if replicasToRemove <= 0 {
			continue
		}

		targets[vc.VariantName] = current - replicasToRemove
		remaining -= float64(replicasToRemove) * vc.PerReplicaCapacity

		logger.V(logging.DEBUG).Info("Scale-down allocation",
			"variant", vc.VariantName,
			"removed", replicasToRemove,
			"cost", vc.Cost)
	}
}

// buildStateMap creates a lookup map from variant name to VariantReplicaState.
func buildStateMap(states []interfaces.VariantReplicaState) map[string]interfaces.VariantReplicaState {
	m := make(map[string]interfaces.VariantReplicaState, len(states))
	for _, s := range states {
		m[s.VariantName] = s
	}
	return m
}

// buildCapacityMap creates a lookup map from variant name to VariantCapacity.
func buildCapacityMap(capacities []interfaces.VariantCapacity) map[string]interfaces.VariantCapacity {
	m := make(map[string]interfaces.VariantCapacity, len(capacities))
	for _, vc := range capacities {
		m[vc.VariantName] = vc
	}
	return m
}

// initTargets creates initial targets from current replica counts.
func initTargets(states []interfaces.VariantReplicaState) map[string]int {
	targets := make(map[string]int, len(states))
	for _, s := range states {
		targets[s.VariantName] = s.CurrentReplicas
	}
	return targets
}

// findCheapestVariant returns the variant name with the lowest cost.
func findCheapestVariant(capacities []interfaces.VariantCapacity) string {
	cheapest := ""
	minCost := math.MaxFloat64
	for _, vc := range capacities {
		if vc.Cost < minCost {
			minCost = vc.Cost
			cheapest = vc.VariantName
		}
	}
	return cheapest
}

// sortByCostEfficiencyAsc returns variants sorted by cost/perReplicaCapacity ascending.
func sortByCostEfficiencyAsc(capacities []interfaces.VariantCapacity) []interfaces.VariantCapacity {
	sorted := make([]interfaces.VariantCapacity, len(capacities))
	copy(sorted, capacities)
	sort.Slice(sorted, func(i, j int) bool {
		return costEfficiency(sorted[i]) < costEfficiency(sorted[j])
	})
	return sorted
}

// sortByCostDesc returns variants sorted by absolute cost descending.
func sortByCostDesc(capacities []interfaces.VariantCapacity) []interfaces.VariantCapacity {
	sorted := make([]interfaces.VariantCapacity, len(capacities))
	copy(sorted, capacities)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Cost > sorted[j].Cost
	})
	return sorted
}

// costEfficiency returns the cost per unit of capacity.
func costEfficiency(vc interfaces.VariantCapacity) float64 {
	if vc.PerReplicaCapacity <= 0 {
		return math.MaxFloat64
	}
	return vc.Cost / vc.PerReplicaCapacity
}

// buildDecisionsWithOptimizer converts targets map into VariantDecision slice.
// optimizerName is included in reason strings for observability.
func buildDecisionsWithOptimizer(
	req ModelScalingRequest,
	stateMap map[string]interfaces.VariantReplicaState,
	vcMap map[string]interfaces.VariantCapacity,
	targets map[string]int,
	optimizerName string,
) []interfaces.VariantDecision {
	decisions := make([]interfaces.VariantDecision, 0, len(targets))
	for name, target := range targets {
		state := stateMap[name]
		vc := vcMap[name]

		var action interfaces.SaturationAction
		var reason string
		switch {
		case target > state.CurrentReplicas:
			action = interfaces.ActionScaleUp
			reason = fmt.Sprintf("V2 scale-up (optimizer: %s, required: %.0f)", optimizerName, req.Result.RequiredCapacity)
		case target < state.CurrentReplicas:
			action = interfaces.ActionScaleDown
			reason = fmt.Sprintf("V2 scale-down (optimizer: %s, spare: %.0f)", optimizerName, req.Result.SpareCapacity)
		default:
			action = interfaces.ActionNoChange
			reason = "V2 steady state"
		}

		decisions = append(decisions, interfaces.VariantDecision{
			VariantName:     name,
			ModelID:         req.ModelID,
			Namespace:       req.Namespace,
			AcceleratorName: vc.AcceleratorName,
			Cost:            vc.Cost,
			CurrentReplicas: state.CurrentReplicas,
			TargetReplicas:  target,
			Action:          action,
			Reason:          reason,
		})
	}
	return decisions
}

// mergeConstraints combines constraints from multiple providers.
// Currently unused in CostAwareOptimizer but available for limited mode.
func mergeConstraints(constraints []*ResourceConstraints) map[string]int {
	merged := make(map[string]int)
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for accType, pool := range c.Pools {
			if existing, ok := merged[accType]; !ok || pool.Available() < existing {
				merged[accType] = pool.Available()
			}
		}
	}
	return merged
}

// Ensure CostAwareOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*CostAwareOptimizer)(nil)
