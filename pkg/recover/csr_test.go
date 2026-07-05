package recover

import "testing"

// buildTestCSR builds a hand-verifiable 3-row x 4-col CSR:
//
//	row0: col0=1.0, col2=-2.0
//	row1: col1=0.5
//	row2: col0=1.0, col1=1.0, col3=3.0
func buildTestCSR() *CSR {
	return &CSR{
		NRows:  3,
		NCols:  4,
		RowPtr: []int32{0, 2, 3, 6},
		ColIdx: []int32{0, 2, 1, 0, 1, 3},
		Vals:   []float64{1.0, -2.0, 0.5, 1.0, 1.0, 3.0},
	}
}

func TestCSRMulInto(t *testing.T) {
	c := buildTestCSR()
	x := []float64{2, 3, 4, 5} // length NCols
	out := make([]float64, c.NRows)
	c.MulInto(x, out)

	want := []float64{
		1.0*2 + -2.0*4,        // row0: col0*x0 + col2*x2 = 2 - 8 = -6
		0.5 * 3,               // row1: col1*x1 = 1.5
		1.0*2 + 1.0*3 + 3.0*5, // row2 = 2+3+15 = 20
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("MulInto[%d] = %v, want %v", i, out[i], want[i])
		}
	}
}

func TestCSRMulTransposeInto(t *testing.T) {
	c := buildTestCSR()
	x := []float64{10, 20, 30} // length NRows
	out := make([]float64, c.NCols)
	c.MulTransposeInto(x, out)

	// col0 gets row0's 1.0*x0 + row2's 1.0*x2 = 10 + 30 = 40
	// col1 gets row1's 0.5*x1 + row2's 1.0*x2 = 10 + 30 = 40
	// col2 gets row0's -2.0*x0 = -20
	// col3 gets row2's 3.0*x2 = 90
	want := []float64{40, 40, -20, 90}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("MulTransposeInto[%d] = %v, want %v", i, out[i], want[i])
		}
	}
}

// TestCSRAdjointConsistency checks <c@x, y> == <x, c^T@y> for a random-ish
// pair of vectors, the defining property of Mul/MulTranspose being a true
// adjoint pair (needed for FISTA's gradient to be correct).
func TestCSRAdjointConsistency(t *testing.T) {
	c := buildTestCSR()
	x := []float64{1, -2, 3}      // length NRows (input to Mul)
	y := []float64{4, -1, 2, 0.5} // length NCols (input to MulTranspose)

	cx := make([]float64, c.NRows)
	c.MulInto(y, cx) // c @ y  (y has NCols length, matches Mul's x param)

	cty := make([]float64, c.NCols)
	c.MulTransposeInto(x, cty) // c^T @ x

	var lhs, rhs float64
	for i := range x {
		lhs += x[i] * cx[i]
	}
	for i := range y {
		rhs += y[i] * cty[i]
	}
	if lhs != rhs {
		t.Fatalf("<x, C@y> = %v != <C^T@x, y> = %v", lhs, rhs)
	}
}

func TestCSRMulTransposeIntoZeroesOutput(t *testing.T) {
	c := buildTestCSR()
	out := []float64{99, 99, 99, 99}
	c.MulTransposeInto([]float64{0, 0, 0}, out)
	for i, v := range out {
		if v != 0 {
			t.Errorf("out[%d] = %v, want 0 (stale value not cleared)", i, v)
		}
	}
}
