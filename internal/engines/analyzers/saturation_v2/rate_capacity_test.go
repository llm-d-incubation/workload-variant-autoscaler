package saturation_v2

import (
	"fmt"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// rateCycle and ceilingCycle mirror one optimize cycle: replicas observe, then the
// boundary folds what they observed. Reading without crossing a boundary is not a
// state production can be in, so the tests do not exercise one.
func rateCycle(s *bucketStore, key string, rates []float64, at time.Time) { //nolint:unparam // one bucket is enough for these; the parameter keeps the helper honest
	for _, r := range rates {
		s.ObserveRate(key, r, at)
	}
	s.BeginCycle(at)
}

func ceilingCycle(s *bucketStore, key string, tokens []float64, at time.Time) {
	for _, t := range tokens {
		s.ObserveCeiling(key, t, at)
	}
	s.BeginCycle(at)
}

var _ = Describe("Bucket store — service rate", func() {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	It("tracks the mean of what it sees, in both directions", func() {
		s := newBucketStore()
		rateCycle(s, "k", []float64{4.0}, now)
		rateCycle(s, "k", []float64{9.0}, now.Add(time.Minute))
		rateCycle(s, "k", []float64{6.0}, now.Add(2*time.Minute))

		rate, ok := s.Rate("k", now.Add(2*time.Minute))
		Expect(ok).To(BeTrue())
		// Between the samples, not pinned to the largest. Under backlog the server is
		// never idle, so every sample is a reading of the service rate — preferring
		// the maximum would only ratchet capacity upward and under-scale.
		Expect(rate).To(BeNumerically(">", 4.0))
		Expect(rate).To(BeNumerically("<", 9.0))
	})

	It("comes down when the workload gets heavier within a bucket", func() {
		s := newBucketStore()
		at := now
		for i := 0; i < 6; i++ { // calibrate at 10 req/s
			rateCycle(s, "k", []float64{10.0}, at)
			at = at.Add(30 * time.Second)
		}
		fast, _ := s.Rate("k", at)

		for i := 0; i < 12; i++ { // longer prompts: the same replica now serves 5/s
			rateCycle(s, "k", []float64{5.0}, at)
			at = at.Add(30 * time.Second)
		}
		slow, ok := s.Rate("k", at)

		Expect(ok).To(BeTrue())
		Expect(slow).To(BeNumerically("<", fast*0.8),
			"a running maximum would still be reporting the old rate here")
	})

	It("withholds an estimate until a second cycle", func() {
		s := newBucketStore()
		rateCycle(s, "k", []float64{7.0}, now)
		_, ok := s.Rate("k", now)
		Expect(ok).To(BeFalse(), "one cycle cannot distinguish a limit from a slow interval")

		rateCycle(s, "k", []float64{7.0}, now.Add(time.Minute))
		_, ok = s.Rate("k", now.Add(time.Minute))
		Expect(ok).To(BeTrue())
	})

	It("counts cycles, not the replicas reporting in them", func() {
		s := newBucketStore()
		// Four replicas of one bucket, all backlogged, all in the same cycle. That is
		// one interval of evidence however many pods produced it.
		rateCycle(s, "k", []float64{7.0, 7.0, 7.0, 7.0}, now)
		_, ok := s.Rate("k", now)
		Expect(ok).To(BeFalse(), "MinServiceRateSamples must not be satisfiable within one cycle")
	})

	It("averages the cycle's replicas rather than taking whichever reported first", func() {
		s := newBucketStore()
		rateCycle(s, "k", []float64{4.0, 8.0, 12.0}, now)
		rateCycle(s, "k", []float64{4.0, 8.0, 12.0}, now.Add(time.Minute))

		rate, ok := s.Rate("k", now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		// ReplicaMetrics is built by ranging a map, so "whichever reported first" is
		// not even stable between cycles — mu would jitter with no smoothing at all.
		Expect(rate).To(BeNumerically("~", 8.0, 0.01))
	})

	It("ignores rates that are not usable numbers", func() {
		s := newBucketStore()
		rateCycle(s, "k", []float64{0, -3, math.NaN(), math.Inf(1)}, now)
		rateCycle(s, "k", []float64{0, -3, math.NaN(), math.Inf(1)}, now.Add(time.Minute))
		_, ok := s.Rate("k", now.Add(time.Minute))
		Expect(ok).To(BeFalse())

		// And a poisoned value must not survive a later good one: NaN folded into the
		// EWMA would stay NaN forever, and Rate's own `<= 0` guard does not catch it.
		rateCycle(s, "k", []float64{math.NaN()}, now.Add(2*time.Minute))
		rateCycle(s, "k", []float64{6.0}, now.Add(3*time.Minute))
		rateCycle(s, "k", []float64{6.0}, now.Add(4*time.Minute))
		rate, ok := s.Rate("k", now.Add(4*time.Minute))
		Expect(ok).To(BeTrue())
		Expect(math.IsNaN(rate)).To(BeFalse())
		Expect(rate).To(BeNumerically("~", 6.0, 0.01))
	})

	It("holds an unrefreshed rate and expires it past the window", func() {
		s := newBucketStore()
		rateCycle(s, "k", []float64{10.0}, now)
		rateCycle(s, "k", []float64{10.0}, now)

		rate, ok := s.Rate("k", now.Add(ServiceRateWindow))
		Expect(ok).To(BeTrue())
		Expect(rate).To(BeNumerically("~", 10.0, 0.01))

		_, ok = s.Rate("k", now.Add(ServiceRateWindow+time.Second))
		Expect(ok).To(BeFalse(), "stale evidence is dropped rather than aged into a guess")
	})
})

var _ = Describe("Bucket store — token ceiling", func() {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	It("keeps the lowest occupancy at which a limit was seen", func() {
		s := newBucketStore()
		ceilingCycle(s, "k", []float64{90_000, 64_000, 80_000}, now)

		c, ok := s.Ceiling("k", now)
		Expect(ok).To(BeTrue())
		Expect(c).To(BeNumerically("~", 64_000, 1),
			"a running minimum: the conservative reading of capacity")
	})

	It("relaxes upward when no fresh limit is observed", func() {
		s := newBucketStore()
		ceilingCycle(s, "k", []float64{64_000}, now)

		c, ok := s.Ceiling("k", now.Add(ServiceRateWindow))
		Expect(ok).To(BeTrue())
		Expect(c).To(BeNumerically("~", 64_000*CeilingRelaxPerWindow, 1),
			"a single pessimistic measurement must not cap the bucket forever")
	})

	It("lets a fresh higher measurement win over a relaxed one", func() {
		s := newBucketStore()
		ceilingCycle(s, "k", []float64{64_000}, now)
		ceilingCycle(s, "k", []float64{120_000}, now.Add(ServiceRateWindow))

		c, ok := s.Ceiling("k", now.Add(ServiceRateWindow))
		Expect(ok).To(BeTrue())
		Expect(c).To(BeNumerically("~", 80_000, 1), "64k relaxed to 80k, still below 120k")
	})

	It("expires past the window and evicts with the rate", func() {
		s := newBucketStore()
		s.ObserveCeiling("k", 64_000, now)
		_, ok := s.Ceiling("k", now.Add(ServiceRateWindow+time.Second))
		Expect(ok).To(BeFalse())

		Expect(s.EvictStale(time.Hour, now.Add(2*time.Hour))).To(Equal(1))
	})

	It("separates buckets by role and by input length", func() {
		decode := serviceRateKey("m", "H100", domain.RoleDecode, 1, variantShape{1000, 250})
		prefill := serviceRateKey("m", "H100", domain.RolePrefill, 1, variantShape{1000, 250})
		shortIn := serviceRateKey("m", "H100", domain.RoleDecode, 1, variantShape{300, 250})
		Expect(decode).NotTo(Equal(prefill))
		Expect(decode).NotTo(Equal(shortIn),
			"prompt lengths get their own thresholds; 300 and 1000 are different services")
	})

	It("keys on the variant's shape, so siblings cannot land in different buckets", func() {
		// Two replicas of one variant, their averages a few tokens either side of the
		// 500-token input threshold — sampling noise, not different workloads. Keyed
		// per replica they would learn independent ceilings, and aggregateByVariant's
		// median would then blend two figures that measure different things.
		shapes := variantShapes([]domain.ReplicaMetrics{
			{VariantName: "v1", AvgInputTokens: 498, AvgOutputTokens: 250},
			{VariantName: "v1", AvgInputTokens: 506, AvgOutputTokens: 250},
			{VariantName: "v2", AvgInputTokens: 4000, AvgOutputTokens: 250},
		})
		Expect(shapes).To(HaveLen(2))
		Expect(shapes["v1"].avgInput).To(BeNumerically("~", 502, 0.01))

		one := serviceRateKey("m", "H100", domain.RoleBoth, 1, shapes["v1"])
		Expect(serviceRateKey("m", "H100", domain.RoleBoth, 1, shapes["v1"])).To(Equal(one))
		Expect(serviceRateKey("m", "H100", domain.RoleBoth, 1, shapes["v2"])).NotTo(Equal(one))
	})

	It("ignores replicas reporting no shape at all", func() {
		shapes := variantShapes([]domain.ReplicaMetrics{
			{VariantName: "v1", AvgInputTokens: 1000, AvgOutputTokens: 250},
			{VariantName: "v1"}, // scraped before it served anything
		})
		Expect(shapes["v1"].avgInput).To(BeNumerically("~", 1000, 0.01),
			"a replica with no data must not halve the variant's shape")
	})
})

var _ = Describe("Arrival smoothing over the residence time", func() {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	It("returns the first sample unchanged", func() {
		Expect(newArrivalSmoother().Smooth("pod", 10.0, 60, now)).To(Equal(10.0))
	})

	It("lags a step change instead of following it", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("pod", 4.0, 60, now)
		got := s.Smooth("pod", 16.0, 60, now.Add(15*time.Second))
		Expect(got).To(BeNumerically(">", 4.0))
		Expect(got).To(BeNumerically("<", 16.0))
	})

	It("converges once a few time constants have passed", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("pod", 4.0, 10, now)
		got := 0.0
		for i := 1; i <= 10; i++ {
			got = s.Smooth("pod", 16.0, 10, now.Add(time.Duration(i)*10*time.Second))
		}
		Expect(got).To(BeNumerically("~", 16.0, 0.5))
	})

	It("discards a stale average rather than blending it", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("pod", 4.0, 10, now)
		Expect(s.Smooth("pod", 16.0, 10, now.Add(time.Hour))).To(Equal(16.0))
	})

	It("passes through when the residence estimate is unavailable", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("pod", 4.0, 0, now)
		Expect(s.Smooth("pod", 16.0, 0, now.Add(time.Second))).To(Equal(16.0))
	})

	It("keeps replicas separate and evicts the absent", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("a", 4.0, 60, now)
		Expect(s.Smooth("b", 20.0, 60, now)).To(Equal(20.0))
		_ = s.Smooth("gone", 4.0, 60, now.Add(-2*time.Hour))
		Expect(s.EvictStale(time.Hour, now)).To(Equal(1))
	})
})

var _ = Describe("Residence estimate", func() {
	It("is time to first token plus one ITL per output token", func() {
		Expect(residenceSeconds(domain.ReplicaMetrics{
			AvgTTFT: 2.0, AvgITL: 0.02, AvgOutputTokens: 250,
		})).To(BeNumerically("~", 2.0+250*0.02, 0.001))
	})

	It("declines without latency data", func() {
		Expect(residenceSeconds(domain.ReplicaMetrics{AvgOutputTokens: 250})).To(BeZero())
		Expect(residenceSeconds(domain.ReplicaMetrics{AvgITL: 0.02})).To(BeZero())
	})

	It("bounds an implausible reading from above", func() {
		Expect(residenceSeconds(domain.ReplicaMetrics{AvgTTFT: 1e6, AvgITL: 1, AvgOutputTokens: 1})).
			To(Equal(MaxResidenceSeconds))
	})

	It("floors the smoothing constant but not the residence itself", func() {
		short := domain.ReplicaMetrics{AvgITL: 0.008, AvgOutputTokens: 16} // W = 0.128 s
		Expect(residenceSeconds(short)).To(BeNumerically("~", 0.128, 1e-9),
			"capacity is mu x W x tokensPerRequest, so a floor here inflates supply "+
				"in one direction while demand has no matching floor")
		Expect(smoothingTau(short)).To(Equal(MinResidenceSeconds),
			"the floor exists to keep the arrival average from becoming a passthrough")
		Expect(smoothingTau(domain.ReplicaMetrics{})).To(BeZero())
	})
})

// siblingPod names the second replica wherever a test needs two of them.
const siblingPod = "pod-b"

var _ = Describe("Rate-anchored k2", func() {
	const (
		kvCapacity     = int64(400_000)
		k1             = int64(320_000)
		queueThreshold = 5.0
		occupancy      = int64(64_000) // 16% of KV — the prefill-heavy regime
	)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	shape := variantShape{avgInput: 1000, avgOutput: 250}

	atLimit := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			TokensInUse:           occupancy,
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	// step is one optimize cycle: cross the boundary that folds what the last cycle
	// observed, then look at the replica. Mirrors Analyze.
	step := func(a *SaturationAnalyzer, rm domain.ReplicaMetrics, at time.Time) (int64, int64, k2Source, bool) {
		a.serviceRates.BeginCycle(at)
		return a.rateAnchoredK2(rm, "m", "", 1, shape, k1, queueThreshold, at)
	}

	// learn drives enough cycles to establish both the service rate and the ceiling.
	learn := func(a *SaturationAnalyzer, rm domain.ReplicaMetrics) {
		for i := 0; i <= MinServiceRateSamples+1; i++ {
			_, _, _, _ = step(a, rm, now.Add(time.Duration(i)*15*time.Second))
		}
	}

	It("declines when the estimator is not enabled", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		_, _, _, ok := a.rateAnchoredK2(atLimit(), "m", "", 1, shape, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("reports the measured occupancy from the cycle after a first overload", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))

		// Nothing is folded until a cycle boundary, so the first overloaded cycle has
		// nothing to answer with and the occupancy chain handles it.
		_, _, _, ok := step(a, atLimit(), now)
		Expect(ok).To(BeFalse())

		k2, _, src, ok := step(a, atLimit(), now.Add(15*time.Second))
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog))
		Expect(k2).To(BeNumerically("~", float64(occupancy), 1),
			"saturation at 16% KV is representable, which the occupancy path cannot do")
	})

	It("gives every replica of the bucket the same learned ceiling", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		hot := atLimit()
		learn(a, hot)

		hotK2, _, hotSrc, ok := step(a, hot, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		Expect(hotSrc).To(Equal(k2SrcRateBacklog), "backlogged: at its limit this cycle")

		// An idle sibling: different pod, no queue, a quarter of the arrival rate.
		// It must report the SAME capacity, because capacity is a property of the
		// bucket. aggregateByVariant takes the median of per-replica capacities, so
		// anything load-dependent here would blend incommensurable numbers and could
		// lift variant capacity while a sibling queues.
		cold := atLimit()
		cold.PodName = siblingPod
		cold.QueueLength = 0
		cold.ArrivalRate = 2.0
		coldK2, _, coldSrc, ok := a.rateAnchoredK2(cold, "m", "", 1, shape, k1, queueThreshold, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		Expect(coldSrc).To(Equal(k2SrcRateAnchored), "not at its limit: carrying the bucket's ceiling")
		Expect(coldK2).To(Equal(hotK2), "the median across replicas must be a no-op")
	})

	It("returns the same value cycle after cycle for unchanged input", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		learn(a, rm)

		// Arrival rate wobbles around the service rate; capacity must not.
		seen := make([]int64, 0, 5)
		for i, lambda := range []float64{8.0, 7.2, 8.6, 7.9, 8.3} {
			rm.ArrivalRate = lambda
			rm.QueueLength = 0
			k2, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, shape, k1, queueThreshold,
				now.Add(time.Duration(i)*15*time.Second))
			Expect(ok).To(BeTrue())
			seen = append(seen, k2)
		}
		// lambda swings +-9% here. The only permitted movement is the ceiling's slow
		// relaxation with age (CeilingRelaxPerWindow over ServiceRateWindow), which
		// over a minute is a fraction of a percent — not a per-cycle response to load.
		for _, v := range seen {
			Expect(v).To(BeNumerically("~", float64(seen[0]), float64(seen[0])*0.01),
				"a capacity that moved with lambda each cycle is an oscillation waiting to happen")
		}
	})

	It("detects the limit from arrivals reaching the service rate, with no queue", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		learn(a, rm)

		// Queue drained but arrivals still at the service rate: the replica is at its
		// limit, and the detector says so with no queue to go on.
		rm.QueueLength = 0
		rm.QueueLengthInstant, rm.HasQueueLengthInstant = 0, true
		rm.TokensInUse = 48_000
		rm.KvUsageInstant = 48_000 / float64(kvCapacity)
		k2, ref, src, ok := step(a, rm, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog), "the arrivals path fired without a queue")

		// Capacity is held at what the replica is holding right now, so demand meets it
		// and the fleet scales before a queue forms. Waiting for the queue would put
		// the ~90 s a replica takes to start on top of a backlog already building.
		Expect(k2).To(BeNumerically("~", 48_000, 1))

		// The learned ceiling itself is untouched: with no queue, a lower occupancy is
		// evidence the replica is keeping up, not evidence its limit has fallen.
		Expect(ref).To(BeNumerically("~", occupancy, float64(occupancy)*0.01))
	})

	It("uses completions as arrivals when there is no EPP and no queue", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		learn(a, rm)

		rm.QueueLength = 0
		rm.ArrivalRate = 0   // no EPP
		rm.RequestRate = 8.0 // completions == arrivals with no queue
		_, _, src, ok := step(a, rm, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		// RATE-now is only reachable here through the completions substitution: with
		// no queue and no EPP there is nothing else that could flag the limit.
		Expect(src).To(Equal(k2SrcRateBacklog))
	})

	It("declines for an idle replica in a bucket that has learned nothing", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.QueueLength = 0
		rm.ArrivalRate = 1.0
		_, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, shape, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("declines for a cold replica with no occupancy", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.TokensInUse = 0 // just became ready, KV still empty
		_, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, shape, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("does not let a shallow queue teach the bucket anything", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.QueueLength = 1 // arrival jitter, not a limit
		for i := 0; i < 5; i++ {
			_, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, shape, k1, queueThreshold, now)
			Expect(ok).To(BeFalse())
		}
	})

	It("ignores a replica that queues without completing anything", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		healthy := atLimit()
		learn(a, healthy)
		before, _, _, ok := step(a, healthy, now.Add(time.Minute))
		Expect(ok).To(BeTrue())

		// A replica with a deep queue, almost nothing resident, and no completions: a
		// pod that has just started and taken a routed burst, or one that has stalled.
		// Either way it is not evidence of what this bucket can hold, and letting it
		// set the ceiling would pin every sibling near the floor for hours.
		cold := atLimit()
		cold.PodName = siblingPod
		cold.TokensInUse = 100
		cold.KvUsageInstant = 0
		cold.RequestRate = 0
		for i := 1; i <= 4; i++ {
			_, _, _, _ = step(a, cold, now.Add(time.Minute+time.Duration(i)*15*time.Second))
		}
		after, _, _, ok := step(a, healthy, now.Add(2*time.Minute))
		Expect(ok).To(BeTrue())
		Expect(after).To(BeNumerically(">=", before), "the cold replica taught the bucket nothing")
	})

	It("needs the same reading twice before it lowers the ceiling", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		learn(a, atLimit())

		lower := atLimit()
		lower.TokensInUse = 20_000 // well under the learned 64k
		first, _, _, ok := step(a, lower, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		Expect(first).To(BeNumerically("~", occupancy, float64(occupancy)*0.01),
			"the first low reading has not been folded yet")

		second, _, _, ok := step(a, lower, now.Add(time.Minute+15*time.Second))
		Expect(ok).To(BeTrue())
		Expect(second).To(BeNumerically("~", occupancy, float64(occupancy)*0.01),
			"one cycle of evidence is not enough to lower it")

		third, _, _, ok := step(a, lower, now.Add(time.Minute+30*time.Second))
		Expect(ok).To(BeTrue())
		Expect(third).To(BeNumerically("~", 20_000, 200), "sustained, so it is adopted")
	})

	It("survives negative and NaN arrival rates", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		learn(a, rm)

		for _, bad := range []float64{-5, math.NaN(), math.Inf(1)} {
			rm.QueueLength = 0
			rm.ArrivalRate = bad
			rm.RequestRate = 0
			k2, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, shape, k1, queueThreshold, now)
			if ok {
				Expect(k2).To(BeNumerically(">", 0))
			}
		}
	})

	It("keeps roles apart", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))

		dec := atLimit()
		dec.RequestRate, dec.ArrivalRate = 20, 20
		for i := 0; i < MinServiceRateSamples+1; i++ {
			_, _, _, _ = a.rateAnchoredK2(dec, "m", domain.RoleDecode, 1, shape, k1, queueThreshold, now)
		}

		// A prefill replica of the same model on the same accelerator must not
		// inherit decode's limit.
		pre := atLimit()
		pre.PodName = "pod-p"
		pre.QueueLength = 0
		pre.ArrivalRate = 3
		_, _, _, ok := a.rateAnchoredK2(pre, "m", domain.RolePrefill, 1, shape, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("never fabricates a limit for a prefill replica reporting no completions", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		// A disaggregated prefill pod can report zero completions even under load.
		rm := atLimit()
		rm.RequestRate = 0
		for i := 0; i < 5; i++ {
			a.serviceRates.BeginCycle(now.Add(time.Duration(i) * 15 * time.Second))
			_, _, _, ok := a.rateAnchoredK2(rm, "m", domain.RolePrefill, 1, shape, k1, queueThreshold,
				now.Add(time.Duration(i)*15*time.Second))
			// No completions means no service rate and no ceiling: there is nothing to
			// measure a limit from, so the estimator declines rather than inventing one
			// from a queue depth alone.
			Expect(ok).To(BeFalse())
		}
	})
})

var _ = Describe("Bucket store — bounded growth", func() {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	It("prunes stale buckets when a new one is inserted", func() {
		s := newBucketStore()
		// Fill past the prune threshold with buckets nobody has touched in a day.
		old := now.Add(-2 * HistoryEvictionTimeout)
		for i := 0; i < BucketPruneThreshold; i++ {
			ceilingCycle(s, fmt.Sprintf("stale-%d", i), []float64{1000}, old)
		}
		Expect(s.entries).To(HaveLen(BucketPruneThreshold))

		ceilingCycle(s, "fresh", []float64{2000}, now)
		Expect(s.entries).To(HaveLen(1), "the stale buckets went with the insert")
		_, ok := s.Ceiling("fresh", now)
		Expect(ok).To(BeTrue())
	})

	It("keeps buckets that are still in use", func() {
		s := newBucketStore()
		for i := 0; i < BucketPruneThreshold; i++ {
			s.ObserveCeiling(fmt.Sprintf("live-%d", i), 1000, now)
		}
		s.ObserveCeiling("one-more", 1000, now)
		Expect(s.entries).To(HaveLen(BucketPruneThreshold + 1))
	})

	It("prunes per-pod arrival entries the same way", func() {
		s := newArrivalSmoother()
		old := now.Add(-2 * HistoryEvictionTimeout)
		for i := 0; i < BucketPruneThreshold; i++ {
			_ = s.Smooth(fmt.Sprintf("gone-%d", i), 4, 60, old)
		}
		_ = s.Smooth("current", 4, 60, now)
		Expect(s.entries).To(HaveLen(1))
	})
})

var _ = Describe("Rate-anchored k2 at the current operating point", func() {
	const (
		kvCapacity     = int64(400_000)
		k1             = int64(320_000)
		queueThreshold = 5.0
		interval       = 15 * time.Second
	)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	shape := variantShape{avgInput: 1000, avgOutput: 250}

	// The numbers are Little's-law consistent, or the arithmetic below would be a
	// coincidence rather than the property under test: a replica serving mu = 8 req/s
	// with residence W = 6 s at 1250 tokens per request holds 8 x 6 x 1250 = 60_000
	// tokens. That is the occupancy at the limit, hence the ceiling.
	atLimit := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			TokensInUse:           60_000,
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
			AvgTTFT:               1.0,
			AvgITL:                0.02, // W = 1.0 + 250 x 0.02 = 6 s
		}
	}

	// Same arrivals, no queue, half the residence: what a replica looks like once
	// siblings have absorbed the backlog. Occupancy follows W down (8 x 3 x 1250).
	drained := func() domain.ReplicaMetrics {
		rm := atLimit()
		rm.QueueLength = 0
		rm.TokensInUse = 30_000
		rm.AvgTTFT = 0.5
		rm.AvgITL = 0.01 // W = 3 s
		return rm
	}

	// cycle mirrors production: freeze the bucket's operating point, then compute.
	cycle := func(a *SaturationAnalyzer, rm domain.ReplicaMetrics, at time.Time) (int64, int64, k2Source, bool) {
		a.serviceRates.BeginCycle(at)
		return a.rateAnchoredK2(rm, "m", "", 1, shape, k1, queueThreshold, at)
	}

	run := func(a *SaturationAnalyzer, rm domain.ReplicaMetrics, from time.Time, n int) (int64, k2Source) {
		var k2 int64
		var src k2Source
		for i := 0; i < n; i++ {
			k2, _, src, _ = cycle(a, rm, from.Add(time.Duration(i)*interval))
		}
		return k2, src
	}

	It("does not move the capacity it just measured", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		k2, src := run(a, atLimit(), start, 3)

		// mu x W x tokensPerRequest equals the occupancy that set the ceiling, so
		// engaging the scaling changes nothing at the point it was calibrated —
		// whichever of the two the clamp happens to pick.
		Expect(k2).To(BeNumerically("~", 60_000, 600))
		Expect(src).To(BeElementOf(k2SrcRateBacklog, k2SrcRateResidence))
	})

	It("holds utilization flat when contention falls away", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		hotK2, _ := run(a, atLimit(), start, 3)
		hot := atLimit()
		hotUtilization := float64(hot.TokensInUse) / float64(hotK2)

		// The operating point is smoothed over WorkSmoothingWindow, deliberately the
		// same order as the one-minute window demand is collected over, so it takes
		// several cycles rather than one to follow the drain down.
		coldK2, coldSrc := run(a, drained(), start.Add(3*interval), 40)
		cold := drained()
		coldUtilization := float64(cold.TokensInUse) / float64(coldK2)

		Expect(coldSrc).To(Equal(k2SrcRateResidence))
		Expect(coldK2).To(BeNumerically("<", hotK2), "capacity follows the operating point down")
		// This is the whole fix: demand fell by half when the backlog cleared, and
		// capacity fell with it, so nothing reads as spare capacity and nothing is
		// shed. Round 1 held capacity flat here and sheds to one replica.
		Expect(coldUtilization).To(BeNumerically("~", hotUtilization, 0.02))
	})

	It("never scales capacity above the limit it measured", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		run(a, atLimit(), start, 3)

		// A deep queue inflates TTFT, so W reads high — that is queueing time, not
		// capacity. Letting it raise the bound would relax it exactly when the
		// replica is failing.
		queued := atLimit()
		queued.QueueLength = 140
		queued.AvgTTFT = 8.0 // W = 13 s, more than double the calibration point
		k2, _, src, ok := cycle(a, queued, start.Add(4*interval))

		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically("<=", int64(60_000)))
		Expect(src).NotTo(Equal(k2SrcRateResidence))
	})

	It("gives every replica of a bucket the same capacity within a cycle", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		run(a, atLimit(), start, 3)
		run(a, drained(), start.Add(3*interval), 2)

		// Siblings report slightly different residences; they must still scale by one
		// number, because aggregateByVariant takes the MEDIAN of per-replica values.
		at := start.Add(5 * interval)
		a.serviceRates.BeginCycle(at)
		first := drained()
		second := drained()
		second.PodName = siblingPod
		second.AvgTTFT = 0.9
		second.AvgITL = 0.03 // a materially longer W than pod-a's

		k2a, _, _, _ := a.rateAnchoredK2(first, "m", "", 1, shape, k1, queueThreshold, at)
		k2b, _, _, _ := a.rateAnchoredK2(second, "m", "", 1, shape, k1, queueThreshold, at)
		Expect(k2b).To(Equal(k2a))
	})

	It("holds at the ceiling when there is no residence to scale by", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		// A fleet whose latency metrics are not being collected: the limit is still
		// measurable from backlog and occupancy, but there is no operating point to
		// scale it to, so the estimator answers with the ceiling rather than guessing.
		blind := atLimit()
		blind.AvgTTFT, blind.AvgITL = 0, 0

		var k2 int64
		var src k2Source
		var ok bool
		for i := 0; i < 5; i++ {
			at := start.Add(time.Duration(i) * interval)
			a.serviceRates.BeginCycle(at)
			k2, _, src, ok = a.rateAnchoredK2(blind, "m", "", 1, shape, k1, queueThreshold, at)
		}
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically("~", 60_000, 600))
		Expect(src).NotTo(Equal(k2SrcRateResidence))
	})
})

var _ = Describe("Rate-anchored k2 operating point across siblings", func() {
	const (
		k1             = int64(320_000)
		queueThreshold = 5.0
		interval       = 15 * time.Second
	)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	shape := variantShape{avgInput: 1000, avgOutput: 250}

	atLimit := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			TokensInUse:           60_000,
			TotalKvCapacityTokens: 400_000,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
			AvgTTFT:               1.0,
			AvgITL:                0.02, // W = 6 s, work = 7500 token-seconds
		}
	}

	It("averages the operating point over the cycle's replicas, whatever their order", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		for i := 0; i < MinServiceRateSamples+1; i++ {
			at := start.Add(time.Duration(i) * interval)
			a.serviceRates.BeginCycle(at)
			_, _, _, _ = a.rateAnchoredK2(atLimit(), "m", "", 1, shape, k1, queueThreshold, at)
		}

		// One cycle, two replicas with materially different residences: 3 s and 1.2 s,
		// so 3750 and 1500 token-seconds. Whichever the loop reaches first, the bucket
		// must land on the mean — 2625 — and not on either endpoint.
		slow := atLimit()
		slow.QueueLength = 0
		slow.AvgTTFT, slow.AvgITL = 0.5, 0.01 // W = 3 s
		fast := slow
		fast.PodName = siblingPod
		fast.AvgTTFT, fast.AvgITL = 0.2, 0.004 // W = 1.2 s

		var k2 int64
		var src k2Source
		var ok bool
		for i := 3; i < 43; i++ { // long enough for the smoothed value to settle
			at := start.Add(time.Duration(i) * interval)
			a.serviceRates.BeginCycle(at)
			k2, _, src, ok = a.rateAnchoredK2(slow, "m", "", 1, shape, k1, queueThreshold, at)
			_, _, _, _ = a.rateAnchoredK2(fast, "m", "", 1, shape, k1, queueThreshold, at)
		}

		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateResidence))
		// mu x mean work = 8 x 2625. Taking the first replica alone would give 30_000
		// and the second alone 12_000 — and which one it was would depend on the order
		// the loop reached them.
		Expect(k2).To(BeNumerically("~", 21_000, 420)) // within 2%
	})
})

var _ = Describe("Rate-anchored k2 ceiling measurement", func() {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	shape := variantShape{avgInput: 1000, avgOutput: 250}

	It("measures the limit from the instantaneous reading, not the minute's peak", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12, // max_over_time: the last minute's peak
			QueueLengthInstant:    12, // and still queueing right now
			HasQueueLengthInstant: true,
			RequestRate:           8.0,
			TokensInUse:           120_000, // max_over_time: the last minute's peak
			KvUsageInstant:        0.15,    // 60k: where it actually is now
			TotalKvCapacityTokens: 400_000,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
		for i := 0; i <= MinServiceRateSamples+1; i++ {
			at := now.Add(time.Duration(i) * 15 * time.Second)
			a.serviceRates.BeginCycle(at)
			_, _, _, _ = a.rateAnchoredK2(rm, "m", "", 1, shape, 320_000, 5.0, at)
		}

		// The ceiling is a running minimum, so feeding it a peak biases it high in the
		// one direction that costs replicas.
		a.serviceRates.BeginCycle(now.Add(time.Minute))
		k2, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, shape, 320_000, 5.0, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically("~", 60_000, 600))
	})

	It("falls back to the averaged reading when no instantaneous one is collected", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			TokensInUse:           120_000,
			TotalKvCapacityTokens: 400_000,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
		for i := 0; i <= MinServiceRateSamples+1; i++ {
			at := now.Add(time.Duration(i) * 15 * time.Second)
			a.serviceRates.BeginCycle(at)
			_, _, _, _ = a.rateAnchoredK2(rm, "m", "", 1, shape, 320_000, 5.0, at)
		}
		a.serviceRates.BeginCycle(now.Add(time.Minute))
		k2, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, shape, 320_000, 5.0, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically("~", 120_000, 1200))
	})
})

var _ = Describe("Rate-anchored k2 responsiveness", func() {
	const (
		k1             = int64(320_000)
		queueThreshold = 5.0
		interval       = 15 * time.Second
	)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	shape := variantShape{avgInput: 1000, avgOutput: 250}

	base := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			TokensInUse:           60_000,
			KvUsageInstant:        0.15,
			TotalKvCapacityTokens: 400_000,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
			AvgTTFT:               1.0,
			AvgITL:                0.02, // W = 6 s
		}
	}

	// A replica takes about 90 seconds from scale-up decision to serving, so a
	// capacity estimate that lagged a rising load would put that delay on top of a
	// queue that is already building. Nothing here may hold a scale-up back.
	It("reports saturation on the first cycle of a rising load", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		for i := 0; i < 5; i++ {
			at := start.Add(time.Duration(i) * interval)
			a.serviceRates.BeginCycle(at)
			_, _, _, _ = a.rateAnchoredK2(base(), "m", "", 1, shape, k1, queueThreshold, at)
		}

		// Load steps up: more queued, more resident, and a longer residence because
		// requests are now waiting. Capacity must not follow the residence upward.
		ramp := base()
		ramp.QueueLength = 60
		ramp.TokensInUse = 90_000
		ramp.KvUsageInstant = 0.225
		ramp.AvgTTFT = 9.0 // W = 14 s, more than double

		at := start.Add(5 * interval)
		a.serviceRates.BeginCycle(at)
		k2, _, _, ok := a.rateAnchoredK2(ramp, "m", "", 1, shape, k1, queueThreshold, at)

		Expect(ok).To(BeTrue())
		// Clamped at the measured ceiling: an inflated residence is queueing time, and
		// letting it raise capacity would mask the very overload that produced it.
		Expect(k2).To(BeNumerically("<=", int64(60_000)))
		Expect(k2).To(BeNumerically("<", ramp.TokensInUse),
			"demand already exceeds capacity, on the first cycle, with no window to wait out")
	})

	It("holds the operating point for the window, then steps down with demand", func() {
		s := newBucketStore()
		at := start
		s.ObserveWork("k", 7500, at)
		s.BeginCycle(at)
		peak, _ := s.FrozenWork("k", at)
		Expect(peak).To(BeNumerically("~", 7500, 1))

		// Contention falls away. Demand is max_over_time(...[1m]) and still carries
		// its peak, so the operating point must carry its own for the same minute —
		// decaying here instead would drop capacity while demand stayed high, and the
		// ratio would read as spare capacity.
		for i := 1; i <= 3; i++ {
			at = start.Add(time.Duration(i) * interval)
			s.ObserveWork("k", 3750, at)
			s.BeginCycle(at)
			held, ok := s.FrozenWork("k", at)
			Expect(ok).To(BeTrue())
			Expect(held).To(BeNumerically("~", 7500, 1), "still inside the window")
		}

		at = start.Add(WorkWindow + interval)
		s.ObserveWork("k", 3750, at)
		s.BeginCycle(at)
		stepped, ok := s.FrozenWork("k", at)
		Expect(ok).To(BeTrue())
		Expect(stepped).To(BeNumerically("~", 3750, 1), "the peak has aged out, as demand's has")
	})

	It("keeps only a window's worth of samples", func() {
		s := newBucketStore()
		for i := 0; i < 200; i++ {
			at := start.Add(time.Duration(i) * interval)
			s.ObserveWork("k", 5000, at)
			s.BeginCycle(at)
		}
		Expect(len(s.entries["k"].workSamples)).To(BeNumerically("<=",
			int(WorkWindow/interval)+1), "bounded by the window, not by the run length")
	})
})

// The validation runs showed RATE-W — the only label where capacity is scaled below
// the learned ceiling — firing once in thirty-five minutes on prefill-heavy traffic
// against twenty-seven times on symmetric traffic. The cause is that TTFT carries
// queue wait, so under backlog the residence it implies is several times the real
// service time and the clamp holds capacity at the ceiling for the whole run.
var _ = Describe("Service residence under a deep queue", func() {
	const (
		k1             = int64(320_000)
		queueThreshold = 5.0
		interval       = 15 * time.Second
	)
	start := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	shape := variantShape{avgInput: 1000, avgOutput: 250}

	// Uncontended: nothing queued, so TTFT is prefill and nothing else.
	quiet := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           0,
			QueueLengthInstant:    0,
			HasQueueLengthInstant: true,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			TokensInUse:           60_000,
			KvUsageInstant:        0.15,
			TotalKvCapacityTokens: 400_000,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
			AvgTTFT:               0.10, // prefill only
			AvgITL:                0.02, // service residence = 0.1 + 5 = 5.1 s
		}
	}

	It("uses prefill learned while idle instead of a queue-inflated TTFT", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		at := start

		// Calibrate: a few quiet cycles teach the bucket what prefill costs, then a
		// backlogged stretch teaches it the service rate and the ceiling.
		for i := 0; i < 6; i++ {
			a.serviceRates.BeginCycle(at)
			_, _, _, _ = a.rateAnchoredK2(quiet(), "m", "", 1, shape, k1, queueThreshold, at)
			at = at.Add(interval)
		}
		hot := quiet()
		hot.QueueLength, hot.QueueLengthInstant = 140, 140
		hot.AvgTTFT = 8.0 // queue wait dominates: TTFT is now 80x prefill
		hot.TokensInUse = 300_000
		hot.KvUsageInstant = 0.75
		for i := 0; i < 4; i++ {
			a.serviceRates.BeginCycle(at)
			_, _, _, _ = a.rateAnchoredK2(hot, "m", "", 1, shape, k1, queueThreshold, at)
			at = at.Add(interval)
		}

		a.serviceRates.BeginCycle(at)
		k2, _, src, ok := a.rateAnchoredK2(hot, "m", "", 1, shape, k1, queueThreshold, at)
		Expect(ok).To(BeTrue())

		// mu x W_service x tokensPerRequest = 8 x 5.1 x 1250 = 51k, well under the
		// ceiling, so the scaling engages and capacity reflects what the replica can
		// actually serve. Read from TTFT the residence would be 13 s, the product
		// 130k, and the clamp would have returned the ceiling instead.
		Expect(src).To(Equal(k2SrcRateResidence))
		Expect(k2).To(BeNumerically("~", 51_000, 5_000))
		Expect(k2).To(BeNumerically("<", 130_000), "not the queue-inflated figure")
	})

	It("falls back to the plain residence until an idle cycle has been seen", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		hot := quiet()
		hot.QueueLength, hot.QueueLengthInstant = 140, 140
		hot.AvgTTFT = 8.0

		at := start
		for i := 0; i < 4; i++ {
			a.serviceRates.BeginCycle(at)
			_, _, _, _ = a.rateAnchoredK2(hot, "m", "", 1, shape, k1, queueThreshold, at)
			at = at.Add(interval)
		}
		_, ok := a.serviceRates.Prefill(serviceRateKey("m", "H100", "", 1, shape), at)
		Expect(ok).To(BeFalse(), "a backlogged TTFT must never be recorded as prefill")

		// And the estimator still answers, from the contaminated residence, exactly as
		// it did before — no worse, and still bounded by the clamp.
		_, _, _, answered := a.rateAnchoredK2(hot, "m", "", 1, shape, k1, queueThreshold, at)
		Expect(answered).To(BeTrue())
	})
})

var _ = Describe("Learned prefill retention", func() {
	now := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	It("outlives the service-rate window", func() {
		s := newBucketStore()
		s.ObservePrefill("k", 0.1, now)
		s.BeginCycle(now)

		// A sustained overload has no unqueued cycles to refresh this, and expiring it
		// would drop the estimator back to the queue-contaminated residence exactly
		// when the queue is deep.
		_, ok := s.Prefill("k", now.Add(ServiceRateWindow+time.Minute))
		Expect(ok).To(BeTrue())

		_, ok = s.Prefill("k", now.Add(HistoryEvictionTimeout+time.Minute))
		Expect(ok).To(BeFalse(), "but not past the bucket's own lifetime")
	})
})

// The scale-down floor rests on arrivals being invariant to the replica count. The
// measurement is not: ArrivalRate is a one-minute rate per pod, summed across pods, so
// removing a pod drops its share from the sum at once while the survivors take a
// minute to report their larger share. Left alone, each shed weakens the floor enough
// to permit the next one — the cascade to a single replica, produced by the guard that
// exists to prevent it.
var _ = Describe("Arrival peak window", func() {
	now := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)

	It("holds the sum through the dip a scale-down produces", func() {
		p := newPeakWindow()
		Expect(p.Observe("v1", 24, now)).To(BeNumerically("~", 24, 0.01))

		// A replica goes. Four of five pods still report their old per-pod share, so
		// the sum reads a fifth low even though arrivals have not changed at all.
		Expect(p.Observe("v1", 19.2, now.Add(15*time.Second))).To(BeNumerically("~", 24, 0.01))
		Expect(p.Observe("v1", 19.2, now.Add(30*time.Second))).To(BeNumerically("~", 24, 0.01))

		// And once the survivors catch up, the true figure is back on its own.
		Expect(p.Observe("v1", 24, now.Add(75*time.Second))).To(BeNumerically("~", 24, 0.01))
	})

	It("lets a genuine drop through once the window has passed", func() {
		p := newPeakWindow()
		_ = p.Observe("v1", 24, now)
		Expect(p.Observe("v1", 6, now.Add(ArrivalPeakWindow+time.Second))).
			To(BeNumerically("~", 6, 0.01), "real load changes must not be held forever")
	})

	It("keeps its map bounded", func() {
		p := newPeakWindow()
		for i := 0; i < 200; i++ {
			p.Observe(fmt.Sprintf("stale-%d", i), 10, now)
		}
		p.Observe("fresh", 10, now.Add(2*ArrivalPeakWindow))
		Expect(len(p.entries)).To(BeNumerically("<=", BucketPruneThreshold+1))
	})
})
