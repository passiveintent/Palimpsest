/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import (
	"math"
	"testing"
)

func TestAccumulatorUpdateProjectsIntoY(t *testing.T) {
	name := []byte("svc.requests.count|agg=sum")
	p := Params{M: 64, D: 4, Seed: 0xC0FFEE, Bits: 8}
	a := NewAccumulator(p)

	a.Update(name, 10)
	y, energy := a.Flush()

	idx, sign := Buckets(name, p.Seed, p.M, p.D)
	want := make([]float64, p.M)
	invSqrtD := 1 / math.Sqrt(float64(p.D))
	for i := range idx {
		want[idx[i]] += float64(sign[i]) * invSqrtD * 10
	}
	for i := range want {
		if math.Abs(y[i]-want[i]) > 1e-9 {
			t.Fatalf("y[%d] = %v, want %v", i, y[i], want[i])
		}
	}

	var wantEnergy float64
	for _, v := range want {
		wantEnergy += v * v
	}
	wantEnergy /= float64(p.M)
	if math.Abs(energy-wantEnergy) > 1e-9 {
		t.Fatalf("energy = %v, want %v", energy, wantEnergy)
	}
}

func TestAccumulatorFlushResetsY(t *testing.T) {
	name := []byte("svc.errors.count|agg=sum")
	a := NewAccumulator(Params{M: 32, D: 3, Seed: 1, Bits: 8})
	a.Update(name, 5)
	if _, energy := a.Flush(); energy == 0 {
		t.Fatal("expected non-zero energy after Update")
	}
	// y should now be zeroed; a second Flush with no intervening
	// Update/UpdateByID must report zero energy.
	y, energy := a.Flush()
	if energy != 0 {
		t.Fatalf("expected zero energy after Flush reset, got %v", energy)
	}
	for i, v := range y {
		if v != 0 {
			t.Fatalf("y[%d] = %v after Flush reset, want 0", i, v)
		}
	}
}

func TestAccumulatorUpdateByIDRequiresCachedEntry(t *testing.T) {
	a := NewAccumulator(Params{M: 32, D: 3, Seed: 1, Bits: 8})
	// No prior Update(name, ...) call: UpdateByID for an unknown id must be
	// a harmless no-op, not a panic or a spurious contribution.
	a.UpdateByID(0xdeadbeef, 100)
	if _, energy := a.Flush(); energy != 0 {
		t.Fatalf("expected zero energy for uncached UpdateByID, got %v", energy)
	}
}

func TestAccumulatorUpdateByIDMatchesUpdate(t *testing.T) {
	name := []byte("svc.latency.p99|agg=max")
	id := SeriesID(name)

	a1 := NewAccumulator(Params{M: 64, D: 4, Seed: 99, Bits: 8})
	a1.Update(name, 3)
	a1.Update(name, -2) // second call: cache hit inside Update itself

	a2 := NewAccumulator(Params{M: 64, D: 4, Seed: 99, Bits: 8})
	a2.Update(name, 3) // establishes the cache entry (residual can be 0 or real; here matched to a1)
	a2.UpdateByID(id, -2)

	y1, _ := a1.Flush()
	y2, _ := a2.Flush()
	for i := range y1 {
		if y1[i] != y2[i] {
			t.Fatalf("y[%d]: Update-path=%v UpdateByID-path=%v", i, y1[i], y2[i])
		}
	}
}

func TestAccumulatorEvictDropsCache(t *testing.T) {
	name := []byte("svc.cpu.usage|agg=sum")
	id := SeriesID(name)
	a := NewAccumulator(Params{M: 32, D: 3, Seed: 5, Bits: 8})
	a.Update(name, 1)
	a.Flush() // clears y but not the bucket cache

	a.Evict(id)
	a.UpdateByID(id, 1000) // must be a no-op post-eviction
	if _, energy := a.Flush(); energy != 0 {
		t.Fatalf("expected zero energy after Evict + UpdateByID, got %v", energy)
	}
}

func TestAccumulatorParams(t *testing.T) {
	p := Params{M: 128, D: 5, Seed: 0xABCD, Bits: 16}
	a := NewAccumulator(p)
	if got := a.Params(); got != p {
		t.Fatalf("Params() = %+v, want %+v", got, p)
	}
}
