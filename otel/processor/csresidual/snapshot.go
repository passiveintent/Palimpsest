/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"math"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// residualStats is an online (Welford) mean/variance estimator used to
// convert a logical series' raw residual into a local z-score
// (snapshot.residual_threshold is documented as a "local z-score"):
// baseline noise differs per series, so a single fixed absolute threshold
// would either miss anomalies on quiet series or false-positive on
// naturally noisy ones.
type residualStats struct {
	n    int
	mean float64
	m2   float64
}

// zscore folds x into the running statistics and returns its z-score
// against the mean/variance observed *before* this sample (so a single
// huge outlier is scored as an outlier rather than immediately absorbed
// into its own baseline). The first sample never scores (there is no
// variance yet). When the running variance is exactly zero (a perfectly
// flat series so far, e.g. a birth followed by identical steady-state
// windows), a sample equal to that flat baseline still scores 0, but any
// deviation from it is reported as maximally significant rather than
// silently returning 0 forever: with no variance on record, there is no
// finite basis to call a real deviation "not a spike", and a system whose
// very first sign of movement gets ignored defeats the point of watching
// it at all.
func (s *residualStats) zscore(x float64) float64 {
	s.n++
	if s.n == 1 {
		s.mean = x
		return 0
	}
	meanBefore, varBefore := s.mean, s.variance()
	delta := x - s.mean
	s.mean += delta / float64(s.n)
	s.m2 += delta * (x - s.mean)
	if varBefore <= 0 {
		if x == meanBefore {
			return 0
		}
		return math.Copysign(math.MaxFloat64, x-meanBefore)
	}
	return (x - meanBefore) / math.Sqrt(varBefore)
}

func (s *residualStats) variance() float64 {
	if s.n < 2 {
		return 0
	}
	return s.m2 / float64(s.n-1)
}

// stagedSnapshot is one group's ring-buffer content, captured because its
// residual crossed threshold, waiting to be picked up (merged into a
// frame.snapshot_blob) by a flush.
type stagedSnapshot struct {
	groupKey string
	stagedAt time.Time
	entries  []wire.SnapshotEntry
}

// snapshotStager implements the speculative-push side of ADR-009/ADR-012:
// residual > threshold stages a group's ring-buffer window; drain picks up
// to max_series_per_flush of the oldest staged windows per call (bounding
// how much gzip/merge work one flush does), carrying any excess over to
// later flushes; anything staged longer than blob_ttl without being picked
// is dropped rather than left to hang in memory (guardrail).
type snapshotStager struct {
	cfg     SnapshotConfig
	stats   map[uint64]*residualStats // dict id -> rolling residual stats
	pending []stagedSnapshot
}

func newSnapshotStager(cfg SnapshotConfig) *snapshotStager {
	return &snapshotStager{cfg: cfg, stats: make(map[uint64]*residualStats)}
}

// observe folds id's residual into its rolling stats and, if enabled and
// the resulting z-score crosses residual_threshold, stages groupKey's
// current ring-buffer content (fetched lazily, only on threshold-cross, to
// avoid encoding every group's buffer every window) for pickup by drain.
func (s *snapshotStager) observe(id uint64, groupKey string, residual float64, now time.Time, fetch func(groupKey string) []wire.SnapshotEntry) {
	if !s.cfg.Enabled {
		return
	}
	st, ok := s.stats[id]
	if !ok {
		st = &residualStats{}
		s.stats[id] = st
	}
	z := st.zscore(residual)
	if math.Abs(z) < s.cfg.ResidualThreshold {
		return
	}
	entries := fetch(groupKey)
	if len(entries) == 0 {
		return
	}
	s.pending = append(s.pending, stagedSnapshot{groupKey: groupKey, stagedAt: now, entries: entries})
}

// drain expires stagings older than blob_ttl (guardrail: unpicked
// snapshots do not hang in memory), picks up to max_series_per_flush of
// whatever remains (oldest first), and returns their entries merged into
// one list ready for a single wire.EncodeSnapshot call (docs/SPEC.md: one
// snapshot_blob per frame). Anything beyond the per-flush cap stays
// pending for the next drain call.
func (s *snapshotStager) drain(now time.Time) []wire.SnapshotEntry {
	kept := s.pending[:0]
	var merged []wire.SnapshotEntry
	picked := 0
	for _, staged := range s.pending {
		if s.cfg.BlobTTL > 0 && now.Sub(staged.stagedAt) > s.cfg.BlobTTL {
			continue
		}
		if picked >= s.cfg.MaxSeriesPerFlush {
			kept = append(kept, staged)
			continue
		}
		merged = append(merged, staged.entries...)
		picked++
	}
	s.pending = kept
	return merged
}
