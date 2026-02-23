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

// CreateHPA creates a HorizontalPodAutoscaler resource for WVA integration
func CreateHPA(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, deploymentName, vaName string,
	minReplicas, maxReplicas int32,
) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-hpa",
			Namespace: namespace,
			Labels: map[string]string{
				"test-resource": "true",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
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
								MatchLabels: map[string]string{
									"variant_name": vaName,
								},
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
					Policies: []autoscalingv2.HPAScalingPolicy{
						{
							Type:          autoscalingv2.PodsScalingPolicy,
							Value:         10,
							PeriodSeconds: 15,
						},
					},
				},
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(60)),
					Policies: []autoscalingv2.HPAScalingPolicy{
						{
							Type:          autoscalingv2.PodsScalingPolicy,
							Value:         1,
							PeriodSeconds: 60,
						},
					},
				},
			},
		},
	}

	// Add scale-to-zero behavior if minReplicas is 0
	if minReplicas == 0 {
		hpa.Annotations = map[string]string{
			"autoscaling.alpha.kubernetes.io/feature-gates": "HPAScaleToZero=true",
		}
	}

	// Check if HPA already exists and delete it if it does (idempotent cleanup)
	existing, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, hpa.Name, metav1.GetOptions{})
	if err == nil && existing != nil {
		// HPA exists, delete it first to ensure clean state
		deleteErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, hpa.Name, metav1.DeleteOptions{})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("failed to delete existing HPA %s: %w", hpa.Name, deleteErr)
		}
		// Wait for deletion to complete
		waitErr := wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, hpa.Name, metav1.GetOptions{})
			return errors.IsNotFound(err), nil
		})
		if waitErr != nil {
			return fmt.Errorf("timeout waiting for HPA %s deletion: %w", hpa.Name, waitErr)
		}
	} else if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing HPA %s: %w", hpa.Name, err)
	}

	_, err = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa, metav1.CreateOptions{})
	return err
}
