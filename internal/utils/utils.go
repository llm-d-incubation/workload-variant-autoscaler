package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	interfaces "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	infernoConfig "github.com/llm-d/llm-d-workload-variant-autoscaler/pkg/config"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

// Helper functions for common resource types with standard backoff
func GetConfigMapWithBackoff(ctx context.Context, c client.Client, name, namespace string, cm *corev1.ConfigMap) error {
	return resources.GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, cm, constants.StandardBackoff, "ConfigMap")
}

func GetVariantAutoscalingWithBackoff(ctx context.Context, c client.Client, name, namespace string, va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) error {
	return resources.GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, va, constants.StandardBackoff, "VariantAutoscaling")
}

// UpdateStatusWithBackoff performs a Status Update operation with exponential backoff retry logic.
// This function is kept for backward compatibility but doesn't handle resource version conflicts properly.
func UpdateStatusWithBackoff[T client.Object](ctx context.Context, c client.Client, obj T, backoff wait.Backoff, resourceType string) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		err := c.Status().Update(ctx, obj)
		if err != nil {
			if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
				ctrl.LoggerFrom(ctx).V(logging.VERBOSE).Error(err, "permanent error updating status for resource", "resourceType", resourceType, "name", obj.GetName())
				return false, err // Don't retry on permanent errors
			}
			if apierrors.IsConflict(err) {
				// Resource version conflict - object was modified since we read it
				ctrl.LoggerFrom(ctx).V(logging.TRACE).Info("conflict updating status (resource version mismatch), retrying", "resource", resourceType, "name", obj.GetName())
				return false, nil // Retry on conflict
			}
			ctrl.LoggerFrom(ctx).V(logging.TRACE).Error(err, "transient error updating status, retrying for resource", "resourceType", resourceType, "name", obj.GetName())
			return false, nil // Retry on transient errors
		}
		return true, nil
	})
}

// Adapter to create wva system data types from config maps.
// Note: WVA operates in unlimited mode, so capacity data is not used.
func CreateSystemData(
	acceleratorCm map[string]map[string]string,
	serviceClassCm map[string]string) *infernoConfig.SystemData {

	systemData := &infernoConfig.SystemData{
		Spec: infernoConfig.SystemSpec{
			Accelerators:   infernoConfig.AcceleratorData{},
			Models:         infernoConfig.ModelData{},
			ServiceClasses: infernoConfig.ServiceClassData{},
			Servers:        infernoConfig.ServerData{},
			Optimizer:      infernoConfig.OptimizerData{},
			Capacity:       infernoConfig.CapacityData{},
		},
	}

	// get accelerator data
	acceleratorData := []infernoConfig.AcceleratorSpec{}
	for key, val := range acceleratorCm {
		cost, err := strconv.ParseFloat(val["cost"], 32)
		if err != nil {
			ctrl.Log.Info("failed to parse accelerator cost in configmap, skipping accelerator", "name", key)
			continue
		}
		acceleratorData = append(acceleratorData, infernoConfig.AcceleratorSpec{
			Name:         key,
			Type:         val["device"],
			Multiplicity: 1,                         // TODO: multiplicity should be in the configured accelerator spec
			Power:        infernoConfig.PowerSpec{}, // Not currently used
			Cost:         float32(cost),
		})
	}
	systemData.Spec.Accelerators.Spec = acceleratorData

	// Capacity data is not used in unlimited mode - initialize empty for future limited mode work
	systemData.Spec.Capacity.Count = []infernoConfig.AcceleratorCount{}

	// get service class data
	serviceClassData := []infernoConfig.ServiceClassSpec{}
	for key, val := range serviceClassCm {
		var sc interfaces.ServiceClass
		if err := yaml.Unmarshal([]byte(val), &sc); err != nil {
			ctrl.Log.Info("failed to parse service class data, skipping service class", "key", key, "err", err)
			continue
		}
		serviceClassSpec := infernoConfig.ServiceClassSpec{
			Name:         sc.Name,
			Priority:     sc.Priority,
			ModelTargets: make([]infernoConfig.ModelTarget, len(sc.Data)),
		}
		for i, entry := range sc.Data {
			serviceClassSpec.ModelTargets[i] = infernoConfig.ModelTarget{
				Model:    entry.Model,
				SLO_ITL:  float32(entry.SLOTPOT),
				SLO_TTFT: float32(entry.SLOTTFT),
			}
		}
		serviceClassData = append(serviceClassData, serviceClassSpec)
	}
	systemData.Spec.ServiceClasses.Spec = serviceClassData

	// set optimizer configuration
	// TODO: make it configurable
	systemData.Spec.Optimizer.Spec = infernoConfig.OptimizerSpec{
		Unlimited: true,
		// SaturationPolicy omitted - defaults to "None" (not relevant in unlimited mode)
	}

	// initialize model data
	systemData.Spec.Models.PerfData = []infernoConfig.ModelAcceleratorPerfData{}

	// initialize dynamic server data
	systemData.Spec.Servers.Spec = []infernoConfig.ServerSpec{}

	return systemData
}

// add model accelerator pair profile data to inferno system data

// Add server specs to inferno system data
func AddServerInfoToSystemData(
	sd *infernoConfig.SystemData,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAlloc *interfaces.Allocation,
	className string) (err error) {

	// server load statistics
	var arrivalRate, avgOutputTokens, avgInputTokens, cost, itlAverage, ttftAverage float64
	if currentAlloc == nil {
		// Use empty/default values if no current allocation
		currentAlloc = &interfaces.Allocation{}
	}

	if arrivalRate, err = strconv.ParseFloat(currentAlloc.Load.ArrivalRate, 32); err != nil || !CheckValue(arrivalRate) {
		arrivalRate = 0
	}
	if avgOutputTokens, err = strconv.ParseFloat(currentAlloc.Load.AvgOutputTokens, 32); err != nil || !CheckValue(avgOutputTokens) {
		avgOutputTokens = 0
	}
	if avgInputTokens, err = strconv.ParseFloat(currentAlloc.Load.AvgInputTokens, 32); err != nil || !CheckValue(avgInputTokens) {
		avgInputTokens = 0
	}

	serverLoadSpec := &infernoConfig.ServerLoadSpec{
		ArrivalRate:  float32(arrivalRate),
		AvgInTokens:  int(avgInputTokens),
		AvgOutTokens: int(avgOutputTokens),
	}

	// server allocation
	// Calculate cost from Spec.VariantCost (unit cost) * Replicas
	var unitCost float64
	if va.Spec.VariantCost != "" {
		if val, err := strconv.ParseFloat(va.Spec.VariantCost, 64); err == nil {
			unitCost = val
		}
	}
	// TODO: Use a constant for default cost if not set, or rely on CRD defaulting
	if unitCost == 0 {
		unitCost = 10.0 // Fallback/Default
	}

	cost = unitCost * float64(currentAlloc.NumReplicas)
	if !CheckValue(cost) {
		cost = 0
	}
	if itlAverage, err = strconv.ParseFloat(currentAlloc.ITLAverage, 32); err != nil || !CheckValue(itlAverage) {
		itlAverage = 0
	}
	if ttftAverage, err = strconv.ParseFloat(currentAlloc.TTFTAverage, 32); err != nil || !CheckValue(ttftAverage) {
		ttftAverage = 0
	}

	AllocationData := &infernoConfig.AllocationData{
		Accelerator: currentAlloc.Accelerator,
		NumReplicas: currentAlloc.NumReplicas,
		MaxBatch:    currentAlloc.MaxBatch,
		Cost:        float32(cost),
		ITLAverage:  float32(itlAverage),
		TTFTAverage: float32(ttftAverage),
		Load:        *serverLoadSpec,
	}

	// all server data
	minNumReplicas := 1 // scale to zero is disabled by default
	if os.Getenv("WVA_SCALE_TO_ZERO") == "true" {
		minNumReplicas = 0
	}
	serverSpec := &infernoConfig.ServerSpec{
		Name:            FullName(va.Name, va.Namespace),
		Class:           className,
		Model:           va.Spec.ModelID,
		KeepAccelerator: true,
		MinNumReplicas:  minNumReplicas,
		CurrentAlloc:    *AllocationData,
		DesiredAlloc:    infernoConfig.AllocationData{},
	}

	// set max batch size if configured
	maxBatchSize := 32 // Default value now that ModelProfile is removed

	// set max batch size if configured - now handled by capacity/hardcoded or label-based lookups
	// For now, removing the dependency on ModelProfile.
	// TODO: Retrieve this from a ConfigMap or other source if needed.

	if maxBatchSize > 0 {
		serverSpec.MaxBatchSize = maxBatchSize
	}

	sd.Spec.Servers.Spec = append(sd.Spec.Servers.Spec, *serverSpec)
	return nil
}

// Adapter from inferno alloc solution to optimized alloc
func CreateOptimizedAlloc(name string,
	namespace string,
	allocationSolution *infernoConfig.AllocationSolution) (*llmdVariantAutoscalingV1alpha1.OptimizedAlloc, error) {

	serverName := FullName(name, namespace)
	var allocationData infernoConfig.AllocationData
	var exists bool
	if allocationData, exists = allocationSolution.Spec[serverName]; !exists {
		return nil, fmt.Errorf("server %s not found", serverName)
	}
	ctrl.Log.Info("Setting accelerator name ", "Name ", allocationData.Accelerator, "allocationData ", allocationData)
	numReplicas := int32(allocationData.NumReplicas)
	optimizedAlloc := &llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
		LastRunTime: metav1.NewTime(time.Now()),
		Accelerator: allocationData.Accelerator,
		NumReplicas: &numReplicas,
	}
	return optimizedAlloc, nil
}

// Helper to create a (unique) full name from name and namespace
func FullName(name string, namespace string) string {
	return name + ":" + namespace
}

// Helper to check if a value is valid (not NaN or infinite)
func CheckValue(x float64) bool {
	return !(math.IsNaN(x) || math.IsInf(x, 0))
}

func GetZapLevelFromEnv() zapcore.Level {
	levelStr := strings.ToLower(os.Getenv("LOG_LEVEL"))
	switch levelStr {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel // fallback
	}
}

func MarshalStructToJsonString(t any) string {
	jsonBytes, err := json.MarshalIndent(t, "", " ")
	if err != nil {
		return fmt.Sprintf("error marshalling: %v", err)
	}
	re := regexp.MustCompile("\"|\n")
	return re.ReplaceAllString(string(jsonBytes), "")
}

// Helper to find SLOs for a model variant
func FindModelSLO(cmData map[string]string, targetModel string) (*interfaces.ServiceClassEntry, string /* class name */, error) {
	for key, val := range cmData {
		var sc interfaces.ServiceClass
		if err := yaml.Unmarshal([]byte(val), &sc); err != nil {
			return nil, "", fmt.Errorf("failed to parse %s: %w", key, err)
		}

		for _, entry := range sc.Data {
			if entry.Model == targetModel {
				return &entry, sc.Name, nil
			}
		}
	}
	return nil, "", fmt.Errorf("model %q not found in any service class", targetModel)
}

func Ptr[T any](v T) *T {
	return &v
}

func QueryPrometheusWithBackoff(ctx context.Context, promAPI promv1.API, query string) (val model.Value, warn promv1.Warnings, err error) {
	var lastErr error

	err = wait.ExponentialBackoffWithContext(ctx, constants.PrometheusQueryBackoff, func(ctx context.Context) (bool, error) {
		val, warn, err = promAPI.Query(ctx, query, time.Now())
		if err != nil {
			// Record the last error so that we can surface it if the backoff is exhausted.
			lastErr = err
			ctrl.Log.Info("Query Prometheus failed, retrying",
				"query", query,
				"error", err.Error())
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if lastErr != nil {
			return nil, nil, lastErr
		}
		return nil, nil, err
	}

	return
}

// ValidatePrometheusAPIWithBackoff validates Prometheus API connectivity with retry logic
func ValidatePrometheusAPIWithBackoff(ctx context.Context, promAPI promv1.API, backoff wait.Backoff) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		// Test with a simple query that should always work
		query := "up"
		_, _, err := promAPI.Query(ctx, query, time.Now())
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "Prometheus API validation failed, retrying - ", "query: ", query)
			return false, nil // Retry on transient errors
		}

		ctrl.LoggerFrom(ctx).Info("Prometheus API validation successful with query", "query", query)
		return true, nil
	})
}

// ValidatePrometheusAPI validates Prometheus API connectivity using standard Prometheus backoff
func ValidatePrometheusAPI(ctx context.Context, promAPI promv1.API) error {
	return ValidatePrometheusAPIWithBackoff(ctx, promAPI, constants.PrometheusValidationBackoff)
}

// GetAcceleratorNameFromScaleTarget extracts GPU product information from a scale target's nodeSelector or nodeAffinity.
// It checks for the following keys in order:
// - nvidia.com/gpu.product
// - amd.com/gpu.product-name
// - cloud.google.com/gke-accelerator
// If not found in nodeSelector or nodeAffinity, falls back to the AcceleratorNameLabel on the VariantAutoscaling.
// Returns the first matching value found, or an empty string if none are found.
func GetAcceleratorNameFromScaleTarget(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling, scaleTarget scaletarget.ScaleTargetAccessor) string {
	// Check scaleTarget for accelerator name if it's not nil
	if scaleTarget != nil {
		podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
		if podTemplateSpec == nil {
			return ""
		}
		// Check nodeSelector first
		if podTemplateSpec.Spec.NodeSelector != nil {
			for _, key := range constants.GpuProductKeys {
				if val, ok := podTemplateSpec.Spec.NodeSelector[key]; ok {
					return val
				}
			}
		}

		// Check nodeAffinity
		if podTemplateSpec.Spec.Affinity != nil && podTemplateSpec.Spec.Affinity.NodeAffinity != nil {
			if val := extractGPUFromNodeAffinity(podTemplateSpec.Spec.Affinity.NodeAffinity, constants.GpuProductKeys); val != "" {
				return val
			}
		}
	}

	// Fall back to VariantAutoscaling label
	if va != nil && va.Labels != nil {
		if accName, exists := va.Labels[AcceleratorNameLabel]; exists {
			return accName
		}
	}
	return ""
}

// extractGPUFromNodeAffinity extracts GPU product information from NodeAffinity.
// It checks both required and preferred node affinity terms for the given GPU keys.
func extractGPUFromNodeAffinity(nodeAffinity *corev1.NodeAffinity, gpuKeys []string) string {
	// Check required node affinity
	if nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, term := range nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if val := extractGPUFromNodeSelectorTerm(term, gpuKeys); val != "" {
				return val
			}
		}
	}

	// Check preferred node affinity
	if nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
		for _, preferred := range nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			if val := extractGPUFromNodeSelectorTerm(preferred.Preference, gpuKeys); val != "" {
				return val
			}
		}
	}

	return ""
}

// extractGPUFromNodeSelectorTerm extracts GPU product from a NodeSelectorTerm.
// It checks MatchExpressions for the given GPU keys with "In" or "Exists" operators.
func extractGPUFromNodeSelectorTerm(term corev1.NodeSelectorTerm, gpuKeys []string) string {
	for _, expr := range term.MatchExpressions {
		for _, key := range gpuKeys {
			if expr.Key == key {
				// For "In" operator, return the first value
				if expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
					return expr.Values[0]
				}
				// For "Exists" operator, we found the key but no specific value
				// Continue searching for other keys that might have values
			}
		}
	}
	return ""
}
