/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package forensics

import (
	"errors"
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

func encodeBlob(t *testing.T, entries []wire.SnapshotEntry, codec wire.Codec) []byte {
	t.Helper()
	blob, err := wire.EncodeSnapshot(entries, codec)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	return blob
}

func TestSelectAndPersist_FiltersToFlaggedIDs(t *testing.T) {
	s := NewSnapshotStore(time.Hour, 100)
	entries := []wire.SnapshotEntry{
		{ID: 1, TSMs: 1000, Value: 1.5},
		{ID: 1, TSMs: 2000, Value: 1.6},
		{ID: 2, TSMs: 1500, Value: 9.0},
		{ID: 3, TSMs: 1200, Value: 3.0},
	}
	blob := encodeBlob(t, entries, wire.CodecGzip)

	now := time.Unix(100, 0)
	snaps, err := s.SelectAndPersist([]uint64{1, 3}, blob, wire.CodecGzip, now)
	if err != nil {
		t.Fatalf("SelectAndPersist: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("len(snaps) = %d, want 2 (ids 1 and 3, id 2 not flagged)", len(snaps))
	}

	snap1, ok := s.Get(1, now)
	if !ok {
		t.Fatalf("Get(1): not found")
	}
	if len(snap1.Entries) != 2 {
		t.Fatalf("snap1.Entries = %d, want 2", len(snap1.Entries))
	}
	if _, ok := s.Get(2, now); ok {
		t.Fatalf("Get(2): found, want absent (never flagged)")
	}

	latency := snap1.Latency(now.Add(5 * time.Second))
	if latency <= 0 {
		t.Fatalf("Latency = %v, want > 0 (measured from freshest source sample)", latency)
	}
}

func TestSelectAndPersist_UncompressedBlob(t *testing.T) {
	s := NewSnapshotStore(time.Hour, 100)
	entries := []wire.SnapshotEntry{{ID: 42, TSMs: 500, Value: 2.0}}
	blob := encodeBlob(t, entries, wire.CodecNone)

	snaps, err := s.SelectAndPersist([]uint64{42}, blob, wire.CodecNone, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("SelectAndPersist: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("len(snaps) = %d, want 1", len(snaps))
	}
}

func TestSnapshotStore_TTLEviction(t *testing.T) {
	s := NewSnapshotStore(time.Minute, 100)
	blob := encodeBlob(t, []wire.SnapshotEntry{{ID: 1, TSMs: 0, Value: 1}}, wire.CodecGzip)
	start := time.Unix(1000, 0)
	if _, err := s.SelectAndPersist([]uint64{1}, blob, wire.CodecGzip, start); err != nil {
		t.Fatalf("SelectAndPersist: %v", err)
	}

	if _, ok := s.Get(1, start); !ok {
		t.Fatalf("Get(1) immediately after persist: not found")
	}

	removed := s.Prune(start.Add(2 * time.Minute))
	if removed != 1 {
		t.Fatalf("Prune removed %d, want 1", removed)
	}
	if _, ok := s.Get(1, start.Add(2*time.Minute)); ok {
		t.Fatalf("Get(1) after TTL expiry: found, want absent")
	}
}

func TestSnapshotStore_CapacityEviction(t *testing.T) {
	s := NewSnapshotStore(0, 2)
	now := time.Unix(1, 0)
	for id := uint64(1); id <= 3; id++ {
		blob := encodeBlob(t, []wire.SnapshotEntry{{ID: id, TSMs: 0, Value: 1}}, wire.CodecGzip)
		if _, err := s.SelectAndPersist([]uint64{id}, blob, wire.CodecGzip, now); err != nil {
			t.Fatalf("SelectAndPersist(%d): %v", id, err)
		}
	}
	if _, ok := s.Get(1, now); ok {
		t.Fatalf("Get(1): found, want evicted (oldest, over capacity 2)")
	}
	if _, ok := s.Get(3, now); !ok {
		t.Fatalf("Get(3): not found, want present (most recent)")
	}
}

func TestSelectAndPersist_NoMatchesReturnsNil(t *testing.T) {
	s := NewSnapshotStore(time.Hour, 10)
	blob := encodeBlob(t, []wire.SnapshotEntry{{ID: 1, TSMs: 0, Value: 1}}, wire.CodecGzip)
	snaps, err := s.SelectAndPersist([]uint64{999}, blob, wire.CodecGzip, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("SelectAndPersist: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("len(snaps) = %d, want 0", len(snaps))
	}
}

func TestSelectAndPersist_UnregisteredCodec(t *testing.T) {
	s := NewSnapshotStore(time.Hour, 10)
	blob := encodeBlob(t, []wire.SnapshotEntry{{ID: 1, TSMs: 0, Value: 1}}, wire.CodecGzip)
	const unregistered wire.Codec = 99
	if _, err := s.SelectAndPersist([]uint64{1}, blob, unregistered, time.Unix(1, 0)); !errors.Is(err, wire.ErrUnregisteredCodec) {
		t.Fatalf("SelectAndPersist(unregistered codec): want ErrUnregisteredCodec, got %v", err)
	}
}
