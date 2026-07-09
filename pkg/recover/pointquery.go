/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

import (
	"math"
	"sort"
)

// PointQuery implements ADR-009's named-series fast path (substrate b):
// given the accumulated sketch y and a known series' bucket/sign
// assignment (sketch.Buckets), it recovers that series' residual in O(d)
// via the count-sketch median estimator, without running full recovery.
//
// Each of the d buckets independently estimates the residual as
// sign[i]*y[idx[i]]*sqrt(d) (undoing the encode-side 1/sqrt(d) scaling,
// see sketch.Accumulator); the median of these d estimates is robust to
// whatever other series share those buckets. errEstimate is docs/SPEC.md's
// "error ~ ||y||/sqrt(m)" bound: a conservative, series-independent
// estimate of the interference any point query in this sketch is subject
// to.
func PointQuery(y []float64, idx []uint32, sign []int8) (value, errEstimate float64) {
	d := len(idx)
	if d == 0 {
		return 0, 0
	}
	sqrtD := math.Sqrt(float64(d))
	estimates := make([]float64, d)
	for i, bucket := range idx {
		estimates[i] = float64(sign[i]) * y[bucket] * sqrtD
	}
	sort.Float64s(estimates)
	value = median(estimates)

	m := len(y)
	if m == 0 {
		return value, 0
	}
	errEstimate = l2norm(y) / math.Sqrt(float64(m))
	return value, errEstimate
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
