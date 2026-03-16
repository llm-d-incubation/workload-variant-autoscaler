package saturation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

var _ = Describe("V2 Engine Integration", func() {

	Context("CostAwareOptimizer via engine path", func() {

		It("should scale up cheapest variant by cost-efficiency", func() {
			optimizer := pipeline.NewCostAwareOptimizer()
			requests := []pipeline.ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						ModelID:          "model-1",
						Namespace:        "default",
						RequiredCapacity: 5000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "variant-cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
							{VariantName: "variant-expensive", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "variant-cheap", CurrentReplicas: 2},
						{VariantName: "variant-expensive", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			// cost-efficiency: cheap=5/10000=0.0005, expensive=15/20000=0.00075
			// cheap is more cost-efficient, ceil(5000/10000)=1
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["variant-expensive"].TargetReplicas).To(Equal(1))
		})

		It("should scale down most expensive variant", func() {
			optimizer := pipeline.NewCostAwareOptimizer()
			requests := []pipeline.ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						ModelID:       "model-1",
						Namespace:     "default",
						SpareCapacity: 25000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "variant-cheap", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
							{VariantName: "variant-expensive", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "variant-cheap", CurrentReplicas: 3},
						{VariantName: "variant-expensive", CurrentReplicas: 2},
					},
				},
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			Expect(dm["variant-expensive"].TargetReplicas).To(Equal(1))
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(3))
		})

		It("should protect cheapest variant at 1 during scale-down", func() {
			optimizer := pipeline.NewCostAwareOptimizer()
			requests := []pipeline.ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						ModelID:       "model-1",
						Namespace:     "default",
						SpareCapacity: 30000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "variant-expensive", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
							{VariantName: "variant-cheap", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "variant-expensive", CurrentReplicas: 1},
						{VariantName: "variant-cheap", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			Expect(dm["variant-expensive"].TargetReplicas).To(Equal(0))
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(1))
		})

		It("should not skip variants with pending replicas", func() {
			optimizer := pipeline.NewCostAwareOptimizer()
			requests := []pipeline.ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						ModelID:          "model-1",
						Namespace:        "default",
						RequiredCapacity: 5000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "variant-cheap", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
							{VariantName: "variant-mid", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "variant-cheap", CurrentReplicas: 2, PendingReplicas: 1},
						{VariantName: "variant-mid", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			// cheap has pending but is more cost-efficient → still gets allocation
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["variant-mid"].TargetReplicas).To(Equal(1))
		})
	})
})

var _ = Describe("getRoleFromDeployment", func() {

	It("should return 'both' for nil deployment", func() {
		Expect(getRoleFromDeployment(nil)).To(Equal("both"))
	})

	It("should return 'both' for deployment without labels", func() {
		deploy := &appsv1.Deployment{}
		Expect(getRoleFromDeployment(deploy)).To(Equal("both"))
	})

	It("should return 'prefill' for prefill label", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"llm-d.ai/role": "prefill",
		}
		Expect(getRoleFromDeployment(deploy)).To(Equal("prefill"))
	})

	It("should return 'decode' for decode label", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"llm-d.ai/role": "decode",
		}
		Expect(getRoleFromDeployment(deploy)).To(Equal("decode"))
	})

	It("should return 'both' for unknown role value", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"llm-d.ai/role": "unknown",
		}
		Expect(getRoleFromDeployment(deploy)).To(Equal("both"))
	})

	It("should return 'both' when no role label present", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"app": "vllm",
		}
		Expect(getRoleFromDeployment(deploy)).To(Equal("both"))
	})
})

var _ = Describe("resolveSaturationConfig", func() {

	It("should return model-specific config when present", func() {
		configMap := map[string]config.SaturationScalingConfig{
			"default": {
				KvCacheThreshold: 0.80,
				AnalyzerName:     "saturation",
			},
			"llama-70b#production": {
				KvCacheThreshold: 0.85,
				Priority:         5.0,
				AnalyzerName:     "saturation",
			},
		}
		cfg := resolveSaturationConfig(configMap, "llama-70b", "production")
		Expect(cfg.KvCacheThreshold).To(Equal(0.85))
		Expect(cfg.Priority).To(Equal(5.0))
	})

	It("should fall back to default config when model-specific not found", func() {
		configMap := map[string]config.SaturationScalingConfig{
			"default": {
				KvCacheThreshold: 0.80,
				AnalyzerName:     "saturation",
			},
		}
		cfg := resolveSaturationConfig(configMap, "unknown-model", "default")
		Expect(cfg.KvCacheThreshold).To(Equal(0.80))
		Expect(cfg.Priority).To(Equal(config.DefaultPriority))
	})

	It("should return zero-value with defaults when map is empty", func() {
		configMap := map[string]config.SaturationScalingConfig{}
		cfg := resolveSaturationConfig(configMap, "model-1", "ns-1")
		Expect(cfg.Priority).To(Equal(config.DefaultPriority))
		Expect(cfg.KvCacheThreshold).To(Equal(0.0))
	})

	It("should apply defaults on model-specific config", func() {
		configMap := map[string]config.SaturationScalingConfig{
			"model-1#ns-1": {
				AnalyzerName: "saturation",
			},
		}
		cfg := resolveSaturationConfig(configMap, "model-1", "ns-1")
		Expect(cfg.ScaleUpThreshold).To(Equal(config.DefaultScaleUpThreshold))
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary))
		Expect(cfg.Priority).To(Equal(config.DefaultPriority))
	})
})

func decisionsByVariant(decisions []interfaces.VariantDecision) map[string]interfaces.VariantDecision {
	m := make(map[string]interfaces.VariantDecision, len(decisions))
	for _, d := range decisions {
		m[d.VariantName] = d
	}
	return m
}
