package pipeline

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

var _ = Describe("GreedyBySaturationOptimizer", func() {

	var (
		optimizer *GreedyBySaturationOptimizer
		ctx       context.Context
	)

	BeforeEach(func() {
		optimizer = NewGreedyBySaturationOptimizer()
		ctx = context.Background()
	})

	It("should return 'greedy-by-saturation' as name", func() {
		Expect(optimizer.Name()).To(Equal("greedy-by-saturation"))
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

			// Single model: mean = 20000 (only one model), target = remaining - mean = 0
			// Wait — with single model, mean = remaining. target = remaining - mean = 0.
			// The algorithm should still try to allocate. Let me trace:
			// active=[model-1(20000)], mean=20000, pick model-1
			// w.remaining(20000) <= mean(20000) AND len(active)==1 → condition is len(active)>1, so we don't break
			// allocateForModel: target = 20000 - 20000 = 0 → returns false (already at mean)
			// → w.remaining = -1 (removed)
			// Actually this means single model with target=0 won't allocate. That's wrong.
			// Need to handle single model case: when len(active)==1, mean = remaining, so target=0.
			// The correct fix: when single model, target should be the full remaining.
			// Actually re-reading the design doc: "If mean is 0 (single model), target is to satisfy all demand"
			// But mean isn't 0 when single model, mean = remaining. The fix in the design doc says target = w.remaining - mean,
			// if target <= 0 return false. So with single model, this returns false immediately.
			// That means we need to handle the single model case differently.
			// Let me re-examine the walkthrough: Iteration 6 has {A(5000)}, mean=5000, pick A,
			// "A tries to allocate → 0 GPUs available → removed". So the doc expects allocation to be attempted.
			// The check "w.remaining <= mean && len(active) > 1" doesn't break when len==1.
			// But allocateForModel with target = 5000 - 5000 = 0 → returns false.
			// Wait — the iteration 6 says "no GPUs" not "target is 0". Something's off.
			//
			// Actually looking more carefully at the design: for single model, we WANT to
			// allocate its full demand. The correct approach is: when there's only one model,
			// use target = remaining (don't subtract mean). Or equivalently, mean=0 for the
			// single active model case.

			// After the fix: cheap is most cost-efficient (5/10000 vs 15/20000)
			// ceil(20000/10000) = 2 replicas, needs 4 A100 GPUs (2 per replica)
			// A100 has 10 available → sufficient
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

			// Iteration 1: mean=(50000+10000)/2=30000. Pick A(50000).
			// target=50000-30000=20000, ceil(20000/15000)=2 replicas (4 GPUs).
			// A.remaining=50000-30000=20000. GPUs left: 4.
			// 20000 < 30000 → A stays.
			//
			// Iteration 2: active={A(20000), B(10000)}. mean=15000. Pick A(20000).
			// target=20000-15000=5000, ceil(5000/15000)=1 replica (2 GPUs).
			// A.remaining=20000-15000=5000. GPUs left: 2.
			// 5000 < 15000 → A stays.
			//
			// Iteration 3: active={A(5000), B(10000)}. mean=7500. Pick B(10000).
			// target=10000-7500=2500, ceil(2500/15000)=1 replica (2 GPUs).
			// B.remaining=10000-15000=-5000. GPUs left: 0.
			// B satisfied, removed.
			//
			// Iteration 4: active={A(5000)}. No GPUs left → A removed.

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

			// Design doc walkthrough: A gets 3 replicas, B gets 2, C gets 1
			// Total GPUs: (3+2+1)*2 = 12 ✓

			// A: 1 original + 3 added = 4
			Expect(dm["a-v1"].TargetReplicas).To(Equal(4))
			// B: 1 original + 2 added = 3
			Expect(dm["b-v1"].TargetReplicas).To(Equal(3))
			// C: 1 original + 1 added = 2
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
					"A100": {Limit: 8}, // 4 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Equal demand: mean=20000. Both tied at mean.
			// Fair-share: allocationMean = mean - (remaining / N) = 20000 - 10000 = 10000
			// target = 20000 - 10000 = 10000. Each model gets 1 replica per iteration.
			// With 8 GPUs (4 replicas worth), both models get 2 replicas each.
			// Total: X=1+2=3, Y=1+2=3. 4 replicas × 2 GPUs = 8 GPUs used.
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
					"H100": {Limit: 4}, // 1 H100 replica
					"A100": {Limit: 6}, // 3 A100 replicas
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// H100: only 4 GPUs available / 4 per replica = 1 replica max
			// A100: 6 GPUs / 2 per replica = 3 replicas max
			// Iter 1: mean=25000. Pick h100(30000). target=5000, ceil(5000/20000)=1.
			//   h100.remaining=10000. H100 GPUs left: 0.
			// Iter 2: mean=15000. Pick a100(20000). target=5000, ceil(5000/10000)=1.
			//   a100.remaining=10000. A100 GPUs left: 4.
			// Iter 3: mean=10000. Both tied. Fair-share: allocationMean=10000-5000=5000.
			//   Pick h100(10000): target=5000, but H100 GPUs=0 → can't allocate → removed.
			// Iter 4: a100(10000) single model. allocationMean=0. target=10000.
			//   ceil(10000/10000)=1 replica (2 GPUs). A100 GPUs left: 2.

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
					"A100": {Limit: 4}, // 2 A100 replicas
					"H100": {Limit: 0}, // no H100 GPUs
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// A100 is more cost-efficient: 5/10000=0.0005 vs H100: 15/20000=0.00075
			// Try A100 first: 4 GPUs / 2 per replica = 2 replicas
			// Single model: allocate full remaining. ceil(30000/10000)=3 but max 2.
			// a100-v gets +2 (20000 tokens), remaining=10000
			// Then try H100: 0 GPUs → skip
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

			// No GPUs available → no scale-up
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

			// Nil constraints → empty available map → no GPUs → no scale-up
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

			// Same behavior as CostAwareOptimizer for scale-down
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

			// Scale-up model gets GPUs from constraints
			Expect(dm["up-v1"].TargetReplicas).To(Equal(2))
			Expect(dm["up-v1"].Action).To(Equal(interfaces.ActionScaleUp))

			// Scale-down model handled independently
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

			// Pending replicas are NOT skipped — the V2 analyzer already accounts
			// for pending capacity in anticipated supply. If RequiredCapacity > 0,
			// demand exceeds total supply including pending.
			// cheap-pending is more cost-efficient (5/10000=0.0005 vs 15/20000=0.00075)
			// → it gets the allocation.
			Expect(dm["cheap-pending"].TargetReplicas).To(Equal(3))    // +1
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
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 0}, // 0 defaults to 1
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 2}, // enough for 2 replicas at 1 GPU each
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// ceil(10000/10000) = 1 replica, needs 1 GPU. 2 GPUs available.
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

		It("should contain greedy-by-saturation in reason strings", func() {
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
			Expect(decisions[0].Reason).To(ContainSubstring("greedy-by-saturation"))
		})
	})

	Context("Helper Functions", func() {

		It("filterActive should return only models with remaining > 0", func() {
			work := []*modelWork{
				{remaining: 100},
				{remaining: -1}, // removed
				{remaining: 50},
				{remaining: 0},  // satisfied
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
	})
})
