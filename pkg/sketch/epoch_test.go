/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import (
	"math/rand"
	"testing"
	"time"
)

func TestEpochJitterDisabledByNonPositiveWindow(t *testing.T) {
	for _, w := range []time.Duration{0, -time.Second} {
		if got := EpochJitter(12345, w); got != 0 {
			t.Fatalf("EpochJitter(_, %v) = %v, want 0", w, got)
		}
	}
}

func TestEpochJitterDeterministic(t *testing.T) {
	const jitterWindow = 60 * time.Second
	for _, id := range []uint64{0, 1, 42, 1 << 63} {
		a := EpochJitter(id, jitterWindow)
		b := EpochJitter(id, jitterWindow)
		if a != b {
			t.Fatalf("EpochJitter(%d, ...) not deterministic: %v vs %v", id, a, b)
		}
		if a < 0 || a >= jitterWindow {
			t.Fatalf("EpochJitter(%d, ...) = %v, want in [0, %v)", id, a, jitterWindow)
		}
	}
}

// TestEpochJitterSpread is the ADR-013 §Addendum acceptance test: 1000
// simulated emitters sharing one epoch boundary must not all rotate in the
// same instant. Bucketing each emitter's jittered rotation offset into
// 1-second buckets across jitterWindow, no single bucket may hold more than
// 5% of the fleet.
func TestEpochJitterSpread(t *testing.T) {
	const (
		numEmitters  = 1000
		jitterWindow = 60 * time.Second
	)
	rng := rand.New(rand.NewSource(1))
	buckets := make([]int, int(jitterWindow/time.Second))
	for i := 0; i < numEmitters; i++ {
		emitterID := rng.Uint64()
		offset := EpochJitter(emitterID, jitterWindow)
		bucket := int(offset / time.Second)
		buckets[bucket]++
	}

	max := 0
	for _, c := range buckets {
		if c > max {
			max = c
		}
	}
	limit := numEmitters * 5 / 100
	if max >= limit {
		t.Fatalf("max per-second arrival count = %d, want < %d (%.0f%% of %d emitters)", max, limit, 5.0, numEmitters)
	}
	t.Logf("max per-second arrival count = %d (limit %d) across %d one-second buckets", max, limit, len(buckets))
}

func TestEpochIndexMatchesUnjitteredWhenWindowZero(t *testing.T) {
	const rotate = time.Hour
	base := time.Unix(1_700_000_000, 0)
	for _, emitterID := range []uint64{0, 1, 999999} {
		want := uint64(base.UnixNano() / rotate.Nanoseconds())
		if got := EpochIndex(base, rotate, 0, emitterID); got != want {
			t.Fatalf("EpochIndex(emitter=%d, jitterWindow=0) = %d, want %d (unjittered)", emitterID, got, want)
		}
	}
}

func TestEpochIndexNonPositiveRotateIsZero(t *testing.T) {
	if got := EpochIndex(time.Now(), 0, time.Minute, 1); got != 0 {
		t.Fatalf("EpochIndex(rotate<=0) = %d, want 0", got)
	}
}

// TestEpochIndexDelaysRotationByJitter verifies the exact contract: emitter
// e's transition into the next epoch happens at epoch_boundary +
// EpochJitter(e, jitterWindow), not at the unshifted boundary.
func TestEpochIndexDelaysRotationByJitter(t *testing.T) {
	const rotate = time.Hour
	const jitterWindow = 5 * time.Minute
	const emitterID = 0xABCD1234

	jitter := EpochJitter(emitterID, jitterWindow)
	boundary := time.Unix(0, 0).Add(rotate) // the epoch=0 -> epoch=1 boundary

	before := EpochIndex(boundary.Add(jitter-time.Nanosecond), rotate, jitterWindow, emitterID)
	at := EpochIndex(boundary.Add(jitter), rotate, jitterWindow, emitterID)

	if before != 0 {
		t.Fatalf("EpochIndex just before boundary+jitter = %d, want 0 (still old epoch)", before)
	}
	if at != 1 {
		t.Fatalf("EpochIndex at boundary+jitter = %d, want 1 (rotated)", at)
	}
}

// TestEpochIndexEmittersRotateAtDifferentInstants demonstrates the point of
// the whole mechanism: two emitters with different jitter offsets do not
// both flip from epoch 0 to epoch 1 at the plain, unshifted boundary.
func TestEpochIndexEmittersRotateAtDifferentInstants(t *testing.T) {
	const rotate = time.Hour
	const jitterWindow = time.Minute

	// Find two emitter IDs whose jitter offsets differ (overwhelmingly
	// likely for any two distinct small ids, but assert it rather than
	// assume it so a future xxhash change can't silently make this
	// vacuous).
	var idA, idB uint64 = 1, 2
	jitterA := EpochJitter(idA, jitterWindow)
	jitterB := EpochJitter(idB, jitterWindow)
	if jitterA == jitterB {
		t.Skip("chosen emitter ids happen to share a jitter offset; not a meaningful test run")
	}

	// Sample strictly between the two jitter offsets: the emitter with the
	// smaller offset has already rotated to epoch 1 by this instant, the
	// one with the larger offset has not — proving the two do not rotate
	// at the same wall-clock instant.
	earlier, later := jitterA, jitterB
	earlierID, laterID := idA, idB
	if jitterB < jitterA {
		earlier, later = jitterB, jitterA
		earlierID, laterID = idB, idA
	}
	sampleAt := earlier + (later-earlier)/2

	boundary := time.Unix(0, 0).Add(rotate)
	epochEarlier := EpochIndex(boundary.Add(sampleAt), rotate, jitterWindow, earlierID)
	epochLater := EpochIndex(boundary.Add(sampleAt), rotate, jitterWindow, laterID)
	if epochEarlier != 1 {
		t.Fatalf("earlier-jitter emitter epoch = %d at t=boundary+%v, want 1 (already rotated)", epochEarlier, sampleAt)
	}
	if epochLater != 0 {
		t.Fatalf("later-jitter emitter epoch = %d at t=boundary+%v, want 0 (not yet rotated)", epochLater, sampleAt)
	}
}
