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
// GPUs across competing models, ordered by fair-share priority value
// (priority × Σᵢ(Remainingᵢ × Scoreᵢ) across analyzers).
//
// Key differences from CostAwareOptimizer:
//   - Respects ResourceConstraints (GPU budgets per accelerator type)
//   - Fair-shares GPUs across models (highest-priority model gets GPUs first)
//   - Disaggregated models use paired (n_P, n_D) allocation via the paired helpers
//   - Scale-down uses scaleDownRoleIterated (role-iterated unified path)
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
	req       ModelScalingRequest
	s         []NamedAnalyzerResult      // working slice; Remaining/Spare decremented in place
	satEntry  *interfaces.AnalyzerResult // variant metadata keeper (Cost, AcceleratorName, Role)
	ps        RolePairedState            // picker-local per-role demand (from initRoleState)
	roles     []string                   // active roles for this model
	remaining float64                    // fair-share priority metric (negative = fully satisfied)
	targets   map[string]int             // variant name → target replicas (ALL variants)
}

// fairShareValue computes the fair-share priority metric for one model.
// Phase 3: reads picker-local role-remaining (sum over roles × analyzer Score)
// so the metric reflects actual per-role demand remaining rather than the
// P-anchor model-level scalar.
//
//	fsv = priority × Σᵢ Score_i × Σ_role pickerState[i][role]
//
// Falls back to max remaining demand when the weighted result is zero.
func fairShareValue(priority float64, s []NamedAnalyzerResult, ps RolePairedState, roles []string) float64 {
	weighted := 0.0
	for i, e := range s {
		if e.Result == nil {
			continue
		}
		roleSum := 0.0
		for _, role := range roles {
			if i < len(ps) {
				roleSum += ps[i][role]
			}
		}
		weighted += roleSum * e.Score
	}
	if fsv := priority * weighted; fsv > 0 {
		return fsv
	}
	// Fallback: max remaining demand across roles when Score=0 or priority=0.
	maxDemand := 0.0
	for i, e := range s {
		if e.Result == nil {
			continue
		}
		if i < len(ps) {
			for _, role := range roles {
				if ps[i][role] > maxDemand {
					maxDemand = ps[i][role]
				}
			}
		}
	}
	return maxDemand
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

	var scaleUpWork []*modelWork
	var otherRequests []ModelScalingRequest

	for _, req := range requests {
		satEntry := saturationEntry(req.AnalyzerResults)
		if satEntry == nil {
			continue
		}

		s := req.AnalyzerResults
		roles, ps := initRoleState(s)
		fsv := fairShareValue(req.Priority, s, ps, roles)
		if anyRoleNeedsScaleUp(ps, roles) || fsv > 0 {
			w := o.buildScaleUpWork(req, satEntry, s, ps, roles, fsv)
			if w != nil {
				scaleUpWork = append(scaleUpWork, w)
			}
		} else {
			otherRequests = append(otherRequests, req)
		}
	}

	o.fairShareScaleUp(ctx, scaleUpWork, available)

	allDecisions := make([]interfaces.VariantDecision, 0, len(scaleUpWork))

	for _, w := range scaleUpWork {
		stateMap := buildStateMap(w.req.VariantStates)
		vcMap := buildCapacityMap(w.satEntry.VariantCapacities)
		decisions := buildDecisionsWithOptimizer(w.req, stateMap, vcMap, w.targets, "greedy-by-score")
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (scale-up)",
			"modelID", w.req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	for _, req := range otherRequests {
		satEntry := saturationEntry(req.AnalyzerResults)
		if satEntry == nil {
			continue
		}

		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(satEntry.VariantCapacities)
		targets := initTargets(req.VariantStates)

		// Unified scale-down path via scaleDownRoleIterated.
		s := req.AnalyzerResults
		_, _ = initRoleState(s) // populates RoleSpare for all roles
		scaleDownRoleIterated(ctx, s, satEntry.VariantCapacities, targets, stateMap)

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "greedy-by-score")
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (other)",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// buildScaleUpWork creates a single work unit for a scale-up request.
func (o *GreedyByScoreOptimizer) buildScaleUpWork(req ModelScalingRequest, satEntry *interfaces.AnalyzerResult, s []NamedAnalyzerResult, ps RolePairedState, roles []string, fsv float64) *modelWork {
	if fsv <= 0 {
		return nil
	}
	return &modelWork{
		req:       req,
		s:         s,
		satEntry:  satEntry,
		ps:        ps,
		roles:     roles,
		remaining: fsv,
		targets:   initTargets(req.VariantStates),
	}
}

// fairShareScaleUp implements the iterative mean-based fair-sharing algorithm.
func (o *GreedyByScoreOptimizer) fairShareScaleUp(
	ctx context.Context,
	work []*modelWork,
	available map[string]int,
) {
	logger := ctrl.LoggerFrom(ctx)

	for {
		active := filterActive(work)
		if len(active) == 0 {
			break
		}

		totalGPUs := 0
		for _, v := range available {
			totalGPUs += v
		}
		if totalGPUs == 0 {
			logger.V(logging.DEBUG).Info("GreedyByScore: no GPUs remaining, stopping fair-share")
			break
		}

		mean := computeMean(active)
		logger.V(logging.DEBUG).Info("GreedyByScore: iteration",
			"activeModels", len(active), "meanRemaining", mean)

		sortByRemainingDesc(active)
		w := active[0]

		allocationMean := mean
		if len(active) == 1 {
			allocationMean = 0
		} else if w.remaining <= mean {
			allocationMean = mean - (w.remaining / float64(len(active)))
		}

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

// allocateForModel allocates replicas to bring the model's remaining score below
// the mean. Dispatches to the paired path for disaggregated models.
// After allocation, w.remaining is recomputed from the working slice.
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
	oldRemaining := w.remaining

	// Re-initialize picker-state from current s[i].Remaining each call so
	// multi-iteration fair-sharing sees the correct post-allocation demand.
	// Cap at target so the loop exits when the fair-share budget is exhausted.
	_, ps := initRoleState(w.s)
	for i := range ps {
		for _, role := range w.roles {
			if ps[i][role] > target {
				ps[i][role] = target
			}
		}
	}

	// Unified path: fairShareRolePick behind the RolePickFn interface.
	// α logic removed in commit 3.
	pick := fairShareRolePick(target, w.s, w.roles)
	allocateForModelPaired(ctx, w.s, w.satEntry.VariantCapacities, stateMap, available,
		w.targets, pick, ps, w.roles)

	// Recompute w.remaining for fair-share ordering.
	// For "both" (non-disag): use fresh ps so applyAllocation-decremented
	// s[i].Remaining is read (budget-capped ps is already 0).
	// For P/D: use local capped ps which correctly reaches 0 when both roles served.
	if len(w.roles) == 1 && w.roles[0] == interfaces.RoleBoth {
		_, freshPs := initRoleState(w.s)
		w.remaining = fairShareValue(w.req.Priority, w.s, freshPs, w.roles)
	} else {
		w.remaining = fairShareValue(w.req.Priority, w.s, ps, w.roles)
	}
	return w.remaining < oldRemaining
}

// fairShareRolePick returns a RolePickFn for the unified allocateForModelPaired loop.
// Each role receives the same target fair-share budget. The joint Δ_util commit
// inside allocateForModelPaired enforces P/D coupling — α is no longer needed.
func fairShareRolePick(target float64, s []NamedAnalyzerResult, roles []string) RolePickFn {
	_ = s     // slice available for future multi-analyzer demand inspection
	_ = roles // roles available for future per-role budget splitting
	return func(
		role string,
		_ []NamedAnalyzerResult,
		variants []interfaces.VariantCapacity,
		stateMap map[string]interfaces.VariantReplicaState,
		available map[string]int,
		targets map[string]int,
	) (string, int) {
		roleVCs := variantsForRole(variants, role)
		for _, vc := range sortByCostEfficiencyAsc(roleVCs) {
			if vc.PerReplicaCapacity <= 0 {
				continue
			}
			state := stateMap[vc.VariantName]
			gpusPR := state.GPUsPerReplica
			if gpusPR <= 0 {
				gpusPR = 1
			}
			gpusAvail := available[vc.AcceleratorName]
			if gpusAvail < gpusPR {
				continue
			}
			fairShareCap := int(math.Ceil(target / vc.PerReplicaCapacity))
			capN := min(fairShareCap, gpusAvail/gpusPR)
			if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
				headroom := *state.MaxReplicas - targets[vc.VariantName]
				if headroom <= 0 {
					continue
				}
				capN = min(capN, headroom)
			}
			if capN > 0 {
				return vc.VariantName, capN
			}
		}
		return "", 0
	}
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

// prcFromVCs returns the PerReplicaCapacity for variant v from a slice of VCs.
func prcFromVCs(vcs []interfaces.VariantCapacity, v string) float64 {
	for _, vc := range vcs {
		if vc.VariantName == v {
			return vc.PerReplicaCapacity
		}
	}
	return 0
}

// accFromVCs returns the AcceleratorName for variant v from a slice of VCs.
func accFromVCs(vcs []interfaces.VariantCapacity, v string) string {
	for _, vc := range vcs {
		if vc.VariantName == v {
			return vc.AcceleratorName
		}
	}
	return ""
}

// gpusPerReplicaFromState returns GPUsPerReplica for variant v, defaulting to 1.
func gpusPerReplicaFromState(stateMap map[string]interfaces.VariantReplicaState, v string) int {
	if state, ok := stateMap[v]; ok && state.GPUsPerReplica > 0 {
		return state.GPUsPerReplica
	}
	return 1
}

// Ensure GreedyByScoreOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*GreedyByScoreOptimizer)(nil)
