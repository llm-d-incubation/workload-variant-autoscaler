package fixtures

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	scaledObjectSuffix   = "-so"
	kedaAPIVersion       = "keda.sh/v1alpha1"
	kedaKindScaledObject = "ScaledObject"
)

// ScaledObjectOption is a functional option for configuring ScaledObject resources.
type ScaledObjectOption func(map[string]interface{})

// WithScaledObjectScaleTargetKind sets the Kind and APIVersion on the ScaledObject's ScaleTargetRef.
func WithScaledObjectScaleTargetKind(kind string) ScaledObjectOption {
	return func(spec map[string]interface{}) {
		scaleTargetRef, ok := spec["scaleTargetRef"].(map[string]interface{})
		if !ok {
			return
		}
		scaleTargetRef["kind"] = kind
		// Set appropriate APIVersion based on kind
		switch kind {
		case "LeaderWorkerSet":
			scaleTargetRef["apiVersion"] = "leaderworkerset.x-k8s.io/v1"
		case "Deployment":
			scaleTargetRef["apiVersion"] = "apps/v1"
		default:
			// Keep existing APIVersion for unknown kinds
		}
	}
}

// CreateScaledObject creates a KEDA ScaledObject for WVA. Fails if it already exists.
func CreateScaledObject(
	ctx context.Context,
	crClient client.Client,
	namespace, name, deploymentName, vaName string,
	minReplicas, maxReplicas int32,
	monitoringNamespace string,
	opts ...ScaledObjectOption,
) error {
	obj := buildScaledObject(namespace, name, deploymentName, vaName, minReplicas, maxReplicas, monitoringNamespace, opts...)
	return crClient.Create(ctx, obj)
}

// DeleteScaledObject deletes the ScaledObject. Idempotent; ignores NotFound.
func DeleteScaledObject(ctx context.Context, crClient client.Client, namespace, name string) error {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(kedaAPIVersion)
	obj.SetKind(kedaKindScaledObject)
	obj.SetNamespace(namespace)
	obj.SetName(name + scaledObjectSuffix)
	err := crClient.Delete(ctx, obj)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete ScaledObject %s: %w", name+scaledObjectSuffix, err)
	}
	return nil
}

// EnsureScaledObject creates or replaces the ScaledObject (idempotent for test setup).
func EnsureScaledObject(
	ctx context.Context,
	crClient client.Client,
	namespace, name, deploymentName, vaName string,
	minReplicas, maxReplicas int32,
	monitoringNamespace string,
	opts ...ScaledObjectOption,
) error {
	obj := buildScaledObject(namespace, name, deploymentName, vaName, minReplicas, maxReplicas, monitoringNamespace, opts...)
	existing := &unstructured.Unstructured{}
	existing.SetAPIVersion(kedaAPIVersion)
	existing.SetKind(kedaKindScaledObject)
	key := client.ObjectKey{Namespace: namespace, Name: obj.GetName()}
	err := crClient.Get(ctx, key, existing)
	if err == nil {
		deleteErr := crClient.Delete(ctx, existing)
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing ScaledObject %s: %w", obj.GetName(), deleteErr)
		}
		waitErr := wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetAPIVersion(kedaAPIVersion)
			check.SetKind(kedaKindScaledObject)
			getErr := crClient.Get(ctx, key, check)
			return errors.IsNotFound(getErr), nil
		})
		if waitErr != nil {
			return fmt.Errorf("timeout waiting for ScaledObject %s deletion: %w", obj.GetName(), waitErr)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("check existing ScaledObject %s: %w", obj.GetName(), err)
	}
	return crClient.Create(ctx, obj)
}

func buildScaledObject(namespace, name, deploymentName, vaName string, minReplicas, maxReplicas int32, monitoringNamespace string, opts ...ScaledObjectOption) *unstructured.Unstructured {
	objName := name + scaledObjectSuffix
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(kedaAPIVersion)
	obj.SetKind(kedaKindScaledObject)
	obj.SetNamespace(namespace)
	obj.SetName(objName)
	obj.SetLabels(map[string]string{"test-resource": "true"})

	prometheusURL := "https://kube-prometheus-stack-prometheus." + monitoringNamespace + ".svc.cluster.local:9090"
	// Use "namespace" not "exported_namespace": WVA controller emits the metric with label namespace;
	// exported_namespace is only used by Prometheus Adapter for the external metrics API.
	query := fmt.Sprintf("wva_desired_replicas{variant_name=%q,namespace=%q}", vaName, namespace)

	spec := map[string]interface{}{
		"scaleTargetRef": map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"name":       deploymentName,
		},
		"pollingInterval": int64(5),
		"cooldownPeriod":  int64(30),
		"minReplicaCount": int64(minReplicas),
		"maxReplicaCount": int64(maxReplicas),
		"triggers": []interface{}{
			map[string]interface{}{
				"type": "prometheus",
				"name": "wva-desired-replicas",
				"metadata": map[string]interface{}{
					"serverAddress":       prometheusURL,
					"query":               query,
					"threshold":           "1",
					"activationThreshold": "0",
					"metricType":          "Value", // desired replicas is an absolute gauge; use value directly, not per-pod average
					"unsafeSsl":           "true",
				},
			},
		},
	}
	// Apply functional options
	for _, opt := range opts {
		opt(spec)
	}
	if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
		panic(err)
	}
	return obj
}
