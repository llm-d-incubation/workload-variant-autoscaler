package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

// Helper functions for common resource types with standard backoff
func GetConfigMapWithBackoff(ctx context.Context, c client.Client, name, namespace string, cm *corev1.ConfigMap) error {
	return resources.GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, cm, constants.StandardBackoff, "ConfigMap")
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
func FindModelSLO(cmData map[string]string, targetModel string) (*domain.ServiceClassEntry, string /* class name */, error) {
	for key, val := range cmData {
		var sc domain.ServiceClass
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

// GetProductKeys returns unique vendor product (node label) keys, in stable (sorted) order
func GetProductKeys() []string {
	labels := make(map[string]bool, len(constants.VendorResources))
	for _, res := range constants.VendorResources {
		labels[res.ProductLabel] = true
		for _, label := range res.ProductLabelAliases {
			labels[label] = true
		}
	}
	return slices.Sorted(maps.Keys(labels))
}

// GetAcceleratorNameFromScaleTarget extracts GPU product information from a scale target's nodeSelector or nodeAffinity.
// GPU product information is checked against keys listed in constants.VendorResources.
// If not found in nodeSelector or nodeAffinity, falls back to the AcceleratorNameLabel on the VariantAutoscaling.
// Returns the first matching value found, or constants.DefaultAcceleratorName ("unknown") if none are found.
// The sentinel allows callers to proceed without hard-stopping; the GPU limiter resolves
// it to the real type in homogeneous clusters before it reaches status or metrics.
func GetAcceleratorNameFromScaleTarget(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling, scaleTarget scaletarget.ScaleTargetAccessor) string {
	// Check scaleTarget for accelerator name if it's not nil
	if scaleTarget != nil {
		podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
		if podTemplateSpec != nil {
			prodKeys := GetProductKeys()

			// Check nodeSelector first
			if podTemplateSpec.Spec.NodeSelector != nil {
				for _, key := range prodKeys {
					if val, ok := podTemplateSpec.Spec.NodeSelector[key]; ok {
						return val
					}
				}
			}

			// Check nodeAffinity
			if podTemplateSpec.Spec.Affinity != nil && podTemplateSpec.Spec.Affinity.NodeAffinity != nil {
				if val := extractGPUFromNodeAffinity(podTemplateSpec.Spec.Affinity.NodeAffinity, prodKeys); val != "" {
					return val
				}
			}
		}
	}

	// Fall back to VariantAutoscaling label
	if va != nil && va.Labels != nil {
		if accName, exists := va.Labels[AcceleratorNameLabel]; exists {
			return accName
		}
	}
	return constants.DefaultAcceleratorName
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
