/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

import (
	"math"
	"sort"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// dictEntry is one active series known to a Dictionary: its name (needed
// to recompute sketch.Buckets for BuildCSR) and its last exact value (from
// a birth's init_value, or a later applied keyframe — ADR-008's open-loop
// baseline, mirroring predict.Predictor on the decode side).
type dictEntry struct {
	name  []byte
	value float64
}

// Dictionary is the decode-side mirror of sketch.Tracker (ADR-008): it
// tracks which logical series are currently active for a (shard, epoch,
// view) coordinate system from a stream of dict_delta births/tombstones,
// so recovery can be restricted to a known candidate universe rather than
// the (unbounded) space of all possible series names. Tombstoned series
// are dropped outright: they never contribute a row to BuildCSR again
// (until a later birth re-adds the same ID), so they structurally cannot
// appear in a Recover result's SupportIDs.
//
// Dictionary also tracks per-window emitter presence for ADR-013's
// coverage annotation: a shard's sketch merges contributions from every
// emitter reporting into it, and coverage — how many of the emitters
// expected to report this window actually have — gates a Recover result's
// confidence (see ObserveEmitter/Coverage and fista.go's Recover).
//
// Dictionary is NOT safe for concurrent use.
type Dictionary struct {
	active          map[uint64]*dictEntry
	presentEmitters map[uint64]struct{}
}

// NewDictionary returns an empty Dictionary.
func NewDictionary() *Dictionary {
	return &Dictionary{
		active:          make(map[uint64]*dictEntry),
		presentEmitters: make(map[uint64]struct{}),
	}
}

// ApplyDelta applies one dict_delta (ADR-008). A tombstone removes dl.ID
// from the active set and reports birth=false. Otherwise dl.ID becomes (or
// remains) active with dl.Name/dl.InitValue as its known name/baseline,
// and birth reports whether this call transitioned it from not-active to
// active — true for a first-ever birth or a re-birth after a tombstone,
// matching sketch.Tracker.Observe's isNew semantics.
func (dict *Dictionary) ApplyDelta(dl wire.DictDelta) (birth bool) {
	if dl.IsTombstone() {
		delete(dict.active, dl.ID)
		return false
	}
	_, existed := dict.active[dl.ID]
	dict.active[dl.ID] = &dictEntry{
		name:  append([]byte(nil), dl.Name...),
		value: float64(dl.InitValue),
	}
	return !existed
}

// ApplyKeyframeValues updates the known baseline for every id in values
// (e.g. after decoding a KEYFRAME frame), keeping LastKeyframe's "prev" map
// in sync with what the encoder last confirmed exactly (ADR-011). IDs not
// already active are ignored: a keyframe only ever updates existing
// dictionary entries, never births one (births come from dict_delta via
// ApplyDelta).
func (dict *Dictionary) ApplyKeyframeValues(values map[uint64]float32) {
	for id, v := range values {
		if e, ok := dict.active[id]; ok {
			e.value = float64(v)
		}
	}
}

// ObserveEmitter records that emitterID's frame has been merged into the
// current recovery window (ADR-013). Coverage reports how many distinct
// emitters have been observed this way.
func (dict *Dictionary) ObserveEmitter(emitterID uint64) {
	dict.presentEmitters[emitterID] = struct{}{}
}

// Coverage reports (present, total) = (distinct emitters observed via
// ObserveEmitter, emittersExpected), matching ADR-013/docs/SPEC.md's
// "coverage = emitters_present / emitters_expected".
func (dict *Dictionary) Coverage(emittersExpected int) (present, total int) {
	return len(dict.presentEmitters), emittersExpected
}

// Len returns the number of currently active (non-tombstoned) series,
// without the allocation + sort ActiveIDs pays for the ordered ID slice —
// use this when only the count is needed (e.g. ADR-015's merged-trust
// guardrail, evaluated against N but never the IDs themselves).
func (dict *Dictionary) Len() int { return len(dict.active) }

// ActiveIDs returns the currently active (non-tombstoned) series IDs,
// sorted ascending. This is the row order BuildCSR uses, and the order
// Recover uses to correlate a recovered candidate index back to a series
// ID.
func (dict *Dictionary) ActiveIDs() []uint64 {
	ids := make([]uint64, 0, len(dict.active))
	for id := range dict.active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// VerifyKeyframe reports whether root matches ComputeDictRoot over the
// Dictionary's current active ID set (ADR-008's dict_root digest, checked
// decode-side against a keyframe frame's carried value).
func (dict *Dictionary) VerifyKeyframe(root uint64) bool {
	return wire.ComputeDictRoot(dict.ActiveIDs()) == root
}

// LastKeyframe returns a copy of the Dictionary's current per-ID baseline
// values (births' init_value, or ApplyKeyframeValues' updates): the "prev"
// map a subsequent KDELTA keyframe decode needs (wire.DecodeKeyframe).
func (dict *Dictionary) LastKeyframe() map[uint64]float64 {
	out := make(map[uint64]float64, len(dict.active))
	for id, e := range dict.active {
		out[id] = e.value
	}
	return out
}

// BuildCSR builds the Phi^T submatrix (ADR-002) over the Dictionary's
// currently active series, under the given epoch seed and (m, d) sketch
// parameters: row i is ActiveIDs()[i]'s d hash-implicit bucket/sign
// entries (sketch.Buckets), scaled by 1/sqrt(d) (docs/SPEC.md "Phi entry
// value = sign[i] / sqrt(d)").
func (dict *Dictionary) BuildCSR(seed uint64, m, d int) *CSR {
	ids := dict.ActiveIDs()
	n := len(ids)
	rowPtr := make([]int32, n+1)
	colIdx := make([]int32, n*d)
	vals := make([]float32, n*d)
	invSqrtD := float32(1 / math.Sqrt(float64(d)))

	for i, id := range ids {
		entry := dict.active[id]
		idx, sign := sketch.Buckets(entry.name, seed, m, d)
		base := i * d
		rowPtr[i] = int32(base)
		for k := 0; k < d; k++ {
			colIdx[base+k] = int32(idx[k])
			if sign[k] > 0 {
				vals[base+k] = invSqrtD
			} else {
				vals[base+k] = -invSqrtD
			}
		}
	}
	rowPtr[n] = int32(n * d)

	return &CSR{NRows: n, NCols: m, RowPtr: rowPtr, ColIdx: colIdx, Vals: vals}
}
