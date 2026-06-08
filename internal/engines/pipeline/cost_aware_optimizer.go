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

const (
	// CostAwareOptimizerName is the identifier for the cost-aware optimizer
	CostAwareOptimizerName = "cost-aware"
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
// environments, use GreedyByScoreOptimizer instead.
type CostAwareOptimizer struct{}

// NewCostAwareOptimizer creates a new CostAwareOptimizer.
func NewCostAwareOptimizer() *CostAwareOptimizer {
	return &CostAwareOptimizer{}
}

// Name returns the optimizer identifier.
func (o *CostAwareOptimizer) Name() string {
	return CostAwareOptimizerName
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
			costAwareScaleUp(ctx, req.Result, targets, stateMap)
		} else if req.Result.SpareCapacity > 0 {
			costAwareScaleDown(ctx, req.Result, targets, stateMap)
		}

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, CostAwareOptimizerName)
		logger.V(logging.DEBUG).Info("Cost-aware optimizer decisions",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// costAwareScaleUp adds replicas to the most cost-efficient variant.
// Sorts by cost-efficiency (cost/perReplicaCapacity) ascending, picks first eligible.
// Respects maxReplicas per variant — if a variant hits its cap, remaining capacity
// spills over to the next variant.
func costAwareScaleUp(
	ctx context.Context,
	result *interfaces.AnalyzerResult,
	targets map[string]int,
	stateMap map[string]interfaces.VariantReplicaState,
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

		// Cap by maxReplicas if set
		state := stateMap[vc.VariantName]
		if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
			maxAdd := *state.MaxReplicas - targets[vc.VariantName]
			if maxAdd <= 0 {
				continue // already at max
			}
			if replicasNeeded > maxAdd {
				replicasNeeded = maxAdd
			}
		}

		targets[vc.VariantName] += replicasNeeded
		remaining -= float64(replicasNeeded) * vc.PerReplicaCapacity

		logger.V(logging.DEBUG).Info("Scale-up allocation",
			"variant", vc.VariantName,
			"added", replicasNeeded,
			"costEfficiency", costEfficiency(vc))
	}
}

// costAwareScaleDown removes replicas to shed spare capacity, most-expensive
// variant first, respecting minReplicas.
//
// For disaggregated (prefill/decode) models it sheds each role independently
// against that role's own spare. Prefill and decode capacity are not fungible, so
// removing by the model-level SpareCapacity — which aggregates all roles — would
// let a role with slack drive removal of replicas from a saturated role (e.g.
// trimming prefill because decode has spare). RoleCapacities is non-nil only when
// disaggregation is active; non-disaggregated models keep the model-level shed.
func costAwareScaleDown(
	ctx context.Context,
	result *interfaces.AnalyzerResult,
	targets map[string]int,
	stateMap ...map[string]interfaces.VariantReplicaState,
) {
	var states map[string]interfaces.VariantReplicaState
	if len(stateMap) > 0 {
		states = stateMap[0]
	}

	if len(result.RoleCapacities) > 0 {
		// Each role owns a disjoint set of variants and sheds against its own
		// spare, so the map's iteration order does not affect the outcome.
		// Assumes RoleCapacities keys partition VariantCapacities by role
		// (one role per variant; no overlap); mixed-role configurations would
		// break disjointness.
		for role, rc := range result.RoleCapacities {
			if rc.SpareCapacity <= 0 {
				continue // saturated or under-supplied role — never trim it
			}
			scaleDownVariantSet(ctx, variantsForRole(result.VariantCapacities, role), rc.SpareCapacity, targets, states)
		}
		return
	}

	scaleDownVariantSet(ctx, result.VariantCapacities, result.SpareCapacity, targets, states)
}

// scaleDownVariantSet removes replicas from the given variant set, most-expensive
// first, until `spare` capacity is shed or each variant's minReplicas floor is
// reached. The cheapest variant — last in the cost-descending order — is protected
// at one replica when it would otherwise be the last variant with replicas in the
// set, preventing a scale-to-zero deadlock.
func scaleDownVariantSet(
	ctx context.Context,
	variants []interfaces.VariantCapacity,
	spare float64,
	targets map[string]int,
	states map[string]interfaces.VariantReplicaState,
) {
	logger := ctrl.LoggerFrom(ctx)

	sorted := sortByCostDesc(variants)
	remaining := spare

	for i, vc := range sorted {
		if remaining <= 0 {
			break
		}
		if vc.PerReplicaCapacity <= 0 {
			continue
		}

		current := targets[vc.VariantName]

		// Annotation floor caps removal.
		minReplicas := 0
		if states != nil {
			if state, ok := states[vc.VariantName]; ok && state.MinReplicas != nil {
				minReplicas = *state.MinReplicas
			}
		}
		removable := current - minReplicas
		if removable <= 0 {
			continue
		}

		toRemove := int(math.Floor(remaining / vc.PerReplicaCapacity))
		if toRemove > removable {
			toRemove = removable
		}

		// Protect the cheapest variant (last in cost-descending order) at one
		// replica when removing toRemove would drop it below one and no
		// more-expensive variant still holds replicas — i.e. it is the last with
		// replicas in the set. When minReplicas >= 1, removable <= current-1 so
		// toRemove <= current-1 and current-toRemove >= 1 already, so this clause
		// never triggers.
		if i == len(sorted)-1 && current-toRemove < 1 && !anyHasReplicas(sorted[:i], targets) {
			toRemove = current - 1
		}
		if toRemove <= 0 {
			continue
		}

		targets[vc.VariantName] = current - toRemove
		remaining -= float64(toRemove) * vc.PerReplicaCapacity

		logger.V(logging.DEBUG).Info("Scale-down allocation",
			"variant", vc.VariantName,
			"removed", toRemove,
			"cost", vc.Cost)
	}
}

// anyHasReplicas reports whether any of the given variants has a positive target.
func anyHasReplicas(variants []interfaces.VariantCapacity, targets map[string]int) bool {
	for _, vc := range variants {
		if targets[vc.VariantName] > 0 {
			return true
		}
	}
	return false
}

// variantsForRole returns the capacities whose role matches exactly (an empty
// role is treated as "both"). Unlike filterVariantCapacitiesByRole, which treats
// "both" as a wildcard for scale-up allocation, this matches the role exactly so
// per-role scale-down operates on disjoint variant sets.
func variantsForRole(capacities []interfaces.VariantCapacity, role string) []interfaces.VariantCapacity {
	out := make([]interfaces.VariantCapacity, 0, len(capacities))
	for _, vc := range capacities {
		r := vc.Role
		if r == "" {
			r = interfaces.RoleBoth
		}
		if r == role {
			out = append(out, vc)
		}
	}
	return out
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

// sortByCostEfficiencyAsc returns variants sorted by cost/perReplicaCapacity ascending.
func sortByCostEfficiencyAsc(capacities []interfaces.VariantCapacity) []interfaces.VariantCapacity {
	sorted := make([]interfaces.VariantCapacity, len(capacities))
	copy(sorted, capacities)
	sort.Slice(sorted, func(i, j int) bool {
		return costEfficiency(sorted[i]) < costEfficiency(sorted[j])
	})
	return sorted
}

// sortByCostDesc returns variants sorted by absolute cost descending. Equal-cost
// variants are tie-broken by per-replica capacity ascending, so the highest-PRC
// variant at the cheapest cost tier lands last — the deterministic slot the
// scale-down protection keeps at one replica (prefer keeping the more capable
// replica among equal-cost variants).
func sortByCostDesc(capacities []interfaces.VariantCapacity) []interfaces.VariantCapacity {
	sorted := make([]interfaces.VariantCapacity, len(capacities))
	copy(sorted, capacities)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Cost != sorted[j].Cost {
			return sorted[i].Cost > sorted[j].Cost
		}
		return sorted[i].PerReplicaCapacity < sorted[j].PerReplicaCapacity
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
			VariantName:      name,
			ModelID:          req.ModelID,
			Namespace:        req.Namespace,
			AcceleratorName:  vc.AcceleratorName,
			Cost:             vc.Cost,
			Role:             state.Role,
			CurrentReplicas:  state.CurrentReplicas,
			TargetReplicas:   target,
			Action:           action,
			Reason:           reason,
			MinReplicas:      state.MinReplicas,
			MaxReplicas:      state.MaxReplicas,
			Utilization:      vc.Utilization,
			SpareCapacity:    1.0 - vc.Utilization,
			RequiredCapacity: req.Result.RequiredCapacity,
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
