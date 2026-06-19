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
// environments, use GreedyByScoreOptimizer instead.
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
		satEntry := saturationEntry(req.AnalyzerResults)
		if satEntry == nil {
			continue
		}

		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(satEntry.VariantCapacities)
		targets := initTargets(req.VariantStates)

		// Unified dispatch: one path for all models via (model, role) math.
		// Non-disaggregated uses synthetic "both" role; disaggregated uses actual roles.
		s := req.AnalyzerResults
		roles, ps := initRoleState(s)
		if anyRoleNeedsScaleUp(ps, roles) {
			allocateForModelPaired(ctx, s, satEntry.VariantCapacities, stateMap, nil, targets,
				costGreedyRolePick, ps, roles)
		} else {
			scaleDownRoleIterated(ctx, s, satEntry.VariantCapacities, targets, stateMap)
		}

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "cost-aware")
		logger.V(logging.DEBUG).Info("Cost-aware optimizer decisions",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// costGreedyRolePick is a RolePickFn that picks the cheapest-by-cost-efficiency
// variant in the given role. For "both" (non-disaggregated), all variants are
// eligible. For role-tagged roles, only variants with a matching Role are picked.
func costGreedyRolePick(
	role string,
	_ []NamedAnalyzerResult,
	variants []interfaces.VariantCapacity,
	stateMap map[string]interfaces.VariantReplicaState,
	_ map[string]int,
	targets map[string]int,
) (string, int) {
	roleVCs := variantsForRole(variants, role)
	for _, vc := range sortByCostEfficiencyAsc(roleVCs) {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		state := stateMap[vc.VariantName]
		if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
			headroom := *state.MaxReplicas - targets[vc.VariantName]
			if headroom <= 0 {
				continue
			}
			return vc.VariantName, headroom
		}
		return vc.VariantName, math.MaxInt
	}
	return "", 0
}

// scaleDownVariantSet sheds replicas from sortedVariants (PRE-SORTED cost-desc,
// cheapest last). minReplicas floor and cheapest-at-1 protection are enforced
// here. maxRemovable returns how many replicas of vc the caller permits to remove;
// onRemove is invoked after committing n so the caller can update its spare bookkeeping.
func scaleDownVariantSet(
	ctx context.Context,
	sortedVariants []interfaces.VariantCapacity,
	targets map[string]int,
	states map[string]interfaces.VariantReplicaState,
	maxRemovable func(vc interfaces.VariantCapacity) int,
	onRemove func(vc interfaces.VariantCapacity, n int),
) {
	logger := ctrl.LoggerFrom(ctx)
	for i, vc := range sortedVariants {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		current := targets[vc.VariantName]
		minReplicas := 0
		if states != nil {
			if st, ok := states[vc.VariantName]; ok && st.MinReplicas != nil {
				minReplicas = *st.MinReplicas
			}
		}
		removable := current - minReplicas
		if removable <= 0 {
			continue
		}
		n := maxRemovable(vc)
		if n > removable {
			n = removable
		}
		// cheapest-at-1: the last (cheapest) variant is protected at 1 only when no
		// more-expensive variant still holds replicas (#1237's positional rule).
		if i == len(sortedVariants)-1 && current-n < 1 && !anyHasReplicas(sortedVariants[:i], targets) {
			n = current - 1
		}
		if n <= 0 {
			continue
		}
		targets[vc.VariantName] = current - n
		onRemove(vc, n)
		logger.V(logging.DEBUG).Info("scale-down: removed replicas",
			"variant", vc.VariantName, "removed", n, "cost", vc.Cost)
	}
}

// sortVariantsForScaleDown orders a role's variants for cost-greedy scale-down:
//  1. Cost descending — shed the most expensive first.
//  2. Tie: score-weighted per-replica capacity ascending — Σ_i Score_i·PRC_i[v].
//  3. Tie: variant name ascending — full determinism.
//
// With a single analyzer (Score=1) this reduces to Cost-desc then PRC-asc, i.e.
// #1237's existing tie-break.
func sortVariantsForScaleDown(s []NamedAnalyzerResult, roleVCs []interfaces.VariantCapacity) []interfaces.VariantCapacity {
	weighted := func(name string) float64 {
		sum := 0.0
		for _, e := range s {
			if e.Result == nil {
				continue
			}
			sum += e.Score * prcForVariant(e.Result, name)
		}
		return sum
	}
	out := append([]interfaces.VariantCapacity(nil), roleVCs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		wi, wj := weighted(out[i].VariantName), weighted(out[j].VariantName)
		if wi != wj {
			return wi < wj
		}
		return out[i].VariantName < out[j].VariantName
	})
	return out
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
		var decisionReason interfaces.DecisionReason
		var detailedReason string
		switch {
		case target > state.CurrentReplicas:
			action = interfaces.ActionScaleUp
			decisionReason = interfaces.DecisionReasonV2
			detailedReason = fmt.Sprintf("%s (optimizer: %s)", string(decisionReason), optimizerName)
		case target < state.CurrentReplicas:
			action = interfaces.ActionScaleDown
			decisionReason = interfaces.DecisionReasonV2
			detailedReason = fmt.Sprintf("%s (optimizer: %s)", string(decisionReason), optimizerName)
		default:
			action = interfaces.ActionNoChange
			decisionReason = interfaces.DecisionReasonV2
			detailedReason = string(decisionReason)
		}

		decision := interfaces.VariantDecision{
			VariantName:     name,
			ModelID:         req.ModelID,
			Namespace:       req.Namespace,
			AcceleratorName: vc.AcceleratorName,
			Cost:            vc.Cost,
			Role:            state.Role,
			CurrentReplicas: state.CurrentReplicas,
			TargetReplicas:  target,
			MinReplicas:     state.MinReplicas,
			MaxReplicas:     state.MaxReplicas,
		}
		// SetDecisionReason is the single place that sets d.Action (avoids a
		// redundant Action assignment in the struct literal above).
		decision.SetDecisionReason(action, decisionReason, detailedReason)
		decisions = append(decisions, decision)
	}
	return decisions
}

// mergeConstraints combines GPU budget constraints from multiple providers.
// Used by GreedyByScoreOptimizer; lives here since CostAwareOptimizer owns the shared helpers.
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

// scaleDownRoleIterated removes replicas role-by-role using the generalized
// scaleDownVariantSet primitive. Roles are sorted for determinism.
// Arity-1 (roles=["both"]) handles non-disaggregated models.
func scaleDownRoleIterated(
	ctx context.Context,
	s []NamedAnalyzerResult,
	variants []interfaces.VariantCapacity,
	targets map[string]int,
	stateMap ...map[string]interfaces.VariantReplicaState,
) {
	var states map[string]interfaces.VariantReplicaState
	if len(stateMap) > 0 {
		states = stateMap[0]
	}
	rolesSet := make(map[string]struct{})
	for _, vc := range variants {
		role := vc.Role
		if role == "" {
			role = interfaces.RoleBoth
		}
		rolesSet[role] = struct{}{}
	}
	roles := make([]string, 0, len(rolesSet))
	for role := range rolesSet {
		roles = append(roles, role)
	}
	sort.Strings(roles)

	for _, role := range roles {
		if !needsScaleDownForRole(s, role) {
			continue
		}
		roleVCs := variantsForRole(variants, role)
		if len(roleVCs) == 0 {
			continue
		}
		sorted := sortVariantsForScaleDown(s, roleVCs)
		scaleDownVariantSet(ctx, sorted, targets, states,
			func(vc interfaces.VariantCapacity) int {
				return safeRemovalReplicasForRole(s, vc.VariantName, role)
			},
			func(vc interfaces.VariantCapacity, n int) {
				applyDeallocationForRole(s, vc.VariantName, role, n)
			},
		)
	}
}

// Ensure CostAwareOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*CostAwareOptimizer)(nil)
