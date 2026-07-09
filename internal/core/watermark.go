/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import "time"

// Watermark implements ADR-013's late/out-of-order frame handling for one
// (shard, epoch, view) coordinate system: a window (Frame.Seq) becomes
// eligible for its *initial* recovery once the stream's high-water mark
// (the highest window index observed so far) has advanced AllowedLateness
// windows past it, giving slightly-late frames for the same window (jitter,
// multiple contributing emitters) a chance to arrive first. A window that
// has already been recovered can still be *repaired*: a further frame
// arriving within RepairHorizon (wall-clock, measured from that window's
// first recovery) triggers a re-solve over the additively-merged sketch;
// beyond RepairHorizon, further frames for that window are too late and
// must be dropped.
//
// Watermark is NOT safe for concurrent use (mirrors the rest of this
// package's per-shard-mutex-guarded state).
type Watermark struct {
	AllowedLateness int           // windows
	RepairHorizon   time.Duration // wall-clock

	maxSeq     uint32
	haveMaxSeq bool
	marks      map[uint32]*windowMark
}

type windowMark struct {
	recoveredAt time.Time
	lateCount   int
}

// NewWatermark returns a Watermark configured by allowedLateness (windows)
// and repairHorizon (wall-clock duration).
func NewWatermark(allowedLateness int, repairHorizon time.Duration) *Watermark {
	return &Watermark{
		AllowedLateness: allowedLateness,
		RepairHorizon:   repairHorizon,
		marks:           make(map[uint32]*windowMark),
	}
}

// OnFrameArrival advances the watermark's high-water mark to account for a
// frame belonging to windowID arriving at wall-clock now, then reports how
// the caller should react:
//
//   - ready:   windowID has never been recovered and has now reached the
//     allowed-lateness threshold: run the initial Recover for it.
//   - repair:  windowID was already recovered, and now is still within
//     RepairHorizon of that first recovery: merge this frame's
//     contribution and re-solve, incrementing the revision counter.
//   - tooLate: windowID was already recovered, but RepairHorizon has
//     elapsed: the caller must drop this frame's contribution
//     entirely (not merge it) and count it.
//
// At most one of (ready, repair, tooLate) is ever true. Neither ready nor
// repair firing (all false) means: merge the contribution, but don't
// (re)solve yet — the window hasn't reached its watermark.
func (w *Watermark) OnFrameArrival(windowID uint32, now time.Time) (ready, repair, tooLate bool) {
	if !w.haveMaxSeq || windowID > w.maxSeq {
		w.maxSeq = windowID
		w.haveMaxSeq = true
	}

	m, ok := w.marks[windowID]
	if !ok {
		m = &windowMark{}
		w.marks[windowID] = m
	}

	if !m.recoveredAt.IsZero() {
		if w.RepairHorizon > 0 && now.Sub(m.recoveredAt) > w.RepairHorizon {
			m.lateCount++
			return false, false, true
		}
		m.lateCount++
		return false, true, false
	}

	if int(w.maxSeq)-int(windowID) >= w.AllowedLateness {
		return true, false, false
	}
	return false, false, false
}

// Ready reports whether windowID has reached the allowed-lateness
// threshold and has not yet been recovered, without recording an arrival.
// Used by the engine to sweep windows that became ready as a side effect
// of a *different* window's frame advancing the high-water mark.
func (w *Watermark) Ready(windowID uint32) bool {
	if m, ok := w.marks[windowID]; ok && !m.recoveredAt.IsZero() {
		return false
	}
	return int(w.maxSeq)-int(windowID) >= w.AllowedLateness
}

// MarkRecovered records that windowID was (re)solved at wall-clock now:
// the reference point RepairHorizon is measured from for future late
// arrivals.
func (w *Watermark) MarkRecovered(windowID uint32, now time.Time) {
	m, ok := w.marks[windowID]
	if !ok {
		m = &windowMark{}
		w.marks[windowID] = m
	}
	m.recoveredAt = now
}

// SummarizeWindowState reports windowID's late-arrival count and whether
// it has been recovered at least once.
func (w *Watermark) SummarizeWindowState(windowID uint32) (lateCount int, recovered bool) {
	m, ok := w.marks[windowID]
	if !ok {
		return 0, false
	}
	return m.lateCount, !m.recoveredAt.IsZero()
}

// Forget drops windowID's bookkeeping (called once it's safely outside
// RepairHorizon and its WindowState has been garbage collected), bounding
// Watermark's memory to in-flight/recently-repairable windows.
func (w *Watermark) Forget(windowID uint32) {
	delete(w.marks, windowID)
}

// ReadyWindows returns every tracked window index that has not yet been
// recovered but has now reached the allowed-lateness threshold, for the
// engine's ready-window sweep: a window can become ready as a side effect
// of a *different* window's frame advancing the high-water mark, not just
// its own direct arrival.
func (w *Watermark) ReadyWindows() []uint32 {
	var out []uint32
	for id, m := range w.marks {
		if m.recoveredAt.IsZero() && int(w.maxSeq)-int(id) >= w.AllowedLateness {
			out = append(out, id)
		}
	}
	return out
}

// RecoveredAt reports the wall-clock time windowID was first recovered, if
// it has been.
func (w *Watermark) RecoveredAt(windowID uint32) (time.Time, bool) {
	m, ok := w.marks[windowID]
	if !ok || m.recoveredAt.IsZero() {
		return time.Time{}, false
	}
	return m.recoveredAt, true
}
