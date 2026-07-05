// Package forensics implements ADR-009's "dashcam" snapshot store: when a
// window's recovery flags a series, its speculatively-pushed ring-buffer
// window (wire.SnapshotBlob) is decoded, filtered down to the flagged
// series, and held for retrieval — beating a 15-minute polling SLA to a
// ring buffer that would otherwise have already rotated past the incident
// by the time anyone thought to look.
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package forensics

import (
	"sort"
	"sync"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/wire"
)

// Snapshot is one series' persisted forensic window: the ring-buffer
// samples selected because SeriesID was flagged by a recovery.
type Snapshot struct {
	SeriesID   uint64
	CapturedAt time.Time // wall-clock time this Snapshot was persisted
	Entries    []wire.SnapshotEntry

	// sourceLatestMs is the newest ts_ms among Entries: Latency measures
	// from here, not CapturedAt, so it reflects end-to-end (sample ->
	// decode) delay rather than just the decode step.
	sourceLatestMs uint64
}

// Latency returns how long elapsed between Snapshot's freshest source
// sample and now (typically the moment it was persisted).
func (s Snapshot) Latency(now time.Time) time.Duration {
	if s.sourceLatestMs == 0 {
		return 0
	}
	return now.Sub(time.UnixMilli(int64(s.sourceLatestMs)))
}

// SnapshotStore holds recently-captured Snapshots, bounded by both count
// and age so a flood of flagged series (or an operator who never queries
// them) can't grow it without bound.
//
// SnapshotStore is safe for concurrent use.
type SnapshotStore struct {
	ttl      time.Duration
	maxCount int

	mu      sync.Mutex
	byID    map[uint64]Snapshot
	touched []uint64 // insertion/update order, oldest first, for capacity eviction
}

// NewSnapshotStore returns a SnapshotStore evicting entries older than ttl
// (<=0 disables age-based eviction) and bounding total entries to maxCount
// (<=0 disables count-based eviction).
func NewSnapshotStore(ttl time.Duration, maxCount int) *SnapshotStore {
	return &SnapshotStore{
		ttl:      ttl,
		maxCount: maxCount,
		byID:     make(map[uint64]Snapshot),
	}
}

// SelectAndPersist decodes blob (a wire snapshot blob, gzip-compressed per
// ADR-012's default), keeps only entries whose ID is in flaggedIDs, and
// upserts one Snapshot per surviving ID (most recent entries win),
// captured at wall-clock now. Returns the Snapshots newly persisted by this
// call.
func (s *SnapshotStore) SelectAndPersist(flaggedIDs []uint64, blob []byte, now time.Time) ([]Snapshot, error) {
	entries, err := decodeSnapshotBlob(blob)
	if err != nil {
		return nil, err
	}
	if len(flaggedIDs) == 0 || len(entries) == 0 {
		return nil, nil
	}
	flagged := make(map[uint64]bool, len(flaggedIDs))
	for _, id := range flaggedIDs {
		flagged[id] = true
	}

	byID := make(map[uint64][]wire.SnapshotEntry)
	for _, e := range entries {
		if flagged[e.ID] {
			byID[e.ID] = append(byID[e.ID], e)
		}
	}
	if len(byID) == 0 {
		return nil, nil
	}

	ids := make([]uint64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Snapshot, 0, len(ids))
	for _, id := range ids {
		es := byID[id]
		var latest uint64
		for _, e := range es {
			if e.TSMs > latest {
				latest = e.TSMs
			}
		}
		snap := Snapshot{SeriesID: id, CapturedAt: now, Entries: es, sourceLatestMs: latest}
		s.byID[id] = snap
		s.touched = append(s.touched, id)
		out = append(out, snap)
	}
	s.evictLocked(now)
	return out, nil
}

// Get returns the most recently persisted Snapshot for seriesID as of now,
// if any and if it hasn't expired.
func (s *SnapshotStore) Get(seriesID uint64, now time.Time) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.byID[seriesID]
	if !ok {
		return Snapshot{}, false
	}
	if s.ttl > 0 && now.Sub(snap.CapturedAt) > s.ttl {
		delete(s.byID, seriesID)
		return Snapshot{}, false
	}
	return snap, true
}

// Prune evicts every Snapshot older than ttl as of now, returning the
// number removed. Callers with their own housekeeping cadence may call this
// directly instead of relying on Get's lazy expiry.
func (s *SnapshotStore) Prune(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.byID)
	s.evictLocked(now)
	return before - len(s.byID)
}

// evictLocked drops age-expired entries, then trims to maxCount (oldest
// first) if still over capacity. Caller must hold s.mu.
func (s *SnapshotStore) evictLocked(now time.Time) {
	if s.ttl > 0 {
		for id, snap := range s.byID {
			if now.Sub(snap.CapturedAt) > s.ttl {
				delete(s.byID, id)
			}
		}
	}
	if s.maxCount > 0 {
		for len(s.byID) > s.maxCount && len(s.touched) > 0 {
			oldest := s.touched[0]
			s.touched = s.touched[1:]
			delete(s.byID, oldest)
		}
	}
	// Bound the touched slice itself so a store that's never at capacity
	// doesn't still grow this bookkeeping slice forever.
	if len(s.touched) > 4*max(s.maxCount, 1) {
		s.touched = append([]uint64(nil), s.touched[len(s.touched)-max(s.maxCount, 1):]...)
	}
}

func decodeSnapshotBlob(blob []byte) ([]wire.SnapshotEntry, error) {
	if entries, err := wire.DecodeSnapshot(blob, true); err == nil {
		return entries, nil
	}
	return wire.DecodeSnapshot(blob, false)
}
