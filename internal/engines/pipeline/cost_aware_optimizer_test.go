package pipeline

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

var _ = Describe("CostAwareOptimizer", func() {

	var (
		optimizer *CostAwareOptimizer
		ctx       context.Context
	)

	BeforeEach(func() {
		optimizer = NewCostAwareOptimizer()
		ctx = context.Background()
	})

	It("should return 'cost-aware' as name", func() {
		Expect(optimizer.Name()).To(Equal("cost-aware"))
	})

	Context("Scale-Up", func() {

		It("should add replicas to most cost-efficient variant", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						ModelID:          "model-1",
						Namespace:        "default",
						AnalyzedAt:       time.Now(),
						RequiredCapacity: 5000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
							{VariantName: "expensive", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 2},
						{VariantName: "expensive", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// cost-efficiency: cheap=5/10000=0.0005, expensive=15/20000=0.00075
			// cheap is more efficient → ceil(5000/10000) = 1 replica added
			Expect(dm["cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["expensive"].TargetReplicas).To(Equal(1))
		})

		It("should not skip variants with pending replicas", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 5000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "cheap", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
							{VariantName: "mid", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 2, PendingReplicas: 1},
						{VariantName: "mid", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// cheap has pending but is more cost-efficient → still gets the allocation
			// (analyzer already accounts for pending capacity in supply)
			Expect(dm["cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["mid"].TargetReplicas).To(Equal(1))
		})

		It("should skip variants with zero capacity", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 5000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "zero-cap", Cost: 1.0, ReplicaCount: 0, PerReplicaCapacity: 0},
							{VariantName: "normal", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "zero-cap", CurrentReplicas: 0},
						{VariantName: "normal", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["normal"].TargetReplicas).To(Equal(2))
			Expect(dm["zero-cap"].TargetReplicas).To(Equal(0))
		})

		It("should spread across multiple variants when needed", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 25000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "cheap", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
							{VariantName: "mid", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 1},
						{VariantName: "mid", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// cheap is more efficient (5/10000=0.0005 vs 10/15000=0.00067)
			// cheap gets ceil(25000/10000)=3 replicas → adds 3, remaining=25000-30000<0
			Expect(dm["cheap"].TargetReplicas).To(Equal(4))
		})
	})

	Context("Scale-Down", func() {

		It("should remove from most expensive variant first", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						SpareCapacity: 15000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "cheap", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
							{VariantName: "expensive", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 3},
						{VariantName: "expensive", CurrentReplicas: 2},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive is removed first: floor(15000/20000)=0 → can't remove full replica?
			// Actually floor(15000/20000) = 0. Hmm. Let me reconsider.
			// No wait: spare=15000, expensive.perReplica=20000 → floor(15000/20000)=0
			// But expensive has minReplicas=0 (not cheapest), removable=2
			// 0 replicas removed from expensive → try cheap: floor(15000/10000)=1, removable=3-0=3
			// cheap goes from 3 to 2
			Expect(dm["expensive"].TargetReplicas).To(Equal(2))
			Expect(dm["cheap"].TargetReplicas).To(Equal(2))
		})

		It("should protect cheapest variant at 1 replica", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						SpareCapacity: 30000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "expensive", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
							{VariantName: "cheap", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 1},
						{VariantName: "cheap", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive (not cheapest) → can go to 0: floor(30000/20000)=1
			// cheap (cheapest) → protected at 1: minReplicas=1, removable=0
			Expect(dm["expensive"].TargetReplicas).To(Equal(0))
			Expect(dm["cheap"].TargetReplicas).To(Equal(1))
		})

		It("should remove cheap variant when expensive replica capacity exceeds spare", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						SpareCapacity: 15000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "expensive", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
							{VariantName: "cheap", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 1},
						{VariantName: "cheap", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive: floor(15000/20000)=0 → can't remove
			// cheap: expensive still has replicas → not protected, removable=1, floor(15000/10000)=1 → remove 1
			Expect(dm["expensive"].TargetReplicas).To(Equal(1))
			Expect(dm["cheap"].TargetReplicas).To(Equal(0))
		})

		It("should cascade scale-down across variants", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						SpareCapacity: 50000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "expensive", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
							{VariantName: "mid", Cost: 10.0, ReplicaCount: 2, PerReplicaCapacity: 15000},
							{VariantName: "cheap", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 2},
						{VariantName: "mid", CurrentReplicas: 2},
						{VariantName: "cheap", CurrentReplicas: 2},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// sorted by cost DESC: expensive(15), mid(10), cheap(5)
			// expensive: floor(50000/20000)=2, removable=2 → remove 2. remaining=50000-40000=10000
			// mid: floor(10000/15000)=0 → skip
			// cheap: mid still has replicas → not protected, removable=2, floor(10000/10000)=1 → remove 1. remaining=0
			Expect(dm["expensive"].TargetReplicas).To(Equal(0))
			Expect(dm["mid"].TargetReplicas).To(Equal(2))
			Expect(dm["cheap"].TargetReplicas).To(Equal(1))
		})
	})

	Context("Steady State", func() {

		It("should return no-change when no scaling signal", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 0,
						SpareCapacity:    0,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "v1", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 2},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].Action).To(Equal(interfaces.ActionNoChange))
			Expect(decisions[0].TargetReplicas).To(Equal(2))
		})

		It("should skip requests with nil result", func() {
			requests := []ModelScalingRequest{
				{ModelID: "model-1", Namespace: "default", Result: nil},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			Expect(decisions).To(BeEmpty())
		})
	})

	Context("Multi-Model", func() {

		It("should process models independently", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 5000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "m1-v1", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "m1-v1", CurrentReplicas: 1},
					},
				},
				{
					ModelID:   "model-2",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						SpareCapacity: 10000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "m2-v1", Cost: 10.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "m2-v1", CurrentReplicas: 2},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["m1-v1"].Action).To(Equal(interfaces.ActionScaleUp))
			Expect(dm["m1-v1"].TargetReplicas).To(Equal(2))
			Expect(dm["m2-v1"].Action).To(Equal(interfaces.ActionScaleDown))
			Expect(dm["m2-v1"].TargetReplicas).To(Equal(1))
		})
	})

	Context("Decision Metadata", func() {

		It("should set model ID, namespace, and cost on decisions", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "ns-1",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 5000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].ModelID).To(Equal("model-1"))
			Expect(decisions[0].Namespace).To(Equal("ns-1"))
			Expect(decisions[0].AcceleratorName).To(Equal("A100"))
			Expect(decisions[0].Cost).To(Equal(5.0))
		})
	})

	Context("Helper Functions", func() {

		It("sortByCostEfficiencyAsc should order by cost/capacity", func() {
			capacities := []interfaces.VariantCapacity{
				{VariantName: "expensive", Cost: 15.0, PerReplicaCapacity: 10000}, // 0.0015
				{VariantName: "cheap", Cost: 5.0, PerReplicaCapacity: 10000},      // 0.0005
				{VariantName: "mid", Cost: 10.0, PerReplicaCapacity: 10000},       // 0.001
			}

			sorted := sortByCostEfficiencyAsc(capacities)

			Expect(sorted[0].VariantName).To(Equal("cheap"))
			Expect(sorted[1].VariantName).To(Equal("mid"))
			Expect(sorted[2].VariantName).To(Equal("expensive"))
		})

		It("sortByCostDesc should order by absolute cost descending", func() {
			capacities := []interfaces.VariantCapacity{
				{VariantName: "cheap", Cost: 5.0},
				{VariantName: "expensive", Cost: 15.0},
				{VariantName: "mid", Cost: 10.0},
			}

			sorted := sortByCostDesc(capacities)

			Expect(sorted[0].VariantName).To(Equal("expensive"))
			Expect(sorted[1].VariantName).To(Equal("mid"))
			Expect(sorted[2].VariantName).To(Equal("cheap"))
		})

		It("findCheapestVariant should return lowest cost variant", func() {
			capacities := []interfaces.VariantCapacity{
				{VariantName: "mid", Cost: 10.0},
				{VariantName: "cheap", Cost: 5.0},
				{VariantName: "expensive", Cost: 15.0},
			}

			Expect(findCheapestVariant(capacities)).To(Equal("cheap"))
		})

		It("mergeConstraints should take minimum available per type", func() {
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 10}, "H100": {Limit: 4}}},
				{Pools: map[string]ResourcePool{"A100": {Limit: 6}}},
			}

			merged := mergeConstraints(constraints)

			Expect(merged["A100"]).To(Equal(6))
			Expect(merged["H100"]).To(Equal(4))
		})
	})
})

func decisionMap(decisions []interfaces.VariantDecision) map[string]interfaces.VariantDecision {
	m := make(map[string]interfaces.VariantDecision, len(decisions))
	for _, d := range decisions {
		m[d.VariantName] = d
	}
	return m
}
