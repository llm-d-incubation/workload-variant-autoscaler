package pipeline

import (
	"context"
	"math"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// GreedyByScoreOptimizer is a multi-model optimizer for GPU-constrained
// environments. It uses iterative mean-based fair-sharing to distribute scarce
// GPUs across competing models, ordered by composite score
// (priority * sum(requiredCapacity_i * analyzerScore_i)).
//
// Key differences from CostAwareOptimizer:
//   - Respects ResourceConstraints (GPU budgets per accelerator type)
//   - Fair-shares GPUs across models (highest-score model gets GPUs first)
//   - Distributes replicas between P/D roles proportional to per-role demand
//   - Scale-down is identical to CostAwareOptimizer (reuses costAwareScaleDown)
type GreedyByScoreOptimizer struct{}

// NewGreedyByScoreOptimizer creates a new GreedyByScoreOptimizer.
func NewGreedyByScoreOptimizer() *GreedyByScoreOptimizer {
	return &GreedyByScoreOptimizer{}
}

// Name returns the optimizer identifier.
func (o *GreedyByScoreOptimizer) Name() string {
	return "greedy-by-score"
}

// modelWork tracks per-model allocation state during fair-share iteration.
type modelWork struct {
	req         ModelScalingRequest
	remaining   float64            // remaining Score (negative = fully satisfied)
	targets     map[string]int     // variant name → target replicas (ALL variants)
	roleDemands map[string]float64 // role → demand fraction; nil for non-disaggregated
}

// Optimize produces VariantDecisions for all models, fair-sharing GPUs across
// models that need to scale up. Scale-down models are handled independently.
func (o *GreedyByScoreOptimizer) Optimize(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx).WithName(o.Name())
	available := mergeConstraints(constraints)

	// Separate scale-up and scale-down/steady models
	var scaleUpWork []*modelWork
	var otherRequests []ModelScalingRequest

	for _, req := range requests {
		if req.Result == nil {
			continue
		}

		if req.Result.RequiredCapacity > 0 || req.Result.Score > 0 {
			w := o.buildScaleUpWork(req)
			if w != nil {
				scaleUpWork = append(scaleUpWork, w)
			}
		} else {
			otherRequests = append(otherRequests, req)
		}
	}

	// Scale-up: iterative mean-based fair sharing
	o.fairShareScaleUp(ctx, scaleUpWork, available)

	// Build all decisions
	allDecisions := make([]interfaces.VariantDecision, 0, len(scaleUpWork))

	for _, w := range scaleUpWork {
		stateMap := buildStateMap(w.req.VariantStates)
		vcMap := buildCapacityMap(w.req.Result.VariantCapacities)
		decisions := buildDecisionsWithOptimizer(w.req, stateMap, vcMap, w.targets, "greedy-by-score")
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (scale-up)",
			"modelID", w.req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	for _, req := range otherRequests {
		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(req.Result.VariantCapacities)
		targets := initTargets(req.VariantStates)

		if req.Result.SpareCapacity > 0 {
			costAwareScaleDown(ctx, req.Result, targets, stateMap)
		}

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "greedy-by-score")
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (other)",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// buildScaleUpWork creates a single work unit for a scale-up request.
// For disaggregated models, computes demand fractions per role so that
// allocateForModel distributes replicas proportional to per-role demand.
func (o *GreedyByScoreOptimizer) buildScaleUpWork(req ModelScalingRequest) *modelWork {
	remaining := req.Result.Score
	if remaining <= 0 {
		remaining = req.Result.RequiredCapacity
	}
	if remaining <= 0 {
		return nil
	}

	w := &modelWork{
		req:       req,
		remaining: remaining,
		targets:   initTargets(req.VariantStates),
	}

	// For disaggregated models, compute demand fractions per role
	if req.Disaggregated && req.Result.RoleCapacities != nil {
		totalDemand := 0.0
		for _, rc := range req.Result.RoleCapacities {
			if rc.RequiredCapacity > 0 {
				totalDemand += rc.RequiredCapacity
			}
		}
		if totalDemand > 0 {
			w.roleDemands = make(map[string]float64)
			for role, rc := range req.Result.RoleCapacities {
				if rc.RequiredCapacity > 0 {
					w.roleDemands[role] = rc.RequiredCapacity / totalDemand
				}
			}
		}
	}

	return w
}

// fairShareScaleUp implements the iterative mean-based fair-sharing algorithm.
// Each iteration picks the most starved model and allocates enough replicas to
// bring its remaining score below the current mean.
func (o *GreedyByScoreOptimizer) fairShareScaleUp(
	ctx context.Context,
	work []*modelWork,
	available map[string]int,
) {
	logger := ctrl.LoggerFrom(ctx)

	for {
		// Filter active models (remaining > 0)
		active := filterActive(work)
		if len(active) == 0 {
			break
		}

		// Check if any GPUs remain
		totalGPUs := 0
		for _, v := range available {
			totalGPUs += v
		}
		if totalGPUs == 0 {
			logger.V(logging.DEBUG).Info("GreedyByScore: no GPUs remaining, stopping fair-share")
			break
		}

		// Compute mean remaining score
		mean := computeMean(active)
		logger.V(logging.DEBUG).Info("GreedyByScore: iteration",
			"activeModels", len(active), "meanRemaining", mean)

		// Sort by remaining DESC (most starved first)
		sortByRemainingDesc(active)

		// Pick the most starved model
		w := active[0]

		// Compute allocation target
		allocationMean := mean
		if len(active) == 1 {
			allocationMean = 0
		} else if w.remaining <= mean {
			allocationMean = mean - (w.remaining / float64(len(active)))
		}

		// Allocate replicas
		allocated := o.allocateForModel(ctx, w, allocationMean, available)

		if !allocated {
			w.remaining = -1
			logger.V(logging.DEBUG).Info("GreedyByScore: no GPUs available for model, removing",
				"model", w.req.ModelID)
			continue
		}

		if w.remaining > mean {
			logger.V(logging.DEBUG).Info("GreedyByScore: model still above mean, removing",
				"model", w.req.ModelID, "remaining", w.remaining, "mean", mean)
			w.remaining = -1
		}
	}
}

// allocateForModel allocates replicas to bring the model's remaining score
// below the mean. For disaggregated models, distributes replicas between
// roles proportional to their per-role demand.
func (o *GreedyByScoreOptimizer) allocateForModel(
	ctx context.Context,
	w *modelWork,
	mean float64,
	available map[string]int,
) bool {
	target := w.remaining - mean
	if target <= 0 {
		return false
	}

	stateMap := buildStateMap(w.req.VariantStates)

	if w.roleDemands != nil {
		return o.allocateByRole(ctx, w, target, stateMap, available)
	}

	return o.allocateToVariants(ctx, w, target, w.req.Result.VariantCapacities, stateMap, available, "both")
}

// allocateByRole distributes replicas between roles proportional to their demand.
// Higher-demand roles are allocated first. If a role cannot fully allocate
// (e.g. accelerator exhausted), its unallocated portion is consumed from
// remaining so it does not overflow to other roles in subsequent iterations.
func (o *GreedyByScoreOptimizer) allocateByRole(
	ctx context.Context,
	w *modelWork,
	totalTarget float64,
	stateMap map[string]interfaces.VariantReplicaState,
	available map[string]int,
) bool {
	logger := ctrl.LoggerFrom(ctx)

	// Sort roles by demand fraction DESC to prioritize higher-demand roles
	type roleFraction struct {
		role     string
		fraction float64
	}
	roles := make([]roleFraction, 0, len(w.roleDemands))
	for role, fraction := range w.roleDemands {
		roles = append(roles, roleFraction{role, fraction})
	}
	sort.Slice(roles, func(i, j int) bool {
		return roles[i].fraction > roles[j].fraction
	})

	allocated := false
	for _, rf := range roles {
		roleTarget := totalTarget * rf.fraction
		if roleTarget <= 0 {
			continue
		}

		roleVariants := filterVariantCapacitiesByRole(w.req.Result.VariantCapacities, rf.role)
		if len(roleVariants) == 0 {
			// Role has no variants — consume its share so it doesn't overflow
			w.remaining -= roleTarget
			logger.V(logging.DEBUG).Info("GreedyByScore: no variants for role, consuming share",
				"model", w.req.ModelID, "role", rf.role, "consumed", roleTarget)
			continue
		}

		remainingBefore := w.remaining
		if o.allocateToVariants(ctx, w, roleTarget, roleVariants, stateMap, available, rf.role) {
			allocated = true
		}
		// Consume any unallocated portion so it doesn't overflow to other roles
		capacityAllocated := remainingBefore - w.remaining
		unallocated := roleTarget - capacityAllocated
		if unallocated > 0 {
			w.remaining -= unallocated
			logger.V(logging.DEBUG).Info("GreedyByScore: role partially allocated, consuming remainder",
				"model", w.req.ModelID, "role", rf.role, "allocated", capacityAllocated, "consumed", unallocated)
		}
	}
	return allocated
}

// allocateToVariants allocates replicas from the cheapest available variants
// within the given capacity set, up to the specified target.
func (o *GreedyByScoreOptimizer) allocateToVariants(
	ctx context.Context,
	w *modelWork,
	target float64,
	capacities []interfaces.VariantCapacity,
	stateMap map[string]interfaces.VariantReplicaState,
	available map[string]int,
	role string,
) bool {
	logger := ctrl.LoggerFrom(ctx)
	sorted := sortByCostEfficiencyAsc(capacities)

	allocated := false
	for _, vc := range sorted {
		if target <= 0 {
			break
		}
		if vc.PerReplicaCapacity <= 0 {
			continue
		}

		state := stateMap[vc.VariantName]
		gpusPerReplica := state.GPUsPerReplica
		if gpusPerReplica <= 0 {
			gpusPerReplica = 1
		}
		gpusAvail := available[vc.AcceleratorName]
		if gpusAvail < gpusPerReplica {
			continue
		}

		n := int(math.Ceil(target / vc.PerReplicaCapacity))
		maxByGPU := gpusAvail / gpusPerReplica
		if n > maxByGPU {
			n = maxByGPU
		}

		// Cap by maxReplicas if set
		if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
			maxAdd := *state.MaxReplicas - w.targets[vc.VariantName]
			if maxAdd <= 0 {
				continue // already at max
			}
			if n > maxAdd {
				n = maxAdd
			}
		}

		if n <= 0 {
			continue
		}

		w.targets[vc.VariantName] += n
		capacityAdded := float64(n) * vc.PerReplicaCapacity
		w.remaining -= capacityAdded
		target -= capacityAdded
		available[vc.AcceleratorName] -= n * gpusPerReplica

		logger.V(logging.DEBUG).Info("GreedyByScore: allocated replicas",
			"model", w.req.ModelID,
			"role", role,
			"variant", vc.VariantName,
			"replicas", n,
			"gpusUsed", n*gpusPerReplica,
			"remainingScore", w.remaining)
		allocated = true
	}
	return allocated
}

// filterVariantCapacitiesByRole returns variant capacities matching the specified role.
// For role "both" or empty, returns all capacities.
func filterVariantCapacitiesByRole(capacities []interfaces.VariantCapacity, role string) []interfaces.VariantCapacity {
	if role == "both" || role == "" {
		return capacities
	}
	var filtered []interfaces.VariantCapacity
	for _, vc := range capacities {
		vcRole := vc.Role
		if vcRole == "" {
			vcRole = "both"
		}
		if vcRole == role {
			filtered = append(filtered, vc)
		}
	}
	return filtered
}

// filterActive returns modelWork entries that still have remaining > 0.
func filterActive(work []*modelWork) []*modelWork {
	var active []*modelWork
	for _, w := range work {
		if w.remaining > 0 {
			active = append(active, w)
		}
	}
	return active
}

// computeMean returns the average remaining across active models.
func computeMean(active []*modelWork) float64 {
	if len(active) == 0 {
		return 0
	}
	total := 0.0
	for _, w := range active {
		total += w.remaining
	}
	return total / float64(len(active))
}

// sortByRemainingDesc sorts active models by remaining descending.
func sortByRemainingDesc(active []*modelWork) {
	sort.Slice(active, func(i, j int) bool {
		return active[i].remaining > active[j].remaining
	})
}

// Ensure GreedyByScoreOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*GreedyByScoreOptimizer)(nil)
