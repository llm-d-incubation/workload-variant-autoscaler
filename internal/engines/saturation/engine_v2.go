package saturation

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// runV2AnalysisOnly runs the V2 saturation analyzer and returns the raw AnalyzerResult
// without building targets or converting to V1 types. The optimizer will handle
// target building across all models.
func (e *Engine) runV2AnalysisOnly(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []interfaces.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (*interfaces.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// 1. Pre-populate capacity store with scale target-derived params
	for _, va := range variantAutoscalings {
		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		scaleTarget := scaleTargets[key]
		if scaleTarget == nil {
			logger.V(logging.DEBUG).Info("No scale target found for VA, skipping capacity store pre-population",
				"variant", va.Name, "scaleTargetKey", key)
			continue
		}
		// Get accelerator name from scale target nodeSelector/nodeAffinity or VA label
		accelerator := utils.GetAcceleratorNameFromScaleTarget(va, scaleTarget)
		gpuCount := scaleTarget.GetTotalGPUsPerReplica()
		e.capacityStore.LoadFromScaleTarget(namespace, modelID, va.Name, accelerator, gpuCount, scaleTarget)
		logger.V(logging.DEBUG).Info("Pre-populated capacity store from scale target",
			"variant", va.Name, "accelerator", accelerator, "gpuCount", gpuCount)
	}

	// 2. Build AnalyzerInput
	input := interfaces.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         &config,
		// TODO: populate SchedulerQueue when flow control metrics are collected
	}

	// 3. Run V2 analyzer
	result, err := e.saturationV2Analyzer.Analyze(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("V2 saturation analysis failed: %w", err)
	}

	logger.Info("V2 saturation analysis completed",
		"modelID", modelID,
		"totalSupply", result.TotalSupply,
		"totalDemand", result.TotalDemand,
		"utilization", result.Utilization,
		"requiredCapacity", result.RequiredCapacity,
		"spareCapacity", result.SpareCapacity)

	return result, nil
}

// runAnalyzersAndScore runs the V2 saturation analyzer, applies the universal
// threshold post-step to every analyzer's result (using per-analyzer config
// overrides where set), and computes the weighted composite score from
// saturation's signal and the model's priority.
//
// The engine applies applyUniversalThreshold to every analyzer — saturation and
// all registered non-saturation analyzers. Per-analyzer ScaleUpThreshold /
// ScaleDownBoundary overrides from AnalyzerScoreConfig are resolved and used for
// each analyzer's calibration call. Non-saturation results are calibrated but
// intentionally discarded on this branch; the optimizer only receives saturation's
// result. Combine semantics and per-analyzer score consumption land in a follow-up
// PR (multi-analyzer-optimizer).
func (e *Engine) runAnalyzersAndScore(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []interfaces.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (*interfaces.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Run saturation analyzer (always needed for PerReplicaCapacity).
	baseResult, err := e.runV2AnalysisOnly(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, scaleTargets, variantAutoscalings)
	if err != nil {
		return nil, err
	}

	// Universal threshold post-step for saturation: recalibrate RC/SC using the
	// resolved threshold for the saturation entry (per-analyzer override over global).
	satUp, satDown := resolveThresholds(interfaces.SaturationAnalyzerName, config)
	applyUniversalThreshold(baseResult, satUp, satDown)

	// Build AnalyzerInput once; shared by all non-saturation analyzers.
	// Note: &config has had saturation's per-entry threshold overrides applied
	// (the loop above). Non-saturation analyzers therefore receive the
	// saturation-adjusted config rather than the original. This is harmless
	// on this branch (their results are discarded), and the clean fix —
	// engine applies thresholds universally after each analyzer runs —
	// is tracked on multi-analyzer-threshold (PR #1228).
	input := interfaces.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         &config,
		// SchedulerQueue: nil — wired in a later PR
	}

	// Iterate every registered non-saturation analyzer. Each result is
	// calibrated with the universal threshold post-step (using per-analyzer
	// config overrides where set), then discarded on this branch — combine /
	// per-analyzer-result consumption lands in follow-up PRs.
	e.runRegisteredAnalyzers(ctx, logger, modelID, input, config)

	// Compute weighted score from enabled analyzers.
	// On this branch only saturation drives the score; non-saturation results
	// are not yet consumed by the optimizer (see multi-analyzer-optimizer PR).
	totalWeighted := 0.0
	for _, aw := range config.Analyzers {
		if aw.Enabled != nil && !*aw.Enabled {
			continue
		}
		if aw.Name == interfaces.SaturationAnalyzerName {
			totalWeighted += baseResult.RequiredCapacity * aw.Score
			// future: add "throughput", "slo" cases
		}
	}

	// Score = priority * weighted sum; used by GreedyByScoreOptimizer for
	// fair-share ordering across models. Computed from saturation's calibrated
	// RC only; will be recomputed per-analyzer in the multi-analyzer-optimizer PR.
	baseResult.Score = config.Priority * totalWeighted
	return baseResult, nil
}

// resolveThresholds returns the effective scaleUp and scaleDown threshold values
// for the named analyzer. If config.Analyzers contains an entry for that name,
// its per-entry overrides (ScaleUpThreshold / ScaleDownBoundary) take precedence
// over the model-level globals. Falls back to the global values when no matching
// entry exists or the override fields are nil.
func resolveThresholds(analyzerName string, cfg config.SaturationScalingConfig) (scaleUp, scaleDown float64) {
	for _, aw := range cfg.Analyzers {
		if aw.Name == analyzerName {
			return aw.EffectiveScaleUpThreshold(cfg.ScaleUpThreshold),
				aw.EffectiveScaleDownBoundary(cfg.ScaleDownBoundary)
		}
	}
	return cfg.ScaleUpThreshold, cfg.ScaleDownBoundary
}

// runRegisteredAnalyzers invokes Analyze on every registered non-saturation
// analyzer in registration order, reading from the frozen analyzersSnapshot
// built by StartOptimizeLoop. For each result the universal threshold post-step
// is applied using per-analyzer config overrides where set (falling back to the
// model-level globals). Results are discarded on this branch — combine logic
// lands in follow-up PRs. Saturation is skipped; it runs via runV2AnalysisOnly.
func (e *Engine) runRegisteredAnalyzers(
	ctx context.Context,
	logger logr.Logger,
	modelID string,
	input interfaces.AnalyzerInput,
	cfg config.SaturationScalingConfig,
) {
	for _, entry := range e.analyzersSnapshot {
		if entry.name == interfaces.SaturationAnalyzerName {
			continue
		}
		result := runRegisteredAnalyzer(ctx, logger, entry, modelID, input)
		if result != nil {
			up, down := resolveThresholds(entry.name, cfg)
			applyUniversalThreshold(result, up, down)
		}
		// result discarded: optimizer receives only saturation on this branch
	}
}

// runRegisteredAnalyzer invokes a single non-saturation analyzer's Analyze
// method, isolating the call from the rest of the cycle. Errors are logged
// and nil is returned; panics are recovered and logged. Returns the result
// so the caller can apply the universal threshold post-step before discarding.
func runRegisteredAnalyzer(
	ctx context.Context,
	logger logr.Logger,
	entry analyzerEntry,
	modelID string,
	input interfaces.AnalyzerInput,
) (result *interfaces.AnalyzerResult) {
	defer func() {
		if r := recover(); r != nil {
			// Plugin failure is non-fatal; log at debug to avoid spamming
			// operator logs every optimize cycle.
			logger.V(logging.DEBUG).Info("registered analyzer panicked; result discarded",
				"name", entry.name, "modelID", modelID, "panic", fmt.Sprintf("%v", r))
			result = nil
		}
	}()
	var err error
	result, err = entry.analyzer.Analyze(ctx, input)
	if err != nil {
		// Plugin failure is non-fatal; log at debug to avoid spamming
		// operator logs every optimize cycle.
		logger.V(logging.DEBUG).Info("registered analyzer failed; result discarded",
			"name", entry.name, "modelID", modelID, "error", err)
		return nil
	}
	return result
}

// applyUniversalThreshold recalibrates RequiredCapacity and SpareCapacity in
// place for the analyzer result and every RoleCapacity entry using the pure
// formula:
//
//	RC = max(0, TotalDemand/scaleUp − TotalAnticipatedSupply)
//	SC = max(0, TotalSupply    − TotalDemand/scaleDown)
//
// TotalAnticipatedSupply is read as-is — zero is a literal value, not a
// sentinel. The analyzer is responsible for populating it correctly (see
// internal/engines/aggregation for shared helpers). The asymmetry preserves
// the conservative "don't double-scale while replicas are launching, don't
// count pending as removable" stance.
//
// The same formula and the same (scaleUp, scaleDown) are applied at every
// scope — model-level and each RoleCapacity entry. There are no per-role
// threshold overrides. A non-positive scaleUp or scaleDown leaves the
// corresponding signal unchanged.
func applyUniversalThreshold(r *interfaces.AnalyzerResult, scaleUp, scaleDown float64) {
	if r == nil {
		return
	}

	if scaleUp > 0 {
		rc := r.TotalDemand/scaleUp - r.TotalAnticipatedSupply
		if rc < 0 {
			rc = 0
		}
		r.RequiredCapacity = rc
	}
	if scaleDown > 0 {
		sc := r.TotalSupply - r.TotalDemand/scaleDown
		if sc < 0 {
			sc = 0
		}
		r.SpareCapacity = sc
	}

	for role, rc := range r.RoleCapacities {
		if scaleUp > 0 {
			v := rc.TotalDemand/scaleUp - rc.TotalAnticipatedSupply
			if v < 0 {
				v = 0
			}
			rc.RequiredCapacity = v
		}
		if scaleDown > 0 {
			v := rc.TotalSupply - rc.TotalDemand/scaleDown
			if v < 0 {
				v = 0
			}
			rc.SpareCapacity = v
		}
		r.RoleCapacities[role] = rc
	}
}

// computeCurrentGPUUsage iterates over model scaling requests to compute the
// current GPU usage per accelerator type. Used to provide current usage to
// the ConstraintProvider when building GPU constraints for the optimizer.
func computeCurrentGPUUsage(requests []pipeline.ModelScalingRequest) map[string]int {
	usage := make(map[string]int)
	for _, req := range requests {
		if req.Result == nil {
			continue
		}
		stateMap := make(map[string]interfaces.VariantReplicaState, len(req.VariantStates))
		for _, s := range req.VariantStates {
			stateMap[s.VariantName] = s
		}
		for _, vc := range req.Result.VariantCapacities {
			state := stateMap[vc.VariantName]
			gpusPerReplica := state.GPUsPerReplica
			if gpusPerReplica <= 0 {
				gpusPerReplica = 1
			}
			usage[vc.AcceleratorName] += state.CurrentReplicas * gpusPerReplica
		}
	}
	return usage
}

// collectV2ModelRequest performs V2 analysis for a single model and returns
// a ModelScalingRequest for the optimizer, or nil if analysis should be skipped.
func (e *Engine) collectV2ModelRequest(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []interfaces.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (*pipeline.ModelScalingRequest, error) {
	result, err := e.runAnalyzersAndScore(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, scaleTargets, variantAutoscalings)
	if err != nil {
		return nil, fmt.Errorf("collecting V2 model request for %s/%s: %w", namespace, modelID, err)
	}

	// Detect P/D disaggregation: true when any variant has role != interfaces.RoleBoth
	disaggregated := false
	for _, vs := range variantStates {
		if vs.Role != "" && vs.Role != interfaces.RoleBoth {
			disaggregated = true
			break
		}
	}

	return &pipeline.ModelScalingRequest{
		ModelID:       modelID,
		Namespace:     namespace,
		Result:        result,
		VariantStates: variantStates,
		Priority:      config.Priority,
		Disaggregated: disaggregated,
	}, nil
}
