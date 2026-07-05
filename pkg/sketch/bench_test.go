/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import (
	"fmt"
	"testing"
	"time"
)

func benchParams() Params {
	return Params{M: 4096, D: 4, Seed: 0xC0FFEE, Bits: 8}
}

// BenchmarkUpdateCached measures the steady-state path: the series' Phi
// columns are already cached (via a prior Update), so UpdateByID does no
// hashing at all. This is the < 20 ns/op target (not a CI gate).
func BenchmarkUpdateCached(b *testing.B) {
	a := NewAccumulator(benchParams())
	name := []byte("svc.requests.count|region=us-east-1|agg=sum")
	id := SeriesID(name)
	a.Update(name, 0) // warm the bucket cache

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.UpdateByID(id, 1.23)
	}
}

// BenchmarkUpdateUncached measures the cold path: every call is for a
// distinct, never-before-seen series, forcing SeriesID + Buckets hashing
// and a cache insert on every call.
func BenchmarkUpdateUncached(b *testing.B) {
	a := NewAccumulator(benchParams())
	names := make([][]byte, b.N)
	for i := range names {
		names[i] = []byte(fmt.Sprintf("svc.metric.%d|agg=sum", i))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Update(names[i], 1.23)
	}
}

// BenchmarkFlush measures draining the accumulated sketch vector and
// computing its pre-quantization energy.
func BenchmarkFlush(b *testing.B) {
	a := NewAccumulator(benchParams())
	a.Update([]byte("svc.requests.count|agg=sum"), 5.0)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = a.Flush()
	}
}

// BenchmarkSnapshot measures encoding a RingBuffer's current contents to a
// wire snapshot blob.
func BenchmarkSnapshot(b *testing.B) {
	rb := NewRingBuffer(1, 15*time.Minute)
	base := uint64(time.Now().UnixMilli())
	for i := 0; i < 1000; i++ {
		rb.Push(base+uint64(i), float64(i))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = rb.Snapshot()
	}
}
