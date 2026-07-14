/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

import (
	"math"
	"sort"
)

// madToSigma converts a median absolute deviation to a Gaussian standard
// deviation (1/Phi^-1(3/4)).
const madToSigma = 1 / 0.6745

// MADSigma returns a robust estimate of the standard deviation of v:
// median(|v_i - median(v)|) / 0.6745. Robust to a sparse contamination of
// large entries (an anomaly touches only ~d of the sketch's m buckets), so
// it estimates the *noise* scale of a sketch window rather than its total
// energy. Returns 0 for an empty input.
func MADSigma(v []float64) float64 {
	n := len(v)
	if n == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	med := medianSorted(s)
	for i, x := range s {
		s[i] = math.Abs(x - med)
	}
	sort.Float64s(s)
	return medianSorted(s) * madToSigma
}

func medianSorted(s []float64) float64 {
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// QuietSigma keeps a rolling window of per-window noise-scale estimates
// (fed only from windows the caller judged quiet) and reports their
// median. This is the sigma_hat backing the G9 fixes: F1's normalized
// residual gate (gate = c_gate * sqrt(n) * sigma_hat) and F2's
// magnitude-independent lambda (lambda = kappa * sigma_hat * sqrt(2 ln N)).
// The median over a window of quiet readings makes the estimate robust to
// an occasional mis-classified window.
//
// QuietSigma is NOT safe for concurrent use.
type QuietSigma struct {
	ring []float64
	cap  int
	pos  int
	n    int
}

// NewQuietSigma returns a QuietSigma with the given rolling-window size.
func NewQuietSigma(window int) *QuietSigma {
	if window < 1 {
		window = 1
	}
	return &QuietSigma{ring: make([]float64, window), cap: window}
}

// Observe folds one quiet window's noise-scale estimate into the rolling
// window. The caller decides quietness; Observe never filters.
func (q *QuietSigma) Observe(sigma float64) {
	q.ring[q.pos] = sigma
	q.pos = (q.pos + 1) % q.cap
	if q.n < q.cap {
		q.n++
	}
}

// Count reports how many quiet estimates have been observed (saturating at
// the window size).
func (q *QuietSigma) Count() int { return q.n }

// Estimate returns the median of the observed quiet estimates, and false
// when nothing has been observed yet.
func (q *QuietSigma) Estimate() (float64, bool) {
	if q.n == 0 {
		return 0, false
	}
	s := append([]float64(nil), q.ring[:q.n]...)
	sort.Float64s(s)
	return medianSorted(s), true
}
