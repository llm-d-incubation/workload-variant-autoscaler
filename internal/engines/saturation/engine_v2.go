package saturation

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
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
	schedulerQueue *interfaces.SchedulerQueueMetrics,
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
		SchedulerQueue: schedulerQueue,
	}

	// 3. Run V2 analyzer
	result, err := e.saturationV2Analyzer.Analyze(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("V2 saturation analysis failed: %w", err)
	}

	// Analysis results are logged by the caller (runAnalyzersAndScore) after the
	// applyUniversalThreshold post-step, so the single Info line can include the
	// real RequiredCapacity/SpareCapacity. They are left zero here, so logging
	// them at this point would always report 0 and be misleading.
	return result, nil
}

// runAnalyzersAndScore runs the V2 saturation analyzer, applies the universal
// threshold post-step to every analyzer's result (using per-analyzer config
// overrides where set), and computes the weighted composite score from
// saturation's signal and the model's priority.
//
// The engine applies applyUniversalThreshold to every analyzer (saturation and
// all registered non-saturation analyzers) and collects the calibrated results
// into a per-analyzer slice returned to the optimizer. Saturation is always the
// first entry; it is the keeper of per-variant metadata (Cost, AcceleratorName,
// Role) until a future pre-analysis-extraction PR separates that concern.
func (e *Engine) runAnalyzersAndScore(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []interfaces.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	schedulerQueue *interfaces.SchedulerQueueMetrics,
) ([]pipeline.NamedAnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Run saturation analyzer (always needed for PerReplicaCapacity).
	baseResult, err := e.runV2AnalysisOnly(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, scaleTargets, variantAutoscalings, schedulerQueue)
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
		SchedulerQueue: schedulerQueue,
	}

	// Collect per-analyzer results. Saturation is first; each non-saturation
	// analyzer is run, calibrated with its resolved thresholds, and appended.
	namedResults := []pipeline.NamedAnalyzerResult{{
		Name:              interfaces.SaturationAnalyzerName,
		Result:            baseResult,
		Score:             scoreForAnalyzer(interfaces.SaturationAnalyzerName, config),
		Remaining:         baseResult.RequiredCapacity,
		Spare:             baseResult.SpareCapacity,
		ScaleUpThreshold:  satUp,
		ScaleDownBoundary: satDown,
	}}
	for _, entry := range e.analyzersSnapshot {
		if entry.name == interfaces.SaturationAnalyzerName {
			continue
		}
		if !effectiveEnabled(entry.name, config) {
			continue
		}
		result := runRegisteredAnalyzer(ctx, logger, entry, modelID, input)
		if result == nil {
			continue
		}
		up, down := resolveThresholds(entry.name, config)
		applyUniversalThreshold(result, up, down)
		namedResults = append(namedResults, pipeline.NamedAnalyzerResult{
			Name:              entry.name,
			Result:            result,
			Score:             scoreForAnalyzer(entry.name, config),
			Remaining:         result.RequiredCapacity,
			Spare:             result.SpareCapacity,
			ScaleUpThreshold:  up,
			ScaleDownBoundary: down,
		})
	}
	for _, nr := range namedResults {
		logAnalyzerResult(ctx, modelID, namespace, nr)
	}
	return namedResults, nil
}

// scoreForAnalyzer returns the AnalyzerScoreConfig.Score for the named analyzer,
// defaulting to 1.0 when the analyzer has no explicit entry in cfg.Analyzers.
// This value is the per-analyzer weight used by GreedyByScoreOptimizer for
// fair-share priority ordering across models.
func scoreForAnalyzer(analyzerName string, cfg config.SaturationScalingConfig) float64 {
	for _, aw := range cfg.Analyzers {
		if aw.Name == analyzerName {
			if aw.Score > 0 {
				return aw.Score
			}
			return 1.0
		}
	}
	return 1.0
}

func resolveThresholds(analyzerName string, cfg config.SaturationScalingConfig) (scaleUp, scaleDown float64) {
	for _, aw := range cfg.Analyzers {
		if aw.Name == analyzerName {
			return aw.EffectiveScaleUpThreshold(cfg.ScaleUpThreshold),
				aw.EffectiveScaleDownBoundary(cfg.ScaleDownBoundary)
		}
	}
	return cfg.ScaleUpThreshold, cfg.ScaleDownBoundary
}

// effectiveEnabled returns false only when the analyzer has an explicit
// Enabled:false entry in cfg.Analyzers. Absent entries and nil Enabled
// pointers default to true (consistent with ApplyDefaults).
func effectiveEnabled(analyzerName string, cfg config.SaturationScalingConfig) bool {
	for _, aw := range cfg.Analyzers {
		if aw.Name == analyzerName {
			if aw.Enabled != nil {
				return *aw.Enabled
			}
			return true
		}
	}
	return true
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
		var satEntry *interfaces.AnalyzerResult
		for _, e := range req.AnalyzerResults {
			if e.Name == interfaces.SaturationAnalyzerName {
				satEntry = e.Result
				break
			}
		}
		if satEntry == nil {
			continue
		}
		stateMap := make(map[string]interfaces.VariantReplicaState, len(req.VariantStates))
		for _, s := range req.VariantStates {
			stateMap[s.VariantName] = s
		}
		for _, vc := range satEntry.VariantCapacities {
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
	schedulerQueue *interfaces.SchedulerQueueMetrics,
) (*pipeline.ModelScalingRequest, error) {
	namedResults, err := e.runAnalyzersAndScore(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, scaleTargets, variantAutoscalings, schedulerQueue)
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
		ModelID:         modelID,
		Namespace:       namespace,
		AnalyzerResults: namedResults,
		VariantStates:   variantStates,
		Priority:        config.Priority,
		Disaggregated:   disaggregated,
	}, nil
}

// logAnalyzerResult emits one INFO "analyzer-result" line for a single named
// analyzer result. Called for every analyzer that ran in a model's reconcile
// cycle, after the universal threshold post-step has been applied.
func logAnalyzerResult(ctx context.Context, modelID, namespace string, nr pipeline.NamedAnalyzerResult) {
	if nr.Result == nil {
		return
	}
	logger := ctrl.LoggerFrom(ctx)

	type variantEntry struct {
		Name   string  `json:"name"`
		PRC    float64 `json:"prc"`
		Reason string  `json:"reason,omitempty"`
	}
	variants := make([]variantEntry, 0, len(nr.Result.VariantCapacities))
	for _, vc := range nr.Result.VariantCapacities {
		variants = append(variants, variantEntry{
			Name:   vc.VariantName,
			PRC:    vc.PerReplicaCapacity,
			Reason: vc.Reason,
		})
	}

	logger.Info("analyzer-result",
		"modelID", modelID,
		"namespace", namespace,
		"analyzer", nr.Name,
		"supply", nr.Result.TotalSupply,
		"demand", nr.Result.TotalDemand,
		"util", nr.Result.Utilization,
		"rc", nr.Result.RequiredCapacity,
		"sc", nr.Result.SpareCapacity,
		"scaleUpThreshold", nr.ScaleUpThreshold,
		"scaleDownBoundary", nr.ScaleDownBoundary,
		"variants", variants,
	)
}

// logScalingDecisions emits one INFO "scaling-decision" line per model after
// the optimizer has produced per-variant decisions.
func logScalingDecisions(
	ctx context.Context,
	modelRequests []pipeline.ModelScalingRequest,
	decisions []interfaces.VariantDecision,
) {
	logger := ctrl.LoggerFrom(ctx)

	type modelKey struct{ ns, modelID string }
	type decisionEntry struct {
		Name   string `json:"name"`
		Curr   int    `json:"curr"`
		Tgt    int    `json:"tgt"`
		Action string `json:"action"`
	}

	grouped := make(map[modelKey][]decisionEntry, len(modelRequests))
	for _, d := range decisions {
		k := modelKey{d.Namespace, d.ModelID}
		grouped[k] = append(grouped[k], decisionEntry{
			Name:   d.VariantName,
			Curr:   d.CurrentReplicas,
			Tgt:    d.TargetReplicas,
			Action: string(d.Action),
		})
	}

	for _, req := range modelRequests {
		k := modelKey{req.Namespace, req.ModelID}
		entries := grouped[k]
		if len(entries) == 0 {
			continue
		}
		logger.Info("scaling-decision",
			"modelID", req.ModelID,
			"namespace", req.Namespace,
			"decisions", entries,
		)
	}
}
