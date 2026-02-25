package saturation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

func decisionsByVariant(decisions []interfaces.VariantDecision) map[string]interfaces.VariantDecision {
	m := make(map[string]interfaces.VariantDecision, len(decisions))
	for _, d := range decisions {
		m[d.VariantName] = d
	}
	return m
}
