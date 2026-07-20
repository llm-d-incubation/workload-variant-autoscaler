/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package saturation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	actuator "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/actuator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
	queueingmodel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/queueingmodel"
	saturation_v2 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation_v2"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// analyzerEntry binds a registered analyzer to its name. The engine stores
// these in registration order so runAnalyzersAndScore iterates analyzers
// deterministically.
type analyzerEntry struct {
	name     string
	analyzer interfaces.Analyzer
}

// v1Analyzer is the minimal surface of *saturation.Analyzer that optimizeV1
// depends on. Defined here so tests can substitute a stub via Engine's
// v1AnalyzerFactory field without exposing a public interface.
type v1Analyzer interface {
	AnalyzeModelSaturation(
		ctx context.Context,
		modelID, namespace string,
		replicaMetrics []interfaces.ReplicaMetrics,
		config config.SaturationScalingConfig,
	) (*interfaces.ModelSaturationAnalysis, error)
	CalculateSaturationTargets(
		ctx context.Context,
		saturationAnalysis *interfaces.ModelSaturationAnalysis,
		variantStates []interfaces.VariantReplicaState,
	) map[string]int
}

// defaultV1AnalyzerFactory returns a fresh production V1 saturation analyzer.
// NewEngine wires this into Engine.v1AnalyzerFactory; tests can swap the
// factory per-instance without touching shared state.
func defaultV1AnalyzerFactory() v1Analyzer { return saturation.NewAnalyzer() }

// safetyNetEmitter reports a per-role analysis failure to the saturation
// engine's safety-net metrics path. Extracted as a function type so the
// per-role loop can be unit-tested with a spy.
type safetyNetEmitter func(
	ctx context.Context,
	roleVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
)

// resolveSaturationConfig resolves config for a model.
// Starts from the "default" entry (or zero-value), then merges the model-specific
// override "{modelID}#{namespace}" on top (if present). This allows per-model
// overrides to specify only the fields they want to change.
// ApplyDefaults is called last to fill any remaining zero-valued fields.
func resolveSaturationConfig(
	configMap map[string]config.SaturationScalingConfig,
	modelID, namespace string,
) config.SaturationScalingConfig {
	// Start with default config as base
	base := config.SaturationScalingConfig{}
	if defaultCfg, ok := configMap["default"]; ok {
		base = defaultCfg
	}
	// Overlay model-specific override if present (non-zero fields win)
	if override, ok := configMap[modelID+"#"+namespace]; ok {
		base.Merge(override)
	}
	base.ApplyDefaults()
	return base
}

type Engine struct {
	client   client.Client
	scheme   *runtime.Scheme
	executor executor.Executor

	// Recorder - use wrapper function recordEvent to limit number of events per va in an optimization cycle
	Recorder record.EventRecorder

	// vaEventTracker tracks whether a K8S event has been issued for a variant in an optimization cycle.
	// Key is namespace/name from utils.GetNamespacedKey.
	vaEventTracker map[string]bool

	Config *config.Config // Unified configuration (injected from main.go)

	// ReplicaMetricsCollector is the collector for replica metrics using the source infrastructure
	ReplicaMetricsCollector *collector.ReplicaMetricsCollector

	// ScaleToZeroEnforcer applies scale-to-zero and minimum replica enforcement
	ScaleToZeroEnforcer *pipeline.Enforcer

	// GPULimiter constrains scaling decisions based on available GPU resources.
	// Only applied when EnableLimiter is true in the saturation config.
	GPULimiter pipeline.Limiter

	// metricsRegistry is used to access metrics sources for request count queries
	metricsRegistry *source.SourceRegistry

	// saturationV2Analyzer is the V2 token-based saturation analyzer (initialized once).
	// Also pre-registered in analyzers under interfaces.SaturationAnalyzerName.
	// Typed as interfaces.Analyzer to allow injection in tests.
	saturationV2Analyzer interfaces.Analyzer

	// queueingModelAnalyzer is the queueing model-based analyzer (initialized once).
	// Selected via analyzerName: "queueing-model" in SaturationScalingConfig.
	queueingModelAnalyzer *queueingmodel.QueueingModelAnalyzer

	// capacityStore is shared with the V2 analyzer for caching capacity knowledge.
	capacityStore *saturation_v2.CapacityKnowledgeStore

	// analyzers is the engine's analyzer registry, mutated only during setup
	// (NewEngine + RegisterAnalyzer). After StartOptimizeLoop it is frozen —
	// further RegisterAnalyzer calls return an error. The optimize goroutine reads
	// analyzersSnapshot, never analyzers, so iteration is race-free without
	// runtime locking.
	analyzers []analyzerEntry

	// analyzersSnapshot is the frozen, registration-ordered view that
	// runAnalyzersAndScore iterates. Built from analyzers in StartOptimizeLoop
	// before the goroutine launches. Saturation always runs and drives scaling
	// decisions; other registered analyzers are invoked but their results are
	// not consumed yet — combine and per-analyzer threshold logic lands in
	// follow-up PRs.
	analyzersSnapshot []analyzerEntry

	// started transitions to true in StartOptimizeLoop. Late RegisterAnalyzer
	// calls return an error so the contract "register before Start" is enforced
	// rather than just documented.
	started bool

	// optimizer is the V2 scaling optimizer that produces VariantDecisions from
	// AnalyzerResults. Selected per-cycle based on enableLimiter config:
	// CostAwareOptimizer (unlimited) or GreedyByScoreOptimizer (limited).
	optimizer pipeline.ScalingOptimizer

	metricsEmitter *metrics.MetricsEmitter
	// v1AnalyzerFactory produces a fresh V1 saturation analyzer for each
	// role group in optimizeV1. NewEngine sets defaultV1AnalyzerFactory;
	// tests can replace this per-instance to inject stubs or spies.
	v1AnalyzerFactory func() v1Analyzer
}

// NewEngine creates a new instance of the saturation engine.
// Config must be non-nil (validated in main.go before engine creation).
// Panics if cfg is nil to fail fast on programming errors.
func NewEngine(client client.Client, apiReader client.Reader, scheme *runtime.Scheme, recorder record.EventRecorder, metricsRegistry *source.SourceRegistry, cfg *config.Config) *Engine {
	if cfg == nil {
		panic("config is nil in NewEngine - this should not happen (validated in main.go before engine creation)")
	}
	promSource := metricsRegistry.Get("prometheus") // assume prometheus source is registered

	// Create request count function wrapper for scale-to-zero enforcer
	requestCountFunc := func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
		return registration.CollectModelRequestCount(ctx, promSource, modelID, namespace, retentionPeriod)
	}

	// Create GPU limiter with TypeInventory and GreedyBySaturation algorithm
	gpuDiscovery := discovery.NewK8sWithGpuOperator(client)
	gpuInventory := pipeline.NewTypeInventoryWithUsage("cluster-gpu-inventory", gpuDiscovery)
	gpuAlgorithm := pipeline.NewGreedyBySaturation()
	gpuLimiter := pipeline.NewDefaultLimiter("gpu-limiter", gpuInventory, gpuAlgorithm)

	capacityStore := saturation_v2.NewCapacityKnowledgeStore()
	satV2 := saturation_v2.NewSaturationAnalyzer(capacityStore)

	// Initialize with default optimizer. The actual optimizer is selected
	// per-cycle in optimize() based on dynamic config (enableLimiter flag
	// from ConfigMap), since config arrives after engine init.
	var scalingOptimizer pipeline.ScalingOptimizer = pipeline.NewCostAwareOptimizer()

	podLocator, err := locator.New(client, apiReader)
	if err != nil {
		// locator.New only fails when defaultCacheSize <= 0, which is a
		// programming error we cannot recover from at runtime.
		panic(fmt.Sprintf("locator.New: %v", err))
	}

	engine := Engine{
		client:                  client,
		scheme:                  scheme,
		Recorder:                recorder,
		Config:                  cfg,
		ReplicaMetricsCollector: collector.NewReplicaMetricsCollector(promSource, client, recorder, podLocator),
		ScaleToZeroEnforcer:     pipeline.NewEnforcer(requestCountFunc),
		GPULimiter:              gpuLimiter,
		metricsRegistry:         metricsRegistry,
		saturationV2Analyzer:    satV2,
		queueingModelAnalyzer:   queueingmodel.NewQueueingModelAnalyzer(),
		capacityStore:           capacityStore,
		optimizer:               scalingOptimizer,
		metricsEmitter:          metrics.NewMetricsEmitter(),
		v1AnalyzerFactory:       defaultV1AnalyzerFactory,
		analyzers: []analyzerEntry{
			{name: interfaces.SaturationAnalyzerName, analyzer: satV2},
		},
	}

	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     30 * time.Second,
		RetryBackoff: 100 * time.Millisecond,
	})

	// Register saturation queries in the metrics registry.
	// Both V1 (percentage-based) and V2 (token-based) analyzers share the same
	// base queries (kv_cache_usage, queue_length). V2-specific queries
	// (cache_config_info, avg_output_tokens, etc.) are registered but unused
	// when V1 is active — they're just query templates with no runtime cost.
	registration.RegisterSaturationQueries(metricsRegistry)

	// Register scale-to-zero queries in the metrics registry
	registration.RegisterScaleToZeroQueries(metricsRegistry)

	// Register queueing model queries (scheduler dispatch rate per endpoint).
	// These are collected alongside saturation metrics into the shared
	// ReplicaMetrics struct and used by the queueing model analyzer to
	// estimate per-replica arrival rate and model queue behavior.
	registration.RegisterQueueingModelQueries(metricsRegistry)

	return &engine
}

// RegisterAnalyzer adds an external analyzer to the engine's analyzer
// registry. Returns an error if called after StartOptimizeLoop or if name
// is already registered — callers must check the error. The analyzer is
// appended in registration order.
func (e *Engine) RegisterAnalyzer(name string, a interfaces.Analyzer) error {
	if e.started {
		return errors.New("RegisterAnalyzer: called after StartOptimizeLoop")
	}
	for i := range e.analyzers {
		if e.analyzers[i].name == name {
			return fmt.Errorf("RegisterAnalyzer: duplicate analyzer name %q", name)
		}
	}
	e.analyzers = append(e.analyzers, analyzerEntry{name: name, analyzer: a})
	return nil
}

// StartOptimizeLoop starts the optimization loop for the saturation engine.
// It runs until the context is cancelled.
//
// Before launching the goroutine, the registered analyzers are snapshotted
// to a frozen slice that runAnalyzersAndScore iterates. The started flag is
// flipped so subsequent RegisterAnalyzer calls return an error. The snapshot is the
// natural place to invoke any future per-analyzer Init(ctx) hook.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.analyzersSnapshot = make([]analyzerEntry, len(e.analyzers))
	copy(e.analyzersSnapshot, e.analyzers)
	e.started = true

	e.recordActiveOptimizer() // record active optimizer
	metrics.SetConfigOptimizationInterval(float64(e.Config.OptimizationInterval().Seconds()))
	e.executor.Start(ctx)
}

func (e *Engine) recordActiveOptimizer() {
	// Record metrics for which optimizer is active
	optimizerNames := []string{"greedy-by-score", "cost-aware"}
	for _, name := range optimizerNames {
		isActive := false // default is false
		if name == e.optimizer.Name() {
			isActive = true // only one active at a time
		}
		e.metricsEmitter.RecordOptimizerActiveMetric(name, isActive)
	}
}

func (e *Engine) recordDefaultConfigMetrics() {
	metrics.SetConfigOptimizationInterval(float64(e.Config.OptimizationInterval().Seconds()))

	globalSatCfgMap := e.Config.SaturationConfig()
	// record global default config
	if cfg, ok := globalSatCfgMap["default"]; ok {
		metrics.SetConfigKvSpareThreshold(cfg.KvSpareTrigger)
		metrics.SetConfigQueueSpareThreshold(cfg.QueueSpareTrigger)
		metrics.SetConfigInfo(cfg.GetAnalyzerName(), cfg.EnableLimiter, e.Config.ScaleToZeroEnabled())
	}
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) (retErr error) {
	start := time.Now()
	var modelsProcessed int
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.ObserveOptimizationDuration(time.Since(start).Seconds(), status)
		metrics.SetModelsProcessed(modelsProcessed)
	}()

	logger := ctrl.LoggerFrom(ctx)
	e.recordDefaultConfigMetrics() // record as soon as possible to reflect any changes in configuration

	// Get optimization interval from Config (already a time.Duration)
	interval := e.Config.OptimizationInterval()

	// Update the executor interval if changed
	// Note: simple polling executor might not support dynamic interval update easily without restart,
	// but here we just check it. The original code used RequeueAfter.
	// The PollingExecutor uses fixed interval.
	// TODO: Support dynamic interval in Executor if needed. For now, we log and proceed.
	if interval > 0 {
		// e.executor.SetInterval(interval) // If supported
		_ = interval
	}

	if e.Config.ScaleToZeroEnabled() {
		logger.Info("Scaling to zero is enabled")
	}

	activeVAs, _, err := utils.ActiveVariantAutoscaling(ctx, e.client)
	if err != nil {
		logger.Error(err, "Unable to get active variant autoscalings")
		return err
	}

	if len(activeVAs) == 0 {
		logger.Info("No active VariantAutoscalings found, skipping optimization")
		return nil
	}

	// Initialize vaEventTracker for this optimize cycle
	e.vaEventTracker = make(map[string]bool)

	// Collected accelerator inventory (only in limited mode)
	if e.Config.LimitedModeEnabled() {
		inventory, err := collector.CollectInventoryK8S(ctx, e.client)
		if err != nil {
			logger.Error(err, "Failed to collect cluster inventory")
			// do not proceed to optimization if inventory collection fails in limited mode
			return err
		}
		// always print inventory until optimizer consumes it
		logger.Info("Collected cluster accelerator inventory (Limited Mode)", "inventory", inventory)
	}

	// Group VAs by model for per-model capacity analysis
	modelGroups := utils.GroupVariantAutoscalingByModel(activeVAs)
	modelsProcessed = len(modelGroups)
	logger.Info("Grouped VAs by model",
		"modelCount", len(modelGroups),
		"totalVAs", len(activeVAs))

	// Create VA lookup map for applySaturationDecisions (used to access VA status and update decisions)
	// Use namespace/vaName as key to avoid collisions when multiple namespaces have same VA name
	// Use slice index directly to avoid pointer-to-loop-variable bug
	vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, len(activeVAs))
	for i := range activeVAs {
		vaMap[utils.GetNamespacedKey(activeVAs[i].Namespace, activeVAs[i].Name)] = &activeVAs[i]
	}

	// Create map to store current allocations populated during metrics collection
	// Keyed by VariantAutoscaling Namespace/Name
	currentAllocations := make(map[string]*interfaces.Allocation)

	// Determine which analyzer to use.
	// Priority: queueing model ConfigMap (presence-based) > saturation config analyzerName.
	// If wva-queueing-model-config exists with a "default" entry, the queueing model
	// analyzer is active regardless of the saturation config's analyzerName field.
	qmConfigMap := e.Config.QMAnalyzerConfig()
	_, hasQMAnalyzerConfig := qmConfigMap["default"]

	// Read saturation config for fallback analyzer selection and limiter flag.
	globalSatCfgMap := e.Config.SaturationConfig()
	analyzerName := ""
	enableLimiter := false
	if cfg, ok := globalSatCfgMap["default"]; ok {
		cfg.ApplyDefaults()
		analyzerName = cfg.GetAnalyzerName()
		enableLimiter = cfg.EnableLimiter
	}

	// Queueing model ConfigMap takes priority over saturation analyzerName.
	if hasQMAnalyzerConfig {
		analyzerName = interfaces.QueueingModelAnalyzerName
	}

	// Select optimizer based on enableLimiter flag (both are stateless, safe to swap)
	// Applies to V2 and queueing-model paths which both use the optimizer pipeline.
	if analyzerName == interfaces.SaturationAnalyzerName || analyzerName == interfaces.QueueingModelAnalyzerName {
		savedOptimizer := e.optimizer
		if enableLimiter {
			e.optimizer = pipeline.NewGreedyByScoreOptimizer()
		} else {
			e.optimizer = pipeline.NewCostAwareOptimizer()
		}
		if savedOptimizer != e.optimizer {
			e.recordActiveOptimizer() // optimizer has changed, record active optimizer
		}
		logger.V(logging.DEBUG).Info("Optimizer selected", "analyzer", analyzerName, "optimizer", e.optimizer.Name(), "enableLimiter", enableLimiter)
	}

	var allDecisions []interfaces.VariantDecision

	// Each analyzer has a separate optimize path because they use fundamentally
	// different analysis types and target-building flows:
	//   - V1: saturation.Analyzer → ModelSaturationAnalysis → CalculateSaturationTargets → Enforcer → Limiter
	//   - V2 (saturation): saturation_v2.Analyzer → AnalyzerResult → Optimizer.Optimize → Enforcer bridge
	//   - Queueing model: QueueingModelAnalyzer → AnalyzerResult → Optimizer.Optimize → Enforcer bridge
	// V1 will be deprecated once V2 is fully validated.
	// Queueing model is activated by presence of wva-queueing-model-config ConfigMap.
	switch analyzerName {
	case interfaces.QueueingModelAnalyzerName:
		allDecisions = e.optimizeQueueingModel(ctx, modelGroups, currentAllocations)
	case interfaces.SaturationAnalyzerName:
		allDecisions = e.optimizeV2(ctx, modelGroups, currentAllocations)
	default:
		allDecisions = e.optimizeV1(ctx, modelGroups, currentAllocations)
	}

	// STEP 3: Apply decisions and update VA status
	// Always call applySaturationDecisions, even with empty decisions.
	// This function also updates VA.Status.CurrentAlloc with collected metrics
	// and emits HPA metrics, which must happen every reconciliation cycle.
	if len(allDecisions) > 0 {
		logger.Info("Applying scaling decisions",
			"totalDecisions", len(allDecisions))
	} else {
		logger.Info("No scaling decisions to apply, updating VA status with metrics")
	}
	e.applySaturationDecisions(ctx, allDecisions, vaMap, currentAllocations)

	logger.Info("Optimization completed successfully",
		"mode", "saturation-only",
		"modelsProcessed", len(modelGroups),
		"decisionsApplied", len(allDecisions))

	return nil
}

// recordEvent ensures only one event is recorded per VA in an optimization cycle.
// Exception: K8SEventResourceConstrained events bypass deduplication and can be
// recorded alongside other event types (e.g., ScaledUp + ResourceConstrained).
func (e *Engine) recordEvent(
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	eventType, reason, message string,
) {
	if e.Recorder == nil {
		return
	}

	if reason == constants.K8SEventResourceConstrained {
		// This is the only exception where a variant can have 2 K8S events in an optimize cycle: K8SEventScaledUp & K8SEventResourceConstrained
		e.Recorder.Event(va, eventType, reason, message)
		return
	}

	key := utils.GetNamespacedKey(va.Namespace, va.Name)
	if e.vaEventTracker != nil {
		if _, ok := e.vaEventTracker[key]; ok { // ensures only one event is recorded per VA
			return
		}
	}
	e.Recorder.Event(va, eventType, reason, message)
	if e.vaEventTracker != nil {
		e.vaEventTracker[key] = true
	}
}

func (e *Engine) recordOptimizationFailedEvent(
	variantAutoscalings []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	message string,
) {
	if e.Recorder == nil {
		return
	}
	for _, va := range variantAutoscalings {
		e.recordEvent(&va, corev1.EventTypeWarning, constants.K8SEventOptimizationFailed, message)
	}
}

func (e *Engine) recordScalingEvent(
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	action interfaces.SaturationAction,
	targetReplicas int,
	reason string,
) {
	if e.Recorder == nil {
		return
	}
	switch action {
	case interfaces.ActionScaleUp:
		e.recordEvent(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, reason)
	case interfaces.ActionScaleDown:
		if targetReplicas == 0 {
			e.recordEvent(va, corev1.EventTypeNormal, constants.K8SEventScaledToZero, reason)
		} else {
			e.recordEvent(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, reason)
		}
	}
}

// Resolve saturation config and record config metrics
func (e *Engine) resolveSaturationConfig(
	configMap map[string]config.SaturationScalingConfig,
	modelID, namespace string,
) config.SaturationScalingConfig {
	return resolveSaturationConfig(configMap, modelID, namespace)
}

// optimizeV1 runs the V1 percentage-based saturation analysis path (saturation-percentage-based).
// Processes each model independently: analyze → enforce → convert → limiter.
//
// For P/D disaggregation: within each model, variants are sub-grouped by role
// (prefill, decode, both). Each role group gets its own saturation analysis,
// transition blocking, and scale-up/down decisions. This ensures that a prefill
// variant transitioning doesn't block decode scaling, and that spare-capacity
// averaging doesn't mix semantically different workload stages.
func (e *Engine) optimizeV1(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)
	var allDecisions []interfaces.VariantDecision

	for groupKey, modelVAs := range modelGroups {
		modelID := modelVAs[0].Spec.ModelID
		namespace := modelVAs[0].Namespace
		logger.Info("Processing model (V1)",
			"modelID", modelID,
			"namespace", namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		// Get namespace-aware saturation config (namespace-local > global)
		saturationConfigMap := e.Config.SaturationConfigForNamespace(namespace)
		if len(saturationConfigMap) == 0 {
			logger.Info("Saturation scaling config not loaded yet for namespace, skipping model",
				"namespace", namespace,
				"modelID", modelID)
			continue
		}

		saturationConfig := e.resolveSaturationConfig(saturationConfigMap, modelID, namespace)

		// Prepare model data once per model (single metrics collection pass).
		data, err := e.prepareModelData(ctx, modelID, modelVAs, e.client)

		if err != nil {
			msg := "Saturation data preparation failed"
			logger.Error(err, msg, "modelID", modelID)
			e.recordOptimizationFailedEvent(modelVAs, msg)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, nil)
			continue
		}
		if data == nil {
			e.recordOptimizationFailedEvent(modelVAs, "No saturation metrics available for model")
			logger.Info("No saturation metrics available for model, skipping analysis",
				"modelID", modelID, "namespace", namespace)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, nil)
			continue
		}

		// Per-role saturation analysis: each role group gets independent
		// analysis, transition blocking, and scale-up/down decisions.
		modelDecisions := e.analyzeRoleGroups(
			ctx, modelID, namespace, saturationConfig,
			data, modelVAs, currentAllocations,
			e.emitSafetyNetMetrics,
		)

		// Scale-to-zero enforcement is applied per MODEL (all roles together),
		// not per role group. In P/D deployments, scaling prefill to zero while
		// keeping decode (or vice versa) makes the model non-functional — both
		// stages must scale together.
		e.applyScaleToZeroEnforcement(
			ctx, modelID, namespace, "v1-saturation",
			modelDecisions, data.scaleTargets, data.variantStates,
		)

		allDecisions = append(allDecisions, modelDecisions...)
	}

	// Apply GPU limiter if enabled
	// Note: Limiter uses global saturation config since it's applied globally to all decisions
	globalSaturationConfigMap := e.Config.SaturationConfig()
	var globalSaturationConfig config.SaturationScalingConfig
	if len(globalSaturationConfigMap) > 0 {
		if cfg, ok := globalSaturationConfigMap["default"]; ok {
			globalSaturationConfig = cfg
		}
	}
	if globalSaturationConfig.EnableLimiter && len(allDecisions) > 0 {
		logger.Info("Applying GPU limiter to scaling decisions",
			"decisionCount", len(allDecisions))

		decisionPtrs := make([]*interfaces.VariantDecision, len(allDecisions))
		for i := range allDecisions {
			decisionPtrs[i] = &allDecisions[i]
		}

		if err := e.GPULimiter.Limit(ctx, decisionPtrs); err != nil {
			// skip record K8S events since there's no VA
			logger.Error(err, "GPU limiter failed, proceeding with original decisions")
		} else {
			for _, d := range decisionPtrs {
				if d.WasLimited {
					logger.Info("Decision was limited by GPU availability",
						"variant", d.VariantName,
						"originalTarget", d.OriginalTargetReplicas,
						"limitedTarget", d.TargetReplicas,
						"limitedBy", d.LimitedBy)
				}
			}
		}
	}

	return allDecisions
}

// analyzeRoleGroups runs the per-role saturation analysis loop for one model.
// It sub-groups variants by role (prefill/decode/both), runs an independent
// saturation analysis per role group, converts each group's targets to
// VariantDecisions, and returns the merged set for model-level scale-to-zero
// enforcement.
//
// A fresh analyzer is created per group via e.v1AnalyzerFactory
// (overridable in tests). On per-role analysis failure, emitSafetyNet is
// invoked with that role's VAs only so the failure does not poison sibling
// role groups.
func (e *Engine) analyzeRoleGroups(
	ctx context.Context,
	modelID, namespace string,
	saturationConfig config.SaturationScalingConfig,
	data *modelData,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
	emitSafetyNet safetyNetEmitter,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)

	// Sub-group variants by role for P/D-aware analysis.
	// Each role group gets its own saturation analysis and transition blocking
	// so prefill and decode pipelines scale independently.
	roleGroups := groupByRole(data.variantStates)
	sortedRoles := sortedRoleKeys(roleGroups)

	var modelDecisions []interfaces.VariantDecision
	for _, role := range sortedRoles {
		roleStates := roleGroups[role]
		roleMetrics := filterReplicaMetricsByVariants(data.replicaMetrics, roleStates)

		if len(roleMetrics) == 0 {
			// A role group with states but no metrics is a new partial case
			// introduced by P/D grouping (prefill has metrics, decode does
			// not, or vice versa). Emit safety-net signals for this role's
			// VAs so HPA/KEDA doesn't go dark while we wait for metrics to
			// appear; model-level scale-to-zero still sees sibling-group
			// decisions unchanged.
			logger.Info("No metrics for role group, emitting safety-net signals",
				"modelID", modelID, "role", role,
				"variants", variantNames(roleStates))
			roleVAs := filterVAsByVariantStates(modelVAs, roleStates)
			emitSafetyNet(ctx, roleVAs, currentAllocations, data.scaleTargets)
			continue
		}

		// A new analyzer instance is cheap (stateless struct); avoids shared
		// state between role groups.
		roleAnalyzer := e.v1AnalyzerFactory()
		saturationAnalysis, err := roleAnalyzer.AnalyzeModelSaturation(
			ctx, modelID, namespace, roleMetrics, saturationConfig)
		if err != nil {
			logger.Error(err, "Saturation analysis failed for role group",
				"modelID", modelID, "role", role)
			// Scope safety-net emission to this role group's variants only so
			// a failure in one stage does not trigger fallback metrics for a
			// healthy sibling stage.
			roleVAs := filterVAsByVariantStates(modelVAs, roleStates)
			emitSafetyNet(ctx, roleVAs, currentAllocations, data.scaleTargets)
			continue
		}

		logger.Info("Saturation analysis completed",
			"modelID", modelID,
			"role", role,
			"totalReplicas", saturationAnalysis.TotalReplicas,
			"nonSaturated", saturationAnalysis.NonSaturatedCount,
			"avgSpareKv", saturationAnalysis.AvgSpareKvCapacity,
			"avgSpareQueue", saturationAnalysis.AvgSpareQueueLength,
			"shouldScaleUp", saturationAnalysis.ShouldScaleUp,
			"scaleUpReason", saturationAnalysis.ScaleUpReason,
			"scaleDownSafe", saturationAnalysis.ScaleDownSafe)

		// Calculate targets and convert to decisions (transition blocking is per role group)
		saturationTargets := roleAnalyzer.CalculateSaturationTargets(ctx, saturationAnalysis, roleStates)
		roleDecisions := e.convertSaturationTargetsToDecisions(ctx, saturationTargets, saturationAnalysis, roleStates)

		logger.Info("Saturation-only decisions made for role group",
			"modelID", modelID, "role", role,
			"decisionCount", len(roleDecisions))
		modelDecisions = append(modelDecisions, roleDecisions...)
	}

	return modelDecisions
}

// selectV2Optimizer chooses the optimizer and GPU constraints for a V2
// optimization cycle.
//
// When the configured optimizer is GreedyByScore it is GPU-aware and expects
// per-type resource constraints. Those constraints are computed from the GPU
// limiter's inventory. GreedyByScore interprets absent constraints as zero
// available capacity (deny-all), NOT as unlimited — so if it were run with
// empty constraints it would silently suppress all scale-up.
//
// Therefore, whenever real constraints cannot be obtained — the limiter does
// not provide constraints (no ConstraintProvider), or computing them failed
// because, e.g., node objects are not readable on this cluster — we fall back
// to the cost-aware optimizer, which is the engine's unlimited path (the same
// optimizer used when enableLimiter is false), so scale-up proceeds
// unconstrained instead of being blocked. A constraint that is present but
// reports zero GPUs is left intact:
// that is a genuine "no capacity" signal and should still block scale-up.
func (e *Engine) selectV2Optimizer(
	ctx context.Context,
	requests []pipeline.ModelScalingRequest,
) (pipeline.ScalingOptimizer, []*pipeline.ResourceConstraints) {
	logger := ctrl.LoggerFrom(ctx)

	// GreedyByScore is currently the only GPU-aware optimizer; any future
	// constraint-consuming optimizer must be added to this guard.
	optimizer := e.optimizer
	if _, ok := optimizer.(*pipeline.GreedyByScoreOptimizer); !ok {
		return optimizer, nil
	}

	provider, ok := e.GPULimiter.(pipeline.ConstraintProvider)
	if !ok {
		return pipeline.NewCostAwareOptimizer(), nil
	}

	constraint, err := provider.ComputeConstraints(ctx, computeCurrentGPUUsage(requests))
	if err != nil {
		logger.Error(err, "Failed to compute GPU constraints, falling back to unlimited (cost-aware) optimizer for this cycle")
		return pipeline.NewCostAwareOptimizer(), nil
	}
	return optimizer, []*pipeline.ResourceConstraints{constraint}
}

// optimizeV2 runs the V2 token-based optimizer path (saturation-token-based).
// Collects AnalyzerResults for all models, calls the optimizer once, then applies enforcer per-model.
func (e *Engine) optimizeV2(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)

	// Stage 1: Collect ModelScalingRequests for all models
	requests := make([]pipeline.ModelScalingRequest, 0, len(modelGroups))
	// modelReplicaMetrics collects per-model replica metrics for KV token enrichment
	modelReplicaMetrics := make(map[string][]interfaces.ReplicaMetrics)
	// modelScaleTargets carries each model's scale targets into stage 3, where
	// applyScaleToZeroEnforcement needs them to gate the enforcer. Captured here
	// because data.scaleTargets is only in scope during this collection loop.
	modelScaleTargets := make(map[string]map[string]scaletarget.ScaleTargetAccessor)

	for groupKey, modelVAs := range modelGroups {
		modelID := modelVAs[0].Spec.ModelID
		namespace := modelVAs[0].Namespace
		logger.Info("Processing model (V2)",
			"modelID", modelID,
			"namespace", namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		// Get namespace-aware saturation config
		saturationConfigMap := e.Config.SaturationConfigForNamespace(namespace)
		if len(saturationConfigMap) == 0 {
			logger.Info("Saturation scaling config not loaded yet for namespace, skipping model",
				"namespace", namespace, "modelID", modelID)
			continue
		}

		saturationConfig := e.resolveSaturationConfig(saturationConfigMap, modelID, namespace)
		data, err := e.prepareModelData(ctx, modelID, modelVAs, e.client)
		if err != nil {
			msg := "Model data preparation failed"
			logger.Error(err, msg, "modelID", modelID)
			e.recordOptimizationFailedEvent(modelVAs, msg)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, nil)
			continue
		}
		if data == nil {
			logger.V(logging.DEBUG).Info("Skipping model: no metrics available", "modelID", modelID)
			continue
		}

		req, err := e.collectV2ModelRequest(ctx, modelID, namespace,
			data.replicaMetrics, saturationConfig, data.variantStates,
			data.scaleTargets, data.variantAutoscalings, data.schedulerQueue)
		if err != nil {
			msg := "V2 analysis failed"
			logger.Error(err, msg, "modelID", modelID)
			e.recordOptimizationFailedEvent(modelVAs, msg)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, data.scaleTargets)
			continue
		}

		requests = append(requests, *req)
		modelReplicaMetrics[modelID] = data.replicaMetrics
		modelScaleTargets[utils.GetNamespacedKey(namespace, modelID)] = data.scaleTargets
	}

	if len(requests) == 0 {
		return nil
	}

	// Stage 2: Compute GPU constraints and call optimizer
	optimizer, constraints := e.selectV2Optimizer(ctx, requests)
	allDecisions := optimizer.Optimize(ctx, requests, constraints)
	logScalingDecisions(ctx, requests, allDecisions)

	logger.Info("V2 optimizer produced decisions",
		"optimizer", optimizer.Name(),
		"decisionCount", len(allDecisions),
		"modelCount", len(requests))

	// Stage 3: Apply enforcer per-model (directly on decisions)
	for _, req := range requests {
		e.applyScaleToZeroEnforcement(
			ctx, req.ModelID, req.Namespace, optimizer.Name(),
			allDecisions,
			modelScaleTargets[utils.GetNamespacedKey(req.Namespace, req.ModelID)],
			req.VariantStates,
		)
	}

	// Stage 4: Enrich decisions with KV cache token data from replicaMetrics.
	// Utilization, RequiredCapacity, and SpareCapacity are already set by
	// buildDecisionsWithOptimizer from AnalyzerResult.
	enrichDecisionsWithKvTokenData(allDecisions, modelReplicaMetrics)

	return allDecisions
}

// BuildVariantStates extracts current and desired replica counts from VAs for capacity analysis.
func (e *Engine) BuildVariantStates(
	ctx context.Context,
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
) []interfaces.VariantReplicaState {
	states := make([]interfaces.VariantReplicaState, 0, len(vas))

	for _, va := range vas {
		// Get current replicas using ScaleTargetRef
		var scaleTarget scaletarget.ScaleTargetAccessor
		var found bool

		// Try to look up in provided map first (optimization)
		if scaleTargets != nil {
			scaleTarget, found = scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]
		}

		if !found {
			// Fallback to API call
			var fetchedScaleTarget scaletarget.ScaleTargetAccessor
			var err error
			if fetchedScaleTarget, err = scaletarget.FetchScaleTarget(ctx, k8sClient, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace); err != nil {
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Could not get scale target for VA, skipping",
					"variant", va.Name,
					"error", err)
				continue
			}
			scaleTarget = fetchedScaleTarget
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("BuildVariantStates fallback lookup", "variant", va.Name, "scaleTargetName", va.GetScaleTargetName(), "specReplicas", scaleTarget.GetReplicas(), "statusReplicas", scaleTarget.GetStatusReplicas(), "readyReplicas", scaleTarget.GetStatusReadyReplicas())
		} else {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("BuildVariantStates map lookup", "variant", va.Name, "scaleTargetName", va.GetScaleTargetName(), "specReplicas", scaleTarget.GetReplicas(), "statusReplicas", scaleTarget.GetStatusReplicas(), "readyReplicas", scaleTarget.GetStatusReadyReplicas())
		}

		currentReplicas := int(scaleTarget.GetStatusReplicas())
		if currentReplicas == 0 && scaleTarget.GetReplicas() != nil {
			currentReplicas = int(*scaleTarget.GetReplicas())
		}

		// Calculate pending replicas (not yet ready)
		readyReplicas := int(scaleTarget.GetStatusReadyReplicas())
		pendingReplicas := currentReplicas - readyReplicas
		if pendingReplicas < 0 {
			// This indicates an unexpected state where readyReplicas exceeds currentReplicas.
			// Log at Info level since this inconsistency should be visible to operators.
			ctrl.LoggerFrom(ctx).Info("Unexpected state: readyReplicas exceeds currentReplicas, clamping pendingReplicas to 0",
				"variant", va.Name, "currentReplicas", currentReplicas, "readyReplicas", readyReplicas)
			pendingReplicas = 0
		}

		// Extract GPUs per replica from scale target's pod template
		gpusPerReplica := scaleTarget.GetTotalGPUsPerReplica()

		// Extract P/D role from scale target labels
		role := getRoleFromScaleTarget(scaleTarget)

		// Read min/max replica bounds from VA spec fields
		var minReplicas *int
		if va.Spec.MinReplicas != nil {
			v := int(*va.Spec.MinReplicas)
			minReplicas = &v
		}
		var maxReplicas *int
		if va.Spec.MaxReplicas > 0 {
			v := int(va.Spec.MaxReplicas)
			maxReplicas = &v
		}

		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("BuildVariantStates result", "variant", va.Name, "currentReplicas", currentReplicas, "readyReplicas", readyReplicas, "pendingReplicas", pendingReplicas, "gpusPerReplica", gpusPerReplica, "role", role, "minReplicas", minReplicas, "maxReplicas", maxReplicas)

		desiredReplicas := 0
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
			desiredReplicas = int(*va.Status.DesiredOptimizedAlloc.NumReplicas)
		}
		states = append(states, interfaces.VariantReplicaState{
			VariantName:     va.Name,
			CurrentReplicas: currentReplicas,
			DesiredReplicas: desiredReplicas,
			PendingReplicas: pendingReplicas,
			GPUsPerReplica:  gpusPerReplica,
			Role:            role,
			MinReplicas:     minReplicas,
			MaxReplicas:     maxReplicas,
		})
	}

	return states
}

// getRoleFromScaleTarget extracts the P/D role from a scale target's pod template labels.
// Returns "prefill", "decode", or "both" (default when no role label is present).
func getRoleFromScaleTarget(scaleTarget scaletarget.ScaleTargetAccessor) string {
	if scaleTarget == nil {
		return interfaces.RoleBoth
	}
	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil {
		return interfaces.RoleBoth
	}
	labels := podTemplateSpec.Labels
	if labels == nil {
		return interfaces.RoleBoth
	}
	if val, ok := labels["llm-d.ai/role"]; ok {
		switch val {
		case "prefill":
			return "prefill"
		case "decode":
			return "decode"
		default:
			return interfaces.RoleBoth
		}
	}
	return interfaces.RoleBoth
}

// convertSaturationTargetsToDecisions converts saturation-only targets to VariantDecisions.
// Used when model-based optimizer is disabled (saturation-only mode).
func (e *Engine) convertSaturationTargetsToDecisions(
	ctx context.Context,
	saturationTargets map[string]int,
	saturationAnalysis *interfaces.ModelSaturationAnalysis,
	variantStates []interfaces.VariantReplicaState,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)
	decisions := make([]interfaces.VariantDecision, 0, len(saturationTargets))

	// Build variant analysis map for quick lookup
	vaMap := make(map[string]*interfaces.VariantSaturationAnalysis)
	for i := range saturationAnalysis.VariantAnalyses {
		va := &saturationAnalysis.VariantAnalyses[i]
		vaMap[va.VariantName] = va
	}

	// Build state map for quick lookup
	stateMap := make(map[string]interfaces.VariantReplicaState)
	for _, state := range variantStates {
		stateMap[state.VariantName] = state
	}

	for variantName, targetReplicas := range saturationTargets {
		state := stateMap[variantName]
		va := vaMap[variantName]

		var action interfaces.SaturationAction
		switch {
		case targetReplicas > state.CurrentReplicas:
			action = interfaces.ActionScaleUp
		case targetReplicas < state.CurrentReplicas:
			action = interfaces.ActionScaleDown
		default:
			action = interfaces.ActionNoChange
		}

		// Use GPUsPerReplica from variant state (extracted from scale target)
		gpusPerReplica := state.GPUsPerReplica
		if gpusPerReplica <= 0 {
			gpusPerReplica = 1 // Fallback default
		}

		decision := interfaces.VariantDecision{
			VariantName:            variantName,
			Namespace:              saturationAnalysis.Namespace,
			ModelID:                saturationAnalysis.ModelID,
			Role:                   state.Role,
			CurrentReplicas:        state.CurrentReplicas,
			TargetReplicas:         targetReplicas,
			OriginalTargetReplicas: targetReplicas, // Store original before limiter modifies it
			DesiredReplicas:        state.DesiredReplicas,
			SaturationBased:        true,
			SaturationOnly:         true,
			ModelBasedDecision:     false,
			SafetyOverride:         false,
			GPUsPerReplica:         gpusPerReplica,
			MinReplicas:            state.MinReplicas,
			MaxReplicas:            state.MaxReplicas,
		}
		decision.SetDecisionReason(action, interfaces.DecisionReasonSaturationOnly, fmt.Sprintf("%s: %s", string(interfaces.DecisionReasonSaturationOnly), string(action)))

		if va != nil {
			decision.AcceleratorName = va.AcceleratorName
			decision.Cost = va.Cost
			// Use average spare KV capacity as the SpareCapacity indicator for limiter prioritization
			decision.SpareCapacity = va.AvgSpareKvCapacity
			// V1 Utilization: mean of per-replica KvCacheUsage fractions
			decision.Utilization = va.AvgKvCacheUsage
		} else {
			logger.Info("No variant analysis found for decision (metrics may be unavailable)",
				"variant", variantName)
		}

		decisions = append(decisions, decision)
	}

	return decisions
}

// enrichDecisionsWithKvTokenData sets KvCacheTokensUsed, KvCacheTokensCapacity, and
// RequiredCapacityUnit on decisions from replica metrics aggregated per (model, variant).
// Used by V2 path where Utilization and RequiredCapacity are already set from
// AnalyzerResult.
//
// Aggregation is keyed by (modelID, variantName) — not just variantName — because
// variant names can collide across different models in the same reconcile cycle.
func enrichDecisionsWithKvTokenData(decisions []interfaces.VariantDecision, modelReplicaMetrics map[string][]interfaces.ReplicaMetrics) {
	type kvAgg struct {
		kvUsed  int64
		kvTotal int64
	}
	type variantKey struct {
		modelID string
		variant string
	}
	agg := make(map[variantKey]*kvAgg)
	for modelID, metrics := range modelReplicaMetrics {
		for _, rm := range metrics {
			k := variantKey{modelID: modelID, variant: rm.VariantName}
			a, ok := agg[k]
			if !ok {
				a = &kvAgg{}
				agg[k] = a
			}
			a.kvUsed += rm.TokensInUse
			a.kvTotal += rm.TotalKvCapacityTokens
		}
	}

	for i := range decisions {
		d := &decisions[i]
		d.RequiredCapacityUnit = constants.UnitContinuous
		if a, ok := agg[variantKey{modelID: d.ModelID, variant: d.VariantName}]; ok {
			d.KvCacheTokensUsed = a.kvUsed
			d.KvCacheTokensCapacity = a.kvTotal
		}
	}
}

// hasMinReplicasAboveZero returns true if any variant in the states has MinReplicas > 0.
func hasMinReplicasAboveZero(states []interfaces.VariantReplicaState) bool {
	for _, state := range states {
		if state.MinReplicas != nil && *state.MinReplicas > 0 {
			return true
		}
	}
	return false
}

// scaleToZeroSupportedForEngines reports whether scale-to-zero enforcement is safe
// for a model running the given set of scale targets. Scale-to-zero relies on
// CollectModelRequestCount, which is currently hardcoded to vLLM's request counter
// (vllm:request_success_total); routing the per-engine counter through the enforcer
// is the deferred Phase 2 work (see docs/proposals/sglang-backend.md). For any
// non-vLLM engine that counter returns 0, which the enforcer would misread as
// "no traffic" and scale the model to zero. Until the engine is threaded through,
// scale-to-zero is skipped for models that run a non-vLLM engine.
func scaleToZeroSupportedForEngines(scaleTargets map[string]scaletarget.ScaleTargetAccessor) bool {
	for _, eng := range inferenceengine.Present(scaleTargets) {
		if eng != inferenceengine.EngineVLLM {
			return false
		}
	}
	return true
}

// applyScaleToZeroEnforcement runs scale-to-zero / minimum-replica enforcement for a
// single model's decisions, unless a safety gate skips it:
//   - the model runs a non-vLLM engine — CollectModelRequestCount is hardcoded to
//     vLLM's request counter, so an active SGLang model would falsely read as idle
//     and be zeroed (see scaleToZeroSupportedForEngines); or
//   - any variant declares minReplicas > 0 (hasMinReplicasAboveZero).
//
// Decisions are mutated in place; returns true if the model was scaled to zero.
//
// All three optimize paths (V1, V2, queueing-model) funnel their enforcement through
// this one method so the gate lives in a single place — a caller cannot accidentally
// invoke the enforcer ungated, and one test (engine_scale_to_zero_enforce_test.go)
// locks the gate down for every path.
func (e *Engine) applyScaleToZeroEnforcement(
	ctx context.Context,
	modelID, namespace, optimizerName string,
	decisions []interfaces.VariantDecision,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantStates []interfaces.VariantReplicaState,
) bool {
	if len(decisions) == 0 {
		return false
	}
	logger := ctrl.LoggerFrom(ctx)

	if !scaleToZeroSupportedForEngines(scaleTargets) {
		logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement: model runs a non-vLLM engine; engine-aware request counting is not yet wired through the enforcer (see docs/proposals/sglang-backend.md Phase 2)",
			"modelID", modelID, "optimizer", optimizerName)
		return false
	}
	if hasMinReplicasAboveZero(variantStates) {
		logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement: variant has minReplicas > 0",
			"modelID", modelID, "optimizer", optimizerName)
		return false
	}

	scaleToZeroConfig := e.Config.ScaleToZeroConfigForNamespace(namespace)
	scaledToZero := e.ScaleToZeroEnforcer.EnforcePolicyOnDecisions(
		ctx, modelID, namespace, decisions, scaleToZeroConfig, optimizerName,
	)
	if scaledToZero {
		logger.Info("Scale-to-zero enforcement applied",
			"modelID", modelID, "optimizer", optimizerName)
	}
	return scaledToZero
}

// normalizeRole maps empty and "both" roles to the same canonical key so that
// non-disaggregated variants are grouped together regardless of whether the
// deployment carries a role label.
func normalizeRole(role string) string {
	if role == "" {
		return interfaces.RoleBoth
	}
	return role
}

// groupByRole sub-groups variant states by their P/D role.
// Returns a map keyed by normalized role ("both", "prefill", "decode").
// For non-disaggregated models (all "both"/""), the map contains a single entry.
func groupByRole(states []interfaces.VariantReplicaState) map[string][]interfaces.VariantReplicaState {
	groups := make(map[string][]interfaces.VariantReplicaState)
	for _, s := range states {
		key := normalizeRole(s.Role)
		groups[key] = append(groups[key], s)
	}
	return groups
}

// sortedRoleKeys returns the keys of a role group map in sorted order so that
// role processing order is deterministic across cycles (stable log output,
// easier debugging). The caller is expected to pass a map whose keys were
// produced by groupByRole, which has already canonicalized empty roles to
// "both"; the resulting lexicographic order is then "both" < "decode" < "prefill".
func sortedRoleKeys(groups map[string][]interfaces.VariantReplicaState) []string {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// filterReplicaMetricsByVariants returns only the replica metrics whose VariantName
// appears in the given variant state slice. Used to split per-model metrics into
// per-role subsets without re-querying Prometheus.
func filterReplicaMetricsByVariants(metrics []interfaces.ReplicaMetrics, states []interfaces.VariantReplicaState) []interfaces.ReplicaMetrics {
	allowed := make(map[string]struct{}, len(states))
	for _, s := range states {
		allowed[s.VariantName] = struct{}{}
	}
	filtered := make([]interfaces.ReplicaMetrics, 0, len(metrics))
	for _, m := range metrics {
		if _, ok := allowed[m.VariantName]; ok {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

// filterVAsByVariantStates returns the subset of VAs whose Name appears in the
// given variant states. Used to map a role group back to its source VAs for
// safety-net metric emission.
func filterVAsByVariantStates(
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	states []interfaces.VariantReplicaState,
) []llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	allowed := make(map[string]struct{}, len(states))
	for _, s := range states {
		allowed[s.VariantName] = struct{}{}
	}
	filtered := make([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling, 0, len(states))
	for _, va := range vas {
		if _, ok := allowed[va.Name]; ok {
			filtered = append(filtered, va)
		}
	}
	return filtered
}

// variantNames returns variant names from states for logging.
func variantNames(states []interfaces.VariantReplicaState) []string {
	names := make([]string, len(states))
	for i, s := range states {
		names[i] = s.VariantName
	}
	return names
}

// modelData holds the pre-processed data for a model, shared between V1 and V2 paths.
type modelData struct {
	modelID             string
	namespace           string
	replicaMetrics      []interfaces.ReplicaMetrics
	scaleTargets        map[string]scaletarget.ScaleTargetAccessor
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	variantCosts        map[string]float64
	variantStates       []interfaces.VariantReplicaState
	schedulerQueue      *interfaces.SchedulerQueueMetrics
}

// prepareModelData collects metrics and builds lookup maps for a model's VAs.
// This is shared by both V1 and V2 paths.
// Also shared by the Queueing Model Analyzer engine.
// Returns nil modelData (not error) when no metrics are available — caller should skip the model.
func (e *Engine) prepareModelData(
	ctx context.Context,
	modelID string,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	k8sClient client.Client,
) (*modelData, error) {
	if len(modelVAs) == 0 {
		return nil, fmt.Errorf("no VAs provided for model %s", modelID)
	}

	logger := ctrl.LoggerFrom(ctx)
	namespace := modelVAs[0].Namespace

	variantCosts := make(map[string]float64)
	scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	variantAutoscalings := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for i := range modelVAs {
		va := &modelVAs[i]
		scaleTarget, err := scaletarget.FetchScaleTarget(ctx, k8sClient, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace)
		if err != nil {
			logger.V(logging.DEBUG).Info("Could not get scale target for VA",
				"variant", va.Name,
				"scaleTarget", va.GetScaleTargetName(),
				"error", err)
			continue
		}

		cost := saturation.DefaultVariantCost
		if va.Spec.VariantCost != "" {
			if parsedCost, err := strconv.ParseFloat(va.Spec.VariantCost, 64); err == nil {
				cost = parsedCost
			} else {
				logger.V(logging.DEBUG).Info("Failed to parse variant cost, using default",
					"variant", va.Name, "variantCost", va.Spec.VariantCost, "default", cost, "error", err)
			}
		}

		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		scaleTargets[key] = scaleTarget

		variantKey := utils.GetNamespacedKey(va.Namespace, va.Name)
		variantAutoscalings[variantKey] = va
		variantCosts[variantKey] = cost
	}

	logger.V(logging.DEBUG).Info("Using source infrastructure for replica metrics",
		"modelID", modelID,
		"namespace", namespace)
	replicaMetrics, err := e.ReplicaMetricsCollector.CollectReplicaMetrics(ctx, modelID, namespace, scaleTargets, variantAutoscalings, e.vaEventTracker, variantCosts)
	if err != nil {
		return nil, fmt.Errorf("failed to collect Saturation metrics for model %s: %w", modelID, err)
	}

	logger.V(logging.DEBUG).Info("Collected saturation metrics",
		"modelID", modelID,
		"namespace", namespace,
		"metricsCount", len(replicaMetrics))

	if len(replicaMetrics) == 0 {
		logger.Info("No saturation metrics available for model, skipping analysis",
			"modelID", modelID,
			"namespace", namespace)
		return nil, nil // nil modelData signals skip
	}

	variantStates := e.BuildVariantStates(ctx, modelVAs, scaleTargets, k8sClient)
	schedulerQueue := e.ReplicaMetricsCollector.CollectSchedulerQueueMetrics(ctx, modelID)

	return &modelData{
		modelID:             modelID,
		namespace:           namespace,
		replicaMetrics:      replicaMetrics,
		scaleTargets:        scaleTargets,
		variantAutoscalings: variantAutoscalings,
		variantCosts:        variantCosts,
		variantStates:       variantStates,
		schedulerQueue:      schedulerQueue,
	}, nil
}

// applySaturationDecisions updates VA status and emits metrics based on Saturation decisions.
func (e *Engine) applySaturationDecisions(
	ctx context.Context,
	decisions []interfaces.VariantDecision,
	vaMap map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
) {
	logger := ctrl.LoggerFrom(ctx)
	// Create a map of decisions for O(1) lookup
	// Use namespace/variantName as key to match vaMap and avoid collisions
	decisionMap := make(map[string]interfaces.VariantDecision)
	for _, d := range decisions {
		decisionMap[utils.GetNamespacedKey(d.Namespace, d.VariantName)] = d
	}

	// Iterate over ALL active VAs to ensure we update status and trigger reconciliation for everyone
	for vaName, va := range vaMap {
		decision, hasDecision := decisionMap[vaName]

		if hasDecision {
			logger.Info("Processing decision for VA",
				"variant", vaName,
				"action", decision.Action,
				"current", decision.CurrentReplicas,
				"target", decision.TargetReplicas)
		} else {
			logger.V(logging.DEBUG).Info("No scaling decision for VA, but updating status to trigger reconcile",
				"variant", vaName)
		}

		// Variants are synthesized in-memory from annotated HPAs/ScaledObjects;
		// there is no API-server object to fetch, so work on a copy.
		updateVa := *va.DeepCopy()

		// Update CurrentAlloc from local analysis (which has the latest metrics)
		// We use currentAllocations map instead of Status.CurrentAlloc
		if currentAlloc, ok := currentAllocations[vaName]; ok {
			// If we have a decision, attach current alloc to it for cache
			// If we have a decision, attach current alloc to it for cache
			// (Future logic if needed)
			_ = currentAlloc // Used for something?
			// Previously we updated va.Status.CurrentAlloc = currentAlloc
			// Now we just don't update status with it.
		}

		// Determine target replicas and accelerator
		var targetReplicas int
		var acceleratorName string
		var reason string

		if hasDecision {
			targetReplicas = decision.TargetReplicas
			acceleratorName = decision.AcceleratorName
			reason = decision.Reason()
		} else {
			// No change/decision: Keep current target or default to current replicas
			// We effectively explicitly "decide" to keep things as they are if no decision was made
			if updateVa.Status.DesiredOptimizedAlloc.NumReplicas != nil && *updateVa.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
				targetReplicas = int(*updateVa.Status.DesiredOptimizedAlloc.NumReplicas)
			} else if curr, ok := currentAllocations[vaName]; ok {
				targetReplicas = curr.NumReplicas
			}
			// Keep existing accelerator or use current (skip sentinel values)
			if acc := updateVa.Status.DesiredOptimizedAlloc.Accelerator; constants.IsAcceleratorResolved(acc) {
				acceleratorName = acc
			} else if curr, ok := currentAllocations[vaName]; ok && constants.IsAcceleratorResolved(curr.Accelerator) {
				acceleratorName = curr.Accelerator
			}

			// Fallback for new VAs without prior status or collected metrics:
			// resolve accelerator from deployment nodeSelector/nodeAffinity or VA label,
			// and use current deployment replicas as target to avoid unintended scaling.
			if !constants.IsAcceleratorResolved(acceleratorName) {
				scaleTargetName := updateVa.GetScaleTargetName()
				if scaleTargetName != "" {
					var scaleTarget scaletarget.ScaleTargetAccessor
					var err error
					if scaleTarget, err = scaletarget.FetchScaleTarget(ctx, e.client, va.Name, va.Spec.ScaleTargetRef.Kind, scaleTargetName, va.Namespace); err == nil {
						acceleratorName = utils.GetAcceleratorNameFromScaleTarget(&updateVa, scaleTarget)
						if targetReplicas == 0 && scaleTarget.GetReplicas() != nil {
							targetReplicas = int(*scaleTarget.GetReplicas())
						}
					} else {
						// If scaleTarget fetch fails, try VA label directly
						acceleratorName = utils.GetAcceleratorNameFromScaleTarget(&updateVa, nil)
					}
				}
			}

			reason = "No scaling decision (optimization loop)"
		}

		// If we still don't have an accelerator name (e.g. new VA, no decision, no current alloc), we can't update status sensibly
		// But we still need to set MetricsAvailable condition via the cache
		if acceleratorName == "" {
			logger.Info("Skipping status update for variant without accelerator info",
				"variant", vaName, "cacheKey.name", va.Name, "cacheKey.namespace", va.Namespace)
		}

		// Emit a K8s event when accelerator cannot be resolved so operators
		// can see the problem without digging through controller logs.
		// The message is a constant string (not built per-cycle via Eventf with
		// formatted args), so each emission produces an identical
		// (involvedObject, source, type, reason, message) tuple — which the K8s
		// API server's event aggregator collapses into a single Event entry with
		// an updated count, rather than creating a new entry each optimization
		// cycle.
		if !constants.IsAcceleratorResolved(acceleratorName) {
			e.emitAcceleratorNotResolvedEvent(&updateVa)
			logger.V(logging.DEBUG).Info("Accelerator name not resolved - replica scaling metrics emitted with accelerator_type=\"unresolved\"; accelerator-specific saturation/capacity metrics withheld",
				"variant", vaName)
		}

		// Stage the just-computed decision on the in-memory VA so that
		// act.EmitMetrics below — which reads
		// Status.DesiredOptimizedAlloc.{NumReplicas,Accelerator}
		// (see actuator.EmitMetrics) — publishes the fresh target rather than
		// whatever was last persisted by the controller. This object is local
		// to the engine; CRD persistence happens later, via the cache write
		// (DecisionCache.Set) → controller patch path.
		// Sanitize the sentinel out of the staged value so neither EmitMetrics
		// nor the cache (which reuses statusAccelerator) ever sees it.
		numReplicas := int32(targetReplicas)
		statusAccelerator := acceleratorName
		if !constants.IsAcceleratorResolved(statusAccelerator) {
			statusAccelerator = ""
		}
		updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
			NumReplicas: &numReplicas,
			Accelerator: statusAccelerator,
			LastRunTime: metav1.Now(),
		}
		updateVa.Status.Actuation.Applied = false // Reset applied status until Actuator handles it (if needed)

		// Set condition based on decision characteristics (or lack thereof)
		if hasDecision {
			switch {
			case decision.SafetyOverride:
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationSafetyOverride",
					"saturation safety override: "+reason)
			case decision.SaturationOnly:
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationOnlyMode",
					fmt.Sprintf("saturation-only decision: %s (target: %d replicas)", reason, targetReplicas))
			default:
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
					fmt.Sprintf("Hybrid mode: %s (target: %d replicas)", reason, targetReplicas))
			}
		} else {
			// No active decision (just refreshing)
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
				"Optimization loop ran (no scaling change needed)")
		}

		// Emit metrics for external autoscalers (Important: Actuator emits these)
		// We should emit metrics even if no decision changed, to keep HPA alive
		act := actuator.NewActuator(e.client)
		/*
		   NOTE: emitSafetyNetMetrics handles cases where optimization FAILS.
		   Here we are in the success path (optimization ran, even if no change).
		   We should ensure metrics are emitted for the External Scaler.
		*/

		// Ensure we have a valid SAT/Model decision "SaturationOnly" flag for metric emission context if needed
		// For now we assume if no decision, it's not saturation-only forced override, just normal op.
		// isSaturationOnly := false
		// if hasDecision {
		// 	isSaturationOnly = decision.SaturationOnly
		// }

		// Always emit the replica scaling signal (the HPA/KEDA external metric).
		// EmitReplicaMetrics labels an unresolved accelerator as the bounded
		// "unresolved" value (never the internal sentinel), so the scaling signal
		// is never withheld; the scaler matches on variant/namespace rather than
		// accelerator_type, so it keeps working regardless. Accelerator-dimensioned
		// metrics (saturation/capacity) remain gated on resolution below.
		if err := act.EmitMetrics(ctx, &updateVa); err != nil {
			msg := "Failed to emit metrics for external autoscalers"
			// K8s best practice: events should reference the current resource version
			e.recordOptimizationFailedEvent([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{updateVa}, msg)
			logger.Error(err, msg, "variant", updateVa.Name)
		} else {
			// Only log detail if we had a decision or periodically (to avoid spamming logs on every loop for no-ops)
			if hasDecision {
				// Emit Kubernetes event for observability
				e.recordScalingEvent(&updateVa, decision.Action, decision.TargetReplicas, decision.Reason())
				if decision.WasLimited {
					e.recordEvent(va, corev1.EventTypeWarning, constants.K8SEventResourceConstrained, decision.Reason())
				}

				logger.Info("Successfully emitted metrics",
					"variant", updateVa.Name,
					"target", targetReplicas,
					"accelerator", acceleratorName)
			}
			updateVa.Status.Actuation.Applied = true
		}

		// Record saturation and capacity metrics when this cycle produced a
		// fresh decision for the variant. These accelerator-dimensioned series
		// carry the accelerator_type label, so they are emitted only when the
		// type is resolved — otherwise the internal sentinel would leak into a
		// label. When there is no fresh decision the existing series persist with
		// their last-recorded values until Prometheus' staleness marker fires.
		if hasDecision && constants.IsAcceleratorResolved(acceleratorName) {
			act.RecordSaturationMetrics(ctx, decision)
		}

		// The wva_saturation_metrics_up freshness gauge carries only
		// {variant_name, namespace} (no accelerator_type), so it is emitted on
		// every cycle independent of accelerator resolution: 1.0 when a fresh
		// decision was produced for the variant, 0.0 otherwise. Dashboards gate
		// alerts on this gauge instead of relying on Prometheus' 5-minute
		// implicit staleness marker.
		if hasDecision {
			act.RecordSaturationFreshness(ctx, decision.VariantName, decision.Namespace, true)
		} else {
			act.RecordSaturationFreshness(ctx, va.Name, va.Namespace, false)
		}

		// Metric emission above (act.EmitMetrics) is the sole output for
		// annotation-sourced variants: KEDA/HPA reads wva_desired_replicas
		// directly. There is no CRD status to patch, so no cache write or
		// reconcile trigger is needed.

		if hasDecision {
			if decision.Action != interfaces.ActionNoChange {
				if err := e.metricsEmitter.EmitReplicaScalingMetrics(ctx, &updateVa, decision.Action, decision.ReasonCategory()); err != nil {
					logger.Error(err, "Failed to emit replica scaling metrics")
				}
			}
			logger.Info("Applied saturation decision via shared cache",
				"variant", vaName,
				"namespace", updateVa.Namespace,
				"action", decision.Action,
				"target", targetReplicas,
				"reason", reason)
		}
	}
}

// emitAcceleratorNotResolvedEvent records a Warning event on the given
// VariantAutoscaling so operators see at-a-glance that the optimization
// loop ran but could not resolve an accelerator type for it. The message
// is a constant string so the API server's event aggregator collapses
// repeated emissions into a single Event entry with an updated count
// rather than creating a new entry each optimization cycle.
func (e *Engine) emitAcceleratorNotResolvedEvent(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) {
	e.recordEvent(va, corev1.EventTypeWarning, "AcceleratorNotResolved",
		"Cannot resolve accelerator type from Deployment nodeSelector/nodeAffinity or VA label "+
			utils.AcceleratorNameLabel+". "+
			"Set nodeSelector on Deployment or add the label to the VariantAutoscaling resource. "+
			"Replica scaling metrics are still emitted with accelerator_type=\"unresolved\" so HPA/KEDA can scale; "+
			"accelerator-specific saturation/capacity metrics are withheld until the accelerator is resolved.")
}

// emitSafetyNetMetrics emits fallback metrics when saturation analysis fails.
func (e *Engine) emitSafetyNetMetrics(
	ctx context.Context,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) {
	logger := ctrl.LoggerFrom(ctx)
	act := actuator.NewActuator(e.client)

	for _, va := range modelVAs {
		// Determine desired replicas
		var desiredReplicas, currentReplicas int32
		var fallbackSource string
		var scaleTarget scaletarget.ScaleTargetAccessor
		var err error
		if scaleTargets != nil {
			if target, ok := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]; ok {
				scaleTarget = target
				// Get current replicas for metric emission.
				currentReplicas, err = act.GetCurrentScaleTargetReplicasFromScaleTarget(&va, scaleTarget)
				if err != nil {
					logger.Error(err, "Safety net: failed to get current replicas from scale target for metrics, using cached allocation",
						"variant", va.Name)
					if curr, ok := currentAllocations[utils.GetNamespacedKey(va.Namespace, va.Name)]; ok {
						currentReplicas = int32(curr.NumReplicas)
					}
				}
			}
		}

		// Strategy 1: Use previous desired replicas if available
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil && *va.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
			desiredReplicas = *va.Status.DesiredOptimizedAlloc.NumReplicas
			fallbackSource = "previous-desired"
		} else {
			desiredReplicas = currentReplicas
			fallbackSource = "current-replicas"
		}

		// Determine accelerator - try status first, then labels, skip if unavailable
		// TODO: remove this checks when we will move to a new version of the CRD
		// with required accelerator field
		accelerator := va.Status.DesiredOptimizedAlloc.Accelerator
		if accelerator == "" {
			if curr, ok := currentAllocations[utils.GetNamespacedKey(va.Namespace, va.Name)]; ok {
				accelerator = curr.Accelerator
			}
		}
		if !constants.IsAcceleratorResolved(accelerator) {
			// Try to get accelerator name from scale target nodeSelector/nodeAffinity or VA labels
			if scaleTarget == nil {
				logger.V(logging.DEBUG).Info("Safety net: no scale target found for VA",
					"variant", va.Name)
			} else {
				accelerator = utils.GetAcceleratorNameFromScaleTarget(&va, scaleTarget)
			}
		}
		if !constants.IsAcceleratorResolved(accelerator) {
			// Do NOT withhold the scaling signal: EmitReplicaMetrics labels an
			// unresolved accelerator with the bounded "unresolved" value, so the
			// safety-net signal still reaches HPA/KEDA (which match on
			// variant/namespace, not accelerator_type). Mirrors the main path.
			logger.V(logging.DEBUG).Info("Safety net: accelerator unresolved, emitting scaling signal with 'unresolved' accelerator_type",
				"variant", va.Name)
		}

		// Emit safety net metrics
		if err := act.MetricsEmitter.EmitReplicaMetrics(
			ctx,
			&va,
			currentReplicas,
			desiredReplicas,
			accelerator,
		); err != nil {
			logger.Error(err, "Safety net: failed to emit metrics",
				"variant", va.Name)
			continue
		}

		logger.Info("Safety net activated: emitted fallback metrics",
			"variant", va.Name,
			"currentReplicas", currentReplicas,
			"desiredReplicas", desiredReplicas,
			"accelerator", accelerator,
			"fallbackSource", fallbackSource)
	}
}
