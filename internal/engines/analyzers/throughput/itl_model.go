package throughput

import "math"

// ITLModel is the linear inter-token latency model: ITL(k) = A·k + B.
// A is the slope (marginal latency cost per unit of KV utilization) and B is the
// hardware baseline latency observed at near-zero KV load.
type ITLModel struct {
	A float64
	B float64
}

// IsZero returns true when the model has not been fitted (both coefficients are zero).
func (m ITLModel) IsZero() bool {
	return m.A == 0 && m.B == 0
}

// ITLAt returns the predicted ITL (seconds/token) at KV utilization k.
func (m ITLModel) ITLAt(k float64) float64 {
	return m.A*k + m.B
}

// FitITLModel fits the linear model ITL(k) = A·k + B to the observations using
// ordinary least squares (OLS). Returns (model, true) on success.
//
// Returns (zero, false) when:
//   - fewer than 2 observations are provided
//   - k-spread across observations is zero (degenerate — no discriminating signal)
//   - the fitted slope A ≤ 0 (inverted or flat fit — physically implausible)
func FitITLModel(obs []ITLObservation) (ITLModel, bool) {
	n := float64(len(obs))
	if n < 2 {
		return ITLModel{}, false
	}

	var sumK, sumITL, sumK2, sumKITL float64
	for _, o := range obs {
		sumK += o.K
		sumITL += o.ITLSec
		sumK2 += o.K * o.K
		sumKITL += o.K * o.ITLSec
	}

	denom := n*sumK2 - sumK*sumK
	if math.Abs(denom) < 1e-12 {
		return ITLModel{}, false
	}

	A := (n*sumKITL - sumK*sumITL) / denom
	B := (sumITL - A*sumK) / n

	// Defensive guard: NaN/+Inf A both slip past the A <= 0 check below, and a non-zero
	// sumK can leave B finite so the B guard would not catch them. Symmetric with the B guard.
	if math.IsNaN(A) || math.IsInf(A, 0) {
		return ITLModel{}, false
	}
	if A <= 0 {
		return ITLModel{}, false
	}
	// Defensive guard: NaN/Inf B is mathematically possible with degenerate input.
	if math.IsNaN(B) || math.IsInf(B, 0) {
		return ITLModel{}, false
	}
	// Guard: ensure ITL at saturation is positive. A noisy OLS can yield negative
	// B (valid A>0), making ITLAt(DefaultKSat) near-zero and inflating supply.
	if A*DefaultKSat+B <= 0 {
		return ITLModel{}, false
	}

	return ITLModel{A: A, B: B}, true
}
