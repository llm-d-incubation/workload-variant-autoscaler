package fixtures

import (
	"context"
	"fmt"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const (
	kindLeaderWorkerSet = "LeaderWorkerSet"
	kindDeployment      = "Deployment"
	apiVersionLWS       = "leaderworkerset.x-k8s.io/v1"
	apiVersionAppsV1    = "apps/v1"
)

// HPAOption is a functional option for configuring HPA resources.
type HPAOption func(*autoscalingv2.HorizontalPodAutoscaler)

// WithScaleTargetRefKind sets the Kind and APIVersion on the HPA's ScaleTargetRef.
func WithScaleTargetRefKind(kind string) HPAOption {
	return func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
		hpa.Spec.ScaleTargetRef.Kind = kind
		// Set appropriate APIVersion based on kind
		switch kind {
		case kindLeaderWorkerSet:
			hpa.Spec.ScaleTargetRef.APIVersion = apiVersionLWS
		case kindDeployment:
			hpa.Spec.ScaleTargetRef.APIVersion = apiVersionAppsV1
		default:
			// Keep existing APIVersion for unknown kinds
		}
	}
}

// CreateHPA creates a HorizontalPodAutoscaler for WVA integration. Fails if it already exists.
func CreateHPA(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, deploymentName, vaName string,
	minReplicas, maxReplicas int32,
	opts ...HPAOption,
) error {
	hpa := buildHPA(namespace, name, deploymentName, vaName, minReplicas, maxReplicas, opts...)
	_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa, metav1.CreateOptions{})
	return err
}

// DeleteHPA deletes the HorizontalPodAutoscaler. Idempotent; ignores NotFound.
func DeleteHPA(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name string) error {
	hpaName := name + "-hpa"
	err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, hpaName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete HPA %s: %w", hpaName, err)
	}
	return nil
}

// EnsureHPA creates or replaces the HPA (idempotent for test setup).
func EnsureHPA(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, deploymentName, vaName string,
	minReplicas, maxReplicas int32,
	opts ...HPAOption,
) error {
	hpa := buildHPA(namespace, name, deploymentName, vaName, minReplicas, maxReplicas, opts...)
	existing, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, hpa.Name, metav1.GetOptions{})
	if err == nil && existing != nil {
		deleteErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, hpa.Name, metav1.DeleteOptions{})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing HPA %s: %w", hpa.Name, deleteErr)
		}
		waitErr := wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, hpa.Name, metav1.GetOptions{})
			return errors.IsNotFound(err), nil
		})
		if waitErr != nil {
			return fmt.Errorf("timeout waiting for HPA %s deletion: %w", hpa.Name, waitErr)
		}
	} else if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("check existing HPA %s: %w", hpa.Name, err)
	}
	_, err = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa, metav1.CreateOptions{})
	return err
}

func buildHPA(namespace, name, deploymentName, vaName string, minReplicas, maxReplicas int32, opts ...HPAOption) *autoscalingv2.HorizontalPodAutoscaler {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-hpa",
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": "true"},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: apiVersionAppsV1,
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			MinReplicas: ptr.To(minReplicas),
			MaxReplicas: maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ExternalMetricSourceType,
					External: &autoscalingv2.ExternalMetricSource{
						Metric: autoscalingv2.MetricIdentifier{
							Name: "wva_desired_replicas",
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"variant_name": vaName},
							},
						},
						Target: autoscalingv2.MetricTarget{
							Type:         autoscalingv2.AverageValueMetricType,
							AverageValue: resource.NewQuantity(1, resource.DecimalSI),
						},
					},
				},
			},
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(0)),
					Policies:                   []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 10, PeriodSeconds: 15}},
				},
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(60)),
					Policies:                   []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 1, PeriodSeconds: 60}},
				},
			},
		},
	}
	if minReplicas == 0 {
		hpa.Annotations = map[string]string{"autoscaling.alpha.kubernetes.io/feature-gates": "HPAScaleToZero=true"}
	}
	// Apply functional options
	for _, opt := range opts {
		opt(hpa)
	}
	return hpa
}
