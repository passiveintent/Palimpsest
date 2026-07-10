/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import (
	"testing"

	"github.com/cespare/xxhash/v2"
)

func TestSeriesIDMatchesXXHSeedZero(t *testing.T) {
	name := []byte("svc.requests.count|region=us-east-1|agg=sum")
	want := xxhash.Sum64(name)
	if got := SeriesID(name); got != want {
		t.Fatalf("SeriesID = %x, want xxhash.Sum64 = %x", got, want)
	}
}

func TestSeriesIDDeterministicAndDistinguishing(t *testing.T) {
	a := []byte("svc.a|agg=sum")
	b := []byte("svc.b|agg=sum")
	if SeriesID(a) != SeriesID(a) {
		t.Fatal("SeriesID not deterministic for repeated calls")
	}
	if SeriesID(a) == SeriesID(b) {
		t.Fatal("SeriesID collided for distinct names (extremely unlikely; check implementation)")
	}
}

func TestBucketsDeterministic(t *testing.T) {
	name := []byte("svc.latency.p99|agg=max")
	idx1, sign1 := Buckets(name, 0xC0FFEE, 256, 4)
	idx2, sign2 := Buckets(name, 0xC0FFEE, 256, 4)
	if len(idx1) != 4 || len(sign1) != 4 {
		t.Fatalf("want 4 columns, got idx=%d sign=%d", len(idx1), len(sign1))
	}
	for i := range idx1 {
		if idx1[i] != idx2[i] || sign1[i] != sign2[i] {
			t.Fatalf("Buckets not deterministic at i=%d: (%d,%d) vs (%d,%d)", i, idx1[i], sign1[i], idx2[i], sign2[i])
		}
	}
}

func TestBucketsIndexRangeAndSign(t *testing.T) {
	name := []byte("svc.errors.count|agg=sum")
	const m = 128
	const d = 8
	idx, sign := Buckets(name, 42, m, d)
	for i := 0; i < d; i++ {
		if idx[i] >= m {
			t.Fatalf("idx[%d] = %d out of range [0,%d)", i, idx[i], m)
		}
		if sign[i] != 1 && sign[i] != -1 {
			t.Fatalf("sign[%d] = %d, want +1 or -1", i, sign[i])
		}
	}
}

func TestBucketsVariesWithSeed(t *testing.T) {
	name := []byte("svc.latency.p50|agg=max")
	idxA, signA := Buckets(name, 1, 1<<20, 4)
	idxB, signB := Buckets(name, 2, 1<<20, 4)
	same := true
	for i := range idxA {
		if idxA[i] != idxB[i] || signA[i] != signB[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("Buckets produced identical columns for different ephemeral seeds")
	}
}

func TestBucketsVariesWithName(t *testing.T) {
	idxA, _ := Buckets([]byte("series.one|agg=sum"), 7, 1<<20, 4)
	idxB, _ := Buckets([]byte("series.two|agg=sum"), 7, 1<<20, 4)
	same := true
	for i := range idxA {
		if idxA[i] != idxB[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("Buckets produced identical columns for different series names")
	}
}
