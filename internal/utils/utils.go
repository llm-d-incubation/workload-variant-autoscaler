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

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	infernoConfig "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

// Global backoff configurations
var (
	// Standard backoff for most operations
	StandardBackoff = wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    5,
	}

	// Slow backoff for operations that need more time
	ReconcileBackoff = wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2.0,
		Steps:    5,
	}

	// Prometheus validation backoff with longer intervals
	// TODO: investigate why Prometheus needs longer backoff durations
	PrometheusBackoff = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    6, // 5s, 10s, 20s, 40s, 80s, 160s = ~5 minutes total
	}
)

const (
	// DefaultScaleToZeroRetentionPeriod is the default time to wait after the last request
	// before scaling down to zero replicas. This default applies when scale-to-zero is enabled
	// but no explicit retention period is specified.
	DefaultScaleToZeroRetentionPeriod = 10 * time.Minute

	// DefaultScaleToZeroConfigMapName is the default name of the ConfigMap that stores
	// per-model scale-to-zero configuration. This ConfigMap should be in the same namespace
	// as the VariantAutoscaling resources.
	DefaultScaleToZeroConfigMapName = "model-scale-to-zero-config"

	// GlobalDefaultsKey is the special key in the ConfigMap used to specify global defaults
	// for all models. Models can override these defaults with their specific configuration.
	GlobalDefaultsKey = "__defaults__"
)

// ModelScaleToZeroConfig represents the scale-to-zero configuration for a single model.
// Uses pointer for EnableScaleToZero to distinguish between "not set" (nil) and explicitly set to false.
// This allows partial overrides where a model can inherit enableScaleToZero from global defaults
// while overriding only the retentionPeriod.
type ModelScaleToZeroConfig struct {
	// ModelID is the unique identifier for the model (original ID with any characters)
	ModelID string `yaml:"modelID,omitempty" json:"modelID,omitempty"`
	// EnableScaleToZero enables scaling the model to zero replicas when there is no traffic.
	// Use pointer to allow omitting this field and inheriting from global defaults.
	// nil = not set (inherit from defaults), true = enabled, false = disabled
	EnableScaleToZero *bool `yaml:"enableScaleToZero,omitempty" json:"enableScaleToZero,omitempty"`
	// RetentionPeriod specifies how long to wait after the last request before scaling to zero.
	// This is stored as a string duration (e.g., "5m", "1h", "30s").
	// Empty string = not set (inherit from defaults)
	RetentionPeriod string `yaml:"retentionPeriod,omitempty" json:"retentionPeriod,omitempty"`
}

// ScaleToZeroConfigData holds pre-read scale-to-zero configuration data for all models.
// This follows the project pattern of reading ConfigMaps once per reconcile loop.
// Maps model ID to its configuration.
type ScaleToZeroConfigData map[string]ModelScaleToZeroConfig

// IsScaleToZeroEnabled determines if scale-to-zero is enabled for a specific model.
// Supports partial overrides: if a model config exists but EnableScaleToZero is nil,
// it falls through to check global defaults. This allows models to override only
// retentionPeriod while inheriting the enableScaleToZero setting from defaults.
//
// Configuration priority (highest to lowest):
// 1. Per-model configuration in ConfigMap (if EnableScaleToZero is set)
// 2. Global defaults in ConfigMap (under "__defaults__" key)
// 3. WVA_SCALE_TO_ZERO environment variable
// 4. System default (false)
func IsScaleToZeroEnabled(configData ScaleToZeroConfigData, modelID string) bool {
	// Check per-model setting first (highest priority)
	// With the new YAML format, model IDs are used directly without sanitization
	if config, exists := configData[modelID]; exists {
		// If EnableScaleToZero is explicitly set (not nil), use it
		if config.EnableScaleToZero != nil {
			return *config.EnableScaleToZero
		}
		// If nil, fall through to check global defaults (allows partial override)
	}

	// Check global defaults in ConfigMap (second priority)
	if globalConfig, exists := configData[GlobalDefaultsKey]; exists {
		if globalConfig.EnableScaleToZero != nil {
			return *globalConfig.EnableScaleToZero
		}
	}

	// Fall back to global environment variable (third priority)
	return strings.EqualFold(os.Getenv("WVA_SCALE_TO_ZERO"), "true")
}

// ValidateRetentionPeriod validates a retention period string.
// Returns the parsed duration and an error if validation fails.
// Validation rules:
//   - Must be a valid Go duration format (e.g., "5m", "1h", "30s")
//   - Must be positive (> 0)
//   - Should be reasonable for production use (warns if > 24h)
func ValidateRetentionPeriod(retentionPeriod string) (time.Duration, error) {
	if retentionPeriod == "" {
		return 0, fmt.Errorf("retention period cannot be empty")
	}

	duration, err := time.ParseDuration(retentionPeriod)
	if err != nil {
		return 0, fmt.Errorf("invalid duration format: %w", err)
	}

	if duration <= 0 {
		return 0, fmt.Errorf("retention period must be positive, got %v", duration)
	}

	// Warn if retention period is unusually long (> 24 hours)
	// This is not an error, just a warning for potentially misconfigured values
	if duration > 24*time.Hour {
		logger.Log.Warn("Retention period is unusually long",
			"retentionPeriod", retentionPeriod,
			"duration", duration,
			"recommendation", "Consider using a shorter period for better resource utilization")
	}

	return duration, nil
}

// GetScaleToZeroRetentionPeriod returns the pod retention period for scale-to-zero for a specific model.
// Configuration priority (highest to lowest):
// 1. Per-model retention period in ConfigMap
// 2. Global defaults retention period in ConfigMap (under "__defaults__" key)
// 3. System default (10 minutes)
func GetScaleToZeroRetentionPeriod(configData ScaleToZeroConfigData, modelID string) time.Duration {
	// Check per-model retention period first (highest priority)
	// With the new YAML format, model IDs are used directly without sanitization
	if config, exists := configData[modelID]; exists && config.RetentionPeriod != "" {
		duration, err := ValidateRetentionPeriod(config.RetentionPeriod)
		if err != nil {
			logger.Log.Warn("Invalid retention period for model, checking global defaults",
				"modelID", modelID,
				"retentionPeriod", config.RetentionPeriod,
				"error", err)
			// Don't return here, fall through to check global defaults
		} else {
			return duration
		}
	}

	// Check global defaults retention period (second priority)
	if globalConfig, exists := configData[GlobalDefaultsKey]; exists && globalConfig.RetentionPeriod != "" {
		duration, err := ValidateRetentionPeriod(globalConfig.RetentionPeriod)
		if err != nil {
			logger.Log.Warn("Invalid global default retention period, using system default",
				"retentionPeriod", globalConfig.RetentionPeriod,
				"error", err)
			return DefaultScaleToZeroRetentionPeriod
		}
		return duration
	}

	// Fall back to system default (lowest priority)
	return DefaultScaleToZeroRetentionPeriod
}

// GetMinNumReplicas returns the minimum number of replicas for a specific model based on
// scale-to-zero configuration. Returns 0 if scale-to-zero is enabled, otherwise returns 1.
// DEPRECATED: Use GetVariantMinReplicas for per-variant control.
func GetMinNumReplicas(configData ScaleToZeroConfigData, modelID string) int {
	if IsScaleToZeroEnabled(configData, modelID) {
		return 0
	}
	return 1
}

// GetVariantMinReplicas returns the minimum number of replicas for a specific variant.
// If va.Spec.MinReplicas is set, returns that value.
// Otherwise, returns 0 (default).
func GetVariantMinReplicas(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) int32 {
	if va.Spec.MinReplicas != nil {
		return int32(*va.Spec.MinReplicas)
	}
	return 0 // Default value
}

// GetVariantMaxReplicas returns the maximum number of replicas for a specific variant.
// If va.Spec.MaxReplicas is set, returns that value.
// Otherwise, returns -1 to indicate no upper bound (unlimited).
func GetVariantMaxReplicas(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) int32 {
	if va.Spec.MaxReplicas != nil {
		return int32(*va.Spec.MaxReplicas)
	}
	return -1 // No upper bound
}

// GetResourceWithBackoff performs a Get operation with exponential backoff retry logic
func GetResourceWithBackoff[T client.Object](ctx context.Context, c client.Client, objKey client.ObjectKey, obj T, backoff wait.Backoff, resourceType string) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, objKey, obj)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, err // Don't retry on notFound errors
			}

			logger.Log.Error(err, "transient error getting resource, retrying - ",
				"resourceType: ", resourceType,
				" name: ", objKey.Name,
				" namespace: ", objKey.Namespace)
			return false, nil // Retry on transient errors
		}

		return true, nil
	})
}

// Helper functions for common resource types with standard backoff
func GetDeploymentWithBackoff(ctx context.Context, c client.Client, name, namespace string, deploy *appsv1.Deployment) error {
	return GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, deploy, StandardBackoff, "Deployment")
}

func GetConfigMapWithBackoff(ctx context.Context, c client.Client, name, namespace string, cm *corev1.ConfigMap) error {
	return GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, cm, StandardBackoff, "ConfigMap")
}

func GetVariantAutoscalingWithBackoff(ctx context.Context, c client.Client, name, namespace string, va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) error {
	return GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, va, StandardBackoff, "VariantAutoscaling")
}

// UpdateStatusWithBackoff performs a Status Update operation with exponential backoff retry logic
func UpdateStatusWithBackoff[T client.Object](ctx context.Context, c client.Client, obj T, backoff wait.Backoff, resourceType string) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		err := c.Status().Update(ctx, obj)
		if err != nil {
			if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
				logger.Log.Error(err, "permanent error updating status for resource ", resourceType, ", name: ", obj.GetName())
				return false, err // Don't retry on permanent errors
			}
			logger.Log.Error(err, "transient error updating status, retrying for resource ", resourceType, ", name: ", obj.GetName())
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
			logger.Log.Warn("failed to parse accelerator cost in configmap, skipping accelerator", "name", key)
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
			logger.Log.Warn("failed to parse service class data, skipping service class", "key", key, "err", err)
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

// add variant profile data to inferno system data
func AddVariantProfileToSystemData(
	sd *infernoConfig.SystemData,
	modelName string,
	accelerator string,
	acceleratorCount int,
	variantProfile *llmdVariantAutoscalingV1alpha1.VariantProfile) (err error) {

	// extract decode model (itl) parameters
	decodeParms := variantProfile.PerfParms.DecodeParms
	if len(decodeParms) < 2 {
		return fmt.Errorf("length of decodeParms should be 2")
	}

	var alpha, beta float64
	if alpha, err = strconv.ParseFloat(decodeParms["alpha"], 32); err != nil {
		return err
	}
	if beta, err = strconv.ParseFloat(decodeParms["beta"], 32); err != nil {
		return err
	}

	// extract prefill model (ttft) parameters
	prefillParms := variantProfile.PerfParms.PrefillParms
	if len(prefillParms) < 2 {
		return fmt.Errorf("length of prefillParms should be 2")
	}

	var gamma, delta float64
	if gamma, err = strconv.ParseFloat(prefillParms["gamma"], 32); err != nil {
		return err
	}
	if delta, err = strconv.ParseFloat(prefillParms["delta"], 32); err != nil {
		return err
	}

	sd.Spec.Models.PerfData = append(sd.Spec.Models.PerfData,
		infernoConfig.ModelAcceleratorPerfData{
			Name:         modelName,
			Acc:          accelerator,
			AccCount:     acceleratorCount,
			MaxBatchSize: variantProfile.MaxBatchSize,
			DecodeParms: infernoConfig.DecodeParms{
				Alpha: float32(alpha),
				Beta:  float32(beta),
			},
			PrefillParms: infernoConfig.PrefillParms{
				Gamma: float32(gamma),
				Delta: float32(delta),
			},
		})
	return nil
}

// Add server specs to inferno system data
func AddServerInfoToSystemData(
	sd *infernoConfig.SystemData,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	className string,
	metrics *interfaces.VariantMetrics,
	scaleToZeroConfigData ScaleToZeroConfigData) (err error) {

	// Get current allocation (single variant)
	currentAlloc := va.Status.CurrentAlloc

	// Validate VA has required spec fields (single-variant architecture)
	if va.Spec.VariantID == "" || va.Spec.Accelerator == "" {
		return fmt.Errorf("variant spec incomplete for %s", va.Name)
	}

	// Validate metrics are provided
	if metrics == nil {
		return fmt.Errorf("metrics cannot be nil for variant %s", va.Name)
	}

	// Convert internal metrics to inferno config types
	serverLoadSpec := &infernoConfig.ServerLoadSpec{
		ArrivalRate:  metrics.Load.ArrivalRate,
		AvgInTokens:  metrics.Load.AvgInputTokens,
		AvgOutTokens: metrics.Load.AvgOutputTokens,
	}

	// Parse cost from spec (cost per replica)
	var cost float64
	if cost, err = strconv.ParseFloat(va.Spec.VariantCost, 32); err != nil || !CheckValue(cost) {
		cost = 0
	}

	AllocationData := &infernoConfig.AllocationData{
		Accelerator: va.Spec.Accelerator, // Use spec field (single-variant architecture)
		NumReplicas: currentAlloc.NumReplicas,
		MaxBatch:    va.Spec.VariantProfile.MaxBatchSize, // Use spec field (single-variant architecture)
		Cost:        float32(cost),
		ITLAverage:  metrics.ITLAverage,
		TTFTAverage: metrics.TTFTAverage,
		Load:        *serverLoadSpec,
	}

	// all server data
	// Determine minimum replicas from VA spec (per-variant control)
	minNumReplicas := GetVariantMinReplicas(va)

	// Log retention period if scale-to-zero is enabled (for future use in scale-to-zero logic)
	if IsScaleToZeroEnabled(scaleToZeroConfigData, va.Spec.ModelID) {
		retentionPeriod := GetScaleToZeroRetentionPeriod(scaleToZeroConfigData, va.Spec.ModelID)
		logger.Log.Debug("Scale-to-zero retention period configured",
			"modelID", va.Spec.ModelID,
			"variant", va.Name,
			"namespace", va.Namespace,
			"retentionPeriod", retentionPeriod)
	}

	serverSpec := &infernoConfig.ServerSpec{
		Name:            FullName(va.Name, va.Namespace),
		Class:           className,
		Model:           va.Spec.ModelID,
		KeepAccelerator: true,
		MinNumReplicas:  int(minNumReplicas),
		CurrentAlloc:    *AllocationData,
		DesiredAlloc:    infernoConfig.AllocationData{},
	}

	// set max batch size from variant profile
	if va.Spec.VariantProfile.MaxBatchSize > 0 {
		serverSpec.MaxBatchSize = va.Spec.VariantProfile.MaxBatchSize
	}

	sd.Spec.Servers.Spec = append(sd.Spec.Servers.Spec, *serverSpec)
	return nil
}

// Adapter from inferno alloc solution to optimized alloc
func CreateOptimizedAlloc(name string,
	namespace string,
	variantID string, // Still needed for logging/debugging
	allocationSolution *infernoConfig.AllocationSolution) (*llmdVariantAutoscalingV1alpha1.OptimizedAlloc, error) {

	serverName := FullName(name, namespace)
	var allocationData infernoConfig.AllocationData
	var exists bool
	if allocationData, exists = allocationSolution.Spec[serverName]; !exists {
		return nil, fmt.Errorf("server %s not found", serverName)
	}
	logger.Log.Debug("Creating optimized allocation",
		"variant-id", variantID,
		"accelerator", allocationData.Accelerator,
		"num-replicas", allocationData.NumReplicas)
	// Note: VariantID and Accelerator are not included as they're in the parent VA spec
	optimizedAlloc := &llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
		LastRunTime: metav1.NewTime(time.Now()),
		NumReplicas: allocationData.NumReplicas,
		Reason:      "Optimizer solution: cost and latency optimized allocation",
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
// If the specified model is not found, falls back to "default/default" SLO
func FindModelSLO(cmData map[string]string, targetModel string) (*interfaces.ServiceClassEntry, string /* class name */, error) {
	// First pass: try to find exact match for targetModel
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

	// Model not found, try fallback to default/default
	logger.Log.Info("Model SLO not found, attempting fallback to default/default",
		"model", targetModel)

	// Second pass: try to find default/default
	for _, val := range cmData {
		var sc interfaces.ServiceClass
		if err := yaml.Unmarshal([]byte(val), &sc); err != nil {
			continue // Skip unparseable entries
		}

		for _, entry := range sc.Data {
			if entry.Model == "default/default" {
				logger.Log.Info("Using fallback SLO from default/default",
					"original-model", targetModel,
					"service-class", sc.Name,
					"slo-tpot", entry.SLOTPOT,
					"slo-ttft", entry.SLOTTFT)
				return &entry, sc.Name, nil
			}
		}
	}

	// Neither targetModel nor default/default found
	return nil, "", fmt.Errorf("model %q not found in any service class and default/default fallback not found", targetModel)
}

func Ptr[T any](v T) *T {
	return &v
}

// ValidatePrometheusAPIWithBackoff validates Prometheus API connectivity with retry logic
func ValidatePrometheusAPIWithBackoff(ctx context.Context, promAPI promv1.API, backoff wait.Backoff) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		// Test with a simple query that should always work
		query := "up"
		_, _, err := promAPI.Query(ctx, query, time.Now())
		if err != nil {
			logger.Log.Error(err, "Prometheus API validation failed, retrying - ",
				"query: ", query,
				"error: ", err.Error())
			return false, nil // Retry on transient errors
		}

		logger.Log.Info("Prometheus API validation successful with query", "query", query)
		return true, nil
	})
}

// ValidatePrometheusAPI validates Prometheus API connectivity using standard Prometheus backoff
func ValidatePrometheusAPI(ctx context.Context, promAPI promv1.API) error {
	return ValidatePrometheusAPIWithBackoff(ctx, promAPI, PrometheusBackoff)
}

// GetConfigValue retrieves a value from a ConfigMap with a default fallback
func GetConfigValue(data map[string]string, key, def string) string {
	if v, ok := data[key]; ok {
		return v
	}
	return def
}

// SuggestResourceNameFromVariantID transforms a variant_id into a valid Kubernetes resource name.
// Kubernetes resource names must match DNS-1123: ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$
//
// Transformations applied:
//   - Convert to lowercase
//   - Replace slashes (/) with hyphens (-)
//   - Replace dots (.) with hyphens (-)
//   - Remove any remaining invalid characters
//
// Example:
//
//	variant_id: "meta/llama-3.1-8b-A100-1"
//	suggested:  "meta-llama-3-1-8b-a100-1"
//
// Note: The suggested name may differ from the actual VariantAutoscaling resource name
// (va.Name), which is typically the same as the Deployment name. This is intentional:
//   - variant_id is the business identifier (e.g., "meta/llama-3.1-8b-A100-1")
//   - va.Name is the K8s resource name (e.g., "vllm-deployment")
//
// Both are exposed as Prometheus labels:
//   - variant_name = va.Name (K8s resource name)
//   - variant_id = va.Spec.VariantID (business identifier)
func SuggestResourceNameFromVariantID(variantID string) string {
	// Convert to lowercase
	name := strings.ToLower(variantID)

	// Replace slashes with hyphens
	name = strings.ReplaceAll(name, "/", "-")

	// Replace dots with hyphens
	name = strings.ReplaceAll(name, ".", "-")

	// Remove any characters that aren't lowercase letters, numbers, or hyphens
	reg := regexp.MustCompile(`[^a-z0-9-]`)
	name = reg.ReplaceAllString(name, "")

	// Ensure it doesn't start or end with hyphen
	name = strings.Trim(name, "-")

	return name
}

// ValidateVariantAutoscalingName checks if the VariantAutoscaling resource name
// matches the suggested name derived from variant_id and logs a warning if not.
//
// This function does NOT require variant_name to match variant_id. It simply logs
// a notice when they differ to help users understand the relationship between:
//   - variant_name (va.Name): K8s resource name, typically matches Deployment name
//   - variant_id (va.Spec.VariantID): Business identifier with potentially non-K8s-compliant chars
//
// Both are valid and serve different purposes in Prometheus queries and resource management.
func ValidateVariantAutoscalingName(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) {
	suggested := SuggestResourceNameFromVariantID(va.Spec.VariantID)
	if va.Name != suggested {
		logger.Log.Info("VariantAutoscaling name differs from normalized variant_id",
			"resource-name", va.Name,
			"variant-id", va.Spec.VariantID,
			"suggested-name", suggested,
			"note", "This is normal - variant_name (resource name) and variant_id serve different purposes. Both are available as Prometheus labels for querying.")
	}
}
