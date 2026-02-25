package pipeline

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// boolPtr is a helper to create a pointer to a bool value
func boolPtr(b bool) *bool {
	return &b
}

var _ = Describe("Enforcer", func() {
	var (
		ctx             context.Context
		enforcer        *Enforcer
		targets         map[string]int
		variantAnalyses []interfaces.VariantSaturationAnalysis
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("EnforcePolicy", func() {
		Context("when scale-to-zero is enabled", func() {
			Context("and there are no requests", func() {
				BeforeEach(func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, nil
					})
					targets = map[string]int{
						"variant-a": 2,
						"variant-b": 1,
					}
					variantAnalyses = []interfaces.VariantSaturationAnalysis{
						{VariantName: "variant-a", Cost: 1.0},
						{VariantName: "variant-b", Cost: 2.0},
					}
				})

				It("should scale all variants to zero", func() {
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {
							EnableScaleToZero: boolPtr(true),
							RetentionPeriod:   "10m",
						},
					}

					result, applied := enforcer.EnforcePolicy(
						ctx,
						"test-model",
						"test-ns",
						targets,
						variantAnalyses,
						scaleToZeroConfig,
					)

					Expect(applied).To(BeTrue())
					Expect(result["variant-a"]).To(Equal(0))
					Expect(result["variant-b"]).To(Equal(0))
				})
			})

			Context("and there are requests", func() {
				BeforeEach(func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 10, nil
					})
					targets = map[string]int{
						"variant-a": 2,
						"variant-b": 1,
					}
					variantAnalyses = []interfaces.VariantSaturationAnalysis{
						{VariantName: "variant-a", Cost: 1.0},
						{VariantName: "variant-b", Cost: 2.0},
					}
				})

				It("should keep targets unchanged", func() {
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {
							EnableScaleToZero: boolPtr(true),
							RetentionPeriod:   "10m",
						},
					}

					result, applied := enforcer.EnforcePolicy(
						ctx,
						"test-model",
						"test-ns",
						targets,
						variantAnalyses,
						scaleToZeroConfig,
					)

					Expect(applied).To(BeFalse())
					Expect(result["variant-a"]).To(Equal(2))
					Expect(result["variant-b"]).To(Equal(1))
				})
			})

			Context("and request count query fails", func() {
				BeforeEach(func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, errors.New("prometheus unavailable")
					})
					targets = map[string]int{
						"variant-a": 2,
						"variant-b": 1,
					}
					variantAnalyses = []interfaces.VariantSaturationAnalysis{
						{VariantName: "variant-a", Cost: 1.0},
						{VariantName: "variant-b", Cost: 2.0},
					}
				})

				It("should keep targets unchanged", func() {
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {
							EnableScaleToZero: boolPtr(true),
							RetentionPeriod:   "10m",
						},
					}

					result, applied := enforcer.EnforcePolicy(
						ctx,
						"test-model",
						"test-ns",
						targets,
						variantAnalyses,
						scaleToZeroConfig,
					)

					Expect(applied).To(BeFalse())
					Expect(result["variant-a"]).To(Equal(2))
					Expect(result["variant-b"]).To(Equal(1))
				})
			})
		})

		Context("when scale-to-zero is disabled", func() {
			BeforeEach(func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
			})

			Context("and all targets are zero", func() {
				BeforeEach(func() {
					targets = map[string]int{
						"variant-a": 0,
						"variant-b": 0,
					}
					variantAnalyses = []interfaces.VariantSaturationAnalysis{
						{VariantName: "variant-a", Cost: 2.0},
						{VariantName: "variant-b", Cost: 1.0}, // Cheaper
					}
				})

				It("should preserve minimum replica on the cheapest variant", func() {
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {
							EnableScaleToZero: boolPtr(false),
						},
					}

					result, applied := enforcer.EnforcePolicy(
						ctx,
						"test-model",
						"test-ns",
						targets,
						variantAnalyses,
						scaleToZeroConfig,
					)

					Expect(applied).To(BeTrue())
					Expect(result["variant-a"]).To(Equal(0))
					Expect(result["variant-b"]).To(Equal(1))
				})
			})

			Context("and some targets have replicas", func() {
				BeforeEach(func() {
					targets = map[string]int{
						"variant-a": 2,
						"variant-b": 0,
					}
					variantAnalyses = []interfaces.VariantSaturationAnalysis{
						{VariantName: "variant-a", Cost: 2.0},
						{VariantName: "variant-b", Cost: 1.0},
					}
				})

				It("should keep targets unchanged", func() {
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {
							EnableScaleToZero: boolPtr(false),
						},
					}

					result, applied := enforcer.EnforcePolicy(
						ctx,
						"test-model",
						"test-ns",
						targets,
						variantAnalyses,
						scaleToZeroConfig,
					)

					Expect(applied).To(BeFalse())
					Expect(result["variant-a"]).To(Equal(2))
					Expect(result["variant-b"]).To(Equal(0))
				})
			})
		})

		Context("when model is not in config", func() {
			BeforeEach(func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
				targets = map[string]int{
					"variant-a": 0,
					"variant-b": 0,
				}
				variantAnalyses = []interfaces.VariantSaturationAnalysis{
					{VariantName: "variant-a", Cost: 2.0},
					{VariantName: "variant-b", Cost: 1.0},
				}
			})

			It("should default to scale-to-zero disabled and preserve minimum", func() {
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"other-model": {
						EnableScaleToZero: boolPtr(true),
					},
				}

				result, applied := enforcer.EnforcePolicy(
					ctx,
					"test-model",
					"test-ns",
					targets,
					variantAnalyses,
					scaleToZeroConfig,
				)

				Expect(applied).To(BeTrue())
				Expect(result["variant-b"]).To(Equal(1))
			})
		})

		Context("when variants have equal cost", func() {
			BeforeEach(func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
				targets = map[string]int{
					"variant-z": 0,
					"variant-a": 0,
				}
				variantAnalyses = []interfaces.VariantSaturationAnalysis{
					{VariantName: "variant-z", Cost: 1.0},
					{VariantName: "variant-a", Cost: 1.0},
				}
			})

			It("should use alphabetical order as tiebreaker", func() {
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"test-model": {
						EnableScaleToZero: boolPtr(false),
					},
				}

				result, applied := enforcer.EnforcePolicy(
					ctx,
					"test-model",
					"test-ns",
					targets,
					variantAnalyses,
					scaleToZeroConfig,
				)

				Expect(applied).To(BeTrue())
				Expect(result["variant-a"]).To(Equal(1))
				Expect(result["variant-z"]).To(Equal(0))
			})
		})

		Context("when variant cost is missing from analysis", func() {
			BeforeEach(func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
				targets = map[string]int{
					"variant-a":       0,
					"variant-missing": 0,
				}
				variantAnalyses = []interfaces.VariantSaturationAnalysis{
					{VariantName: "variant-a", Cost: 100.0}, // Very expensive
					// variant-missing not in analysis - uses saturation.DefaultVariantCost (10.0)
				}
			})

			It("should use default cost for missing variants", func() {
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"test-model": {
						EnableScaleToZero: boolPtr(false),
					},
				}

				result, applied := enforcer.EnforcePolicy(
					ctx,
					"test-model",
					"test-ns",
					targets,
					variantAnalyses,
					scaleToZeroConfig,
				)

				Expect(applied).To(BeTrue())
				Expect(result["variant-a"]).To(Equal(0))
				Expect(result["variant-missing"]).To(Equal(1))
			})
		})
	})

	Describe("EnforcePolicyOnDecisions", func() {

		Context("when scale-to-zero is enabled", func() {

			Context("and there are no requests", func() {
				It("should set all matching decisions to zero", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, nil
					})
					decisions := []interfaces.VariantDecision{
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2, Action: interfaces.ActionNoChange},
						{VariantName: "variant-b", ModelID: "test-model", Namespace: "test-ns", Cost: 2.0, CurrentReplicas: 1, TargetReplicas: 3, Action: interfaces.ActionScaleUp},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
					}

					applied := enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, "cost-aware")

					Expect(applied).To(BeTrue())
					Expect(decisions[0].TargetReplicas).To(Equal(0))
					Expect(decisions[0].Action).To(Equal(interfaces.ActionScaleDown))
					Expect(decisions[1].TargetReplicas).To(Equal(0))
					Expect(decisions[1].Action).To(Equal(interfaces.ActionScaleDown))
				})
			})

			Context("and there are requests", func() {
				It("should keep decisions unchanged", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 10, nil
					})
					decisions := []interfaces.VariantDecision{
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 3, Action: interfaces.ActionScaleUp, Reason: "original"},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
					}

					applied := enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, "cost-aware")

					Expect(applied).To(BeFalse())
					Expect(decisions[0].TargetReplicas).To(Equal(3))
					Expect(decisions[0].Action).To(Equal(interfaces.ActionScaleUp))
					Expect(decisions[0].Reason).To(Equal("original"))
				})
			})

			Context("and request count query fails", func() {
				It("should keep decisions unchanged", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, errors.New("prometheus unavailable")
					})
					decisions := []interfaces.VariantDecision{
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
					}

					applied := enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, "cost-aware")

					Expect(applied).To(BeFalse())
					Expect(decisions[0].TargetReplicas).To(Equal(2))
				})
			})
		})

		Context("when scale-to-zero is disabled", func() {

			Context("and all targets are zero", func() {
				It("should preserve minimum replica on the cheapest variant", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, nil
					})
					decisions := []interfaces.VariantDecision{
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 2.0, CurrentReplicas: 0, TargetReplicas: 0},
						{VariantName: "variant-b", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 0, TargetReplicas: 0},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(false)},
					}

					enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, "cost-aware")

					Expect(decisions[0].TargetReplicas).To(Equal(0)) // expensive
					Expect(decisions[1].TargetReplicas).To(Equal(1)) // cheapest gets 1
					Expect(decisions[1].Action).To(Equal(interfaces.ActionScaleUp))
				})
			})

			Context("and some targets have replicas", func() {
				It("should keep decisions unchanged", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, nil
					})
					decisions := []interfaces.VariantDecision{
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 2.0, CurrentReplicas: 2, TargetReplicas: 2, Action: interfaces.ActionNoChange, Reason: "original"},
						{VariantName: "variant-b", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 0, TargetReplicas: 0},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(false)},
					}

					enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, "cost-aware")

					Expect(decisions[0].TargetReplicas).To(Equal(2))
					Expect(decisions[0].Reason).To(Equal("original"))
					Expect(decisions[1].TargetReplicas).To(Equal(0))
				})
			})

			Context("and variants have equal cost", func() {
				It("should use alphabetical order as tiebreaker", func() {
					enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
						return 0, nil
					})
					decisions := []interfaces.VariantDecision{
						{VariantName: "variant-z", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 0, TargetReplicas: 0},
						{VariantName: "variant-a", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 0, TargetReplicas: 0},
					}
					scaleToZeroConfig := config.ScaleToZeroConfigData{
						"test-model": {EnableScaleToZero: boolPtr(false)},
					}

					enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, "cost-aware")

					Expect(decisions[0].TargetReplicas).To(Equal(0)) // variant-z
					Expect(decisions[1].TargetReplicas).To(Equal(1)) // variant-a (alphabetically first)
				})
			})
		})

		Context("model filtering", func() {

			It("should only modify decisions matching modelID and namespace", func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
				decisions := []interfaces.VariantDecision{
					{VariantName: "v1", ModelID: "model-1", Namespace: "ns-1", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 3, Action: interfaces.ActionScaleUp, Reason: "original-1"},
					{VariantName: "v2", ModelID: "model-2", Namespace: "ns-1", Cost: 1.0, CurrentReplicas: 1, TargetReplicas: 2, Action: interfaces.ActionScaleUp, Reason: "original-2"},
					{VariantName: "v3", ModelID: "model-1", Namespace: "ns-2", Cost: 1.0, CurrentReplicas: 1, TargetReplicas: 1, Action: interfaces.ActionNoChange, Reason: "original-3"},
				}
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"model-1": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
				}

				applied := enforcer.EnforcePolicyOnDecisions(ctx, "model-1", "ns-1", decisions, scaleToZeroConfig, "cost-aware")

				Expect(applied).To(BeTrue())
				// model-1/ns-1 → scaled to zero
				Expect(decisions[0].TargetReplicas).To(Equal(0))
				Expect(decisions[0].Action).To(Equal(interfaces.ActionScaleDown))
				// model-2/ns-1 → untouched
				Expect(decisions[1].TargetReplicas).To(Equal(2))
				Expect(decisions[1].Action).To(Equal(interfaces.ActionScaleUp))
				Expect(decisions[1].Reason).To(Equal("original-2"))
				// model-1/ns-2 → untouched (different namespace)
				Expect(decisions[2].TargetReplicas).To(Equal(1))
				Expect(decisions[2].Action).To(Equal(interfaces.ActionNoChange))
				Expect(decisions[2].Reason).To(Equal("original-3"))
			})
		})

		Context("reason strings", func() {

			It("should include optimizer name in enforced reason", func() {
				enforcer = NewEnforcer(func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
					return 0, nil
				})
				decisions := []interfaces.VariantDecision{
					{VariantName: "v1", ModelID: "test-model", Namespace: "test-ns", Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2},
				}
				scaleToZeroConfig := config.ScaleToZeroConfigData{
					"test-model": {EnableScaleToZero: boolPtr(true), RetentionPeriod: "10m"},
				}

				enforcer.EnforcePolicyOnDecisions(ctx, "test-model", "test-ns", decisions, scaleToZeroConfig, "greedy-by-saturation")

				Expect(decisions[0].Reason).To(ContainSubstring("greedy-by-saturation"))
				Expect(decisions[0].Reason).To(ContainSubstring("enforced"))
			})
		})
	})
})
