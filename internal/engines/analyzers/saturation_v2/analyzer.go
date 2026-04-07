package saturation_v2

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// SaturationAnalyzer implements the interfaces.Analyzer interface using a
// token-based capacity model with memory-bound (k1) and compute-bound (k2)
// constraints. It replaces the V1 percentage-based analyzer when
// analyzerName is set to "saturation".
type SaturationAnalyzer struct {
	// mu protects computeCapacityHistory from concurrent access.
	mu sync.Mutex
	// computeCapacityHistory stores rolling averages of observed k2 values,
	// keyed by "modelID|accelerator|outputBucket".
	computeCapacityHistory map[string]*rollingAverage
	capacityStore          *CapacityKnowledgeStore
}

// NewSaturationAnalyzer creates a new V2 saturation analyzer backed by the
// given capacity store.
func NewSaturationAnalyzer(store *CapacityKnowledgeStore) *SaturationAnalyzer {
	return &SaturationAnalyzer{
		computeCapacityHistory: make(map[string]*rollingAverage),
		capacityStore:          store,
	}
}

// Name returns the analyzer identifier for logging and result metadata.
// Note: the config value "saturation" (in analyzerName YAML field) selects this analyzer,
// but the descriptive name here is used in AnalyzerResult.AnalyzerName for observability.
func (a *SaturationAnalyzer) Name() string {
	return "saturation-token-based"
}

// EvictStaleHistory removes k2 history entries that have not been updated
// within the given timeout. This prevents unbounded memory growth from
// deleted models or workload buckets that are no longer active.
func (a *SaturationAnalyzer) EvictStaleHistory(timeout time.Duration) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	evicted := 0
	for key, ra := range a.computeCapacityHistory {
		if time.Since(ra.lastUpdated) > timeout {
			delete(a.computeCapacityHistory, key)
			evicted++
		}
	}
	return evicted
}

// Analyze computes capacity signals for a model across all its variants.
func (a *SaturationAnalyzer) Analyze(ctx context.Context, input interfaces.AnalyzerInput) (*interfaces.AnalyzerResult, error) {
	satConfig, ok := input.Config.(*config.SaturationScalingConfig)
	if !ok {
		return nil, fmt.Errorf("expected *SaturationScalingConfig, got %T", input.Config)
	}

	// Build GPU count lookup from variant states
	gpusByVariant := make(map[string]int, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		gpusByVariant[vs.VariantName] = vs.GPUsPerReplica
	}

	// Phase 1: Per-replica capacity computation
	replicaCapacities := make([]ReplicaCapacity, 0, len(input.ReplicaMetrics))
	for _, rm := range input.ReplicaMetrics {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		gpuCount := gpusByVariant[rm.VariantName]
		rc := a.computeReplicaCapacity(rm, satConfig, input.ModelID, input.Namespace, gpuCount)
		if rc != nil {
			replicaCapacities = append(replicaCapacities, *rc)
		}
	}

	// Phase 2: Per-variant aggregation
	variantCapacities := a.aggregateByVariant(replicaCapacities, input.ReplicaMetrics, input.VariantStates, input.ModelID, input.Namespace, satConfig.KvCacheThreshold)

	// Phase 3: Model-level aggregation
	var totalSupply, totalAnticipatedSupply, totalDemand float64
	activeRoles := make(map[string]bool)
	for _, vc := range variantCapacities {
		totalSupply += vc.TotalCapacity
		totalDemand += vc.TotalDemand
		// Anticipated supply includes pending replicas
		anticipatedCapacity := float64(vc.ReplicaCount+vc.PendingReplicas) * vc.PerReplicaCapacity
		totalAnticipatedSupply += anticipatedCapacity
		// Track active roles for queue demand attribution
		role := vc.Role
		if role == "" {
			role = interfaces.RoleBoth
		}
		activeRoles[role] = true
	}

	// Add scheduler queue demand (requests queued upstream in llm-d flow control)
	queueDemand := estimateSchedulerQueueDemand(input.SchedulerQueue, input.ReplicaMetrics, activeRoles)
	totalDemand += queueDemand.total

	var utilization float64
	if totalSupply > 0 {
		utilization = totalDemand / totalSupply
	}

	// Phase 4: Scaling signals
	var requiredCapacity, spareCapacity float64
	if satConfig.ScaleUpThreshold > 0 {
		requiredCapacity = totalDemand/satConfig.ScaleUpThreshold - totalAnticipatedSupply
	}
	if requiredCapacity < 0 {
		requiredCapacity = 0
	}

	if satConfig.ScaleDownBoundary > 0 {
		spareCapacity = totalSupply - totalDemand/satConfig.ScaleDownBoundary
	}
	if spareCapacity < 0 {
		spareCapacity = 0
	}

	// Phase 4b: Per-role aggregation (P/D disaggregation)
	roleCapacities := a.aggregateByRole(variantCapacities, satConfig, queueDemand.byRole)

	// Phase 5: Build result
	result := &interfaces.AnalyzerResult{
		AnalyzerName:      a.Name(),
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        time.Now(),
		VariantCapacities: variantCapacities,
		TotalSupply:       totalSupply,
		TotalDemand:       totalDemand,
		Utilization:       utilization,
		RequiredCapacity:  requiredCapacity,
		SpareCapacity:     spareCapacity,
		RoleCapacities:    roleCapacities,
	}

	return result, nil
}

// computeReplicaCapacity computes the capacity breakdown for a single replica.
// Returns nil if the replica has no V2 capacity data (TotalKvCapacityTokens == 0).
func (a *SaturationAnalyzer) computeReplicaCapacity(
	rm interfaces.ReplicaMetrics,
	config *config.SaturationScalingConfig,
	modelID, namespace string,
	gpuCount int,
) *ReplicaCapacity {
	if rm.TotalKvCapacityTokens <= 0 {
		// TODO: implement proper demand estimation when vllm:cache_config_info is absent.
		// Currently we fall back to percentage-based demand using the deployment-derived
		// capacity from the capacity store. A better approach would be to estimate
		// TotalKvCapacityTokens from deployment args (num_gpu_blocks_override, block_size)
		// or use a dedicated percentage-based demand signal.
		return a.computeReplicaCapacityFallback(rm, config, modelID, namespace)
	}

	// Compute demand
	replicaDemand := rm.TokensInUse
	if rm.AvgInputTokens > 0 {
		replicaDemand += int64(rm.QueueLength) * int64(rm.AvgInputTokens)
	}

	// k1: memory-bound capacity
	k1 := int64(float64(rm.TotalKvCapacityTokens) * config.KvCacheThreshold)

	// k2: compute-bound capacity
	var vllmParams *VLLMEngineParams
	if rec := a.capacityStore.Get(namespace, modelID, rm.VariantName); rec != nil {
		vllmParams = rec.VLLMParams
	}
	k2 := a.computeK2(
		modelID, rm.AcceleratorName,
		rm.QueueLength, rm.TokensInUse,
		rm.AvgOutputTokens, rm.AvgInputTokens,
		config.QueueLengthThreshold,
		vllmParams,
		k1,
	)

	effectiveCapacity := k1
	if k2 < k1 {
		effectiveCapacity = k2
	}

	isSaturated := replicaDemand >= effectiveCapacity

	// Update capacity store with live data, preserving VLLMParams from any
	// existing record (parsed from deployment args and needed for FindCompatible).
	var existingParams *VLLMEngineParams
	if existing := a.capacityStore.Get(namespace, modelID, rm.VariantName); existing != nil && existing.VLLMParams != nil {
		existingParams = existing.VLLMParams
	}
	a.capacityStore.Update(namespace, modelID, rm.VariantName, CapacityRecord{
		AcceleratorName:       rm.AcceleratorName,
		GpuCount:              gpuCount,
		NumGpuBlocks:          rm.NumGpuBlocks,
		BlockSize:             rm.BlockSize,
		TotalKvCapacityTokens: rm.TotalKvCapacityTokens,
		EffectiveCapacity:     effectiveCapacity,
		VLLMParams:            existingParams,
		LearnedFrom:           learnedFromLive,
	})

	return &ReplicaCapacity{
		PodName:               rm.PodName,
		VariantName:           rm.VariantName,
		AcceleratorName:       rm.AcceleratorName,
		TokensInUse:           rm.TokensInUse,
		TotalKvCapacityTokens: rm.TotalKvCapacityTokens,
		MemoryBoundCapacity:   k1,
		ComputeBoundCapacity:  k2,
		EffectiveCapacity:     effectiveCapacity,
		IsSaturated:           isSaturated,
		ReplicaDemand:         replicaDemand,
	}
}

// computeReplicaCapacityFallback handles the case where vllm:cache_config_info
// is not available (TotalKvCapacityTokens == 0). It uses the deployment-derived
// capacity from the capacity store and estimates demand from KvCacheUsage percentage.
// This allows V2 to work with model servers that don't emit cache_config_info
// (e.g., the llm-d-inference-sim).
func (a *SaturationAnalyzer) computeReplicaCapacityFallback(
	rm interfaces.ReplicaMetrics,
	cfg *config.SaturationScalingConfig,
	modelID, namespace string,
) *ReplicaCapacity {
	rec := a.capacityStore.Get(namespace, modelID, rm.VariantName)
	if rec == nil || rec.EffectiveCapacity <= 0 {
		return nil
	}

	// Apply KvCacheThreshold to match the main path (where k1 = totalTokens * threshold).
	// For deployment-derived records, EffectiveCapacity is the raw estimate; the threshold
	// reduces it to the usable portion, consistent with the normal code path.
	effectiveCapacity := int64(float64(rec.EffectiveCapacity) * cfg.KvCacheThreshold)
	if effectiveCapacity <= 0 {
		return nil
	}

	// Estimate demand from KV cache usage percentage applied to the thresholded capacity.
	// This is a coarse approximation — KvCacheUsage reflects memory pressure, not
	// exact token demand — but it's sufficient when token-level metrics are absent.
	replicaDemand := int64(rm.KvCacheUsage * float64(effectiveCapacity))

	// Add queue-based demand if we have average input token info
	if rm.AvgInputTokens > 0 {
		replicaDemand += int64(rm.QueueLength) * int64(rm.AvgInputTokens)
	}

	isSaturated := replicaDemand >= effectiveCapacity

	return &ReplicaCapacity{
		PodName:               rm.PodName,
		VariantName:           rm.VariantName,
		AcceleratorName:       rm.AcceleratorName,
		TokensInUse:           replicaDemand,
		TotalKvCapacityTokens: effectiveCapacity, // synthetic: store-derived
		MemoryBoundCapacity:   effectiveCapacity,
		ComputeBoundCapacity:  effectiveCapacity,
		EffectiveCapacity:     effectiveCapacity,
		IsSaturated:           isSaturated,
		ReplicaDemand:         replicaDemand,
	}
}

// computeK2 determines the compute-bound capacity using a priority chain:
// 1. Observed (queue saturated) → use tokensInUse as k2
// 2. Historical → rolling average from previous observations
// 3. Derived (from deployment args) → formula-based estimate
// 4. Fallback → k1 (memory-bound only)
func (a *SaturationAnalyzer) computeK2(
	modelID, accelerator string,
	queueLen int, tokensInUse int64,
	avgOutput, avgInput float64,
	queueThreshold float64,
	vllmParams *VLLMEngineParams,
	k1 int64,
) int64 {
	outputBucket := classifyOutputLength(avgOutput)
	historyKey := fmt.Sprintf("%s|%s|%s", modelID, accelerator, outputBucket)

	// Priority 1: Observed (queue saturated)
	if queueLen >= int(queueThreshold) && tokensInUse > 0 {
		k2Observed := tokensInUse
		a.mu.Lock()
		ra, ok := a.computeCapacityHistory[historyKey]
		if !ok {
			ra = newRollingAverage(RollingAverageWindowSize)
			a.computeCapacityHistory[historyKey] = ra
		}
		ra.Add(float64(k2Observed))
		a.mu.Unlock()
		return k2Observed
	}

	// Priority 2: Historical — lock must cover Average() since Add() mutates
	// the same slice from Priority 1 under the same lock.
	a.mu.Lock()
	var histAvg float64
	if ra, ok := a.computeCapacityHistory[historyKey]; ok {
		histAvg = ra.Average()
	}
	a.mu.Unlock()
	if histAvg > 0 {
		return int64(histAvg)
	}

	// Priority 3: Derived from deployment args
	if k2Derived := estimateCapacityFromParams(vllmParams, avgInput, avgOutput); k2Derived > 0 {
		return k2Derived
	}

	// Priority 4: Fallback to k1
	return k1
}

// aggregateByVariant groups replica capacities by variant and computes
// per-variant capacity metrics.
func (a *SaturationAnalyzer) aggregateByVariant(
	replicaCapacities []ReplicaCapacity,
	inputMetrics []interfaces.ReplicaMetrics,
	variantStates []interfaces.VariantReplicaState,
	modelID, namespace string,
	kvCacheThreshold float64,
) []interfaces.VariantCapacity {
	// Group replicas by variant
	byVariant := make(map[string][]ReplicaCapacity)
	for _, rc := range replicaCapacities {
		byVariant[rc.VariantName] = append(byVariant[rc.VariantName], rc)
	}

	// Build cost and accelerator lookup from input metrics
	variantCost := make(map[string]float64)
	variantAccel := make(map[string]string)
	for _, rm := range inputMetrics {
		if _, ok := variantCost[rm.VariantName]; !ok {
			variantCost[rm.VariantName] = rm.Cost
			variantAccel[rm.VariantName] = rm.AcceleratorName
		}
	}

	// Compute model-level workload averages from live replica metrics.
	// Used for capacity estimation of zero-replica variants with deployment-derived params.
	modelAvgInput, modelAvgOutput, _ := computeModelWorkloadAverages(inputMetrics)

	result := make([]interfaces.VariantCapacity, 0, len(variantStates))
	for _, vs := range variantStates {
		replicas := byVariant[vs.VariantName]

		var perReplicaCapacity float64
		var totalDemand float64
		accelerator := variantAccel[vs.VariantName]
		cost := variantCost[vs.VariantName]

		readyCount := vs.CurrentReplicas - vs.PendingReplicas
		if readyCount < 0 {
			readyCount = 0
		}

		if len(replicas) > 0 {
			// Use median effective capacity from ready pods
			capacities := make([]int64, 0, len(replicas))
			for _, rc := range replicas {
				capacities = append(capacities, rc.EffectiveCapacity)
				totalDemand += float64(rc.ReplicaDemand)
			}
			perReplicaCapacity = float64(median(capacities))
			if accelerator == "" {
				accelerator = replicas[0].AcceleratorName
			}
		} else if rec := a.capacityStore.Get(namespace, modelID, vs.VariantName); rec != nil && rec.EffectiveCapacity > 0 {
			// No ready replicas — use stored capacity, enhanced with k2 derivation
			// for deployment-derived records when workload data is available.
			perReplicaCapacity = a.estimateStoredCapacity(rec, modelID, kvCacheThreshold, modelAvgInput, modelAvgOutput)
		} else if rec := a.lookupCompatibleCapacity(namespace, modelID, vs.VariantName, accelerator, vs.GPUsPerReplica); rec != nil {
			// No own record — try cross-variant estimation from a compatible variant
			perReplicaCapacity = float64(rec.EffectiveCapacity)
		}

		totalCapacity := float64(readyCount) * perReplicaCapacity

		var utilization float64
		if totalCapacity > 0 {
			utilization = totalDemand / totalCapacity
		}

		vc := interfaces.VariantCapacity{
			VariantName:        vs.VariantName,
			AcceleratorName:    accelerator,
			Cost:               cost,
			Role:               vs.Role,
			ReplicaCount:       readyCount,
			PendingReplicas:    vs.PendingReplicas,
			PerReplicaCapacity: perReplicaCapacity,
			TotalCapacity:      totalCapacity,
			TotalDemand:        totalDemand,
			Utilization:        utilization,
		}
		result = append(result, vc)
	}

	return result
}

// aggregateByRole groups variant capacities by P/D role and computes per-role
// scaling signals. Returns nil when no disaggregation is active (all variants
// are role "both" or empty). The queueDemandByRole map adds scheduler queue
// demand attributed to each role (nil when there's no queue demand).
func (a *SaturationAnalyzer) aggregateByRole(
	variantCapacities []interfaces.VariantCapacity,
	config *config.SaturationScalingConfig,
	queueDemandByRole map[string]float64,
) map[string]interfaces.RoleCapacity {
	// Check if any variant has a non-"both" role
	hasDisaggregation := false
	for _, vc := range variantCapacities {
		if vc.Role != "" && vc.Role != interfaces.RoleBoth {
			hasDisaggregation = true
			break
		}
	}
	if !hasDisaggregation {
		return nil
	}

	// Group supply/demand by role
	type roleAccum struct {
		supply      float64
		anticipated float64
		demand      float64
	}
	roles := make(map[string]*roleAccum)
	for _, vc := range variantCapacities {
		role := vc.Role
		if role == "" {
			role = interfaces.RoleBoth
		}
		ra, ok := roles[role]
		if !ok {
			ra = &roleAccum{}
			roles[role] = ra
		}
		ra.supply += vc.TotalCapacity
		ra.anticipated += float64(vc.ReplicaCount+vc.PendingReplicas) * vc.PerReplicaCapacity
		ra.demand += vc.TotalDemand
	}

	// Add scheduler queue demand attributed to each role
	for role, qd := range queueDemandByRole {
		ra, ok := roles[role]
		if !ok {
			// Queue demand for a role with no variants — skip
			continue
		}
		ra.demand += qd
	}

	// Compute per-role scaling signals
	result := make(map[string]interfaces.RoleCapacity, len(roles))
	for role, ra := range roles {
		var required, spare float64
		if config.ScaleUpThreshold > 0 {
			required = ra.demand/config.ScaleUpThreshold - ra.anticipated
		}
		if required < 0 {
			required = 0
		}
		if config.ScaleDownBoundary > 0 {
			spare = ra.supply - ra.demand/config.ScaleDownBoundary
		}
		if spare < 0 {
			spare = 0
		}
		result[role] = interfaces.RoleCapacity{
			Role:             role,
			TotalSupply:      ra.supply,
			TotalDemand:      ra.demand,
			RequiredCapacity: required,
			SpareCapacity:    spare,
		}
	}
	return result
}

// lookupCompatibleCapacity searches the capacity store for a record from
// another variant with matching hardware and vLLM parameters. This enables
// capacity estimation for zero-replica variants that have no prior data.
// The search is cross-namespace since capacity depends on hardware + config,
// not namespace.
func (a *SaturationAnalyzer) lookupCompatibleCapacity(namespace, modelID, variantName, accelerator string, gpuCount int) *CapacityRecord {
	// Get VLLMParams for this variant (from deployment-derived record)
	rec := a.capacityStore.Get(namespace, modelID, variantName)
	if rec == nil || rec.VLLMParams == nil {
		return nil
	}
	return a.capacityStore.FindCompatible(modelID, accelerator, gpuCount, rec.VLLMParams)
}

// estimateStoredCapacity returns a capacity estimate for a zero-replica variant
// using its stored CapacityRecord. For learnedFromLive records (from a previously running
// pod), the stored EffectiveCapacity is authoritative. For "deployment" records,
// it tries to compute a better estimate using the k2 derivation formula with
// model-level workload averages, bounded by:
//  1. A compatible variant's live EffectiveCapacity (already min(k1,k2))
//  2. Own k1 if TotalKvCapacityTokens is known (from num_gpu_blocks_override)
//
// Falls back to stored EffectiveCapacity (EffectiveMaxBatchedTokens) when no
// workload data is available.
func (a *SaturationAnalyzer) estimateStoredCapacity(rec *CapacityRecord, modelID string, kvCacheThreshold float64, modelAvgInput, modelAvgOutput float64) float64 {
	if rec == nil {
		return 0
	}

	// Live records have observed capacity — use directly
	if rec.LearnedFrom == learnedFromLive {
		return float64(rec.EffectiveCapacity)
	}

	// For deployment-derived records, try k2 derivation with workload data
	if rec.VLLMParams != nil && modelAvgOutput > 0 {
		if derived := estimateCapacityFromParams(rec.VLLMParams, modelAvgInput, modelAvgOutput); derived > 0 {
			bounded := derived

			// Bound by own k1 if TotalKvCapacityTokens is known (num_gpu_blocks_override)
			if rec.TotalKvCapacityTokens > 0 && kvCacheThreshold > 0 {
				k1 := int64(float64(rec.TotalKvCapacityTokens) * kvCacheThreshold)
				if k1 > 0 && k1 < bounded {
					bounded = k1
				}
			}

			// Bound by compatible variant's live EffectiveCapacity (already min(k1,k2))
			if compatible := a.capacityStore.FindCompatible(modelID, rec.AcceleratorName, rec.GpuCount, rec.VLLMParams); compatible != nil && compatible.LearnedFrom == learnedFromLive && compatible.EffectiveCapacity > 0 {
				if compatible.EffectiveCapacity < bounded {
					bounded = compatible.EffectiveCapacity
				}
			}

			return float64(bounded)
		}
	}

	// Fallback: stored EffectiveCapacity (EffectiveMaxBatchedTokens from LoadFromDeployment)
	return float64(rec.EffectiveCapacity)
}

// estimateCapacityFromParams computes a capacity estimate using the k2 derivation
// formula: N_steady = min(B * O / (I + O), S), capacity = N_steady * (I + O/2).
// Used by computeK2 (Priority 3) for per-replica estimation and by
// estimateStoredCapacity for zero-replica variants with model-level workload averages.
// Returns 0 if estimation is not possible.
func estimateCapacityFromParams(params *VLLMEngineParams, avgInput, avgOutput float64) int64 {
	if params == nil || params.EffectiveMaxBatchedTokens <= 0 || avgOutput <= 0 {
		return 0
	}

	B := float64(params.EffectiveMaxBatchedTokens)
	S := float64(params.MaxNumSeqs)
	I := avgInput
	O := avgOutput

	nSteady := B * O / (I + O)
	if nSteady > S {
		nSteady = S
	}
	k2Derived := int64(nSteady * (I + O/2))
	if k2Derived > 0 {
		return k2Derived
	}
	return 0
}

// computeModelWorkloadAverages computes the model-level average input tokens,
// output tokens, and prefix cache hit rate from replica metrics across all
// variants. These averages enable capacity estimation for zero-replica variants
// using the k2 derivation formula, and scheduler queue demand estimation.
func computeModelWorkloadAverages(replicaMetrics []interfaces.ReplicaMetrics) (avgInput, avgOutput, avgHitRate float64) {
	var count int
	for _, rm := range replicaMetrics {
		if rm.AvgInputTokens > 0 || rm.AvgOutputTokens > 0 {
			avgInput += rm.AvgInputTokens
			avgOutput += rm.AvgOutputTokens
			avgHitRate += rm.PrefixCacheHitRate
			count++
		}
	}
	if count > 0 {
		avgInput /= float64(count)
		avgOutput /= float64(count)
		avgHitRate /= float64(count)
	}
	return avgInput, avgOutput, avgHitRate
}

// schedulerQueueDemand holds the estimated token demand from scheduler-queued
// requests, broken down by P/D role for disaggregated models.
type schedulerQueueDemand struct {
	total  float64            // model-level total (inputTokens + outputTokens)
	byRole map[string]float64 // per-role demand: "prefill", "decode", "both"
}

// estimateSchedulerQueueDemand estimates the token demand from requests queued
// in the llm-d inference scheduler's flow control layer, with per-role
// attribution for P/D disaggregated models.
//
// These requests have not yet reached any vLLM pod, so we estimate their
// token footprint using two independent signals:
//
//	inputTokens = max(queueBytes / BytesPerToken, queueSize * avgInputTokens)
//	             * (1 - prefixCacheHitRate)
//	outputTokens = queueSize * avgOutputTokens
//
// Role attribution:
//   - Prefill: inputTokens (prompt KV must be computed and stored)
//   - Decode:  inputTokens + outputTokens (receives KV transfer + generates output)
//   - Both:    inputTokens + outputTokens (handles full request lifecycle)
//   - Model-level total: inputTokens + outputTokens (unchanged for backward compat)
//
// The prefix cache hit rate reduces expected input token KV demand because
// a fraction of prompt tokens will hit the prefix cache and reuse existing
// KV blocks. This does NOT apply to the local vLLM queue (num_requests_waiting)
// because those requests have not yet had prefix cache lookup performed.
func estimateSchedulerQueueDemand(
	sq *interfaces.SchedulerQueueMetrics,
	replicaMetrics []interfaces.ReplicaMetrics,
	activeRoles map[string]bool,
) schedulerQueueDemand {
	if sq == nil || (sq.QueueSize == 0 && sq.QueueBytes == 0) {
		return schedulerQueueDemand{}
	}

	// Compute model-level averages from replica metrics
	avgInput, avgOutput, avgHitRate := computeModelWorkloadAverages(replicaMetrics)

	// Estimate input tokens from two signals, take the max for robustness
	tokensFromBytes := float64(sq.QueueBytes) / BytesPerToken
	tokensFromCount := float64(sq.QueueSize) * avgInput
	inputTokens := tokensFromBytes
	if tokensFromCount > inputTokens {
		inputTokens = tokensFromCount
	}

	// Apply prefix cache hit rate reduction to input tokens only
	inputTokens *= (1 - avgHitRate)

	// Estimate output tokens (no cache reduction — output must be generated)
	outputTokens := float64(sq.QueueSize) * avgOutput

	total := inputTokens + outputTokens

	// Build per-role attribution
	byRole := make(map[string]float64)
	if len(activeRoles) > 0 {
		for role := range activeRoles {
			switch role {
			case "prefill":
				byRole["prefill"] = inputTokens
			case "decode":
				byRole["decode"] = inputTokens + outputTokens
			default: // "both" or unknown
				byRole[role] = total
			}
		}
	}

	return schedulerQueueDemand{total: total, byRole: byRole}
}

// median returns the median value from a sorted slice of int64 values.
// Returns 0 if the slice is empty.
func median(values []int64) int64 {
	n := len(values)
	if n == 0 {
		return 0
	}

	sorted := make([]int64, n)
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}
