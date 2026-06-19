package pipeline

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

var _ = Describe("GreedyByScoreOptimizer", func() {

	var (
		optimizer *GreedyByScoreOptimizer
		ctx       context.Context
	)

	BeforeEach(func() {
		optimizer = NewGreedyByScoreOptimizer()
		ctx = context.Background()
	})

	It("should return 'greedy-by-score' as name", func() {
		Expect(optimizer.Name()).To(Equal("greedy-by-score"))
	})

	Context("Single-Model Scale-Up", func() {

		It("should allocate replicas to cheapest variant within GPU budget", func() {
			r := &interfaces.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				AnalyzedAt:       time.Now(),
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "expensive", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "expensive", CurrentReplicas: 1, GPUsPerReplica: 4},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
					"H100": {Limit: 8},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// cheap is most cost-efficient (5/10000 vs 15/20000)
			// ceil(20000/10000) = 2 replicas, needs 4 A100 GPUs (2 per replica)
			Expect(dm["cheap"].TargetReplicas).To(Equal(3)) // 1 + 2
			Expect(dm["cheap"].Action).To(Equal(interfaces.ActionScaleUp))
			Expect(dm["expensive"].TargetReplicas).To(Equal(1)) // unchanged
		})

		It("should handle GPU exhaustion with partial allocation", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 50000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4}, // Only 2 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Only 4 GPUs / 2 per replica = 2 replicas max
			Expect(dm["v1"].TargetReplicas).To(Equal(3)) // 1 + 2
			Expect(dm["v1"].Action).To(Equal(interfaces.ActionScaleUp))
		})
	})

	Context("Multi-Model Fair-Share", func() {

		It("should give GPUs to most starved model first", func() {
			rA := &interfaces.AnalyzerResult{
				RequiredCapacity: 50000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			rB := &interfaces.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rA, ModelScalingRequest{
					ModelID:   "model-A",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rB, ModelScalingRequest{
					ModelID:   "model-B",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 8}, // 4 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// A got 3 replicas (1 original + 3 added), B got 2 (1 original + 1 added)
			Expect(dm["a-v1"].TargetReplicas).To(Equal(4)) // 1 + 3
			Expect(dm["b-v1"].TargetReplicas).To(Equal(2)) // 1 + 1
		})

		It("should verify 3-model walkthrough from design doc", func() {
			rA := &interfaces.AnalyzerResult{
				RequiredCapacity: 50000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			rB := &interfaces.AnalyzerResult{
				RequiredCapacity: 30000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			rC := &interfaces.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "c-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rA, ModelScalingRequest{
					ModelID:   "model-A",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rB, ModelScalingRequest{
					ModelID:   "model-B",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rC, ModelScalingRequest{
					ModelID:   "model-C",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "c-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 12}, // 6 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["a-v1"].TargetReplicas).To(Equal(4))
			Expect(dm["b-v1"].TargetReplicas).To(Equal(3))
			Expect(dm["c-v1"].TargetReplicas).To(Equal(2))
		})

		It("should distribute evenly with equal RequiredCapacity", func() {
			rX := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "x-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rY := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "y-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rX, ModelScalingRequest{
					ModelID:   "model-X",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "x-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rY, ModelScalingRequest{
					ModelID:   "model-Y",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "y-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 8},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["x-v1"].TargetReplicas).To(Equal(3))
			Expect(dm["y-v1"].TargetReplicas).To(Equal(3))
		})
	})

	Context("GPU Constraints", func() {

		It("should respect per-accelerator-type limits", func() {
			rH := &interfaces.AnalyzerResult{
				RequiredCapacity: 30000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "h100-v", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			rA := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "a100-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rH, ModelScalingRequest{
					ModelID:   "model-h100",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "h100-v", CurrentReplicas: 1, GPUsPerReplica: 4},
					},
				}),
				withSatEntry(rA, ModelScalingRequest{
					ModelID:   "model-a100",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a100-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"H100": {Limit: 4},
					"A100": {Limit: 6},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["h100-v"].TargetReplicas).To(Equal(2)) // 1 + 1
			Expect(dm["a100-v"].TargetReplicas).To(Equal(3)) // 1 + 2
		})

		It("should handle mixed accelerator types across variants", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 30000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "a100-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "h100-v", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-mixed",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a100-v", CurrentReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "h100-v", CurrentReplicas: 1, GPUsPerReplica: 4},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
					"H100": {Limit: 0},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["a100-v"].TargetReplicas).To(Equal(3)) // 1 + 2
			Expect(dm["h100-v"].TargetReplicas).To(Equal(1)) // unchanged
		})

		It("should not allocate when zero GPU budget", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 0},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["v1"].TargetReplicas).To(Equal(1))
		})

		It("should not allocate when nil constraints", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["v1"].TargetReplicas).To(Equal(1))
		})
	})

	Context("Scale-Down", func() {

		It("should apply role-iterated scale-down for scale-down models", func() {
			r := &interfaces.AnalyzerResult{
				SpareCapacity: 15000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "cheap", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
					{VariantName: "expensive", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 3},
						{VariantName: "expensive", CurrentReplicas: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["expensive"].TargetReplicas).To(Equal(2))
			Expect(dm["cheap"].TargetReplicas).To(Equal(2))
		})

		It("should handle mixed scale-up and scale-down models", func() {
			rUp := &interfaces.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "up-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rDown := &interfaces.AnalyzerResult{
				SpareCapacity: 10000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "down-v1", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rUp, ModelScalingRequest{
					ModelID:   "model-up",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "up-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rDown, ModelScalingRequest{
					ModelID:   "model-down",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "down-v1", CurrentReplicas: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["up-v1"].TargetReplicas).To(Equal(2))
			Expect(dm["up-v1"].Action).To(Equal(interfaces.ActionScaleUp))

			Expect(dm["down-v1"].TargetReplicas).To(Equal(1))
			Expect(dm["down-v1"].Action).To(Equal(interfaces.ActionScaleDown))
		})
	})

	Context("Pending Replicas", func() {

		It("should allocate to most cost-efficient variant regardless of pending replicas", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "cheap-pending", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
					{VariantName: "expensive-ready", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap-pending", CurrentReplicas: 2, PendingReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "expensive-ready", CurrentReplicas: 1, PendingReplicas: 0, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["cheap-pending"].TargetReplicas).To(Equal(3))   // +1
			Expect(dm["expensive-ready"].TargetReplicas).To(Equal(1)) // unchanged
		})
	})

	Context("Edge Cases", func() {

		It("should skip requests with nil result", func() {
			requests := []ModelScalingRequest{
				withSatEntry(nil, ModelScalingRequest{ModelID: "model-1", Namespace: "default"}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			Expect(decisions).To(BeEmpty())
		})

		It("should skip variants with zero capacity", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "zero-cap", AcceleratorName: "A100", Cost: 1.0, ReplicaCount: 0, PerReplicaCapacity: 0},
					{VariantName: "normal", AcceleratorName: "A100", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "zero-cap", CurrentReplicas: 0, GPUsPerReplica: 2},
						{VariantName: "normal", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["zero-cap"].TargetReplicas).To(Equal(0))
			Expect(dm["normal"].TargetReplicas).To(Equal(2))
		})

		It("should handle steady state (no scaling needed)", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 0,
				SpareCapacity:    0,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "v1", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].Action).To(Equal(interfaces.ActionNoChange))
			Expect(decisions[0].TargetReplicas).To(Equal(2))
		})

		It("should handle empty requests", func() {
			decisions := optimizer.Optimize(ctx, nil, nil)
			Expect(decisions).To(BeEmpty())
		})

		It("should default GPUsPerReplica to 1 when not specified", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 0},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 2},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["v1"].TargetReplicas).To(Equal(2)) // 1 + 1
		})
	})

	Context("Decision Metadata", func() {

		It("should set correct model ID, namespace, and cost on decisions", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 5000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "ns-1",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].ModelID).To(Equal("model-1"))
			Expect(decisions[0].Namespace).To(Equal("ns-1"))
			Expect(decisions[0].AcceleratorName).To(Equal("A100"))
			Expect(decisions[0].Cost).To(Equal(5.0))
		})

		It("should contain greedy-by-score in reason strings", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 5000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].Reason()).To(ContainSubstring("greedy-by-score"))
		})
	})

	Context("Score-Based Priority", func() {

		It("should give GPUs to higher-score model first", func() {
			rLow := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "low-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rHigh := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "high-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rLow, ModelScalingRequest{
					ModelID:   "low-priority",
					Namespace: "default",
					Priority:  1.0,
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "low-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rHigh, ModelScalingRequest{
					ModelID:   "high-priority",
					Namespace: "default",
					Priority:  5.0,
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "high-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4}, // Only 2 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// High-score model (100000) should get GPU preference over low-score (20000)
			Expect(dm["high-v"].TargetReplicas).To(BeNumerically(">=", 2))
		})

		// T1.3: multi-model fair-share priority integration test.
		// Verifies that fairShareValue = priority × Σ(Remaining × Score) correctly
		// orders models. This test explicitly sets Score on AnalyzerResults, mirroring
		// what the engine populates from config.Analyzers[].Score after the B1 fix.
		// Without B1 (Score=0), fairShareValue falls back to max_i(Remaining) = RC,
		// making both models equal — this test would then produce non-deterministic
		// results and the strict equality assertions would fail intermittently.
		It("T1.3: priority × Score weighting drives fair-share ordering", func() {
			// Model A: RC=20000, Score=1.0, Priority=1.0 → fsv=20000
			// Model B: RC=20000, Score=1.0, Priority=5.0 → fsv=100000
			// With 4 A100 GPUs (2 replicas each, 2 GPUs/replica):
			// B (fsv=100000) should always get served first.
			// Strict assertions require Score to be populated; Score=0 fallback
			// produces equal fsv and non-deterministic ordering.
			rA := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rB := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-A",
					Namespace: "default",
					Priority:  1.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      interfaces.SaturationAnalyzerName,
						Result:    rA,
						Score:     1.0, // explicit: mirrors engine-populated value
						Remaining: rA.RequiredCapacity,
						Spare:     rA.SpareCapacity,
					}},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "model-B",
					Namespace: "default",
					Priority:  5.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      interfaces.SaturationAnalyzerName,
						Result:    rB,
						Score:     1.0, // explicit: mirrors engine-populated value
						Remaining: rB.RequiredCapacity,
						Spare:     rB.SpareCapacity,
					}},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4}, // 2 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// fsv(A) = 1.0 * 20000 * 1.0 = 20000; fsv(B) = 5.0 * 20000 * 1.0 = 100000
			// B is most starved (highest fsv), gets both available replicas.
			// A gets nothing (GPU budget exhausted by B).
			Expect(dm["b-v1"].TargetReplicas).To(Equal(3)) // 1 + 2 (all GPUs)
			Expect(dm["a-v1"].TargetReplicas).To(Equal(1)) // unchanged
		})

		It("T1.4: non-uniform Score across two analyzers drives fair-share ordering", func() {
			// Model A has two AnalyzerResults:
			//   saturation: Score=1.0, RC=20000
			//   throughput: Score=2.0, RC=20000
			//   fsv(A) = 1.0 × (20000×1.0 + 20000×2.0) = 60000
			// Model B has one AnalyzerResult:
			//   saturation: Score=1.0, RC=20000
			//   fsv(B) = 1.0 × (20000×1.0) = 20000
			// With 4 A100 GPUs (2 GPUs/replica): A (higher fsv) gets both
			// available replicas; B gets none.
			rA := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rB := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-A",
					Namespace: "default",
					Priority:  1.0,
					AnalyzerResults: []NamedAnalyzerResult{
						{
							Name:      "saturation",
							Result:    rA,
							Score:     1.0,
							Remaining: rA.RequiredCapacity,
						},
						{
							Name:  "throughput",
							Score: 2.0,
							// throughput shares rA's variant capacity for simplicity;
							// its RC signal adds to the fair-share weight.
							Result:    &interfaces.AnalyzerResult{RequiredCapacity: 20000},
							Remaining: 20000,
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "model-B",
					Namespace: "default",
					Priority:  1.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      "saturation",
						Result:    rB,
						Score:     1.0,
						Remaining: rB.RequiredCapacity,
					}},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4}, // 2 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// fsv(A)=60000 > fsv(B)=20000: A wins the GPU budget.
			Expect(dm["a-v1"].TargetReplicas).To(Equal(3)) // 1 + 2 (all 4 GPUs)
			Expect(dm["b-v1"].TargetReplicas).To(Equal(1)) // unchanged
		})
	})

	Context("Demand-Proportional P/D Distribution", func() {

		It("should distribute replicas proportional to per-role demand", func() {
			// Prefill RequiredCapacity=15000 (75%), Decode RequiredCapacity=5000 (25%)
			// Total model RequiredCapacity=20000, Score=20000
			// With 10 A100 GPUs available, each variant uses 2 GPUs/replica
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				RoleCapacities: map[string]interfaces.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 15000, TotalDemand: 15000},
					"decode":  {Role: "decode", RequiredCapacity: 5000, TotalDemand: 5000},
				},
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:       "model-pd",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 3, GPUsPerReplica: 2, Role: "decode"},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// target = 20000 (single model, allocationMean=0)
			// prefill fraction=0.75: roleTarget=15000 → ceil(15000/10000)=2 replicas
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(3)) // 1 + 2
			// decode fraction=0.25: roleTarget=5000 → ceil(5000/10000)=1 replica
			Expect(dm["decode-v"].TargetReplicas).To(Equal(4)) // 3 + 1
		})

		It("should distribute equally when roles have equal demand", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				RoleCapacities: map[string]interfaces.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {Role: "decode", RequiredCapacity: 10000, TotalDemand: 10000},
				},
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:       "model-equal",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "decode"},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 8},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Each role gets 50%: roleTarget=10000 → ceil(10000/10000)=1 replica each
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(2)) // 1 + 1
			Expect(dm["decode-v"].TargetReplicas).To(Equal(2))  // 1 + 1
		})

		It("should only allocate to the role that needs scale-up", func() {
			// Only prefill needs scale-up; decode has 0 RequiredCapacity
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 10000,
				RoleCapacities: map[string]interfaces.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {Role: "decode", RequiredCapacity: 0, TotalDemand: 0},
				},
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:       "model-prefill-only",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 3, GPUsPerReplica: 2, Role: "decode"},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Only prefill fraction=1.0: roleTarget=10000 → 1 replica
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(2)) // 1 + 1
			// Decode unchanged (0 RequiredCapacity → not in roleDemands)
			Expect(dm["decode-v"].TargetReplicas).To(Equal(3))
		})

		It("should handle GPU exhaustion for one role without affecting the other", func() {
			// Prefill uses H100s (exhausted), decode uses A100s (available)
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 20000,
				RoleCapacities: map[string]interfaces.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {Role: "decode", RequiredCapacity: 10000, TotalDemand: 10000},
				},
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "H100", Cost: 15.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 20000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:       "model-mixed-gpu",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 4, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "decode"},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"H100": {Limit: 0}, // No H100s available
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Paired allocation: if P-side (H100) is exhausted, the pair cannot commit.
			// Both prefill and decode stay at their current replicas.
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(1))
			Expect(dm["decode-v"].TargetReplicas).To(Equal(1))
		})

		It("should handle non-disaggregated model with Score", func() {
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					Priority:  2.0,
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Score inflates the fair-share ordering priority but not the allocation size.
			// Allocation is demand-driven: RC=10000, PRC=10000 → 1 replica added.
			Expect(dm["v1"].TargetReplicas).To(Equal(2)) // 1 + 1
		})
	})

	Context("Helper Functions", func() {

		It("filterActive should return only models with remaining > 0", func() {
			work := []*modelWork{
				{remaining: 100},
				{remaining: -1},
				{remaining: 50},
				{remaining: 0},
			}

			active := filterActive(work)
			Expect(active).To(HaveLen(2))
			Expect(active[0].remaining).To(Equal(100.0))
			Expect(active[1].remaining).To(Equal(50.0))
		})

		It("computeMean should return average of remaining", func() {
			active := []*modelWork{
				{remaining: 100},
				{remaining: 200},
				{remaining: 300},
			}

			mean := computeMean(active)
			Expect(mean).To(Equal(200.0))
		})

		It("computeMean should return 0 for empty slice", func() {
			mean := computeMean(nil)
			Expect(mean).To(Equal(0.0))
		})

		It("allocateForModel should respect maxReplicas", func() {
			intPtr := func(n int) *int { return &n }
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 20}}},
			}

			r := &interfaces.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				AnalyzedAt:       time.Now(),
				RequiredCapacity: 50000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "expensive", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 1, GPUsPerReplica: 1, MaxReplicas: intPtr(3)},
						{VariantName: "expensive", CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// cheap: capped at max=3 (starts at 1, can add 2)
			// expensive: gets remaining capacity
			Expect(dm["cheap"].TargetReplicas).To(BeNumerically("<=", 3))
		})

		It("scale-down should respect minReplicas via scaleDownRoleIterated", func() {
			intPtr := func(n int) *int { return &n }

			r := &interfaces.AnalyzerResult{
				ModelID:       "model-1",
				Namespace:     "default",
				AnalyzedAt:    time.Now(),
				SpareCapacity: 50000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "expensive", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 3, PerReplicaCapacity: 20000},
					{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 3, GPUsPerReplica: 1, MinReplicas: intPtr(2)},
						{VariantName: "cheap", CurrentReplicas: 3, GPUsPerReplica: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive: minReplicas=2, so can only remove 1
			Expect(dm["expensive"].TargetReplicas).To(BeNumerically(">=", 2))
		})

		It("scale-down should zero minReplicas=0 variant while keeping minReplicas>0 sibling", func() {
			intPtr := func(n int) *int { return &n }

			r := &interfaces.AnalyzerResult{
				ModelID:       "model-1",
				Namespace:     "default",
				AnalyzedAt:    time.Now(),
				SpareCapacity: 80000, // enough to remove all
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "keep-alive", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
					{VariantName: "expendable", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "keep-alive", CurrentReplicas: 2, GPUsPerReplica: 1, MinReplicas: intPtr(1)},
						{VariantName: "expendable", CurrentReplicas: 3, GPUsPerReplica: 1, MinReplicas: intPtr(0)},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["keep-alive"].TargetReplicas).To(Equal(1))
			Expect(dm["expendable"].TargetReplicas).To(Equal(0))
		})

		It("sortByRemainingDesc should sort descending", func() {
			active := []*modelWork{
				{remaining: 100, req: ModelScalingRequest{ModelID: "low"}},
				{remaining: 300, req: ModelScalingRequest{ModelID: "high"}},
				{remaining: 200, req: ModelScalingRequest{ModelID: "mid"}},
			}

			sortByRemainingDesc(active)

			Expect(active[0].req.ModelID).To(Equal("high"))
			Expect(active[1].req.ModelID).To(Equal("mid"))
			Expect(active[2].req.ModelID).To(Equal("low"))
		})

		// filterVariantCapacitiesByRole removed (duplicate of variantsForRole in analyzer_helpers.go; N2 cleanup)
	})

	// Phase 3 test: D-only scale-up via the per-role gate.
	Context("Disaggregated D-only scale-up (Phase 3)", func() {
		It("should scale up only decode when RC_P=0 and RC_D>0", func() {
			// Pre-Phase-3 the model-level gate (Remaining=0 from P-anchor) would
			// route the model to scale-down. anyRoleNeedsScaleUp fires on D demand.
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 0,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "pf", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", PerReplicaCapacity: 10000},
					{VariantName: "dc", AcceleratorName: "A100", Cost: 5.0, Role: "decode", PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]interfaces.RoleCapacity{
					"prefill": {RequiredCapacity: 0, TotalDemand: 0},
					"decode":  {RequiredCapacity: 10000, TotalDemand: 10000},
				},
			}
			requests := []ModelScalingRequest{
				{
					ModelID:       "d-only",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      interfaces.SaturationAnalyzerName,
						Result:    r,
						Score:     1.0,
						Remaining: r.RequiredCapacity,
						Spare:     r.SpareCapacity,
					}},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "pf", CurrentReplicas: 2, GPUsPerReplica: 2},
						{VariantName: "dc", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 4}}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["pf"].TargetReplicas).To(Equal(2)) // no P demand
			Expect(dm["dc"].TargetReplicas).To(Equal(2)) // +1 decode
		})
	})

	// Phase 3 test: min-util coupling without α.
	Context("Disaggregated min-util coupling (Phase 3)", func() {
		It("should advance P and D by matched util (not fixed α ratio)", func() {
			// P-demand=10000, D-demand=30000, PRC=10000 each.
			// Without α: P and D are sized independently and joint-committed by Δ_util.
			// n_P=1 (ceil(10000/10000)), n_D=3 (ceil(30000/10000)).
			// util_P=1.0, util_D=3.0 → Δ_util=1.0 → k_P=1, k_D=3.
			// Result: prefill+1, decode+3 — same Δ_util=1.0 for both.
			r := &interfaces.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []interfaces.VariantCapacity{
					{VariantName: "pf", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", PerReplicaCapacity: 10000},
					{VariantName: "dc", AcceleratorName: "A100", Cost: 5.0, Role: "decode", PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]interfaces.RoleCapacity{
					"prefill": {RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {RequiredCapacity: 30000, TotalDemand: 30000},
				},
			}
			requests := []ModelScalingRequest{
				{
					ModelID:       "pd-min-util",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      interfaces.SaturationAnalyzerName,
						Result:    r,
						Score:     1.0,
						Remaining: r.RequiredCapacity,
						Spare:     r.SpareCapacity,
					}},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "pf", CurrentReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "dc", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 12}}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Both roles committed by the same Δ_util=1.0.
			Expect(dm["pf"].TargetReplicas).To(Equal(2)) // 1+1
			Expect(dm["dc"].TargetReplicas).To(Equal(4)) // 1+3
		})
	})
})
