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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						ModelID:          "model-1",
						Namespace:        "default",
						AnalyzedAt:       time.Now(),
						RequiredCapacity: 20000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
							{VariantName: "expensive", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "expensive", CurrentReplicas: 1, GPUsPerReplica: 4},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 50000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-A",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 50000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "model-B",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 10000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-A",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 50000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "model-B",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 30000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "model-C",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 10000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "c-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "c-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-X",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "x-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "x-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "model-Y",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "y-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "y-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-h100",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 30000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "h100-v", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "h100-v", CurrentReplicas: 1, GPUsPerReplica: 4},
					},
				},
				{
					ModelID:   "model-a100",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "a100-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a100-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-mixed",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 30000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "a100-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
							{VariantName: "h100-v", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "a100-v", CurrentReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "h100-v", CurrentReplicas: 1, GPUsPerReplica: 4},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["v1"].TargetReplicas).To(Equal(1))
		})
	})

	Context("Scale-Down", func() {

		It("should reuse costAwareScaleDown for scale-down models", func() {
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

			Expect(dm["expensive"].TargetReplicas).To(Equal(2))
			Expect(dm["cheap"].TargetReplicas).To(Equal(2))
		})

		It("should handle mixed scale-up and scale-down models", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-up",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 10000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "up-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "up-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "model-down",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						SpareCapacity: 10000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "down-v1", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "down-v1", CurrentReplicas: 2},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 10000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "cheap-pending", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
							{VariantName: "expensive-ready", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap-pending", CurrentReplicas: 2, PendingReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "expensive-ready", CurrentReplicas: 1, PendingReplicas: 0, GPUsPerReplica: 2},
					},
				},
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
				{ModelID: "model-1", Namespace: "default", Result: nil},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			Expect(decisions).To(BeEmpty())
		})

		It("should skip variants with zero capacity", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 10000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "zero-cap", AcceleratorName: "A100", Cost: 1.0, ReplicaCount: 0, PerReplicaCapacity: 0},
							{VariantName: "normal", AcceleratorName: "A100", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "zero-cap", CurrentReplicas: 0, GPUsPerReplica: 2},
						{VariantName: "normal", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
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

		It("should handle empty requests", func() {
			decisions := optimizer.Optimize(ctx, nil, nil)
			Expect(decisions).To(BeEmpty())
		})

		It("should default GPUsPerReplica to 1 when not specified", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 10000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 0},
					},
				},
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
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 5000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].Reason).To(ContainSubstring("greedy-by-score"))
		})
	})

	Context("Score-Based Priority", func() {

		It("should give GPUs to higher-score model first", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "low-priority",
					Namespace: "default",
					Priority:  1.0,
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						Score:            20000, // 1.0 * 20000
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "low-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "low-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "high-priority",
					Namespace: "default",
					Priority:  5.0,
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						Score:            100000, // 5.0 * 20000
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "high-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "high-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
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
	})

	Context("Demand-Proportional P/D Distribution", func() {

		It("should distribute replicas proportional to per-role demand", func() {
			// Prefill RequiredCapacity=15000 (75%), Decode RequiredCapacity=5000 (25%)
			// Total model RequiredCapacity=20000, Score=20000
			// With 10 A100 GPUs available, each variant uses 2 GPUs/replica
			requests := []ModelScalingRequest{
				{
					ModelID:       "model-pd",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						Score:            20000,
						RoleCapacities: map[string]interfaces.RoleCapacity{
							"prefill": {Role: "prefill", RequiredCapacity: 15000},
							"decode":  {Role: "decode", RequiredCapacity: 5000},
						},
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
							{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 3, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 3, GPUsPerReplica: 2, Role: "decode"},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:       "model-equal",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						Score:            20000,
						RoleCapacities: map[string]interfaces.RoleCapacity{
							"prefill": {Role: "prefill", RequiredCapacity: 10000},
							"decode":  {Role: "decode", RequiredCapacity: 10000},
						},
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
							{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "decode"},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:       "model-prefill-only",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 10000,
						Score:            10000,
						RoleCapacities: map[string]interfaces.RoleCapacity{
							"prefill": {Role: "prefill", RequiredCapacity: 10000},
							"decode":  {Role: "decode", RequiredCapacity: 0},
						},
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
							{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 3, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 3, GPUsPerReplica: 2, Role: "decode"},
					},
				},
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
			requests := []ModelScalingRequest{
				{
					ModelID:       "model-mixed-gpu",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 20000,
						Score:            20000,
						RoleCapacities: map[string]interfaces.RoleCapacity{
							"prefill": {Role: "prefill", RequiredCapacity: 10000},
							"decode":  {Role: "decode", RequiredCapacity: 10000},
						},
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "prefill-v", AcceleratorName: "H100", Cost: 15.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 20000},
							{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 4, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "decode"},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"H100": {Limit: 0}, // No H100s available
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Prefill can't scale (no H100 GPUs) — its share is consumed, not overflowed
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(1))
			// Decode gets only its proportional share (50% of demand)
			// roleTarget = 10000 (50% of 20000) → +1 replica
			// Prefill's 10000 is consumed (not absorbed by decode)
			Expect(dm["decode-v"].TargetReplicas).To(Equal(2)) // 1 + 1
		})

		It("should handle non-disaggregated model with Score", func() {
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Priority:  2.0,
					Result: &interfaces.AnalyzerResult{
						RequiredCapacity: 10000,
						Score:            20000, // 2.0 * 10000
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Should allocate based on Score (20000), which maps to 2 replicas
			Expect(dm["v1"].TargetReplicas).To(Equal(3)) // 1 + ceil(20000/10000)=2
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

			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						ModelID:          "model-1",
						Namespace:        "default",
						AnalyzedAt:       time.Now(),
						RequiredCapacity: 50000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
							{VariantName: "expensive", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 1, GPUsPerReplica: 1, MaxReplicas: intPtr(3)},
						{VariantName: "expensive", CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// cheap: capped at max=3 (starts at 1, can add 2)
			// expensive: gets remaining capacity
			Expect(dm["cheap"].TargetReplicas).To(BeNumerically("<=", 3))
		})

		It("scale-down should respect minReplicas via costAwareScaleDown", func() {
			intPtr := func(n int) *int { return &n }

			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						ModelID:          "model-1",
						Namespace:        "default",
						AnalyzedAt:       time.Now(),
						SpareCapacity:    50000,
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "expensive", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 3, PerReplicaCapacity: 20000},
							{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 3, GPUsPerReplica: 1, MinReplicas: intPtr(2)},
						{VariantName: "cheap", CurrentReplicas: 3, GPUsPerReplica: 1},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive: minReplicas=2, so can only remove 1
			Expect(dm["expensive"].TargetReplicas).To(BeNumerically(">=", 2))
		})

		It("scale-down should zero minReplicas=0 variant while keeping minReplicas>0 sibling", func() {
			intPtr := func(n int) *int { return &n }

			requests := []ModelScalingRequest{
				{
					ModelID:   "model-1",
					Namespace: "default",
					Result: &interfaces.AnalyzerResult{
						ModelID:          "model-1",
						Namespace:        "default",
						AnalyzedAt:       time.Now(),
						SpareCapacity:    80000, // enough to remove all
						VariantCapacities: []interfaces.VariantCapacity{
							{VariantName: "keep-alive", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
							{VariantName: "expendable", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
						},
					},
					VariantStates: []interfaces.VariantReplicaState{
						{VariantName: "keep-alive", CurrentReplicas: 2, GPUsPerReplica: 1, MinReplicas: intPtr(1)},
						{VariantName: "expendable", CurrentReplicas: 3, GPUsPerReplica: 1, MinReplicas: intPtr(0)},
					},
				},
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

		It("filterVariantCapacitiesByRole should filter by role", func() {
			capacities := []interfaces.VariantCapacity{
				{VariantName: "prefill-v", Role: "prefill"},
				{VariantName: "decode-v", Role: "decode"},
				{VariantName: "both-v", Role: "both"},
				{VariantName: "empty-v", Role: ""},
			}

			prefill := filterVariantCapacitiesByRole(capacities, "prefill")
			Expect(prefill).To(HaveLen(1))
			Expect(prefill[0].VariantName).To(Equal("prefill-v"))

			decode := filterVariantCapacitiesByRole(capacities, "decode")
			Expect(decode).To(HaveLen(1))
			Expect(decode[0].VariantName).To(Equal("decode-v"))

			// "both" returns all
			both := filterVariantCapacitiesByRole(capacities, "both")
			Expect(both).To(HaveLen(4))

			// empty returns all
			empty := filterVariantCapacitiesByRole(capacities, "")
			Expect(empty).To(HaveLen(4))
		})
	})
})
