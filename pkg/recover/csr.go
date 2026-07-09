/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

// CSR is a minimal compressed-sparse-row matrix used to hold the Phi^T
// (series x buckets) submatrix restricted to a Dictionary's currently
// active series (ADR-002's hash-implicit Phi, made concrete only for the
// known-candidate universe the decoder can recover against).
//
// Row i corresponds to one candidate series (Dictionary.ActiveIDs()[i]);
// its NNZ entries are the d hash-implicit bucket/sign pairs sketch.Buckets
// assigns that series under the epoch's seed. Storing rows (not columns)
// lets both directions FISTA needs run as a single pass over
// RowPtr/ColIdx/Vals:
//
//   - MulInto computes out = c @ x, a per-row gather: this is Phi^T @ r,
//     the adjoint step of FISTA's gradient.
//   - MulTransposeInto computes out = c^T @ x, a per-row scatter: this is
//     Phi @ z, FISTA's forward step.
//
// CSR has no dependency beyond the standard library and is built fresh by
// Dictionary.BuildCSR for each recovery call (ADR-002: Phi itself is never
// persisted; only the hash that derives it is).
//
// Vals is float32 (Prompt 12 step 3): Phi's entries are ±1/sqrt(d) — a
// handful of distinct magnitudes — so float32's ~7 significant digits cost
// nothing observable in the recovered support/values (see golden/seed-sweep/
// null test re-runs in PERF.md), while halving the largest array's memory
// footprint (n*d entries — 300,000 at this package's benchmark scale)
// measurably helps the matvecs' cache/bandwidth behavior. Every dot product
// still accumulates in float64 (MulInto/MulTransposeInto's sum/out are
// float64) to avoid compounding rounding error across many terms.
type CSR struct {
	NRows, NCols int
	RowPtr       []int32
	ColIdx       []int32
	Vals         []float32
}

// MulInto computes out = c @ x: out[i] = sum over row i's entries of
// Vals[k]*x[ColIdx[k]]. x must have length NCols, out length NRows.
func (c *CSR) MulInto(x, out []float64) {
	if c.NRows > 0 {
		// BCE hints (Prompt 12 step 4): prove RowPtr/out are long enough
		// once so the compiler drops the per-iteration bounds check below;
		// x[ColIdx[k]] stays checked (data-dependent index).
		_ = c.RowPtr[c.NRows]
		_ = out[c.NRows-1]
	}
	for i := 0; i < c.NRows; i++ {
		var sum float64
		for k := c.RowPtr[i]; k < c.RowPtr[i+1]; k++ {
			sum += float64(c.Vals[k]) * x[c.ColIdx[k]]
		}
		out[i] = sum
	}
}

// MulTransposeInto computes out = c^T @ x: a scatter-add over c's rows,
// out[ColIdx[k]] += Vals[k]*x[i] for each entry k of row i. x must have
// length NRows, out length NCols; out is zeroed before accumulating.
func (c *CSR) MulTransposeInto(x, out []float64) {
	for i := range out {
		out[i] = 0
	}
	c.MulTransposeIntoAdd(x, out)
}

// MulTransposeIntoAdd computes out += c^T @ x: the same scatter-add as
// MulTransposeInto, but accumulating into out's existing contents instead
// of zeroing it first. Lets a caller fuse an initial value into the
// scatter in one pass — e.g. seeding out with -y before scattering Phi@z
// into it computes the residual Phi@z-y directly, with no separate
// subtraction pass over the result (Prompt 12 step 4).
func (c *CSR) MulTransposeIntoAdd(x, out []float64) {
	if c.NRows > 0 {
		_ = c.RowPtr[c.NRows]
		_ = x[c.NRows-1]
	}
	for i := 0; i < c.NRows; i++ {
		xi := x[i]
		if xi == 0 {
			continue
		}
		for k := c.RowPtr[i]; k < c.RowPtr[i+1]; k++ {
			out[c.ColIdx[k]] += float64(c.Vals[k]) * xi
		}
	}
}
