/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"testing"
	"time"
)

func TestWatermark_ReadyOnAllowedLateness(t *testing.T) {
	w := NewWatermark(3, time.Minute)
	now := time.Unix(1000, 0)

	for seq := uint32(0); seq < 3; seq++ {
		ready, repair, tooLate := w.OnFrameArrival(seq, now)
		if ready || repair || tooLate {
			t.Fatalf("seq %d: got (ready=%v repair=%v tooLate=%v), want all false (not yet at allowed lateness)", seq, ready, repair, tooLate)
		}
	}
	if w.Ready(0) {
		t.Fatalf("window 0 should not be ready yet (maxSeq=2, allowedLateness=3)")
	}

	// A frame for a later window (3) advances the high-water mark to 3;
	// window 0 becomes ready as a side effect, even though window 3
	// itself isn't ready yet (this is exactly what the engine's
	// ReadyWindows sweep exists to catch).
	ready, repair, tooLate := w.OnFrameArrival(3, now)
	if ready || repair || tooLate {
		t.Fatalf("seq 3 arrival: got (ready=%v repair=%v tooLate=%v), want all false (window 3 itself isn't ready yet)", ready, repair, tooLate)
	}
	if got := w.ReadyWindows(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("ReadyWindows() = %v, want [0]", got)
	}
}

func TestWatermark_RepairWithinHorizon(t *testing.T) {
	w := NewWatermark(2, time.Minute)
	now := time.Unix(2000, 0)

	w.OnFrameArrival(0, now)
	w.OnFrameArrival(1, now)
	w.OnFrameArrival(2, now)
	if !w.Ready(0) {
		t.Fatalf("window 0 should be ready once maxSeq reaches 2 (allowedLateness=2)")
	}
	w.MarkRecovered(0, now)

	later := now.Add(30 * time.Second)
	ready, repair, tooLate := w.OnFrameArrival(0, later)
	if ready || !repair || tooLate {
		t.Fatalf("late arrival within horizon: got (ready=%v repair=%v tooLate=%v), want repair=true only", ready, repair, tooLate)
	}
	lateCount, recovered := w.SummarizeWindowState(0)
	if !recovered || lateCount != 1 {
		t.Fatalf("SummarizeWindowState(0) = (%d, %v), want (1, true)", lateCount, recovered)
	}
}

func TestWatermark_TooLateBeyondHorizon(t *testing.T) {
	w := NewWatermark(1, 10*time.Second)
	now := time.Unix(3000, 0)

	w.OnFrameArrival(0, now)
	w.OnFrameArrival(1, now)
	w.MarkRecovered(0, now)

	beyond := now.Add(time.Minute)
	ready, repair, tooLate := w.OnFrameArrival(0, beyond)
	if ready || repair || !tooLate {
		t.Fatalf("arrival beyond repair horizon: got (ready=%v repair=%v tooLate=%v), want tooLate=true only", ready, repair, tooLate)
	}
}

func TestWatermark_ForgetDropsBookkeeping(t *testing.T) {
	w := NewWatermark(1, time.Minute)
	now := time.Unix(4000, 0)
	w.OnFrameArrival(5, now)
	if _, ok := w.RecoveredAt(5); ok {
		t.Fatalf("window 5 should not be recovered yet")
	}
	w.MarkRecovered(5, now)
	if _, ok := w.RecoveredAt(5); !ok {
		t.Fatalf("window 5 should be recovered")
	}
	w.Forget(5)
	if _, ok := w.RecoveredAt(5); ok {
		t.Fatalf("window 5 bookkeeping should be gone after Forget")
	}
}
