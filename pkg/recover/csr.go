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
type CSR struct {
	NRows, NCols int
	RowPtr       []int32
	ColIdx       []int32
	Vals         []float64
}

// MulInto computes out = c @ x: out[i] = sum over row i's entries of
// Vals[k]*x[ColIdx[k]]. x must have length NCols, out length NRows.
func (c *CSR) MulInto(x, out []float64) {
	for i := 0; i < c.NRows; i++ {
		var sum float64
		for k := c.RowPtr[i]; k < c.RowPtr[i+1]; k++ {
			sum += c.Vals[k] * x[c.ColIdx[k]]
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
	for i := 0; i < c.NRows; i++ {
		xi := x[i]
		if xi == 0 {
			continue
		}
		for k := c.RowPtr[i]; k < c.RowPtr[i+1]; k++ {
			out[c.ColIdx[k]] += c.Vals[k] * xi
		}
	}
}
