/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import (
	"math"
	"sort"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/predict"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// Tracker manages the ephemeral dictionary lifecycle for one (emitter,
// epoch, view) stream (ADR-008): assigning stable IDs to logical series
// names, queuing dict_delta births, tombstoning series that go quiet, and
// feeding a Predictor's Seed/Evict lifecycle to match.
//
// Tracker is NOT safe for concurrent use.
type Tracker struct {
	pred    predict.Predictor // may be nil: dictionary-only tracking, no residuals
	breaker *Breaker          // may be nil: no birth-rate limiting

	active    map[uint64]struct{}
	name      map[uint64][]byte
	lastValue map[uint64]float64
	lastSeen  map[uint64]time.Time

	windowResiduals map[uint64]float64 // cleared by ResetWindow (ADR-013)
	pending         []wire.DictDelta   // births queued since last DrainDeltas
}

// NewTracker returns an empty Tracker. pred may be nil if the caller only
// needs dictionary lifecycle bookkeeping (no residual computation).
// breaker may be nil to disable birth-rate limiting.
func NewTracker(pred predict.Predictor, breaker *Breaker) *Tracker {
	return &Tracker{
		pred:            pred,
		breaker:         breaker,
		active:          make(map[uint64]struct{}),
		name:            make(map[uint64][]byte),
		lastValue:       make(map[uint64]float64),
		lastSeen:        make(map[uint64]time.Time),
		windowResiduals: make(map[uint64]float64),
	}
}

// Observe records one raw sample for the logical series named name at
// time now, returning its stable id, whether this call is its birth, and
// the residual to feed the sketch.
//
// On birth (isNew=true), residual is always 0: init_value=value becomes
// the series' baseline outright (via Predictor.Seed) with no sketch
// contribution, and a birth dict_delta is queued for DrainDeltas. Per
// ADR-009, a birth may instead be refused by the configured Breaker (see
// BreakerTripped) if the birth rate is too high; a refused birth reports
// isNew=false and residual=0, and does not grow the dictionary — callers
// should route it through an aggregate-only fallback instead.
//
// On a later Observe of the same id (isNew=false), residual reflects the
// Predictor's existing baseline (value - Predict(id)): the baseline is NOT
// re-seeded to value, matching the open-loop coding in ADR-003 — only a
// keyframe or a fresh birth (after a tombstone) moves it.
func (t *Tracker) Observe(name []byte, value float64, now time.Time) (id uint64, isNew bool, residual float64) {
	id = SeriesID(name)
	t.lastSeen[id] = now
	t.lastValue[id] = value

	if _, ok := t.active[id]; !ok {
		if t.breaker != nil && !t.breaker.ObserveBirth(id, now) {
			return id, false, 0
		}
		t.active[id] = struct{}{}
		nameCopy := append([]byte(nil), name...)
		t.name[id] = nameCopy
		if t.pred != nil {
			t.pred.Seed(id, value)
		}
		t.pending = append(t.pending, wire.DictDelta{ID: id, InitValue: float32(value), Name: nameCopy})
		t.windowResiduals[id] = 0
		return id, true, 0
	}

	if t.pred == nil {
		return id, false, 0
	}
	predicted, ok := t.pred.Predict(id)
	if !ok {
		// No baseline despite being an active dictionary entry (e.g. the
		// predictor's own state was reset independently): re-bootstrap
		// rather than compute a residual against nothing.
		t.pred.Seed(id, value)
		t.windowResiduals[id] = 0
		return id, false, 0
	}
	residual = value - predicted
	t.windowResiduals[id] = residual
	return id, false, residual
}

// Expire tombstones every active id last observed more than ttl before
// now: it removes them from the dictionary, evicts the Predictor's
// baseline for each, and returns the tombstone DictDeltas (ADR-008),
// sorted by ID.
func (t *Tracker) Expire(now time.Time, ttl time.Duration) []wire.DictDelta {
	var tombstones []wire.DictDelta
	for id, last := range t.lastSeen {
		if now.Sub(last) < ttl {
			continue
		}
		delete(t.active, id)
		delete(t.name, id)
		delete(t.lastValue, id)
		delete(t.lastSeen, id)
		delete(t.windowResiduals, id)
		if t.pred != nil {
			t.pred.Evict(id)
		}
		tombstones = append(tombstones, wire.DictDelta{ID: id, Flags: wire.DictFlagTombstone})
	}
	sort.Slice(tombstones, func(i, j int) bool { return tombstones[i].ID < tombstones[j].ID })
	return tombstones
}

// DrainDeltas returns and clears the birth DictDeltas queued by Observe
// since the last DrainDeltas call. Tombstones are not queued here: they
// are returned directly by Expire.
func (t *Tracker) DrainDeltas() []wire.DictDelta {
	out := t.pending
	t.pending = nil
	return out
}

// ActiveIDs returns the currently active series IDs, sorted ascending.
func (t *Tracker) ActiveIDs() []uint64 {
	ids := make([]uint64, 0, len(t.active))
	for id := range t.active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// FullDict returns a birth-style DictDelta for every currently active
// series (ID, most recently observed value, name), sorted by ID. This is
// for golden keyframes (ADR-008: "Every emitter MUST open each (emitter,
// epoch) stream with golden keyframe + full dict"), letting a decoder
// bootstrap its whole dictionary without replaying history.
func (t *Tracker) FullDict() []wire.DictDelta {
	ids := t.ActiveIDs()
	out := make([]wire.DictDelta, len(ids))
	for i, id := range ids {
		out[i] = wire.DictDelta{
			ID:        id,
			InitValue: float32(t.lastValue[id]),
			Name:      append([]byte(nil), t.name[id]...),
		}
	}
	return out
}

// CurrentValues returns each active series' most recently observed raw
// value (the same source FullDict uses for InitValue), keyed by ID. Unlike
// a Predictor's own baseline, this always reflects the latest Observe call
// regardless of birth/keyframe timing — callers rebuilding a keyframe
// payload or refreshing a Predictor's baseline via LoadKeyframe (ADR-003)
// should read from here, not from the Predictor, to avoid re-emitting a
// stale birth-time value.
func (t *Tracker) CurrentValues() map[uint64]float64 {
	out := make(map[uint64]float64, len(t.active))
	for id := range t.active {
		out[id] = t.lastValue[id]
	}
	return out
}

// TopKResiduals returns up to k active series' residuals from the current
// window (see ResetWindow), keyed by ID, ranked by absolute magnitude
// (ties broken by ID for determinism). Used to pick FALLBACK heavy-hitters
// (ADR-004) or named-series point-query candidates (ADR-009).
func (t *Tracker) TopKResiduals(k int) map[uint64]float64 {
	if k <= 0 || len(t.windowResiduals) == 0 {
		return map[uint64]float64{}
	}
	type kv struct {
		id uint64
		r  float64
	}
	all := make([]kv, 0, len(t.windowResiduals))
	for id, r := range t.windowResiduals {
		all = append(all, kv{id, r})
	}
	sort.Slice(all, func(i, j int) bool {
		ai, aj := math.Abs(all[i].r), math.Abs(all[j].r)
		if ai != aj {
			return ai > aj
		}
		return all[i].id < all[j].id
	})
	if k > len(all) {
		k = len(all)
	}
	out := make(map[uint64]float64, k)
	for _, e := range all[:k] {
		out[e.id] = e.r
	}
	return out
}

// ResetWindow clears the current window's transient residual snapshot
// (ADR-013): call it once at the start of each new window. Persistent
// dictionary state (active IDs, names, last values/seen times, breaker
// state) is untouched.
func (t *Tracker) ResetWindow() {
	t.windowResiduals = make(map[uint64]float64)
}

// BreakerTripped reports whether this Tracker's Breaker (if any) has
// tripped, flagging a churn/crash-loop event (ADR-009).
func (t *Tracker) BreakerTripped() bool {
	if t.breaker == nil {
		return false
	}
	return t.breaker.Tripped()
}
