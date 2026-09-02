package saturation_v2

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// These exercise computeReplicaCapacity rather than rateAnchoredK2 in isolation:
// what the capacity store learns, that nothing changes when the estimator is off,
// and the two behaviours the design exists for — holding capacity after a drain,
// and giving siblings a value the variant-level median can combine safely.
var _ = Describe("Rate-anchored k2 through computeReplicaCapacity", func() {
	const (
		kvCapacity = int64(400_000)
		namespace  = "ns"
		modelID    = "m"
		variant    = "v1"
	)

	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	shape := variantShape{avgInput: 1000, avgOutput: 250}

	scalingConfig := func() *config.SaturationScalingConfig {
		return &config.SaturationScalingConfig{
			KvCacheThreshold:     0.8, // k1 = 320k
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.75,
			ScaleDownBoundary:    0.60,
		}
	}

	// atLimit is queueing deep enough to calibrate, at 20% KV occupancy.
	atLimit := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			VariantName:           variant,
			ModelID:               modelID,
			Namespace:             namespace,
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			KvCacheUsage:          0.20,
			TokensInUse:           int64(0.20 * float64(kvCapacity)),
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	It("learns the measured limit into the capacity store", func() {
		store := NewCapacityKnowledgeStore()
		clock := start
		a := NewSaturationAnalyzer(store, withRateAnchoredK2(true), withClock(func() time.Time { return clock }))
		cfg := scalingConfig()
		rm := atLimit()

		var rc *ReplicaCapacity
		for i := 0; i <= MinServiceRateSamples+1; i++ {
			clock = clock.Add(15 * time.Second)
			a.serviceRates.BeginCycle(clock)
			rc = a.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth, shape)
		}
		Expect(rc).NotTo(BeNil())
		Expect(rc.K2Priority).To(BeElementOf(k2SrcRateBacklog, k2SrcRateAnchored))

		rec := store.Get(namespace, modelID, variant)
		Expect(rec).NotTo(BeNil())
		// What the store learns is a limit measured under load, so it is the same
		// figure this cycle scaled on — no separate stored value is needed.
		Expect(rec.EffectiveCapacity).To(Equal(rc.EffectiveCapacity))
		Expect(rec.EffectiveCapacity).To(BeNumerically("<=", int64(0.8*float64(kvCapacity))))
	})

	It("is inert when the estimator is off", func() {
		cfg := scalingConfig()
		rm := atLimit()

		off := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		Expect(off.serviceRates).To(BeNil(), "guards the default of EnableRateAnchoredK2")
		Expect(off.arrivals).To(BeNil())

		baseline := off.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth, shape)
		Expect(baseline).NotTo(BeNil())
		Expect(baseline.K2Priority).NotTo(Equal(k2SrcRateAnchored))
		Expect(baseline.K2Priority).NotTo(Equal(k2SrcRateBacklog))

		again := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		repeat := again.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth, shape)
		Expect(repeat.EffectiveCapacity).To(Equal(baseline.EffectiveCapacity))
		Expect(repeat.MemoryBoundCapacity).To(Equal(baseline.MemoryBoundCapacity))
		Expect(repeat.ComputeBoundCapacity).To(Equal(baseline.ComputeBoundCapacity))
		Expect(repeat.IsSaturated).To(Equal(baseline.IsSaturated))
		Expect(repeat.ReplicaDemand).To(Equal(baseline.ReplicaDemand))
	})

	It("holds capacity after a drain where the occupancy estimator releases it", func() {
		cfg := scalingConfig()

		// The peak that validation round 1 actually measured: a replica queueing with
		// KV near full. Its occupancy is ABOVE k1, so the learned ceiling is discarded
		// by min(k1, k2) and cannot help — only the operating point can. Little's law
		// ties the numbers together: mu=8 req/s x W=34 s x 1250 tokens = 340k.
		peak := atLimit()
		peak.KvCacheUsage = 0.85
		peak.TokensInUse = int64(0.85 * float64(kvCapacity)) // 340k, above k1
		peak.AvgTTFT = 4.0
		peak.AvgITL = 0.12 // W = 4 + 250 x 0.12 = 34 s

		clock := start
		tick := func() time.Time {
			clock = clock.Add(15 * time.Second)
			return clock
		}
		off := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withClock(tick))
		on := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true), withClock(tick))
		for i := 0; i < MinServiceRateSamples+1; i++ {
			on.serviceRates.BeginCycle(clock)
			_ = off.computeReplicaCapacity(peak, cfg, modelID, namespace, 1, domain.RoleBoth, shape)
			_ = on.computeReplicaCapacity(peak, cfg, modelID, namespace, 1, domain.RoleBoth, shape)
		}

		// Queue drained and contention with it: residence falls to 6 s, so occupancy
		// falls in step (8 x 6 x 1250 = 60k). Arrivals are unchanged — nothing about
		// the load has improved, only the crowding.
		drained := peak
		drained.QueueLength = 0
		drained.KvCacheUsage = 0.16
		// Little's law puts the resident tokens at 8 x 6 x 1250 = 60k; TokensInUse is
		// max_over_time(...[1m]), so it reads a little above that. That gap is the
		// documented bias between the two sides' time bases, and it errs toward more
		// replicas rather than fewer.
		drained.TokensInUse = 63_000
		drained.ArrivalRate = 8.0
		drained.AvgTTFT = 1.0
		drained.AvgITL = 0.02 // W = 6 s

		var occupancyBased, rateBased *ReplicaCapacity
		for i := 0; i < 40; i++ { // the operating point is smoothed over about a minute
			on.serviceRates.BeginCycle(clock)
			occupancyBased = off.computeReplicaCapacity(drained, cfg, modelID, namespace, 1, domain.RoleBoth, shape)
			rateBased = on.computeReplicaCapacity(drained, cfg, modelID, namespace, 1, domain.RoleBoth, shape)
		}

		// The occupancy path answers from its inflated history and reports abundant
		// spare capacity: demand 60k against a capacity of k1. That is the shed.
		Expect(occupancyBased.EffectiveCapacity).To(BeNumerically(">", drained.TokensInUse*3),
			"documents the behaviour being replaced")
		Expect(occupancyBased.IsSaturated).To(BeFalse())

		// The rate path scales its capacity to the operating point, so utilization is
		// where it was under load and nothing is shed.
		Expect(rateBased.EffectiveCapacity).To(BeNumerically("<=", drained.TokensInUse),
			"capacity follows the operating point, not a stale peak")
		Expect(rateBased.IsSaturated).To(BeTrue())
		Expect(rateBased.K2Priority).To(Equal(k2SrcRateResidence))

		// What the store kept is the load-independent measurement, not the scaled
		// value this cycle decided on: the store feeds variants with no live replicas
		// and cross-variant estimation, where "what it is doing now" is meaningless.
		rec := on.capacityStore.Get(namespace, modelID, variant)
		Expect(rec).NotTo(BeNil())
		// Exactly k1: the measured reference is above the memory bound here, so the
		// clamp decides. Asserting only "> the scaled value" would pass just as
		// happily on an unclamped 340k, or on the scaled value being stored.
		Expect(rec.EffectiveCapacity).To(Equal(int64(0.8 * float64(kvCapacity))))
	})

	It("gives a backlogged replica and an idle sibling the same capacity", func() {
		cfg := scalingConfig()
		clock := start
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true),
			withClock(func() time.Time { return clock }))

		hot := atLimit()
		for i := 0; i <= MinServiceRateSamples+1; i++ {
			clock = clock.Add(15 * time.Second)
			a.serviceRates.BeginCycle(clock)
			_ = a.computeReplicaCapacity(hot, cfg, modelID, namespace, 1, domain.RoleBoth, shape)
		}
		a.serviceRates.BeginCycle(clock.Add(15 * time.Second))
		hotRC := a.computeReplicaCapacity(hot, cfg, modelID, namespace, 1, domain.RoleBoth, shape)

		cold := atLimit()
		cold.PodName = siblingPod
		cold.QueueLength = 0
		cold.ArrivalRate = 2.0
		coldRC := a.computeReplicaCapacity(cold, cfg, modelID, namespace, 1, domain.RoleBoth, shape)

		// aggregateByVariant takes the MEDIAN of per-replica capacities. Equal values
		// make that median a no-op, so an idle sibling cannot lift variant capacity
		// while another replica queues — the regression that reintroduced shed-to-one.
		Expect(coldRC.EffectiveCapacity).To(Equal(hotRC.EffectiveCapacity))
		// The idle replica is not itself saturated: its own demand is what differs.
		Expect(coldRC.ReplicaDemand).To(BeNumerically("<", hotRC.ReplicaDemand))
	})
})

// This one goes through Analyze rather than computeReplicaCapacity, because the
// property it checks lives in the wiring: BeginCycle has to run at the top of the
// cycle, before any replica is looked at. Drop that call, or move it after the
// per-replica loop, and every other test in this file still passes.
var _ = Describe("Rate-anchored k2 through Analyze", func() {
	const (
		kvCapacity = int64(400_000)
		namespace  = "ns"
		modelID    = "m"
		variant    = "v1"
	)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	replica := func(pod string, queue int, tokens int64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               pod,
			VariantName:           variant,
			ModelID:               modelID,
			Namespace:             namespace,
			AcceleratorName:       "H100",
			QueueLength:           queue,
			QueueLengthInstant:    float64(queue),
			HasQueueLengthInstant: true,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			KvCacheUsage:          float64(tokens) / float64(kvCapacity),
			KvUsageInstant:        float64(tokens) / float64(kvCapacity),
			TokensInUse:           tokens,
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
			AvgTTFT:               1.0,
			AvgITL:                0.02,
		}
	}

	It("gives a variant's replicas one capacity, cycle after cycle", func() {
		clock := start
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true),
			withClock(func() time.Time { return clock }))
		cfg := &config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.75,
			ScaleDownBoundary:    0.60,
		}

		// Three backlogged replicas of one variant, at different occupancies — which
		// is the realistic case, and the one where a per-replica capacity would put
		// three different numbers through aggregateByVariant's median.
		input := domain.AnalyzerInput{
			ModelID:   modelID,
			Namespace: namespace,
			ReplicaMetrics: []domain.ReplicaMetrics{
				replica("pod-a", 12, 60_000),
				replica("pod-b", 9, 66_000),
				replica("pod-c", 14, 58_000),
			},
			VariantStates: []domain.VariantReplicaState{{
				VariantName: variant, Role: domain.RoleBoth,
				GPUsPerReplica: 1, CurrentReplicas: 3,
			}},
			Config: cfg,
		}

		var result *domain.AnalyzerResult
		for i := 0; i < 4; i++ {
			clock = start.Add(time.Duration(i) * 15 * time.Second)
			var err error
			result, err = a.Analyze(context.Background(), input)
			Expect(err).NotTo(HaveOccurred())
		}

		Expect(result.VariantCapacities).To(HaveLen(1))
		vc := result.VariantCapacities[0]
		Expect(vc.PerReplicaCapacity).To(BeNumerically(">", 0))
		// Supply is replicaCount x median(per-replica capacities), so equal values are
		// what make that median a no-op. The lowest occupancy seen under backlog is
		// 58k, and every replica must be reading it.
		Expect(vc.TotalCapacity).To(BeNumerically("~", 3*vc.PerReplicaCapacity, 1))
		Expect(vc.PerReplicaCapacity).To(BeNumerically("~", 58_000, 1_000))
	})
})
