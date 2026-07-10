/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"math/rand"
	"testing"
)

func TestComputeDictRootOrderIndependent(t *testing.T) {
	ids := []uint64{5, 1, 3, 2, 4}
	want := ComputeDictRoot(ids)

	shuffled := append([]uint64(nil), ids...)
	rng := rand.New(rand.NewSource(2))
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	if got := ComputeDictRoot(shuffled); got != want {
		t.Fatalf("order dependence: want %d got %d", want, got)
	}
}

func TestComputeDictRootDeterministic(t *testing.T) {
	ids := []uint64{1, 2, 3}
	a := ComputeDictRoot(ids)
	b := ComputeDictRoot(ids)
	if a != b {
		t.Fatalf("non-deterministic: %d != %d", a, b)
	}
}

func TestComputeDictRootDiffersOnContentChange(t *testing.T) {
	a := ComputeDictRoot([]uint64{1, 2, 3})
	b := ComputeDictRoot([]uint64{1, 2, 4})
	if a == b {
		t.Fatalf("different id sets produced same dict_root: %d", a)
	}
}

func TestComputeDictRootEmpty(t *testing.T) {
	// Should not panic, and should be a fixed, deterministic value.
	got := ComputeDictRoot(nil)
	if got != ComputeDictRoot([]uint64{}) {
		t.Fatal("nil vs empty slice produced different dict_root")
	}
}

// TestComputeDictRootRegression pins the digest for a fixed input so an
// accidental change to the hashing scheme (seed, byte order, sort) is
// caught. There is no Python oracle yet to cross-check this constant
// against (see golden_test.go); once oracle/gen_golden.py exists this
// should be replaced/confirmed by a cross-language golden vector.
func TestComputeDictRootRegression(t *testing.T) {
	const want = uint64(0xbc50ebd6bc8fa148)
	got := ComputeDictRoot([]uint64{1, 2, 3, 4, 5})
	if got != want {
		t.Fatalf("dict_root regression: want %#x got %#x (update this constant only if the hashing scheme intentionally changed)", want, got)
	}
}
