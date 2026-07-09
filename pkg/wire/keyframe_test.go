/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

func TestKeyframeFullRoundTrip(t *testing.T) {
	ids := []uint64{1, 2, 5, 9}
	values := map[uint64]float32{1: 1.5, 2: -2.25, 5: 0, 9: 100.125}

	payload, flags, err := EncodeKeyframe(ids, values, nil, true, 0)
	if err != nil {
		t.Fatalf("EncodeKeyframe: %v", err)
	}
	if flags != 0 {
		t.Fatalf("golden keyframe: want flags=0, got %d", flags)
	}
	got, err := DecodeKeyframe(payload, false, nil, nil, 0)
	if err != nil {
		t.Fatalf("DecodeKeyframe: %v", err)
	}
	if !reflect.DeepEqual(got, values) {
		t.Fatalf("want %v got %v", values, got)
	}
}

func TestKeyframeKDeltaRoundTrip(t *testing.T) {
	ids := []uint64{1, 2, 3, 4}
	prev := map[uint64]float32{1: 10, 2: 20, 3: 30, 4: 40}
	next := map[uint64]float32{1: 10, 2: 25, 3: 30, 4: 41} // 2 and 4 changed
	scale := float32(0.01)

	payload, flags, err := EncodeKeyframe(ids, next, prev, false, scale)
	if err != nil {
		t.Fatalf("EncodeKeyframe: %v", err)
	}
	if flags != FlagKDelta {
		t.Fatalf("want FlagKDelta, got %d", flags)
	}
	got, err := DecodeKeyframe(payload, true, ids, prev, scale)
	if err != nil {
		t.Fatalf("DecodeKeyframe: %v", err)
	}
	for id, want := range next {
		if diff := float64(got[id] - want); diff > 0.01 || diff < -0.01 {
			t.Fatalf("id %d: want ~%v got %v", id, want, got[id])
		}
	}
}

func TestKeyframeKDeltaBirth(t *testing.T) {
	ids := []uint64{1, 2}
	prev := map[uint64]float32{1: 5}
	next := map[uint64]float32{1: 5, 2: 7.5} // id 2 is a new birth, no prior value
	scale := float32(0.01)

	payload, _, err := EncodeKeyframe(ids, next, prev, false, scale)
	if err != nil {
		t.Fatalf("EncodeKeyframe: %v", err)
	}
	got, err := DecodeKeyframe(payload, true, ids, prev, scale)
	if err != nil {
		t.Fatalf("DecodeKeyframe: %v", err)
	}
	if diff := got[2] - 7.5; diff > 0.01 || diff < -0.01 {
		t.Fatalf("birth id 2: want ~7.5 got %v", got[2])
	}
}

func TestKeyframeKDeltaOrphanedDeltaRejected(t *testing.T) {
	// "unchanged" bit set for an id absent from prev must be rejected:
	// no orphaned deltas without a golden keyframe baseline.
	ids := []uint64{1}
	changed := []bool{false}
	payload := encodeChangedBitmap(changed)
	if _, err := DecodeKeyframe(payload, true, ids, map[uint64]float32{}, 1); err == nil {
		t.Fatal("want error for orphaned delta, got nil")
	}
}

func TestKeyframeUnsortedIDsRejected(t *testing.T) {
	ids := []uint64{2, 1}
	values := map[uint64]float32{1: 1, 2: 2}
	if _, _, err := EncodeKeyframe(ids, values, nil, true, 0); err == nil {
		t.Fatal("want error for unsorted ids, got nil")
	}
}

// TestKeyframeReplayIdempotent exercises the ADR-013 property required by
// the acceptance criteria: a golden keyframe followed by a chain of
// KDELTA keyframes reconstructs identical final state whether frames are
// applied strictly in seq order, or arrive late/duplicated/reordered and
// are then sorted by seq (deduping repeats) before being folded in.
func TestKeyframeReplayIdempotent(t *testing.T) {
	ids := []uint64{1, 2, 3, 4, 5}
	scale := float32(0.01)

	type step struct {
		seq     uint32
		values  map[uint64]float32
		golden  bool
		payload []byte
		flags   uint8
	}

	rng := rand.New(rand.NewSource(1))
	base := map[uint64]float32{1: 1, 2: 2, 3: 3, 4: 4, 5: 5}
	steps := []step{{seq: 0, values: cloneVals(base), golden: true}}
	cur := cloneVals(base)
	for seq := uint32(1); seq <= 6; seq++ {
		next := cloneVals(cur)
		// Mutate a couple of random ids each step.
		for i := 0; i < 2; i++ {
			id := ids[rng.Intn(len(ids))]
			next[id] = next[id] + float32(rng.Intn(21)-10)
		}
		steps = append(steps, step{seq: seq, values: next})
		cur = next
	}

	// Encode every step against the immediately preceding step's values.
	prevForStep := make([]map[uint64]float32, len(steps))
	for i, s := range steps {
		var prev map[uint64]float32
		if i > 0 {
			prev = steps[i-1].values
		}
		payload, flags, err := EncodeKeyframe(ids, s.values, prev, s.golden, scale)
		if err != nil {
			t.Fatalf("EncodeKeyframe step %d: %v", i, err)
		}
		steps[i].payload = payload
		steps[i].flags = flags
		prevForStep[i] = prev
	}

	replay := func(order []int) map[uint64]float32 {
		// Sort by seq and dedupe (keep first occurrence per seq),
		// exactly what a watermark/repair layer would do with
		// late/duplicate/reordered arrivals before folding into state.
		type arrival struct{ idx int }
		arrivals := make([]arrival, len(order))
		for i, idx := range order {
			arrivals[i] = arrival{idx}
		}
		sort.SliceStable(arrivals, func(i, j int) bool {
			return steps[arrivals[i].idx].seq < steps[arrivals[j].idx].seq
		})
		seen := map[uint32]bool{}
		var state map[uint64]float32
		for _, a := range arrivals {
			s := steps[a.idx]
			if seen[s.seq] {
				continue
			}
			seen[s.seq] = true
			kdelta := s.flags&FlagKDelta != 0
			var prev map[uint64]float32
			if kdelta {
				prev = state
			}
			decoded, err := DecodeKeyframe(s.payload, kdelta, ids, prev, scale)
			if err != nil {
				t.Fatalf("DecodeKeyframe seq=%d: %v", s.seq, err)
			}
			state = decoded
		}
		return state
	}

	inOrder := make([]int, len(steps))
	for i := range inOrder {
		inOrder[i] = i
	}
	want := replay(inOrder)

	// Shuffle + duplicate some entries to simulate late/duplicate/reorder.
	shuffled := append([]int(nil), inOrder...)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	withDupes := append(shuffled, shuffled[2], shuffled[0], shuffled[len(shuffled)-1])

	got := replay(withDupes)
	for id := range want {
		if diff := got[id] - want[id]; diff > 0.01 || diff < -0.01 {
			t.Fatalf("id %d: in-order gave %v, reordered/duplicated replay gave %v", id, want[id], got[id])
		}
	}
}

func cloneVals(m map[uint64]float32) map[uint64]float32 {
	out := make(map[uint64]float32, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
