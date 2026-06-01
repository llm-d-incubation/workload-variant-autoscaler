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

package collector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// buildInstanceKeyTestCase drives a single call through CollectReplicaMetrics with
// a source that returns exactly one KV-cache sample, then checks that the resulting
// ReplicaMetrics carries the expected vaName (or none when the label is absent).
type buildInstanceKeyTestCase struct {
	name        string
	labels      map[string]string
	wantVAName  string
	wantSkipped bool // true when buildInstanceKey returns ("","","") → no entry produced
}

var buildInstanceKeyTestCases = []buildInstanceKeyTestCase{
	{
		name: "pod label present – vaName propagated",
		labels: map[string]string{
			"pod":                               "pod-abc",
			"instance":                          "10.0.0.1:8000",
			constants.VariantLabelPrometheusKey: "my-va",
		},
		wantVAName: "my-va",
	},
	{
		name: "pod_name fallback – vaName propagated",
		labels: map[string]string{
			"pod_name":                          "pod-xyz",
			"instance":                          "10.0.0.2:8000",
			constants.VariantLabelPrometheusKey: "other-va",
		},
		wantVAName: "other-va",
	},
	{
		// Pods without llm_d_ai_variant are skipped at line 669 of replica_metrics.go
		// ("Skipping pod that doesn't match any scale target"), so no ReplicaMetrics is produced.
		name: "llm_d_ai_variant label absent – pod skipped, no result",
		labels: map[string]string{
			"pod":      "pod-no-variant",
			"instance": "10.0.0.3:8000",
		},
		wantSkipped: true,
	},
	{
		// Same: empty string is treated the same as missing.
		name: "llm_d_ai_variant label empty string – pod skipped, no result",
		labels: map[string]string{
			"pod":                               "pod-empty-variant",
			"instance":                          "10.0.0.4:8000",
			constants.VariantLabelPrometheusKey: "",
		},
		wantSkipped: true,
	},
	{
		name: "no pod identity labels – entry skipped entirely",
		labels: map[string]string{
			constants.VariantLabelPrometheusKey: "irrelevant",
		},
		wantSkipped: true,
	},
	{
		name: "instance-only (no pod name) – instance used as key, vaName propagated",
		labels: map[string]string{
			"instance":                          "10.0.0.5:8000",
			constants.VariantLabelPrometheusKey: "instance-va",
		},
		wantVAName: "instance-va",
	},
}

func TestBuildInstanceKey_VANameExtraction(t *testing.T) {
	for _, tc := range buildInstanceKeyTestCases {
		t.Run(tc.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			if err := metrics.InitMetrics(registry); err != nil {
				t.Fatalf("InitMetrics: %v", err)
			}

			scheme := runtime.NewScheme()
			if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme: %v", err)
			}
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

			mockSource := &mockMetricsSource{
				refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
					return map[string]*source.MetricResult{
						"kv_cache_usage": {
							Values: []source.MetricValue{
								{
									Labels:    tc.labels,
									Value:     0.5,
									Timestamp: time.Now(),
								},
							},
						},
					}, nil
				},
			}

			collector := NewReplicaMetricsCollector(mockSource, k8sClient)
			results, err := collector.CollectReplicaMetrics(
				context.Background(),
				"test-model",
				"test-ns",
				make(map[string]scaletarget.ScaleTargetAccessor),
				make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
				make(map[string]float64),
			)
			if err != nil {
				t.Fatalf("CollectReplicaMetrics: %v", err)
			}

			if tc.wantSkipped {
				if len(results) != 0 {
					t.Errorf("expected no results for skipped entry, got %d", len(results))
				}
				return
			}

			if len(results) == 0 {
				t.Fatalf("expected at least one ReplicaMetrics result")
			}

			got := results[0].VariantName
			if got != tc.wantVAName {
				t.Errorf("VariantName: got %q, want %q", got, tc.wantVAName)
			}
		})
	}
}
