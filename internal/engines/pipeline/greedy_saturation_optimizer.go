package pipeline

import (
	"context"
	"math"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// GreedyBySaturationOptimizer is a multi-model optimizer for GPU-constrained
// environments. It uses iterative mean-based fair-sharing to distribute scarce
// GPUs across competing models.
//
// Key differences from CostAwareOptimizer:
//   - Respects ResourceConstraints (GPU budgets per accelerator type)
//   - Fair-shares GPUs across models (most starved model gets GPUs first)
//   - Scale-down is identical to CostAwareOptimizer (reuses costAwareScaleDown)
type GreedyBySaturationOptimizer struct{}

// NewGreedyBySaturationOptimizer creates a new GreedyBySaturationOptimizer.
func NewGreedyBySaturationOptimizer() *GreedyBySaturationOptimizer {
	return &GreedyBySaturationOptimizer{}
}

// Name returns the optimizer identifier.
func (o *GreedyBySaturationOptimizer) Name() string {
	return "greedy-by-saturation"
}

// modelWork tracks per-model allocation state during fair-share iteration.
type modelWork struct {
	req       ModelScalingRequest
	remaining float64        // remaining RequiredCapacity (negative = fully satisfied)
	targets   map[string]int // variant name → target replicas
}

// Optimize produces VariantDecisions for all models, fair-sharing GPUs across
// models that need to scale up. Scale-down models are handled independently.
func (o *GreedyBySaturationOptimizer) Optimize(
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
		if req.Result.RequiredCapacity > 0 {
			targets := initTargets(req.VariantStates)
			scaleUpWork = append(scaleUpWork, &modelWork{
				req:       req,
				remaining: req.Result.RequiredCapacity,
				targets:   targets,
			})
		} else {
			otherRequests = append(otherRequests, req)
		}
	}

	// Scale-up: iterative mean-based fair sharing
	o.fairShareScaleUp(ctx, scaleUpWork, available)

	// Build all decisions
	var allDecisions []interfaces.VariantDecision

	for _, w := range scaleUpWork {
		stateMap := buildStateMap(w.req.VariantStates)
		vcMap := buildCapacityMap(w.req.Result.VariantCapacities)
		decisions := buildDecisionsWithOptimizer(w.req, stateMap, vcMap, w.targets, "greedy-by-saturation")
		logger.V(logging.DEBUG).Info("Greedy-by-saturation optimizer decisions (scale-up)",
			"modelID", w.req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	for _, req := range otherRequests {
		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(req.Result.VariantCapacities)
		targets := initTargets(req.VariantStates)

		if req.Result.SpareCapacity > 0 {
			costAwareScaleDown(ctx, req.Result, targets)
		}

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "greedy-by-saturation")
		logger.V(logging.DEBUG).Info("Greedy-by-saturation optimizer decisions (other)",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// fairShareScaleUp implements the iterative mean-based fair-sharing algorithm.
// Each iteration picks the most starved model and allocates enough replicas to
// bring its RequiredCapacity below the current mean.
func (o *GreedyBySaturationOptimizer) fairShareScaleUp(
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
			logger.V(logging.DEBUG).Info("GreedyBySaturation: no GPUs remaining, stopping fair-share")
			break
		}

		// Compute mean required capacity
		mean := computeMean(active)
		logger.V(logging.DEBUG).Info("GreedyBySaturation: iteration",
			"activeModels", len(active), "meanRequired", mean)

		// Sort by remaining DESC (most starved first)
		sortByRemainingDesc(active)

		// Pick the most starved model
		w := active[0]

		// Compute allocation target (how far below the mean to bring this model).
		// Three cases:
		//   1. Single model: allocationMean=0 → satisfy full demand
		//   2. Tied models (max remaining == mean, implies all equal):
		//      allocationMean = mean * (N-1)/N → each gets 1/N of demand per iteration
		//   3. Normal: allocationMean = mean → bring to average
		allocationMean := mean
		if len(active) == 1 {
			allocationMean = 0
		} else if w.remaining <= mean {
			// All models tied (max ≤ avg implies all equal).
			// Allocate 1/N of demand per iteration for fair distribution.
			allocationMean = mean - (w.remaining / float64(len(active)))
		}

		// Allocate replicas to bring this model below mean
		allocated := o.allocateForModel(ctx, w, allocationMean, available)

		if !allocated {
			// No GPUs available for any variant of this model — remove from working set
			w.remaining = -1
			logger.V(logging.DEBUG).Info("GreedyBySaturation: no GPUs available for model, removing",
				"model", w.req.ModelID)
			continue
		}

		// Check outcome after allocation
		if w.remaining > mean {
			// Still above mean even after allocation — remove from working set
			logger.V(logging.DEBUG).Info("GreedyBySaturation: model still above mean, removing",
				"model", w.req.ModelID, "remaining", w.remaining, "mean", mean)
			w.remaining = -1
		}
		// else: remaining <= 0 (satisfied, filterActive removes it)
		// or remaining dropped below mean but > 0 (stays for next iteration)
	}
}

// allocateForModel allocates replicas from the cheapest available variants
// to bring the model's remaining capacity below the mean.
func (o *GreedyBySaturationOptimizer) allocateForModel(
	ctx context.Context,
	w *modelWork,
	mean float64,
	available map[string]int,
) bool {
	logger := ctrl.LoggerFrom(ctx)

	// Target: bring w.remaining below mean
	target := w.remaining - mean
	if target <= 0 {
		return false // already below mean
	}

	// Sort this model's variants by cost-efficiency
	sorted := sortByCostEfficiencyAsc(w.req.Result.VariantCapacities)
	stateMap := buildStateMap(w.req.VariantStates)

	allocated := false
	for _, vc := range sorted {
		if target <= 0 {
			break
		}

		state := stateMap[vc.VariantName]

		// Pending replicas are NOT skipped: the V2 analyzer already accounts
		// for pending replicas' capacity in the anticipated supply calculation.
		// If RequiredCapacity > 0, demand exceeds total supply including pending.
		if vc.PerReplicaCapacity <= 0 {
			continue
		}

		// Check GPU availability
		gpusPerReplica := state.GPUsPerReplica
		if gpusPerReplica <= 0 {
			gpusPerReplica = 1
		}
		gpusAvail := available[vc.AcceleratorName]
		if gpusAvail < gpusPerReplica {
			continue // not enough GPUs for even one replica
		}

		// How many replicas to cover 'target' capacity
		n := int(math.Ceil(target / vc.PerReplicaCapacity))

		// Constrain by available GPUs
		maxByGPU := gpusAvail / gpusPerReplica
		if n > maxByGPU {
			n = maxByGPU
		}
		if n <= 0 {
			continue
		}

		// Allocate
		w.targets[vc.VariantName] += n
		capacityAdded := float64(n) * vc.PerReplicaCapacity
		w.remaining -= capacityAdded
		target -= capacityAdded
		available[vc.AcceleratorName] -= n * gpusPerReplica

		logger.V(logging.DEBUG).Info("GreedyBySaturation: allocated replicas",
			"model", w.req.ModelID,
			"variant", vc.VariantName,
			"replicas", n,
			"gpusUsed", n*gpusPerReplica,
			"remainingRequired", w.remaining)
		allocated = true
	}
	return allocated
}

// filterActive returns modelWork entries that still have remaining capacity > 0.
func filterActive(work []*modelWork) []*modelWork {
	var active []*modelWork
	for _, w := range work {
		if w.remaining > 0 {
			active = append(active, w)
		}
	}
	return active
}

// computeMean returns the average remaining capacity across active models.
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

// sortByRemainingDesc sorts active models by remaining capacity descending
// (most starved first).
func sortByRemainingDesc(active []*modelWork) {
	sort.Slice(active, func(i, j int) bool {
		return active[i].remaining > active[j].remaining
	})
}

// Ensure GreedyBySaturationOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*GreedyBySaturationOptimizer)(nil)
