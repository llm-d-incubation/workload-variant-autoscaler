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

package utils

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
)

func TestGroupVariantAutoscalingByModel(t *testing.T) {
	tests := []struct {
		name           string
		vas            []wvav1alpha1.VariantAutoscaling
		expectedGroups int
		expectedKeys   []string
	}{
		{
			name: "same model different accelerators groups together for cost optimization",
			vas: []wvav1alpha1.VariantAutoscaling{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-a100",
						Namespace: "default",
						Labels: map[string]string{
							AcceleratorNameLabel: "A100",
						},
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-h100",
						Namespace: "default",
						Labels: map[string]string{
							AcceleratorNameLabel: "H100",
						},
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
			},
			expectedGroups: 1,
			expectedKeys:   []string{"llama-8b|default"},
		},
		{
			name: "same model same namespace groups together",
			vas: []wvav1alpha1.VariantAutoscaling{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-1",
						Namespace: "default",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-2",
						Namespace: "default",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
			},
			expectedGroups: 1,
			expectedKeys:   []string{"llama-8b|default"},
		},
		{
			name: "different namespaces creates separate groups",
			vas: []wvav1alpha1.VariantAutoscaling{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-1",
						Namespace: "ns1",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-2",
						Namespace: "ns2",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
			},
			expectedGroups: 2,
			expectedKeys:   []string{"llama-8b|ns1", "llama-8b|ns2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GroupVariantAutoscalingByModel(tt.vas)

			if len(result) != tt.expectedGroups {
				t.Errorf("GroupVariantAutoscalingByModel() returned %d groups, want %d", len(result), tt.expectedGroups)
			}

			for _, key := range tt.expectedKeys {
				if _, exists := result[key]; !exists {
					t.Errorf("GroupVariantAutoscalingByModel() missing expected key %q", key)
				}
			}
		})
	}
}
