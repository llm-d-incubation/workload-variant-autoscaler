package utils

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

func TestQueryPrometheusWithBackoff(t *testing.T) {
	t.Parallel()

	const query = "test_query"

	cases := []struct {
		name           string
		failures       int
		expectErr      bool
		expectAttempts int
		description    string
	}{
		{
			name:           "retries_then_succeeds",
			failures:       2,
			expectErr:      false,
			expectAttempts: 3,
			description:    "Transient blips resolve before backoff steps are exhausted and we return the mocked result.",
		},
		{
			name:           "exhausts_retries",
			failures:       PrometheusQueryBackoff.Steps + 5,
			expectErr:      true,
			expectAttempts: PrometheusQueryBackoff.Steps,
			description:    "Every attempt keeps failing so backoff gives up and surfaces the last Prometheus error.",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mock := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					query: model.Vector{
						&model.Sample{
							Value: model.SampleValue(42),
							Metric: model.Metric{
								"__name__": "test_metric",
							},
						},
					},
				},
				QueryFailCounts: map[string]int{
					query: tc.failures,
				},
				QueryCallCounts: make(map[string]int),
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			val, warn, err := QueryPrometheusWithBackoff(ctx, mock, query)

			if tc.expectErr {
				assert.Error(t, err, tc.description)
				assert.Nil(t, val)
				assert.Nil(t, warn)
				t.Log(mock.QueryCallCounts[query])
				assert.Equal(t, mock.QueryCallCounts[query], tc.expectAttempts)
				return
			}

			assert.NoError(t, err, tc.description)
			assert.Equal(t, tc.expectAttempts, mock.QueryCallCounts[query])

			vec, ok := val.(model.Vector)
			if assert.True(t, ok, tc.description) {
				assert.Len(t, vec, 1)
				assert.Equal(t, model.SampleValue(42), vec[0].Value)
			}
			assert.Nil(t, warn)
		})
	}
}

func TestGetAcceleratorNameFromDeployment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		va         *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		deployment *appsv1.Deployment
		expected   string
	}{
		{
			name: "nvidia_gpu_from_nodeSelector",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"nvidia.com/gpu.product": "Tesla-T4",
							},
						},
					},
				},
			},
			expected: "Tesla-T4",
		},
		{
			name: "amd_gpu_from_nodeSelector",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"amd.com/gpu.product-name": "MI250",
							},
						},
					},
				},
			},
			expected: "MI250",
		},
		{
			name: "gke_accelerator_from_nodeSelector",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"cloud.google.com/gke-accelerator": "nvidia-tesla-v100",
							},
						},
					},
				},
			},
			expected: "nvidia-tesla-v100",
		},
		{
			name: "nvidia_gpu_from_required_nodeAffinity",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "nvidia.com/gpu.product",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"A100"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "A100",
		},
		{
			name: "amd_gpu_from_preferred_nodeAffinity",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
										{
											Weight: 1,
											Preference: corev1.NodeSelectorTerm{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "amd.com/gpu.product-name",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"MI300"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "MI300",
		},
		{
			name: "fallback_to_va_label",
			va: &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						AcceleratorNameLabel: "H100",
					},
				},
			},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			expected: "H100",
		},
		{
			name: "nodeSelector_takes_precedence_over_va_label",
			va: &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						AcceleratorNameLabel: "H100",
					},
				},
			},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"nvidia.com/gpu.product": "A100",
							},
						},
					},
				},
			},
			expected: "A100",
		},
		{
			name: "nodeAffinity_takes_precedence_over_va_label",
			va: &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						AcceleratorNameLabel: "H100",
					},
				},
			},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "nvidia.com/gpu.product",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"V100"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "V100",
		},
		{
			name: "nil_deployment_with_va_label",
			va: &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						AcceleratorNameLabel: "T4",
					},
				},
			},
			deployment: nil,
			expected:   "T4",
		},
		{
			name:       "nil_va_and_deployment_returns_empty",
			va:         nil,
			deployment: nil,
			expected:   "",
		},
		{
			name: "no_gpu_info_returns_empty",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			expected: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := GetAcceleratorNameFromDeployment(tc.va, tc.deployment)
			assert.Equal(t, tc.expected, result)
		})
	}
}
