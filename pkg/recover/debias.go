/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

import "math"

// debias solves the ridge-regularized normal equations
//
//	(Phi_s^T Phi_s + ridge*I) x = Phi_s^T y
//
// for the columns selected by support (row indices into csr, i.e. the
// FISTA-recovered candidates), undoing FISTA's L1 shrinkage bias with a
// dense least-squares refit (oracle/palimpsest_ref.py's debias). Returns a
// value per entry of support, in the same order; an empty support returns
// an empty (non-nil) slice.
func debias(csr *CSR, y []float64, support []int, ridge float64) []float64 {
	k := len(support)
	if k == 0 {
		return []float64{}
	}

	a := make([]float64, k*k)
	b := make([]float64, k)

	for si, row := range support {
		var bs float64
		for kk := csr.RowPtr[row]; kk < csr.RowPtr[row+1]; kk++ {
			bs += float64(csr.Vals[kk]) * y[csr.ColIdx[kk]]
		}
		b[si] = bs
	}

	for si := 0; si < k; si++ {
		for sj := si; sj < k; sj++ {
			dot := dotRows(csr, support[si], support[sj])
			if si == sj {
				dot += ridge
			}
			a[si*k+sj] = dot
			a[sj*k+si] = dot
		}
	}

	return solveLinear(a, b, k)
}

// dotRows computes the dot product of csr's rows r1 and r2 (each with
// exactly d nonzero entries, per Dictionary.BuildCSR): the (r1, r2) entry
// of Phi^T Phi. d is small (the sketch's hashes-per-series parameter), so
// the O(d^2) linear scan for shared columns is cheap.
func dotRows(csr *CSR, r1, r2 int) float64 {
	var sum float64
	for k1 := csr.RowPtr[r1]; k1 < csr.RowPtr[r1+1]; k1++ {
		c1 := csr.ColIdx[k1]
		v1 := float64(csr.Vals[k1])
		for k2 := csr.RowPtr[r2]; k2 < csr.RowPtr[r2+1]; k2++ {
			if csr.ColIdx[k2] == c1 {
				sum += v1 * float64(csr.Vals[k2])
				break
			}
		}
	}
	return sum
}

// solveLinear solves the k x k system a*x = b (a row-major, symmetric
// positive-definite after debias's ridge term) via Gaussian elimination
// with partial pivoting. a and b are not modified.
func solveLinear(a []float64, b []float64, k int) []float64 {
	A := append([]float64(nil), a...)
	B := append([]float64(nil), b...)

	for col := 0; col < k; col++ {
		piv := col
		best := math.Abs(A[col*k+col])
		for r := col + 1; r < k; r++ {
			if v := math.Abs(A[r*k+col]); v > best {
				best = v
				piv = r
			}
		}
		if piv != col {
			for c := 0; c < k; c++ {
				A[col*k+c], A[piv*k+c] = A[piv*k+c], A[col*k+c]
			}
			B[col], B[piv] = B[piv], B[col]
		}
		pivotVal := A[col*k+col]
		if pivotVal == 0 {
			continue
		}
		for r := col + 1; r < k; r++ {
			factor := A[r*k+col] / pivotVal
			if factor == 0 {
				continue
			}
			for c := col; c < k; c++ {
				A[r*k+c] -= factor * A[col*k+c]
			}
			B[r] -= factor * B[col]
		}
	}

	x := make([]float64, k)
	for r := k - 1; r >= 0; r-- {
		sum := B[r]
		for c := r + 1; c < k; c++ {
			sum -= A[r*k+c] * x[c]
		}
		if A[r*k+r] == 0 {
			continue
		}
		x[r] = sum / A[r*k+r]
	}
	return x
}
