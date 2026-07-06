/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

import (
	"math"
	"testing"
)

func almostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestSolveLinear2x2(t *testing.T) {
	// 4x + 2y = 2
	// 2x + 3y = 3
	// => x=0, y=1
	a := []float64{4, 2, 2, 3}
	b := []float64{2, 3}
	x := solveLinear(a, b, 2)
	if !almostEqual(x[0], 0, 1e-9) || !almostEqual(x[1], 1, 1e-9) {
		t.Fatalf("solveLinear = %v, want [0, 1]", x)
	}
}

func TestSolveLinearIdentity(t *testing.T) {
	a := []float64{1, 0, 0, 0, 1, 0, 0, 0, 1}
	b := []float64{5, -3, 2}
	x := solveLinear(a, b, 3)
	want := []float64{5, -3, 2}
	for i := range want {
		if !almostEqual(x[i], want[i], 1e-9) {
			t.Fatalf("solveLinear identity = %v, want %v", x, want)
		}
	}
}

// TestDebiasOrthogonalColumns builds a CSR by hand where each support row
// touches a disjoint column, so Phi_s^T Phi_s is (near-)diagonal and
// debias should recover values very close to a plain per-column
// projection (ridge=1e-6 perturbs the answer negligibly).
func TestDebiasOrthogonalColumns(t *testing.T) {
	csr := &CSR{
		NRows:  2,
		NCols:  3,
		RowPtr: []int32{0, 1, 2},
		ColIdx: []int32{0, 1},
		Vals:   []float32{1.0, 1.0},
	}
	y := []float64{3.0, -2.0, 0.0}

	values := debias(csr, y, []int{0, 1}, 1e-6)
	if len(values) != 2 {
		t.Fatalf("debias returned %d values, want 2", len(values))
	}
	if !almostEqual(values[0], 3.0, 1e-4) {
		t.Errorf("values[0] = %v, want ~3.0", values[0])
	}
	if !almostEqual(values[1], -2.0, 1e-4) {
		t.Errorf("values[1] = %v, want ~-2.0", values[1])
	}
}

func TestDebiasEmptySupport(t *testing.T) {
	csr := &CSR{NRows: 1, NCols: 1, RowPtr: []int32{0, 1}, ColIdx: []int32{0}, Vals: []float32{1.0}}
	values := debias(csr, []float64{1.0}, nil, 1e-6)
	if values == nil || len(values) != 0 {
		t.Fatalf("debias with empty support = %v, want empty non-nil slice", values)
	}
}

func TestDotRows(t *testing.T) {
	csr := buildTestCSR() // from csr_test.go
	// row0: col0=1.0, col2=-2.0; row2: col0=1.0, col1=1.0, col3=3.0
	// shared column: col0 (1.0*1.0=1.0)
	got := dotRows(csr, 0, 2)
	if got != 1.0 {
		t.Fatalf("dotRows(0,2) = %v, want 1.0", got)
	}
	// row0 vs row1 (col1=0.5): no shared columns.
	if got := dotRows(csr, 0, 1); got != 0 {
		t.Fatalf("dotRows(0,1) = %v, want 0", got)
	}
	// self dot: row0 = 1.0^2 + (-2.0)^2 = 5.0
	if got := dotRows(csr, 0, 0); got != 5.0 {
		t.Fatalf("dotRows(0,0) = %v, want 5.0", got)
	}
}
