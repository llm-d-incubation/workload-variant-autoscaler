package fixtures

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var inferenceObjectiveGVR = schema.GroupVersionResource{
	Group:    "inference.networking.x-k8s.io",
	Version:  "v1alpha2",
	Resource: "inferenceobjectives",
}

// EnsureInferenceObjective creates the e2e-default InferenceObjective for GIE flow-control
// queuing when the CRD exists. poolName must match the InferencePool metadata.name (typically the
// EPP Service name with an "-epp" suffix removed, e.g. gaie-sim-epp → gaie-sim).
//
// Returns applied=true if the object exists or was created. If the InferenceObjective API is not
// available on the cluster, returns (false, nil).
func EnsureInferenceObjective(ctx context.Context, dc dynamic.Interface, namespace, poolName string) (applied bool, err error) {
	ri := dc.Resource(inferenceObjectiveGVR).Namespace(namespace)
	poolGroup, gErr := resolveInferencePoolGroup(ctx, dc, namespace, poolName)
	if gErr != nil {
		return false, gErr
	}
	obj := buildInferenceObjective(namespace, poolName, poolGroup)

	if _, cErr := ri.Create(ctx, obj, metav1.CreateOptions{}); cErr != nil {
		if apierrors.IsAlreadyExists(cErr) {
			current, getErr := ri.Get(ctx, "e2e-default", metav1.GetOptions{})
			if getErr != nil {
				return false, fmt.Errorf("get existing InferenceObjective e2e-default: %w", getErr)
			}

			currentSpec, _, specErr := unstructured.NestedMap(current.Object, "spec")
			if specErr != nil {
				return false, fmt.Errorf("read existing InferenceObjective spec: %w", specErr)
			}
			desiredSpec, _, desiredErr := unstructured.NestedMap(obj.Object, "spec")
			if desiredErr != nil {
				return false, fmt.Errorf("read desired InferenceObjective spec: %w", desiredErr)
			}
			if reflect.DeepEqual(currentSpec, desiredSpec) {
				return true, nil
			}

			if setErr := unstructured.SetNestedMap(current.Object, desiredSpec, "spec"); setErr != nil {
				return false, fmt.Errorf("set desired InferenceObjective spec: %w", setErr)
			}
			if _, uErr := ri.Update(ctx, current, metav1.UpdateOptions{}); uErr != nil {
				return false, fmt.Errorf("update InferenceObjective e2e-default: %w", uErr)
			}
			return true, nil
		}
		if inferenceObjectiveAPIMissing(cErr) {
			return false, nil
		}
		return false, fmt.Errorf("create InferenceObjective e2e-default: %w", cErr)
	}
	return true, nil
}

// DeleteInferenceObjective removes e2e-default InferenceObjective if present.
func DeleteInferenceObjective(ctx context.Context, dc dynamic.Interface, namespace string) error {
	err := dc.Resource(inferenceObjectiveGVR).Namespace(namespace).Delete(ctx, "e2e-default", metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) || inferenceObjectiveAPIMissing(err) {
		return nil
	}
	return err
}

func buildInferenceObjective(namespace, poolName, poolGroup string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "inference.networking.x-k8s.io/v1alpha2",
			"kind":       "InferenceObjective",
			"metadata": map[string]interface{}{
				"name":      "e2e-default",
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"priority": int64(0),
				"poolRef": map[string]interface{}{
					"name":  poolName,
					"kind":  "InferencePool",
					"group": poolGroup,
				},
			},
		},
	}
}

func inferenceObjectiveAPIMissing(err error) bool {
	if err == nil {
		return false
	}
	if meta.IsNoMatchError(err) {
		return true
	}
	se, ok := err.(*apierrors.StatusError)
	if !ok {
		return false
	}
	if se.ErrStatus.Reason != metav1.StatusReasonNotFound {
		return false
	}
	if strings.Contains(strings.ToLower(se.ErrStatus.Message), "the server could not find the requested resource") {
		return true
	}
	details := se.ErrStatus.Details
	if details == nil {
		return false
	}
	return details.Group == inferenceObjectiveGVR.Group && details.Kind == inferenceObjectiveGVR.Resource
}

func resolveInferencePoolGroup(ctx context.Context, dc dynamic.Interface, namespace, poolName string) (string, error) {
	if envPoolGroup := os.Getenv("POOL_GROUP"); envPoolGroup != "" {
		return envPoolGroup, nil
	}

	inferencePoolCandidates := []schema.GroupVersionResource{
		{Group: "inference.networking.k8s.io", Version: "v1", Resource: "inferencepools"},
		{Group: "inference.networking.x-k8s.io", Version: "v1alpha2", Resource: "inferencepools"},
	}

	for _, gvr := range inferencePoolCandidates {
		_, err := dc.Resource(gvr).Namespace(namespace).Get(ctx, poolName, metav1.GetOptions{})
		if err == nil {
			return gvr.Group, nil
		}
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		}
		return "", fmt.Errorf("detect InferencePool API group for %q: %w", poolName, err)
	}

	// Default to the primary group used by llm-d charts.
	return "inference.networking.k8s.io", nil
}
