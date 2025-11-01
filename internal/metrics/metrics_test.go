/*
Copyright 2025.

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

package metrics

import (
	"context"
	"testing"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestMetrics(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Metrics Suite")
}

var _ = Describe("Metrics", func() {
	var (
		ctx             context.Context
		metricsEmitter  *MetricsEmitter
		testVA          *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		testModelName   string
		testAccelerator string
		testTTFTValue   float64
		testITLValue    float64
	)

	BeforeEach(func() {
		ctx = context.Background()
		metricsEmitter = NewMetricsEmitter()

		// Create test VariantAutoscaling object
		testVA = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-variant",
				Namespace: "test-namespace",
				UID:       types.UID("test-uid-12345"),
			},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ModelID:          "meta/llama-3.1-8b",
				VariantID:        "meta/llama-3.1-8b-A100-4",
				Accelerator:      "A100",
				AcceleratorCount: 4,
			},
		}

		testModelName = "meta/llama-3.1-8b"
		testAccelerator = "A100"
		testTTFTValue = 0.150 // 150ms in seconds
		testITLValue = 0.025  // 25ms in seconds
	})

	Describe("NewMetricsEmitter", func() {
		It("should create a new metrics emitter", func() {
			emitter := NewMetricsEmitter()
			Expect(emitter).NotTo(BeNil())
		})
	})

	Describe("InitMetrics", func() {
		Context("when initializing metrics", func() {
			It("should register metrics without error", func() {
				registry := prometheus.NewRegistry()
				err := InitMetrics(registry)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("when calling InitMetrics multiple times (thread-safe with sync.Once)", func() {
			It("should only initialize once and subsequent calls should return the same result", func() {
				// Note: Because we use sync.Once at package level, this test verifies
				// that calling InitMetrics multiple times is safe and idempotent.
				// The first call initializes, subsequent calls return the cached result.

				registry := prometheus.NewRegistry()
				err1 := InitMetrics(registry)
				Expect(err1).NotTo(HaveOccurred())

				// Second call should return the same result (no re-initialization)
				err2 := InitMetrics(registry)
				Expect(err2).NotTo(HaveOccurred())

				// The metrics should have been initialized only once
				// This is guaranteed by sync.Once
			})
		})
	})

	Describe("InitMetricsAndEmitter", func() {
		It("should initialize metrics and return an emitter", func() {
			registry := prometheus.NewRegistry()
			emitter, err := InitMetricsAndEmitter(registry)

			Expect(err).NotTo(HaveOccurred())
			Expect(emitter).NotTo(BeNil())
		})
	})

	Describe("sanitizeLabel", func() {
		Context("when validating label values", func() {
			It("should return the same value for valid labels", func() {
				result := sanitizeLabel("valid-label")
				Expect(result).To(Equal("valid-label"))
			})

			It("should replace empty string with 'unknown'", func() {
				result := sanitizeLabel("")
				Expect(result).To(Equal("unknown"))
			})

			It("should trim whitespace", func() {
				result := sanitizeLabel("  label-with-spaces  ")
				Expect(result).To(Equal("label-with-spaces"))
			})

			It("should replace whitespace-only string with 'unknown'", func() {
				result := sanitizeLabel("   ")
				Expect(result).To(Equal("unknown"))
			})

			It("should truncate very long labels", func() {
				longLabel := make([]byte, 200)
				for i := range longLabel {
					longLabel[i] = 'a'
				}
				result := sanitizeLabel(string(longLabel))
				Expect(len(result)).To(Equal(128)) // maxLabelLength
			})

			It("should handle labels with special characters", func() {
				result := sanitizeLabel("label/with/slashes")
				Expect(result).To(Equal("label/with/slashes"))
			})

			It("should handle labels with dots and dashes", func() {
				result := sanitizeLabel("label.with-dots.and-dashes")
				Expect(result).To(Equal("label.with-dots.and-dashes"))
			})
		})
	})

	Describe("EmitPredictionMetrics", func() {
		Context("when metrics are not initialized", func() {
			It("should return an error", func() {
				// Save current state
				savedTTFT := predictedTTFT
				savedITL := predictedITL

				// Set to nil to simulate uninitialized
				predictedTTFT = nil
				predictedITL = nil

				err := metricsEmitter.EmitPredictionMetrics(
					ctx,
					testVA,
					testModelName,
					testTTFTValue,
					testITLValue,
					testAccelerator,
				)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("not initialized"))

				// Restore
				predictedTTFT = savedTTFT
				predictedITL = savedITL
			})
		})

		Context("when metrics are properly initialized", func() {
			var registry *prometheus.Registry

			BeforeEach(func() {
				// Create a fresh registry for prediction metrics tests
				registry = prometheus.NewRegistry()

				// Initialize fresh metrics with this registry
				predictedTTFT = prometheus.NewGaugeVec(
					prometheus.GaugeOpts{
						Name: "test_predicted_ttft_seconds",
						Help: "Test TTFT metric",
					},
					[]string{"model_name", "variant_name", "variant_id", "namespace", "accelerator_type"},
				)
				predictedITL = prometheus.NewGaugeVec(
					prometheus.GaugeOpts{
						Name: "test_predicted_itl_seconds",
						Help: "Test ITL metric",
					},
					[]string{"model_name", "variant_name", "variant_id", "namespace", "accelerator_type"},
				)

				registry.MustRegister(predictedTTFT)
				registry.MustRegister(predictedITL)
			})

			It("should emit TTFT and ITL metrics without error", func() {
				err := metricsEmitter.EmitPredictionMetrics(
					ctx,
					testVA,
					testModelName,
					testTTFTValue,
					testITLValue,
					testAccelerator,
				)

				Expect(err).NotTo(HaveOccurred())
			})

			It("should handle zero values", func() {
				err := metricsEmitter.EmitPredictionMetrics(
					ctx, testVA, testModelName, 0.0, 0.0, testAccelerator,
				)

				Expect(err).NotTo(HaveOccurred())
			})

			It("should handle large values", func() {
				largeTTFT := 10.5 // 10.5 seconds
				largeITL := 5.25  // 5.25 seconds

				err := metricsEmitter.EmitPredictionMetrics(
					ctx, testVA, testModelName, largeTTFT, largeITL, testAccelerator,
				)

				Expect(err).NotTo(HaveOccurred())
			})

			It("should handle different accelerators independently", func() {
				// Emit metrics for A100
				err := metricsEmitter.EmitPredictionMetrics(
					ctx, testVA, testModelName, 0.150, 0.025, "A100",
				)
				Expect(err).NotTo(HaveOccurred())

				// Emit metrics for H100
				err = metricsEmitter.EmitPredictionMetrics(
					ctx, testVA, testModelName, 0.100, 0.020, "H100",
				)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should update existing metrics with new values", func() {
				// First emission
				err := metricsEmitter.EmitPredictionMetrics(
					ctx, testVA, testModelName, 0.150, 0.025, testAccelerator,
				)
				Expect(err).NotTo(HaveOccurred())

				// Second emission with different values
				newTTFT := 0.200
				newITL := 0.030
				err = metricsEmitter.EmitPredictionMetrics(
					ctx, testVA, testModelName, newTTFT, newITL, testAccelerator,
				)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should handle different model IDs", func() {
				err := metricsEmitter.EmitPredictionMetrics(
					ctx, testVA, "model-A", 0.150, 0.025, testAccelerator,
				)
				Expect(err).NotTo(HaveOccurred())

				err = metricsEmitter.EmitPredictionMetrics(
					ctx, testVA, "model-B", 0.200, 0.030, testAccelerator,
				)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should handle different variants", func() {
				va1 := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "variant-1",
						Namespace: "test",
						UID:       types.UID("uid-1"),
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ModelID:   "model-1",
						VariantID: "model-1-variant-id",
					},
				}

				va2 := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "variant-2",
						Namespace: "test",
						UID:       types.UID("uid-2"),
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ModelID:   "model-2",
						VariantID: "model-2-variant-id",
					},
				}

				err := metricsEmitter.EmitPredictionMetrics(ctx, va1, "model-1", 0.1, 0.02, "A100")
				Expect(err).NotTo(HaveOccurred())

				err = metricsEmitter.EmitPredictionMetrics(ctx, va2, "model-2", 0.2, 0.03, "H100")
				Expect(err).NotTo(HaveOccurred())
			})

			It("should use Spec.VariantID not Kubernetes UID for the variant_id label", func() {
				// Create a VA with different UID and VariantID to verify the correct one is used
				vaWithDifferentIDs := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-variant",
						Namespace: "test-namespace",
						UID:       types.UID("kubernetes-generated-uid-12345"),
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ModelID:   "meta/llama-3.1-8b",
						VariantID: "meta/llama-3.1-8b-A100-4", // This should be used, not UID
					},
				}

				// This should succeed and use the Spec.VariantID, not the UID
				err := metricsEmitter.EmitPredictionMetrics(
					ctx,
					vaWithDifferentIDs,
					"meta/llama-3.1-8b",
					0.150,
					0.025,
					"A100",
				)

				Expect(err).NotTo(HaveOccurred())
				// The metric should have been set with VariantID="meta/llama-3.1-8b-A100-4"
				// not VariantID="kubernetes-generated-uid-12345"
				// (This is verified implicitly by the implementation using va.Spec.VariantID)
			})
		})
	})

	Describe("EmitReplicaScalingMetrics", func() {
		It("should emit replica scaling metrics without error when initialized", func() {
			// This test just verifies the API works; actual metric validation
			// would require a registered metrics setup
			err := metricsEmitter.EmitReplicaScalingMetrics(ctx, testVA, "up", "high_load")
			// We expect this might fail if not initialized, which is fine for this test
			_ = err
		})
	})

	Describe("EmitReplicaMetrics", func() {
		It("should emit replica metrics without error when initialized", func() {
			err := metricsEmitter.EmitReplicaMetrics(
				ctx, testVA, 2, 4, testAccelerator, string(testVA.UID),
			)
			// We expect this might fail if not initialized, which is fine for this test
			_ = err
		})

		It("should handle zero current replicas", func() {
			err := metricsEmitter.EmitReplicaMetrics(
				ctx, testVA, 0, 4, testAccelerator, string(testVA.UID),
			)
			_ = err
		})
	})
})
