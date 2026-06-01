package throughput

import (
	"math"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// ObservationWindow is a rolling window of (k, ITL_obs) pairs for one variant.
// It accumulates observations from all replicas of the variant across reconcile
// cycles to calibrate the linear ITL model: ITL(k) = A·k + B.
//
// The window is cleared when the workload shape changes (see ShapeTracker).
// Observations outside the valid k range [minK, maxK] are silently ignored.
type ObservationWindow struct {
	observations []ITLObservation
	maxSize      int
	maxAge       time.Duration
	minSamples   int
	minKSpread   float64
	minK         float64
	maxK         float64
}

// newObservationWindow creates a window with the given configuration.
func newObservationWindow(maxSize int, maxAge time.Duration, minSamples int, minKSpread, minK, maxK float64) *ObservationWindow {
	return &ObservationWindow{
		observations: make([]ITLObservation, 0, maxSize),
		maxSize:      maxSize,
		maxAge:       maxAge,
		minSamples:   minSamples,
		minKSpread:   minKSpread,
		minK:         minK,
		maxK:         maxK,
	}
}

// Add appends a (k, itl) observation if k ∈ [minK, maxK] and itl > 0.
// When the window is at capacity, the oldest observation is evicted first.
func (w *ObservationWindow) Add(k, itl float64, ts time.Time) {
	if k < w.minK || k > w.maxK {
		ctrl.Log.V(logging.DEBUG).Info("throughput analyzer: k-value outside observable range, observation dropped",
			"k", k, "minK", w.minK, "maxK", w.maxK)
		return
	}
	if itl <= 0 || math.IsNaN(itl) {
		return
	}
	if len(w.observations) >= w.maxSize {
		w.observations = w.observations[1:]
	}
	w.observations = append(w.observations, ITLObservation{K: k, ITLSec: itl, Timestamp: ts})
}

// Prune removes observations older than maxAge relative to now.
func (w *ObservationWindow) Prune(now time.Time) {
	cutoff := now.Add(-w.maxAge)
	i := 0
	for i < len(w.observations) && w.observations[i].Timestamp.Before(cutoff) {
		i++
	}
	w.observations = w.observations[i:]
}

// KSpread returns max_k − min_k over the current observations.
// Returns 0 when the window is empty.
func (w *ObservationWindow) KSpread() float64 {
	if len(w.observations) == 0 {
		return 0
	}
	minK, maxK := w.observations[0].K, w.observations[0].K
	for _, o := range w.observations[1:] {
		if o.K < minK {
			minK = o.K
		}
		if o.K > maxK {
			maxK = o.K
		}
	}
	return maxK - minK
}

// Ready returns true when the window contains at least minSamples observations
// AND the k-spread is at least minKSpread. Both conditions must hold before the
// window is suitable for OLS fitting.
func (w *ObservationWindow) Ready() bool {
	return len(w.observations) >= w.minSamples && w.KSpread() >= w.minKSpread
}

// Observations returns a copy of the current window contents.
// The caller may mutate the returned slice without affecting the window.
func (w *ObservationWindow) Observations() []ITLObservation {
	out := make([]ITLObservation, len(w.observations))
	copy(out, w.observations)
	return out
}

// Len returns the number of observations currently in the window.
func (w *ObservationWindow) Len() int {
	return len(w.observations)
}

// Clear discards all observations. Called when the workload shape changes.
func (w *ObservationWindow) Clear() {
	w.observations = w.observations[:0]
}
