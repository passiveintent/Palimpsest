package sketch

import (
	"testing"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/wire"
)

func TestRingBufferSnapshotRoundTrip(t *testing.T) {
	rb := NewRingBuffer(42, time.Minute)
	rb.Push(1000, 1.5)
	rb.Push(2000, 2.5)

	entries, err := wire.DecodeSnapshot(rb.Snapshot(), false)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		if e.ID != 42 {
			t.Fatalf("entry ID = %d, want 42", e.ID)
		}
	}
	if entries[0].TSMs != 1000 || entries[0].Value != 1.5 {
		t.Fatalf("entries[0] = %+v, want {ID:42 TSMs:1000 Value:1.5}", entries[0])
	}
	if entries[1].TSMs != 2000 || entries[1].Value != 2.5 {
		t.Fatalf("entries[1] = %+v, want {ID:42 TSMs:2000 Value:2.5}", entries[1])
	}
}

// TestRingBufferSnapshotIsolation is the acceptance test: "Snapshot
// isolation: push does NOT consume ring buffer (caller owns when to drain
// for TTL)." Calling Snapshot repeatedly must be non-destructive.
func TestRingBufferSnapshotIsolation(t *testing.T) {
	rb := NewRingBuffer(1, time.Minute)
	rb.Push(0, 1)
	rb.Push(1, 2)

	first := rb.Snapshot()
	second := rb.Snapshot()
	if string(first) != string(second) {
		t.Fatalf("Snapshot is not idempotent:\n first=% x\nsecond=% x", first, second)
	}

	entries, err := wire.DecodeSnapshot(rb.Snapshot(), false)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Snapshot appears to have consumed samples: got %d entries, want 2", len(entries))
	}
}

// TestRingBufferPushBoundsMemory exercises Push's implicit
// construction-time window bound.
func TestRingBufferPushBoundsMemory(t *testing.T) {
	rb := NewRingBuffer(1, 100*time.Millisecond)
	rb.Push(0, 1)
	rb.Push(50, 2)
	rb.Push(250, 3) // cutoff = 250-100 = 150: both earlier samples drop

	entries, err := wire.DecodeSnapshot(rb.Snapshot(), false)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if len(entries) != 1 || entries[0].TSMs != 250 {
		t.Fatalf("entries = %+v, want exactly the ts=250 sample", entries)
	}
}

func TestRingBufferDefaultWindow(t *testing.T) {
	rb := NewRingBuffer(1, 0) // non-positive falls back to the 15-minute default
	rb.Push(0, 1)
	rb.Push(uint64((14 * time.Minute).Milliseconds()), 2)
	entries, err := wire.DecodeSnapshot(rb.Snapshot(), false)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected both samples retained within the 15-min default window, got %d", len(entries))
	}
}

// TestRingBufferDrain exercises the explicit, wall-clock-based TTL sweep,
// independent of Push's own window bound.
func TestRingBufferDrain(t *testing.T) {
	rb := NewRingBuffer(7, time.Hour)
	// These timestamps are ms-since-epoch far in the past relative to
	// wall-clock "now", so any reasonable ttl considers them stale.
	rb.Push(1000, 1)
	rb.Push(2000, 2)

	rb.Drain(time.Second)

	entries, err := wire.DecodeSnapshot(rb.Snapshot(), false)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Drain left %d stale entries, want 0", len(entries))
	}
}

func TestRingBufferDrainKeepsFreshSamples(t *testing.T) {
	rb := NewRingBuffer(7, time.Hour)
	nowMs := uint64(time.Now().UnixMilli())
	rb.Push(nowMs, 1) // fresh: pushed "now"

	rb.Drain(time.Minute)

	entries, err := wire.DecodeSnapshot(rb.Snapshot(), false)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Drain removed a fresh sample: got %d entries, want 1", len(entries))
	}
}
