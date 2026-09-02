package saturation_v2

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Rate-anchored compute capacity.
//
// The occupancy-based estimator records `tokensInUse` at the moment a replica was
// seen queueing and calls that the compute bound. That is a KV *stock* standing in
// for a *rate* limit, and on a prefill-heavy workload the two are unrelated: the
// engine exhausts prompt-token throughput and queues while KV occupancy is still
// low, so the estimate carries no information about the binding constraint.
//
// This estimator splits the problem in two:
//
//	detector:    rates decide WHEN a replica is at its limit
//	measurement: tokens record WHAT that limit is
//
// A replica is at its limit when it has a sustained backlog, or when its arrival
// rate has reached the service rate observed while it was backlogged. At that
// moment its resident token count is a measurement of the limit, and it is stored
// per workload bucket — model, accelerator, role, request shape — because it is a
// property of that bucket, not of the individual replica or of the current cycle.
//
// Keeping the measurement out of the per-cycle path is what makes the estimate
// usable downstream, for two reasons that both bit earlier versions of this code:
//
//   - aggregateByVariant takes the MEDIAN of per-replica capacities. A number that
//     varied per replica with that replica's own load was not commensurable across
//     siblings: an idle replica's figure blended with a backlogged one's and lifted
//     variant capacity enough to turn a scale-up into a scale-down. A bucket
//     ceiling is identical for every replica of the variant, so the median is a
//     no-op and cannot mix kinds.
//   - a value recomputed from this cycle's lambda moved every cycle, which is a
//     scaling oscillation waiting to happen. A stored ceiling changes only as the
//     running minimum is lowered by a new observation or relaxed by decay, both of
//     which are slow by construction.
//
// See docs/plans/engine/rate-anchored-k2.md.

// EnableRateAnchoredK2 selects the rate-anchored estimator. It is a build-time
// constant rather than a ConfigMap setting: the estimator is under evaluation
// against the occupancy-based one and is not something an operator should be
// switching in a running cluster. Flip it here to run the comparison, and see
// docs/plans/engine/rate-anchored-k2.md for the validation plan.
//
// With this false, SaturationAnalyzer.serviceRates stays nil and every path in
// this file returns immediately, leaving the occupancy-based estimator unchanged.
const EnableRateAnchoredK2 = false

const (
	// ServiceRateWindow is how long an observation stays authoritative for its
	// bucket. A replica only calibrates while it is at its limit, which may be
	// rare, so this is generous relative to the optimize interval.
	ServiceRateWindow = 30 * time.Minute

	// ServiceRateSmoothingWindow is the time constant over which mu tracks the
	// completion rates observed under backlog. While a replica is backlogged it is
	// never idle, so every such sample IS its service rate — there is no reason to
	// prefer the largest, and a running maximum could only ratchet upward. It has to
	// move down as readily as up: when prompts get longer within a bucket the true
	// mu falls, and an estimate that lags overstates capacity and under-scales.
	//
	// Five minutes is responsive within a load stage without chasing a single scrape.
	ServiceRateSmoothingWindow = 5 * time.Minute

	// PrefillSmoothingWindow is the time constant over which a bucket learns how long
	// prefill takes when nothing is waiting. Long, because the observations are sparse
	// by construction: only unqueued cycles qualify.
	PrefillSmoothingWindow = 10 * time.Minute

	// ArrivalPeakWindow is how long a variant's summed arrival rate is held at its
	// maximum. Arrivals do not change when replicas do — that invariance is the whole
	// basis of the scale-down floor — but the *measurement* of them does: ArrivalRate
	// is a one-minute rate per pod, summed across pods. Remove a pod and its share
	// leaves the sum at once, while the survivors take a minute to report their larger
	// share. The sum therefore dips by roughly 1/N after every shed, which weakens the
	// floor, which permits the next shed. Two minutes outlasts both the rate window
	// and the interval between drains.
	ArrivalPeakWindow = 2 * time.Minute

	// WorkWindow is the window the bucket's operating point is held over, and it
	// mirrors the demand side exactly: TokensInUse and QueueLength are collected as
	// max_over_time(...[1m]), so the operating point is the MAXIMUM of what replicas
	// reported over the same minute.
	//
	// Matching the operator, not just the timescale, is what keeps the ratio stable
	// through a transient. An average would decay smoothly while demand held its peak
	// and then stepped down, so at the moment demand's window rolled off, capacity
	// would still be high and the fleet would shed — the exact failure this estimator
	// exists to prevent. Reacting within a single cycle instead would drop capacity
	// while demand still carried the peak, spiking utilization into a scale-up right
	// after the fleet caught up. Both sides rise and fall together.
	WorkWindow = time.Minute

	// CeilingRelaxPerWindow is the factor the learned token ceiling relaxes by over
	// one ServiceRateWindow with no fresh observation. The ceiling is a running
	// minimum, so it relaxes *upward*: a single pessimistic measurement — taken
	// while a node was degraded, say — must not cap the variant forever. Capacity
	// drifts back toward the memory bound in the absence of evidence.
	CeilingRelaxPerWindow = 1.25

	// MinServiceRateSamples is how many qualifying CYCLES a bucket needs before its
	// service rate is usable. One cycle cannot distinguish a real limit from a single
	// slow interval. Counting cycles rather than observations matters: every replica
	// of a bucket reports in the same cycle, so counting observations would let two
	// replicas satisfy this in one interval and defeat the guard.
	MinServiceRateSamples = 2

	// MinCeilingLowerCycles is how many consecutive cycles must agree before the
	// learned ceiling is lowered. Lowering is the dangerous direction: the ceiling
	// sets the whole loop gain, so a single unrepresentative reading — a replica that
	// has just started and holds almost nothing, or one degraded pod among healthy
	// siblings — would otherwise pin the entire bucket near the floor and take hours
	// to relax back. Raising it needs no such delay.
	MinCeilingLowerCycles = 2

	// SaturationEnterRatio is the fraction of the service rate at which arrivals
	// are treated as having reached it. Slightly below 1 so the limit is recognised
	// just before it is crossed, and so a lambda hovering at mu does not toggle the
	// detector between cycles.
	SaturationEnterRatio = 0.95

	// MinResidenceSeconds and MaxResidenceSeconds bound the residence estimate used
	// as the arrival-smoothing time constant, so a garbage latency reading cannot
	// turn the average into either a passthrough or a frozen value.
	MinResidenceSeconds = 1.0
	MaxResidenceSeconds = 300.0

	// ArrivalSmoothingResetFactor is how many time constants may pass before the
	// previous arrival average is discarded rather than blended.
	ArrivalSmoothingResetFactor = 3.0

	// BucketPruneThreshold is the map size at which adding a new bucket first tries
	// pruning stale ones. Small enough that the map stays bounded, large enough that
	// a normal fleet never pays for a sweep.
	BucketPruneThreshold = 32

	// MinRateAnchoredFraction floors the learned ceiling at a fraction of k1. A
	// replica that stalled completely while requests queued would otherwise teach
	// the bucket a near-zero capacity and demand an unbounded scale-up.
	MinRateAnchoredFraction = 0.05
)

// bucketStore holds what has been learned about a workload bucket: the service
// rate observed while a replica could not keep up (mu, requests/second), and the
// resident token count measured at that moment (the compute-bound ceiling).
//
// Both are per bucket rather than per replica. Replicas of a variant run the same
// model on the same hardware, so a limit measured on one applies to all — which is
// what makes the value safe to put through aggregateByVariant's median.
type bucketStore struct {
	mu      sync.Mutex
	entries map[string]*bucketEntry
}

// bucketEntry holds one workload bucket's learned state. Every quantity is folded
// once per cycle by BeginCycle rather than per observation: replicas of a bucket all
// report within the same cycle at the same timestamp, so folding on arrival gives the
// first replica the entire weight and makes the result depend on the order the loop
// happened to reach them — which, since ReplicaMetrics is built by ranging a map, is
// not even stable between cycles.
type bucketEntry struct {
	// touched is the last time any replica reported into this bucket. Observations
	// accumulate and are only folded at the cycle boundary, so the folded timestamps
	// lag by a cycle and cannot be used to decide staleness — a bucket observed but
	// not yet folded would look infinitely old and be evicted with its samples.
	touched time.Time

	rate        float64 // mu: mean completion rate under backlog
	rateSamples int     // qualifying cycles, not observations
	rateSeen    time.Time
	rateSum     float64 // this cycle's samples, averaged at the next cycle boundary
	rateCount   int

	ceiling       float64 // resident tokens at the limit; lowered only on sustained evidence
	ceilingSeen   time.Time
	ceilingKnown  bool
	ceilingSum    float64 // this cycle's lowest qualifying observation
	ceilingHas    bool
	pendingLower  float64 // a lower reading waiting to be confirmed by another cycle
	pendingCycles int

	prefill      float64 // time to first token with nothing queued, in seconds
	prefillKnown bool
	prefillSeen  time.Time
	prefillSum   float64
	prefillCount int

	workSeen    time.Time
	workSamples []workSample // one per cycle, held for WorkWindow
	workFrozen  float64      // the value every replica of the bucket reads this cycle
	workSum     float64      // this cycle's samples, averaged at the next cycle boundary
	workCount   int
}

// workSample is one cycle's operating point: residence x tokens-per-request,
// averaged across the replicas that reported it.
type workSample struct {
	at    time.Time
	value float64
}

func newBucketStore() *bucketStore {
	return &bucketStore{entries: make(map[string]*bucketEntry)}
}

// entry returns the bucket's entry, creating it when absent, and opportunistically
// prunes buckets nothing has touched for HistoryEvictionTimeout. Callers hold s.mu.
//
// Pruning on write rather than from a periodic caller is deliberate: nothing in the
// engine currently drives eviction for the analyzer's other stores either, so a
// store that depended on being swept would grow without bound the moment the
// estimator was switched on. Buckets are keyed by model, accelerator, role, GPU
// count and request shape, so the map is small and a sweep is cheap.
func (s *bucketStore) entry(key string, now time.Time) *bucketEntry {
	e, ok := s.entries[key]
	if !ok {
		if len(s.entries) >= BucketPruneThreshold {
			s.pruneLocked(HistoryEvictionTimeout, now)
		}
		e = &bucketEntry{}
		s.entries[key] = e
	}
	e.touched = now
	return e
}

// pruneLocked drops buckets with no observation of either kind within timeout.
// Callers hold s.mu.
func (s *bucketStore) pruneLocked(timeout time.Duration, now time.Time) int {
	removed := 0
	for k, e := range s.entries {
		if now.Sub(e.touched) > timeout {
			delete(s.entries, k)
			removed++
		}
	}
	return removed
}

// ObserveRate folds a completion rate into the bucket's service-rate estimate.
// Callers must only pass rates measured while the replica had a backlog: with no
// backlog, completions equal arrivals at any load and say nothing about the limit.
//
// Symmetric by construction. The ceiling is a running minimum, so it errs toward
// less capacity and more replicas; a running maximum here would err the opposite
// way, toward more capacity and fewer replicas, and only decay slowly back. Under
// backlog every sample is a valid reading of the service rate, so the mean of them
// is both the better estimate and the one that moves in either direction.
func (s *bucketStore) ObserveRate(key string, rate float64, now time.Time) {
	if !(rate > 0) || math.IsInf(rate, 0) { // NaN fails the > comparison
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.entry(key, now)
	e.rateSum += rate
	e.rateCount++
}

// Rate returns the bucket's service-rate estimate, decayed by age, and false when
// it has too few observations or has gone stale.
func (s *bucketStore) Rate(key string, now time.Time) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok || e.rateSamples < MinServiceRateSamples {
		return 0, false
	}
	if now.Sub(e.rateSeen) > ServiceRateWindow {
		return 0, false
	}
	if e.rate <= 0 {
		return 0, false
	}
	return e.rate, true
}

// ObserveCeiling records the resident token count measured while a replica was at
// its limit. Within a cycle the lowest such reading wins; whether it becomes the
// bucket's ceiling is decided at the cycle boundary, in applyCeiling.
func (s *bucketStore) ObserveCeiling(key string, tokens float64, now time.Time) {
	if !(tokens > 0) || math.IsInf(tokens, 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.entry(key, now)
	if !e.ceilingHas || tokens < e.ceilingSum {
		e.ceilingSum, e.ceilingHas = tokens, true
	}
}

// applyCeiling folds one cycle's observation into the learned ceiling. Callers hold
// s.mu.
//
// Raising is immediate: an old pessimistic reading must give way to fresh evidence
// rather than capping the bucket indefinitely. Lowering waits for MinCeilingLowerCycles
// consecutive cycles to agree, and then adopts the least extreme of them, because a
// ceiling that fell on one reading would hand a single cold or degraded replica the
// power to set capacity for every sibling and hold it there for hours.
func (e *bucketEntry) applyCeiling(tokens float64, now time.Time) {
	if !e.ceilingKnown {
		e.ceiling, e.ceilingKnown, e.ceilingSeen = tokens, true, now
		return
	}
	// Compared against the ceiling itself, not against its relaxed value: an
	// unchanged reading is infinitesimally below the relaxed figure every cycle, and
	// treating that as a lowering would keep a pending candidate permanently open.
	if tokens >= e.ceiling {
		relaxed := relaxCeiling(e.ceiling, now.Sub(e.ceilingSeen))
		e.ceiling, e.ceilingSeen = math.Min(tokens, relaxed), now
		e.pendingLower, e.pendingCycles = 0, 0
		return
	}
	e.pendingCycles++
	if e.pendingCycles == 1 || tokens > e.pendingLower {
		e.pendingLower = tokens
	}
	if e.pendingCycles >= MinCeilingLowerCycles {
		e.ceiling, e.ceilingSeen = e.pendingLower, now
		e.pendingLower, e.pendingCycles = 0, 0
	}
}

// Ceiling returns the bucket's learned token ceiling, relaxed by age, and false
// when nothing has been measured or the measurement has gone stale.
func (s *bucketStore) Ceiling(key string, now time.Time) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok || !e.ceilingKnown {
		return 0, false
	}
	if now.Sub(e.ceilingSeen) > ServiceRateWindow {
		return 0, false
	}
	c := relaxCeiling(e.ceiling, now.Sub(e.ceilingSeen))
	if c <= 0 {
		return 0, false
	}
	return c, true
}

// ObserveWork records this replica's work-per-request — residence time multiplied
// by tokens per request, in token-seconds — as one sample of the bucket's operating
// point, to be averaged and folded in at the next freeze.
//
// Samples accumulate rather than folding in one at a time, because every replica of
// a bucket reports in the same cycle at the same timestamp. Folding each in turn
// would give the first replica of the loop the entire weight — the rest arrive with
// a zero time delta and change nothing — making the bucket's operating point depend
// on iteration order. A replica that had just started, with a short residence, could
// then pull the whole bucket's capacity down and drive a spurious scale-up.
//
// The value is deliberately not read back here: see FrozenWork.
func (s *bucketStore) ObserveWork(key string, work float64, now time.Time) {
	if work <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.entry(key, now)
	e.workSum += work
	e.workCount++
}

// ObservePrefill records a time-to-first-token measured while nothing was waiting.
//
// Callers must only pass readings taken with an empty queue. That is the whole point:
// TTFT is measured from arrival at the engine, so it carries queue wait, and under
// backlog it is inflated several-fold. Sampled with no queue it is prefill alone.
func (s *bucketStore) ObservePrefill(key string, ttft float64, now time.Time) {
	if !(ttft > 0) || math.IsInf(ttft, 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.entry(key, now)
	e.prefillSum += ttft
	e.prefillCount++
}

// Prefill returns the bucket's uncontended prefill time, and false when none has been
// observed or it has gone stale.
func (s *bucketStore) Prefill(key string, now time.Time) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok || !e.prefillKnown || e.prefill <= 0 {
		return 0, false
	}
	// Retained for the bucket's lifetime rather than the service-rate window. Prefill
	// time is a property of the model, the hardware and the prompt length, all of
	// which are in the bucket key, so it does not drift the way a service rate does.
	// And it can only be observed while nothing is queued — precisely the cycles a
	// busy fleet does not have. Expiring it after thirty minutes would mean a
	// sustained overload silently reverting to the queue-contaminated residence at
	// the worst possible moment.
	if now.Sub(e.prefillSeen) > HistoryEvictionTimeout {
		return 0, false
	}
	return e.prefill, true
}

// FrozenWork returns the work-per-request every replica of the bucket scales by
// this cycle, and false when none has been frozen yet.
func (s *bucketStore) FrozenWork(key string, now time.Time) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok || e.workFrozen <= 0 {
		return 0, false
	}
	if now.Sub(e.workSeen) > ServiceRateWindow {
		return 0, false
	}
	return e.workFrozen, true
}

// BeginCycle folds everything each bucket collected last cycle and publishes the
// operating point this cycle's replicas will read. It must be called once at the top
// of a cycle, before any replica is looked at.
//
// The service rate takes the cycle's mean, the ceiling its lowest qualifying reading,
// and the operating point the maximum of the per-cycle means still inside WorkWindow
// — the same operator the demand side is collected with, so both hold their peak and
// step down together rather than one decaying past the other.
//
// Publishing at the cycle boundary is also what makes the operating point identical
// across siblings: aggregateByVariant takes the MEDIAN of per-replica capacities, so a
// value that moved as the loop progressed would blend incommensurable figures.
//
// Publishing at the cycle boundary is what makes the number identical across
// siblings: aggregateByVariant takes the MEDIAN of per-replica capacities, so a
// value that moved as the loop progressed would blend incommensurable figures. It
// costs one cycle of lag, which is the price of that guarantee. Analyze is never
// concurrent, so a plain sweep is enough.
func (s *bucketStore) BeginCycle(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.entries {
		if e.rateCount > 0 {
			sample := e.rateSum / float64(e.rateCount)
			e.rateSum, e.rateCount = 0, 0
			if e.rateSamples == 0 {
				e.rate = sample
			} else {
				e.rate = ewmaStep(e.rate, sample, now.Sub(e.rateSeen).Seconds(), ServiceRateSmoothingWindow.Seconds())
			}
			e.rateSamples++
			e.rateSeen = now
		}

		if e.ceilingHas {
			e.applyCeiling(e.ceilingSum, now)
			e.ceilingSum, e.ceilingHas = 0, false
		}

		if e.prefillCount > 0 {
			sample := e.prefillSum / float64(e.prefillCount)
			e.prefillSum, e.prefillCount = 0, 0
			if e.prefillKnown {
				e.prefill = ewmaStep(e.prefill, sample, now.Sub(e.prefillSeen).Seconds(),
					PrefillSmoothingWindow.Seconds())
			} else {
				e.prefill, e.prefillKnown = sample, true
			}
			e.prefillSeen = now
		}

		if e.workCount > 0 {
			e.workSamples = append(e.workSamples, workSample{at: now, value: e.workSum / float64(e.workCount)})
			e.workSum, e.workCount = 0, 0
			e.workSeen = now
		}
		e.workSamples = slices.DeleteFunc(e.workSamples, func(w workSample) bool {
			return now.Sub(w.at) > WorkWindow
		})
		// An empty window means no live evidence of the operating point, so the
		// estimator declines and falls back to the ceiling rather than scaling by a
		// figure from before the gap.
		if len(e.workSamples) == 0 {
			e.workFrozen = 0
			continue
		}
		e.workFrozen = slices.MaxFunc(e.workSamples, func(a, b workSample) int {
			return cmp.Compare(a.value, b.value)
		}).value
	}
}

// EvictStale drops buckets with no observation of either kind within timeout,
// returning the number removed. Mirrors EvictStaleHistory so the stores age out
// together.
func (s *bucketStore) EvictStale(timeout time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(timeout, now)
}

// relaxCeiling grows an unrefreshed ceiling so capacity drifts back toward the
// memory bound when no fresh evidence of a limit arrives.
func relaxCeiling(ceiling float64, age time.Duration) float64 {
	if age <= 0 {
		return ceiling
	}
	return ceiling * math.Pow(CeilingRelaxPerWindow, age.Seconds()/ServiceRateWindow.Seconds())
}

// peakWindow holds a keyed value at its maximum over a window.
type peakWindow struct {
	mu      sync.Mutex
	entries map[string][]workSample
}

func newPeakWindow() *peakWindow {
	return &peakWindow{entries: make(map[string][]workSample)}
}

// Observe records a sample and returns the maximum still inside the window.
func (p *peakWindow) Observe(key string, value float64, now time.Time) float64 {
	if !(value > 0) || math.IsInf(value, 0) {
		return value
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	kept := make([]workSample, 0, len(p.entries[key])+1)
	kept = append(kept, p.entries[key]...)
	kept = append(kept, workSample{at: now, value: value})
	kept = slices.DeleteFunc(kept, func(w workSample) bool {
		return now.Sub(w.at) > ArrivalPeakWindow
	})
	if len(p.entries) >= BucketPruneThreshold {
		for k, samples := range p.entries {
			if len(samples) == 0 || now.Sub(samples[len(samples)-1].at) > ArrivalPeakWindow {
				delete(p.entries, k)
			}
		}
	}
	p.entries[key] = kept
	return slices.MaxFunc(kept, func(a, b workSample) int {
		return cmp.Compare(a.value, b.value)
	}).value
}

// arrivalSmoother holds a per-replica exponentially-weighted arrival rate.
//
// A completion happens one residence time after the arrival that caused it, so a
// completion-derived mu and an instantaneous lambda are measured on different time
// bases: during a ramp, completions still reflect the lighter load of W seconds
// ago and the comparison reads as saturation on a replica that is coping.
// Averaging lambda over roughly W puts the two on the same footing.
type arrivalSmoother struct {
	mu      sync.Mutex
	entries map[string]*arrivalEntry
}

type arrivalEntry struct {
	rate     float64
	observed time.Time
}

func newArrivalSmoother() *arrivalSmoother {
	return &arrivalSmoother{entries: make(map[string]*arrivalEntry)}
}

// Smooth folds a new arrival-rate sample into the replica's EWMA and returns the
// smoothed value. tau is the averaging time constant — the residence estimate.
// The weight is derived from the actual gap between samples, so an irregular
// optimize interval or a missed cycle does not distort the average.
func (s *arrivalSmoother) Smooth(key string, rate, tau float64, now time.Time) float64 {
	if rate <= 0 || tau <= 0 {
		return rate
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		// Keyed per pod, so this map grows with pod churn; prune on insert for the
		// same reason bucketStore does.
		if len(s.entries) >= BucketPruneThreshold {
			for k, old := range s.entries {
				if now.Sub(old.observed) > HistoryEvictionTimeout {
					delete(s.entries, k)
				}
			}
		}
		s.entries[key] = &arrivalEntry{rate: rate, observed: now}
		return rate
	}
	if dt := now.Sub(e.observed).Seconds(); dt > 0 {
		e.rate = ewmaStep(e.rate, rate, dt, tau)
		e.observed = now
	}
	return e.rate
}

// ewmaStep folds sample into prev with a weight derived from the actual gap between
// samples, so an irregular optimize interval or a missed cycle does not distort the
// average. A gap longer than a few time constants discards prev outright: it carries
// no information about the present.
func ewmaStep(prev, sample, dt, tau float64) float64 {
	if dt <= 0 || tau <= 0 {
		return prev
	}
	if dt > tau*ArrivalSmoothingResetFactor {
		return sample
	}
	return prev + (1-math.Exp(-dt/tau))*(sample-prev)
}

// EvictStale drops replicas not seen within timeout, returning the number removed.
func (s *arrivalSmoother) EvictStale(timeout time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for k, e := range s.entries {
		if now.Sub(e.observed) > timeout {
			delete(s.entries, k)
			removed++
		}
	}
	return removed
}

// queueIsEmpty reports whether nothing is waiting on this replica right now. The
// instantaneous reading is required: QueueLength is a one-minute peak and latches, so
// it would keep claiming a queue for a minute after the last one cleared.
func queueIsEmpty(rm domain.ReplicaMetrics) bool {
	return rm.HasQueueLengthInstant && rm.QueueLengthInstant == 0
}

// serviceResidence is how long a request occupies the replica while it is being
// served — excluding time spent waiting to be scheduled.
//
// This is the quantity Little's law needs to turn a service rate into a token count.
// The obvious reading, TTFT + outputTokens x ITL, is not it: TTFT is measured from
// arrival at the engine, so under backlog it carries queue wait and inflates several
// fold. Capacity computed from it then exceeds the learned ceiling, the clamp holds it
// there, and on a queueing workload the estimator spends the entire run reporting the
// ceiling — which is measured at near-full KV and lands at the memory bound. That is
// visible in the validation runs as RATE-W firing once in thirty-five minutes on
// prefill-heavy traffic against twenty-seven times on symmetric traffic.
//
// The decode half is already clean: inter-token latency contains no queue wait. Only
// the prefill half is contaminated, and it can be measured directly — during cycles
// with nothing queued, TTFT *is* prefill. So the bucket learns it then and reuses it
// when the queue is deep.
//
// Falls back to the contaminated form when no unqueued cycle has been seen yet, which
// is no worse than the previous behaviour and still bounded by the clamp. vLLM
// publishes request_inference_time_seconds, which measures this directly and would be
// authoritative where available; SGLang has no equivalent, and deriving it works for
// both engines from metrics already collected.
func (a *SaturationAnalyzer) serviceResidence(rm domain.ReplicaMetrics, key string, now time.Time) float64 {
	// Measured directly where the engine publishes it. Preferred over the derived
	// form because the derived prefill is sampled while nothing is queued, and real
	// prefill grows under contention — an error that matters most where prefill is a
	// large share of the residence, which is exactly where this metric is worth having.
	if rm.AvgInferenceTime > 0 && rm.AvgInferenceTime <= MaxResidenceSeconds {
		return rm.AvgInferenceTime
	}
	prefill, ok := a.serviceRates.Prefill(key, now)
	if !ok {
		return residenceSeconds(rm)
	}
	if rm.AvgITL <= 0 || rm.AvgOutputTokens <= 0 {
		return residenceSeconds(rm)
	}
	w := prefill + rm.AvgOutputTokens*rm.AvgITL
	if w <= 0 || math.IsNaN(w) || math.IsInf(w, 0) {
		return residenceSeconds(rm)
	}
	if w > MaxResidenceSeconds {
		return MaxResidenceSeconds
	}
	return w
}

// residenceSeconds estimates how long a request occupies the replica: time to the
// first token plus one inter-token latency per output token. Both inputs are
// already collected per replica. Returns 0 when they are unavailable, which leaves
// the arrival rate unsmoothed rather than smoothing it by a made-up constant.
//
// Only the upper bound is applied here. MinResidenceSeconds exists to keep a garbage
// latency reading from turning the arrival average into a passthrough, and it is
// applied at that call site — see smoothingTau. Applying it to capacity would be a
// one-directional inflation: capacity is mu x W x tokensPerRequest, so a floor that
// only ever raises W only ever raises supply, while demand has no matching floor. A
// workload generating 16 tokens at 8ms each has a true residence near 0.2s, and
// rounding that up to 1s overstates supply more than fourfold — enough that a fleet
// at full utilization reports a quarter of it and never scales up at all.
func residenceSeconds(rm domain.ReplicaMetrics) float64 {
	if rm.AvgITL <= 0 || rm.AvgOutputTokens <= 0 {
		return 0
	}
	w := rm.AvgTTFT + rm.AvgOutputTokens*rm.AvgITL
	if w <= 0 || math.IsNaN(w) || math.IsInf(w, 0) {
		return 0
	}
	if w > MaxResidenceSeconds {
		return MaxResidenceSeconds
	}
	return w
}

// smoothingTau is the residence estimate as an averaging time constant, floored so a
// tiny reading cannot make the arrival average a passthrough. Returns 0 when there is
// no usable residence, which leaves the rate unsmoothed.
func smoothingTau(rm domain.ReplicaMetrics) float64 {
	w := residenceSeconds(rm)
	if w <= 0 {
		return 0
	}
	if w < MinResidenceSeconds {
		return MinResidenceSeconds
	}
	return w
}

// serviceRateKey identifies a workload bucket.
//
// Role is part of the key because a prefill replica and a decode replica of the
// same model on the same accelerator are different services: they do different
// work per request and complete at entirely different rates. Sharing a bucket
// would let one calibrate the other's limit.
//
// The input bucket is part of it too, unlike the k2 history key: the limit is a
// property of the request shape, and 1000-token prompts and 300-token prompts are
// different services on the same hardware. Keying only by output length would
// average them.
func serviceRateKey(modelID, accelerator, role string, gpuCount int, shape variantShape) string {
	return fmt.Sprintf("%s|%s|%s|%d|in:%s|out:%s",
		modelID, accelerator, canonicalRole(role), gpuCount,
		classifyInputLength(shape.avgInput), classifyOutputLength(shape.avgOutput))
}

// variantShape is the request shape of a variant as a whole, averaged over its live
// replicas.
//
// The shape has to come from the variant, not from each replica, even though the
// metrics are per-replica. Replicas of one variant serve the same traffic, so their
// averages differ only by sampling noise — but that noise is enough to put two
// siblings either side of a bucket threshold, and then they learn independent
// ceilings and service rates. aggregateByVariant takes the MEDIAN of per-replica
// capacities, so the two figures get blended even though they measure different
// things, and a variant whose average sits near a threshold flips its whole estimate
// every cycle as replicas drift across it. Keying on the variant makes the boundary a
// property of the workload rather than of individual scrapes.
type variantShape struct {
	avgInput  float64
	avgOutput float64
}

// variantShapes averages the request shape across each variant's replicas.
func variantShapes(metrics []domain.ReplicaMetrics) map[string]variantShape {
	type acc struct {
		in, out float64
		n       float64
	}
	sums := make(map[string]*acc, len(metrics))
	for _, rm := range metrics {
		if rm.AvgInputTokens <= 0 && rm.AvgOutputTokens <= 0 {
			continue
		}
		a, ok := sums[rm.VariantName]
		if !ok {
			a = &acc{}
			sums[rm.VariantName] = a
		}
		a.in += rm.AvgInputTokens
		a.out += rm.AvgOutputTokens
		a.n++
	}
	shapes := make(map[string]variantShape, len(sums))
	for variant, a := range sums {
		if a.n > 0 {
			shapes[variant] = variantShape{avgInput: a.in / a.n, avgOutput: a.out / a.n}
		}
	}
	return shapes
}

// rateAnchoredK2 returns the compute-bound capacity in KV tokens for this
// replica's bucket, the load-independent reference to store, and which signal
// produced the capacity.
//
// The capacity is the bucket's learned ceiling, scaled down by how far this cycle's
// operating point sits below the one the ceiling was measured at — see
// residenceScaledCapacity for why that scaling is the point of the whole design.
// Before anything has been learned, a replica that is over its limit right now
// still reports its own occupancy, so the first overload is not missed.
//
// The reference return is the unscaled ceiling. Capacity moves with contention and
// so must never be persisted: the capacity store feeds variants with no live
// replicas and cross-variant estimation, both of which need a number that means
// "what this replica can do", not "what it is doing now".
//
// Returns false when nothing has been learned and the replica is not currently
// over its limit, in which case the caller falls through to the occupancy-based
// chain.
func (a *SaturationAnalyzer) rateAnchoredK2(
	rm domain.ReplicaMetrics,
	modelID string,
	role string,
	gpuCount int,
	shape variantShape,
	k1 int64,
	queueThreshold float64,
	now time.Time,
) (int64, int64, k2Source, bool) {
	if a.serviceRates == nil {
		return 0, 0, 0, false
	}

	key := serviceRateKey(modelID, rm.AcceleratorName, role, gpuCount, shape)

	// Detector and measurement, read at the same instant — see limitEvidence.
	backlogged, occupancy := limitEvidence(rm, queueThreshold)
	completions := completionRate(rm, role)
	if backlogged && completions > 0 {
		a.serviceRates.ObserveRate(key, completions, now)
	}
	atLimit := backlogged || a.arrivalsReachedServiceRate(rm, key, now)

	// A replica may only define the bucket's ceiling while it is both backlogged and
	// completing work. Backlog alone is not enough: a replica that has just started
	// takes a routed burst before its cache fills, and one that has stalled queues
	// without completing anything. Either would report a tiny occupancy as "the
	// occupancy at which this bucket cannot keep up" and, since the ceiling sets the
	// whole loop gain, pin every sibling near the floor. Requiring completions is the
	// same guard the service rate already applies, for the same reason.
	//
	// Arrivals reaching the service rate is enough to say the replica is at its limit
	// — it is how the limit is caught before a queue forms — but it is not a
	// measurement of one: with no queue, low occupancy is evidence the replica is
	// keeping up, not that its ceiling has fallen.
	if backlogged && rm.RequestRate > 0 && occupancy > 0 {
		a.serviceRates.ObserveCeiling(key, occupancy, now)
	}

	// Every cycle, at the limit or not, contributes to the bucket's work-per-request.
	// It is the operating point, not a measurement of the limit, so it is recorded
	// unconditionally — the whole point is to know how far below the limit we sit.
	// With nothing queued, time to first token is prefill and nothing else. Learning
	// it here is what lets the operating point be computed from service time when the
	// queue is deep and TTFT no longer can be.
	if !backlogged && queueIsEmpty(rm) && rm.AvgTTFT > 0 {
		a.serviceRates.ObservePrefill(key, rm.AvgTTFT, now)
	}
	if residence := a.serviceResidence(rm, key, now); residence > 0 {
		a.serviceRates.ObserveWork(key, residence*tokensPerRequest(rm), now)
	}

	ceiling, ok := a.serviceRates.Ceiling(key, now)
	if !ok {
		return 0, 0, 0, false
	}
	// The three labels say which regime produced the number: at the limit right now,
	// carrying a limit measured earlier, or holding a limit measured earlier scaled
	// to a lighter operating point. That is what the offline replay has to separate.
	src := k2SrcRateAnchored
	if atLimit {
		src = k2SrcRateBacklog
	}

	reference := clampCeiling(ceiling, k1)
	capacity := reference
	if scaled, ok := a.residenceScaledCapacity(key, ceiling, now); ok {
		capacity = clampCeiling(scaled, k1)
		src = k2SrcRateResidence
	}

	// Arrivals have caught up with the service rate while nothing has queued yet. The
	// replica is at its limit, and reporting capacity above what it is already holding
	// would hide that until a queue forms. Waiting for the queue is what this cannot
	// afford: a replica takes about ninety seconds from decision to serving, so the
	// backlog that accumulates while one starts is set by how early the decision was
	// made. Capacity is held at the current occupancy, which reads as fully utilized
	// without claiming the replica is over its limit.
	//
	// The figure used here is the one demand is built from, not the instantaneous
	// sample the ceiling is measured with. Demand is TokensInUse, a one-minute peak;
	// holding capacity at a lower instantaneous reading would make utilization the
	// ratio between the two, which on bursty traffic is several times one and would
	// ask for several times the replicas. Matching it lands utilization at exactly
	// one: saturated, scale up by the threshold's margin, nothing more.
	//
	// One direction only, and floored by clampCeiling, so a mis-scraped arrival rate
	// cannot drive capacity toward zero.
	if demandSide := float64(rm.TokensInUse); atLimit && !backlogged &&
		demandSide > 0 && demandSide < float64(capacity) {
		capacity = clampCeiling(demandSide, k1)
	}
	return capacity, reference, src, true
}

// residenceScaledCapacity expresses the bucket's capacity at this cycle's operating
// point, and reports false when it should be left at the measured ceiling.
//
// A ceiling alone cannot stop the collapse that validation round 1 measured. Demand
// is resident tokens, lambda x W x tokensPerRequest, so it falls when replicas are
// added: contention drops, residence W drops, and the queue term disappears
// outright. Supply held flat against a shrinking demand reads as abundant spare
// capacity, and the fleet sheds the replicas that had just fixed the problem.
//
// By Little's law a replica at its limit holds mu x W x tokensPerRequest tokens, so
// that product IS the capacity in the units the engine speaks, at whatever operating
// point W describes. Scaling supply by it makes demand/supply equal lambda/mu, which
// does not move when replicas are added — only lambda per replica does. At the
// moment of calibration the two agree exactly (lambda = mu there, so the product
// equals the occupancy that set the ceiling), so nothing jumps when this engages.
//
// The result is clamped at the ceiling and never above it: W is derived from TTFT,
// which includes time queued, so a backlogged replica reports an inflated W. Letting
// that raise capacity would relax the bound exactly when the replica is failing. One
// direction only — contention below the calibration point may lower capacity,
// queueing above it may not raise it.
func (a *SaturationAnalyzer) residenceScaledCapacity(key string, ceiling float64, now time.Time) (float64, bool) {
	muRate, ok := a.serviceRates.Rate(key, now)
	if !ok {
		return 0, false
	}
	work, ok := a.serviceRates.FrozenWork(key, now)
	if !ok {
		return 0, false
	}
	scaled := muRate * work
	if scaled <= 0 || math.IsNaN(scaled) || math.IsInf(scaled, 0) {
		return 0, false
	}
	if scaled >= ceiling {
		return 0, false
	}
	return scaled, true
}

// limitEvidence reports whether the replica is failing to keep up, and the resident
// token count to record if it is. Both readings come from the same time base, and
// that pairing is the point of the function.
//
// QueueLength and TokensInUse are collected as max_over_time(...[1m]): the demand
// path wants the peak, since erring high on outstanding work is its safe direction.
// Read one of them against an instantaneous sample of the other and the estimator
// breaks in a specific, severe way — the latched queue keeps the gate open for a full
// minute after a backlog clears, while the instantaneous occupancy has already
// collapsed, so a replica that is now comfortably keeping up gets recorded as its own
// limit. The ceiling then falls to its floor and the fleet scales out against it.
//
// So: instantaneous gate with instantaneous occupancy when both are collected,
// otherwise the one-minute peak of both. Either pair is self-consistent.
func limitEvidence(rm domain.ReplicaMetrics, queueThreshold float64) (bool, float64) {
	if rm.HasQueueLengthInstant && rm.KvUsageInstant > 0 && rm.TotalKvCapacityTokens > 0 {
		tokens := rm.KvUsageInstant * float64(rm.TotalKvCapacityTokens)
		if tokens > 0 && !math.IsNaN(tokens) && !math.IsInf(tokens, 0) {
			return rm.QueueLengthInstant >= queueThreshold, tokens
		}
	}
	return float64(rm.QueueLength) >= queueThreshold, float64(rm.TokensInUse)
}

// serviceRate returns the bucket's calibrated service rate in requests per second,
// or 0 when it has not been established. Unlike the capacity estimate this is not
// clamped or scaled: it is the measured throughput of a replica that could not keep
// up, which is exactly what a scale-down counterfactual needs.
func (a *SaturationAnalyzer) serviceRate(modelID, role string, gpuCount int, accelerator string,
	shape variantShape, now time.Time) float64 {
	if a.serviceRates == nil {
		return 0
	}
	rate, ok := a.serviceRates.Rate(serviceRateKey(modelID, accelerator, role, gpuCount, shape), now)
	if !ok {
		return 0
	}
	return rate
}

// completionRate is the rate at which this replica finishes the work its role is
// responsible for: prompts processed for a prefill replica, generations completed
// for a decode replica or an undisaggregated one.
//
// A prefill pod completes few or no generations, so measuring it with the
// generation-tokens counter would either learn nothing or learn a service rate an
// order of magnitude below the truth. Falls back to completions when the prompt rate
// is unavailable, which is no worse than before it was collected.
func completionRate(rm domain.ReplicaMetrics, role string) float64 {
	if canonicalRole(role) == domain.RolePrefill && rm.PromptTokenRate > 0 {
		return rm.PromptTokenRate
	}
	return rm.RequestRate
}

// tokensPerRequest is the KV footprint of one request of this bucket's shape.
func tokensPerRequest(rm domain.ReplicaMetrics) float64 {
	t := rm.AvgInputTokens + rm.AvgOutputTokens
	if t <= 0 || math.IsNaN(t) || math.IsInf(t, 0) {
		return 0
	}
	return t
}

// arrivalsReachedServiceRate reports whether arrivals have caught up with the
// service rate measured while the replica was backlogged — the limit being reached
// before a queue has formed.
//
// lambda comes from the EPP dispatch rate where available. Without EPP, and only
// when there is no queue, completions stand in for arrivals: everything that
// arrives is served within the window, so the two are equal. That substitution is
// invalid under backlog, which is why the caller checks the queue first.
func (a *SaturationAnalyzer) arrivalsReachedServiceRate(rm domain.ReplicaMetrics, key string, now time.Time) bool {
	muRate, ok := a.serviceRates.Rate(key, now)
	if !ok {
		return false
	}

	smoothingKey := rm.PodName
	if smoothingKey == "" {
		smoothingKey = key
	}
	lambda := a.arrivals.Smooth(smoothingKey, rm.ArrivalRate, smoothingTau(rm), now)
	if lambda <= 0 {
		lambda = rm.RequestRate
	}
	if lambda <= 0 || math.IsNaN(lambda) || math.IsInf(lambda, 0) {
		return false
	}
	return lambda >= muRate*SaturationEnterRatio
}

// clampCeiling keeps a learned ceiling within usable bounds. There is no upper
// clamp: min(k1, k2) in the caller already prevents a compute bound from exceeding
// the memory bound.
func clampCeiling(tokens float64, k1 int64) int64 {
	if math.IsNaN(tokens) || math.IsInf(tokens, 0) {
		return k1
	}
	if floor := float64(k1) * MinRateAnchoredFraction; tokens < floor {
		tokens = floor
	}
	if tokens <= 0 {
		return k1
	}
	return int64(tokens)
}
