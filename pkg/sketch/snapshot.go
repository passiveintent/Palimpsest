/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import (
	"fmt"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

type ringSample struct {
	tsMs  uint64
	value float64
}

// RingBuffer holds bounded, time-ordered instance-level samples for one
// series (ADR-008: "pod/instance labels live in an agent-side ring
// buffer" rather than being sketched). It backs the "dashcam" snapshots
// pushed speculatively when a residual crosses a threshold (ADR-009,
// ADR-012): the snapshot is the ring buffer's content at that moment, not
// just the current sample.
//
// Push, Snapshot, and Drain are deliberately isolated from each other:
// Push never evicts on the caller's behalf beyond RingBuffer's own
// construction-time window, Snapshot never mutates or drains the buffer,
// and Drain is the only way a caller-chosen TTL takes samples out — so a
// caller can push, decide later whether/when to snapshot, and separately
// decide on its own schedule when to drain for TTL.
//
// RingBuffer is NOT safe for concurrent use.
type RingBuffer struct {
	id      uint64
	window  time.Duration
	samples []ringSample
}

// defaultRingWindow is ADR-009's "default 15 min window" bound.
const defaultRingWindow = 15 * time.Minute

// NewRingBuffer returns a RingBuffer for series id, bounding memory to
// samples within window of the most recently pushed sample (see Push). A
// non-positive window falls back to the 15-minute default.
func NewRingBuffer(id uint64, window time.Duration) *RingBuffer {
	if window <= 0 {
		window = defaultRingWindow
	}
	return &RingBuffer{id: id, window: window}
}

// Push appends a sample. Samples older than window relative to ts_ms (this
// call's timestamp, not wall-clock time) are dropped, bounding the
// buffer's memory to its construction-time window without requiring the
// caller to separately call Drain.
func (rb *RingBuffer) Push(tsMs uint64, value float64) {
	rb.samples = append(rb.samples, ringSample{tsMs: tsMs, value: value})

	cutoff := int64(tsMs) - rb.window.Milliseconds()
	i := 0
	for i < len(rb.samples) && int64(rb.samples[i].tsMs) < cutoff {
		i++
	}
	if i > 0 {
		rb.samples = append(rb.samples[:0], rb.samples[i:]...)
	}
}

// Snapshot encodes the buffer's current contents to a wire snapshot blob
// (docs/SPEC.md snapshot_blob format), uncompressed. It does not modify or
// drain the buffer (see the isolation note on RingBuffer).
func (rb *RingBuffer) Snapshot() []byte {
	entries := make([]wire.SnapshotEntry, len(rb.samples))
	for i, s := range rb.samples {
		entries[i] = wire.SnapshotEntry{ID: rb.id, TSMs: s.tsMs, Value: float32(s.value)}
	}
	blob, err := wire.EncodeSnapshot(entries, false)
	if err != nil {
		// EncodeSnapshot only fails compressing (gzip disabled above);
		// reaching this indicates a wire package invariant broke.
		panic(fmt.Sprintf("sketch: unexpected snapshot encode error: %v", err))
	}
	return blob
}

// Drain removes samples older than ttl relative to wall-clock now,
// independent of Push's own window-based bound and of Snapshot (see the
// isolation note on RingBuffer): the caller decides if/when TTL expiry
// runs, e.g. on its own housekeeping cadence rather than on every push.
func (rb *RingBuffer) Drain(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl).UnixMilli()
	i := 0
	for i < len(rb.samples) && int64(rb.samples[i].tsMs) < cutoff {
		i++
	}
	if i == 0 {
		return
	}
	kept := make([]ringSample, len(rb.samples)-i)
	copy(kept, rb.samples[i:])
	rb.samples = kept
}
