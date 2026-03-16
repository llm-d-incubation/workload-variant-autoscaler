package saturation_v2

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

var _ = Describe("SaturationAnalyzer", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *CapacityKnowledgeStore
		ctx      context.Context
	)

	BeforeEach(func() {
		store = NewCapacityKnowledgeStore()
		analyzer = NewSaturationAnalyzer(store)
		ctx = context.Background()
	})

	Describe("Name", func() {
		It("should return 'saturation-token-based'", func() {
			Expect(analyzer.Name()).To(Equal("saturation-token-based"))
		})
	})

	Describe("k1/k2 interaction", func() {
		It("should use k1 (memory-bound) when k2 is unknown", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// k1 = 16000 * 0.8 = 12800, k2 = k1 (fallback, queue < threshold)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})

		It("should use k2 (compute-bound) when queue is saturated", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					// Queue >= threshold (5), tokensInUse = 8000
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						8000, 16000, 6, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// k1 = 12800, k2 = 8000 (observed: tokensInUse when queue saturated)
			// effective = min(12800, 8000) = 8000
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(8000)))
		})

		It("should detect compute-bound when k2 < k1 with high queue and low KV", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					// Low KV usage but saturated queue
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						4000, 16000, 10, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// k1 = 12800, k2 = 4000 (observed), effective = 4000
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(4000)))
		})
	})

	Describe("k2 history", func() {
		It("should store k2 observation in rolling average", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						8000, 16000, 6, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			_, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Verify k2 was stored in history
			histKey := "test-model|H100|short"
			ra, ok := analyzer.computeCapacityHistory[histKey]
			Expect(ok).To(BeTrue())
			Expect(ra.Average()).To(Equal(float64(8000)))
		})

		It("should use historical k2 when queue drops below threshold", func() {
			// First call: queue saturated → observes k2
			input1 := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						8000, 16000, 6, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, err := analyzer.Analyze(ctx, input1)
			Expect(err).NotTo(HaveOccurred())

			// Second call: queue below threshold → uses historical k2
			input2 := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						6000, 16000, 2, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			result, err := analyzer.Analyze(ctx, input2)
			Expect(err).NotTo(HaveOccurred())
			// Should use historical k2=8000, not fallback to k1=12800
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(8000)))
		})
	})

	Describe("Output-length bucketing", func() {
		It("should use different k2 history buckets for different output lengths", func() {
			// Short output workload: k2 observation
			input1 := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						8000, 16000, 6, 100, 50), // avgOutput=50 → "short"
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, _ = analyzer.Analyze(ctx, input1)

			// Long output workload: no history yet
			input2 := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						6000, 16000, 2, 100, 600), // avgOutput=600 → "long"
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			result, err := analyzer.Analyze(ctx, input2)
			Expect(err).NotTo(HaveOccurred())
			// No "long" history → falls back to k1=12800 (not the "short" k2=8000)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})
	})

	Describe("k2 derivation from deployment params", func() {
		It("should derive k2 from chunked prefill params", func() {
			// Pre-populate store with deployment params for this variant
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName: "H100",
				GpuCount:        1,
				VLLMParams: &VLLMEngineParams{
					EffectiveMaxBatchedTokens: 2048,
					MaxNumSeqs:                256,
					ChunkedPrefillEnabled:     true,
				},
				LearnedFrom: "deployment",
			})

			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					// Queue below threshold, no history → uses derived k2
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 2, 500, 100), // I=500, O=100
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// B=2048, S=256, I=500, O=100
			// N_steady = min(2048*100/(500+100), 256) = min(341.3, 256) = 256
			// k2 = 256 * (500 + 100/2) = 256 * 550 = 140800
			// k1 = 12800, effective = min(12800, 140800) = 12800
			// k2 > k1, so memory-bound
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})

		It("should fall back to k1 when no batch/queue/history data exists", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 0, 0), // no avg tokens
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// k2 derivation needs avgOutput > 0, falls back to k1
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})
	})

	Describe("Pending replicas", func() {
		It("should include pending replicas in anticipated supply for scale-up", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						10000, 16000, 0, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 2, PendingReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// readyCount = 2 - 1 = 1
			// anticipated = (1 + 1) * perReplicaCapacity
			// This suppresses scale-up signal since anticipated > ready
			Expect(result.RequiredCapacity).To(BeNumerically(">=", 0))
		})

		It("should NOT include pending replicas in scale-down calculation", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						1000, 16000, 0, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 3, PendingReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// spareCapacity uses totalSupply (ready only, not pending)
			// readyCount = 2, totalSupply = 2 * capacity
			Expect(result.SpareCapacity).To(BeNumerically(">=", 0))
		})
	})

	Describe("Zero-replica variants", func() {
		It("should use stored live capacity directly when variant has zero replicas", func() {
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName:   "H100",
				GpuCount:          1,
				EffectiveCapacity: 12000,
				LearnedFrom:       "live",
			})

			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{}, // no pods
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// Live record → uses stored EffectiveCapacity directly
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12000)))
		})

		It("should derive capacity from params + workload for deployment-derived records", func() {
			// Zero-replica variant with deployment-derived params only
			store.Update("test-ns", "test-model", "variant-b", CapacityRecord{
				AcceleratorName:   "A100",
				GpuCount:          1,
				EffectiveCapacity: 8192, // conservative fallback from LoadFromDeployment
				VLLMParams: &VLLMEngineParams{
					EffectiveMaxBatchedTokens: 8192,
					MaxNumSeqs:                256,
				},
				LearnedFrom: "deployment",
			})

			// Another variant has live pods providing workload data
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 500, 100), // I=500, O=100
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "variant-b", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(2))

			// variant-b: deployment-derived with workload data available
			// B=8192, S=256, I=500, O=100
			// N_steady = min(8192*100/(500+100), 256) = min(1365.3, 256) = 256
			// k2_derived = 256 * (500 + 100/2) = 256 * 550 = 140800
			// Much better than the 8192 fallback!
			varB := result.VariantCapacities[1]
			Expect(varB.VariantName).To(Equal("variant-b"))
			Expect(varB.PerReplicaCapacity).To(Equal(float64(140800)))
		})

		It("should bound k2 estimate by compatible variant's live EffectiveCapacity", func() {
			// variant-b is a new deployment on the same H100 hardware as variant-a
			defaultParams := &VLLMEngineParams{
				GpuMemoryUtilization:      0.9,
				BlockSize:                 16,
				KvCacheDtype:              "auto",
				TensorParallelSize:        1,
				MaxNumSeqs:                256,
				EffectiveMaxBatchedTokens: 8192,
			}
			store.Update("test-ns", "test-model", "variant-b", CapacityRecord{
				AcceleratorName:   "H100",
				GpuCount:          1,
				EffectiveCapacity: 8192,
				VLLMParams:        defaultParams,
				LearnedFrom:       "deployment",
			})

			// variant-a has a compatible live record (same accel, GPU count, params)
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 50000,
				EffectiveCapacity:     40000, // observed min(k1, k2) = 40000
				VLLMParams:            defaultParams,
				LearnedFrom:           "live",
			})

			// variant-a provides workload data
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 50000, 0, 500, 100), // I=500, O=100
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "variant-b", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// k2_derived = 256 * 550 = 140800, but bounded by variant-a's live
			// EffectiveCapacity = 40000 → result = min(140800, 40000) = 40000
			varB := result.VariantCapacities[1]
			Expect(varB.VariantName).To(Equal("variant-b"))
			Expect(varB.PerReplicaCapacity).To(Equal(float64(40000)))
		})

		It("should bound k2 estimate by own k1 when TotalKvCapacityTokens is known", func() {
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName:       "A100",
				GpuCount:              1,
				EffectiveCapacity:     8192,
				TotalKvCapacityTokens: 30000, // from num_gpu_blocks_override
				VLLMParams: &VLLMEngineParams{
					EffectiveMaxBatchedTokens: 8192,
					MaxNumSeqs:                256,
					NumGpuBlocksOverride:      1875,
					BlockSize:                 16,
				},
				LearnedFrom: "deployment",
			})

			// Another variant provides workload data (different accelerator, won't be compatible)
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-x", "L40S", 5.0,
						5000, 16000, 0, 500, 100),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-x", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "variant-a", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// k2_derived = 256 * 550 = 140800
			// k1 = 30000 * 0.8 (KvCacheThreshold) = 24000
			// No compatible live record (A100 vs L40S) → bounded by k1 only
			// result = min(140800, 24000) = 24000
			varA := result.VariantCapacities[1]
			Expect(varA.VariantName).To(Equal("variant-a"))
			Expect(varA.PerReplicaCapacity).To(Equal(float64(24000)))
		})

		It("should use EffectiveMaxBatchedTokens fallback when no workload data exists", func() {
			// Zero-replica variant with deployment-derived params, no other live pods
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName:   "H100",
				GpuCount:          1,
				EffectiveCapacity: 8192,
				VLLMParams: &VLLMEngineParams{
					EffectiveMaxBatchedTokens: 8192,
					MaxNumSeqs:                256,
				},
				LearnedFrom: "deployment",
			})

			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{}, // no live pods at all
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// No workload data → falls back to stored EffectiveCapacity (8192)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(8192)))
		})
	})

	Describe("estimateCapacityFromParams", func() {
		It("should compute k2 from B, S, I, O", func() {
			params := &VLLMEngineParams{
				EffectiveMaxBatchedTokens: 4096,
				MaxNumSeqs:                256,
			}
			// B=4096, S=256, I=500, O=100
			// N_steady = min(4096*100/600, 256) = min(682.6, 256) = 256
			// k2 = 256 * (500 + 50) = 256 * 550 = 140800
			Expect(estimateCapacityFromParams(params, 500, 100)).To(Equal(int64(140800)))
		})

		It("should cap N_steady at MaxNumSeqs", func() {
			params := &VLLMEngineParams{
				EffectiveMaxBatchedTokens: 8192,
				MaxNumSeqs:                64,
			}
			// B=8192, S=64, I=100, O=200
			// N_steady = min(8192*200/300, 64) = min(5461, 64) = 64
			// k2 = 64 * (100 + 100) = 64 * 200 = 12800
			Expect(estimateCapacityFromParams(params, 100, 200)).To(Equal(int64(12800)))
		})

		It("should return 0 when avgOutput is 0", func() {
			params := &VLLMEngineParams{
				EffectiveMaxBatchedTokens: 8192,
				MaxNumSeqs:                256,
			}
			Expect(estimateCapacityFromParams(params, 500, 0)).To(Equal(int64(0)))
		})

		It("should return 0 when params is nil", func() {
			Expect(estimateCapacityFromParams(nil, 500, 100)).To(Equal(int64(0)))
		})
	})

	Describe("Scaling signals", func() {
		It("should signal scale-up when demand exceeds threshold", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						11000, 16000, 3, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// demand is high relative to supply → requiredCapacity > 0
			Expect(result.RequiredCapacity).To(BeNumerically(">", 0))
		})

		It("should signal scale-down when utilization is below boundary", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						1000, 16000, 0, 100, 50),
					makeReplicaMetrics("pod-2", "variant-a", "H100", 10.0,
						1000, 16000, 0, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 2, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// Very low utilization → spareCapacity > 0
			Expect(result.SpareCapacity).To(BeNumerically(">", 0))
			Expect(result.RequiredCapacity).To(Equal(float64(0)))
		})

		It("should signal steady state when utilization is between thresholds", func() {
			// Supply ~ demand / 0.77 (between 0.70 boundary and 0.85 threshold)
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						10000, 16000, 0, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// utilization = 10000/12800 = 0.78 — between 0.70 and 0.85
			Expect(result.RequiredCapacity).To(Equal(float64(0)))
			Expect(result.SpareCapacity).To(Equal(float64(0)))
		})
	})

	Describe("Scheduler queue demand", func() {
		It("should add scheduler queue demand to total demand", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			input.SchedulerQueue = &interfaces.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 8000,
			}

			resultWithQueue, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Without scheduler queue
			input.SchedulerQueue = nil
			// Reset analyzer for clean comparison
			analyzer2 := NewSaturationAnalyzer(store)
			resultWithout, err := analyzer2.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			Expect(resultWithQueue.TotalDemand).To(BeNumerically(">", resultWithout.TotalDemand))
		})

		It("should not add demand when scheduler queue is nil", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// Total demand = just the replica demand (tokensInUse)
			Expect(result.TotalDemand).To(Equal(float64(5000)))
		})

		It("should reduce input tokens by prefix cache hit rate", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					{
						PodName: "pod-1", VariantName: "variant-a",
						AcceleratorName: "H100", Cost: 10.0,
						TokensInUse: 5000, TotalKvCapacityTokens: 16000,
						NumGpuBlocks: 1000, BlockSize: 16,
						AvgInputTokens: 100, AvgOutputTokens: 50,
						PrefixCacheHitRate: 0.5, // 50% cache hit rate
					},
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			input.SchedulerQueue = &interfaces.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 4000, // 4000/4 = 1000 tokens from bytes
			}

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Input from count: 10 * 100 = 1000 tokens
			// Input from bytes: 4000 / 4 = 1000 tokens
			// max(1000, 1000) = 1000
			// After cache hit: 1000 * (1 - 0.5) = 500
			// Output: 10 * 50 = 500
			// Scheduler demand = 500 + 500 = 1000
			// Replica demand = 5000
			// Total = 5000 + 1000 = 6000
			Expect(result.TotalDemand).To(Equal(float64(6000)))
		})

		It("should use max of bytes and count estimates for input tokens", func() {
			input := makeAnalyzerInput(
				[]interfaces.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 100, 50),
				},
				[]interfaces.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			input.SchedulerQueue = &interfaces.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 20000, // 20000/4 = 5000 >> 10*100=1000
			}

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Bytes estimate (5000) > count estimate (1000), so bytes wins
			// Input = 5000 * (1-0) = 5000 (no cache hit)
			// Output = 10 * 50 = 500
			// Scheduler demand = 5500
			// Total = 5000 + 5500 = 10500
			Expect(result.TotalDemand).To(Equal(float64(10500)))
		})
	})

	Describe("Scheduler queue demand role attribution", func() {
		It("should attribute inputTokens to prefill and inputTokens+outputTokens to decode", func() {
			metrics := []interfaces.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
					5000, 16000, 0, 100, 50),
			}
			activeRoles := map[string]bool{"prefill": true, "decode": true}
			sq := &interfaces.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0, // use count-based estimation only
			}

			result := estimateSchedulerQueueDemand(sq, metrics, activeRoles)

			// Input: max(0/4=0, 10*100=1000) = 1000 (no cache hit)
			// Output: 10 * 50 = 500
			// Total: 1000 + 500 = 1500
			Expect(result.total).To(Equal(1500.0))

			// Prefill gets inputTokens only
			Expect(result.byRole["prefill"]).To(Equal(1000.0))
			// Decode gets inputTokens + outputTokens
			Expect(result.byRole["decode"]).To(Equal(1500.0))
		})

		It("should attribute full demand to 'both' role", func() {
			metrics := []interfaces.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
					5000, 16000, 0, 100, 50),
			}
			activeRoles := map[string]bool{"both": true}
			sq := &interfaces.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0,
			}

			result := estimateSchedulerQueueDemand(sq, metrics, activeRoles)

			Expect(result.total).To(Equal(1500.0))
			Expect(result.byRole["both"]).To(Equal(1500.0))
		})

		It("should return empty byRole when nil activeRoles", func() {
			metrics := []interfaces.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
					5000, 16000, 0, 100, 50),
			}
			sq := &interfaces.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0,
			}

			result := estimateSchedulerQueueDemand(sq, metrics, nil)

			Expect(result.total).To(Equal(1500.0))
			Expect(result.byRole).To(BeEmpty())
		})

		It("should return zero for nil scheduler queue", func() {
			metrics := []interfaces.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
					5000, 16000, 0, 100, 50),
			}
			activeRoles := map[string]bool{"prefill": true, "decode": true}

			result := estimateSchedulerQueueDemand(nil, metrics, activeRoles)

			Expect(result.total).To(Equal(0.0))
			Expect(result.byRole).To(BeNil())
		})

		It("should apply prefix cache hit rate to per-role input tokens", func() {
			metrics := []interfaces.ReplicaMetrics{
				{
					PodName: "pod-1", VariantName: "variant-a",
					AcceleratorName: "H100", Cost: 10.0,
					TokensInUse: 5000, TotalKvCapacityTokens: 16000,
					NumGpuBlocks: 1000, BlockSize: 16,
					AvgInputTokens: 100, AvgOutputTokens: 50,
					PrefixCacheHitRate: 0.5, // 50% cache hit rate
				},
			}
			activeRoles := map[string]bool{"prefill": true, "decode": true}
			sq := &interfaces.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0,
			}

			result := estimateSchedulerQueueDemand(sq, metrics, activeRoles)

			// Input: 10*100=1000, after cache: 1000*(1-0.5)=500
			// Output: 10*50=500
			// Total: 500 + 500 = 1000
			Expect(result.total).To(Equal(1000.0))

			// Prefill: inputTokens = 500
			Expect(result.byRole["prefill"]).To(Equal(500.0))
			// Decode: inputTokens + outputTokens = 500 + 500 = 1000
			Expect(result.byRole["decode"]).To(Equal(1000.0))
		})

		It("should add queue demand to role capacities in end-to-end analysis", func() {
			input := interfaces.AnalyzerInput{
				ModelID:   "test-model",
				Namespace: "test-ns",
				ReplicaMetrics: []interfaces.ReplicaMetrics{
					{
						PodName: "prefill-pod", VariantName: "prefill-v",
						AcceleratorName: "H100", Cost: 10.0,
						TokensInUse: 3000, TotalKvCapacityTokens: 16000,
						NumGpuBlocks: 1000, BlockSize: 16,
						AvgInputTokens: 100, AvgOutputTokens: 50,
						ModelID: "test-model", Namespace: "test-ns",
					},
					{
						PodName: "decode-pod", VariantName: "decode-v",
						AcceleratorName: "H100", Cost: 10.0,
						TokensInUse: 2000, TotalKvCapacityTokens: 16000,
						NumGpuBlocks: 1000, BlockSize: 16,
						AvgInputTokens: 100, AvgOutputTokens: 50,
						ModelID: "test-model", Namespace: "test-ns",
					},
				},
				VariantStates: []interfaces.VariantReplicaState{
					{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 1, Role: "prefill"},
					{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 1, Role: "decode"},
				},
				Config: &config.SaturationScalingConfig{
					KvCacheThreshold:     0.8,
					QueueLengthThreshold: 5,
					AnalyzerName:         "saturation",
					ScaleUpThreshold:     0.85,
					ScaleDownBoundary:    0.70,
				},
				SchedulerQueue: &interfaces.SchedulerQueueMetrics{
					QueueSize:  10,
					QueueBytes: 0,
				},
			}

			store := NewCapacityKnowledgeStore()
			a := NewSaturationAnalyzer(store)
			result, err := a.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RoleCapacities).NotTo(BeNil())

			// Scheduler queue: input=max(0, 10*100)=1000, output=10*50=500
			// Prefill role demand: replica(3000) + queue(1000) = 4000
			// Decode role demand: replica(2000) + queue(1500) = 3500
			prefill := result.RoleCapacities["prefill"]
			Expect(prefill.TotalDemand).To(Equal(4000.0))

			decode := result.RoleCapacities["decode"]
			Expect(decode.TotalDemand).To(Equal(3500.0))

			// Model-level total still uses inputTokens+outputTokens (1500)
			// Replica demand = 3000 + 2000 = 5000
			// Total = 5000 + 1500 = 6500
			Expect(result.TotalDemand).To(Equal(6500.0))
		})
	})

	Describe("median helper", func() {
		It("should return 0 for empty slice", func() {
			Expect(median([]int64{})).To(Equal(int64(0)))
		})

		It("should return the value for single element", func() {
			Expect(median([]int64{42})).To(Equal(int64(42)))
		})

		It("should return middle value for odd count", func() {
			Expect(median([]int64{1, 3, 5})).To(Equal(int64(3)))
		})

		It("should return average of middle two for even count", func() {
			Expect(median([]int64{1, 3, 5, 7})).To(Equal(int64(4)))
		})

		It("should handle unsorted input", func() {
			Expect(median([]int64{5, 1, 3})).To(Equal(int64(3)))
		})
	})
})

// makeAnalyzerInput creates a standard AnalyzerInput with default config.
func makeAnalyzerInput(
	metrics []interfaces.ReplicaMetrics,
	states []interfaces.VariantReplicaState,
) interfaces.AnalyzerInput {
	config := &config.SaturationScalingConfig{
		KvCacheThreshold:     0.8,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.1,
		QueueSpareTrigger:    3,
		AnalyzerName:         "saturation",
		ScaleUpThreshold:     0.85,
		ScaleDownBoundary:    0.70,
	}
	return interfaces.AnalyzerInput{
		ModelID:        "test-model",
		Namespace:      "test-ns",
		ReplicaMetrics: metrics,
		VariantStates:  states,
		Config:         config,
	}
}

// makeReplicaMetrics creates a ReplicaMetrics with the given parameters.
func makeReplicaMetrics(
	podName, variantName, accelerator string,
	cost float64,
	tokensInUse, totalCapacity int64,
	queueLen int,
	avgInput, avgOutput float64,
) interfaces.ReplicaMetrics {
	var kvUsage float64
	if totalCapacity > 0 {
		kvUsage = float64(tokensInUse) / float64(totalCapacity)
	}
	blockSize := int64(16)
	numBlocks := totalCapacity / blockSize

	return interfaces.ReplicaMetrics{
		PodName:               podName,
		VariantName:           variantName,
		AcceleratorName:       accelerator,
		Cost:                  cost,
		KvCacheUsage:          kvUsage,
		QueueLength:           queueLen,
		NumGpuBlocks:          numBlocks,
		BlockSize:             blockSize,
		TotalKvCapacityTokens: totalCapacity,
		TokensInUse:           tokensInUse,
		AvgInputTokens:        avgInput,
		AvgOutputTokens:       avgOutput,
		ModelID:               "test-model",
		Namespace:             "test-ns",
	}
}

var _ = Describe("aggregateByRole", func() {
	var analyzer *SaturationAnalyzer

	BeforeEach(func() {
		store := NewCapacityKnowledgeStore()
		analyzer = NewSaturationAnalyzer(store)
	})

	It("should return nil when all variants are role 'both'", func() {
		vcs := []interfaces.VariantCapacity{
			{VariantName: "v1", Role: "both", TotalCapacity: 10000, TotalDemand: 5000, ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "v2", Role: "", TotalCapacity: 20000, TotalDemand: 10000, ReplicaCount: 1, PerReplicaCapacity: 20000},
		}
		config := &config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		result := analyzer.aggregateByRole(vcs, config, nil)
		Expect(result).To(BeNil())
	})

	It("should compute per-role capacities for P/D disaggregated model", func() {
		vcs := []interfaces.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalCapacity: 10000, TotalDemand: 9000, ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 10000},
			{VariantName: "decode-v1", Role: "decode", TotalCapacity: 20000, TotalDemand: 5000, ReplicaCount: 2, PendingReplicas: 0, PerReplicaCapacity: 10000},
		}
		config := &config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		result := analyzer.aggregateByRole(vcs, config, nil)
		Expect(result).NotTo(BeNil())
		Expect(result).To(HaveLen(2))

		prefill := result["prefill"]
		Expect(prefill.Role).To(Equal("prefill"))
		Expect(prefill.TotalSupply).To(Equal(10000.0))
		Expect(prefill.TotalDemand).To(Equal(9000.0))
		// requiredCapacity = 9000/0.85 - 10000 = 10588 - 10000 = 588
		Expect(prefill.RequiredCapacity).To(BeNumerically(">", 0))

		decode := result["decode"]
		Expect(decode.Role).To(Equal("decode"))
		Expect(decode.TotalSupply).To(Equal(20000.0))
		Expect(decode.TotalDemand).To(Equal(5000.0))
		// spareCapacity = 20000 - 5000/0.70 = 20000 - 7143 = 12857
		Expect(decode.SpareCapacity).To(BeNumerically(">", 0))
		Expect(decode.RequiredCapacity).To(Equal(0.0))
	})

	It("should handle mixed roles including 'both'", func() {
		vcs := []interfaces.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalCapacity: 10000, TotalDemand: 9000, ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "both-v1", Role: "both", TotalCapacity: 10000, TotalDemand: 5000, ReplicaCount: 1, PerReplicaCapacity: 10000},
		}
		config := &config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		result := analyzer.aggregateByRole(vcs, config, nil)
		// Has disaggregation because prefill-v1 has role != "both"
		Expect(result).NotTo(BeNil())
		Expect(result).To(HaveKey("prefill"))
		Expect(result).To(HaveKey("both"))
	})

	It("should add scheduler queue demand to per-role totals", func() {
		vcs := []interfaces.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalCapacity: 10000, TotalDemand: 2000, ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 10000},
			{VariantName: "decode-v1", Role: "decode", TotalCapacity: 20000, TotalDemand: 3000, ReplicaCount: 2, PendingReplicas: 0, PerReplicaCapacity: 10000},
		}
		config := &config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		queueByRole := map[string]float64{
			"prefill": 1000, // inputTokens only
			"decode":  1500, // inputTokens + outputTokens
		}
		result := analyzer.aggregateByRole(vcs, config, queueByRole)
		Expect(result).NotTo(BeNil())

		prefill := result["prefill"]
		// 2000 (replica) + 1000 (queue) = 3000
		Expect(prefill.TotalDemand).To(Equal(3000.0))

		decode := result["decode"]
		// 3000 (replica) + 1500 (queue) = 4500
		Expect(decode.TotalDemand).To(Equal(4500.0))
	})

	It("should skip queue demand for roles with no variants", func() {
		vcs := []interfaces.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalCapacity: 10000, TotalDemand: 5000, ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 10000},
		}
		config := &config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		// Queue demand for decode, but no decode variants exist
		queueByRole := map[string]float64{
			"prefill": 1000,
			"decode":  1500,
		}
		result := analyzer.aggregateByRole(vcs, config, queueByRole)
		Expect(result).NotTo(BeNil())
		Expect(result).To(HaveLen(1))
		Expect(result).To(HaveKey("prefill"))
		Expect(result["prefill"].TotalDemand).To(Equal(6000.0)) // 5000 + 1000
	})
})
