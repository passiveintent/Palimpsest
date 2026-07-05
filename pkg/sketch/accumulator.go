/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import "math"

// Params configures an Accumulator's implicit measurement matrix.
type Params struct {
	M    int    // sketch width (number of buckets in y)
	D    int    // hashes per series (rows of the implicit Phi block)
	Seed uint64 // ephemeral per-epoch seed (see DeriveEphemeralSeed)
	Bits int    // quantization bits (8 or 16); carried for callers building RESIDUAL frames
}

// bucketEntry is a cached Buckets() result for one logical series: the d
// column indices and +1/-1 signs it projects onto in y.
type bucketEntry struct {
	idx  []uint32
	sign []int8
}

// Accumulator implements the encode-path side of ADR-002/ADR-003: each
// Update/UpdateByID call projects one series' residual onto its
// hash-implicit Phi columns and adds the contribution into the accumulated
// sketch vector y; Flush drains y (and reports its pre-quantization energy,
// ADR-004) once per window.
//
// Column assignments (Buckets) are cached per series ID the first time a
// series is seen via Update, so that steady-state traffic — the common
// case, where a window's series were already active in the previous window
// — can use the cheaper UpdateByID path and skip re-hashing the name.
//
// Accumulator is NOT safe for concurrent use: callers must serialize all
// Update/UpdateByID/Flush/Evict calls (e.g., behind a single per-emitter
// encoder goroutine).
type Accumulator struct {
	params   Params
	invSqrtD float64
	y        []float64
	buckets  map[uint64]bucketEntry
}

// NewAccumulator returns an Accumulator configured by p. p.M and p.D must
// be positive.
func NewAccumulator(p Params) *Accumulator {
	return &Accumulator{
		params:   p,
		invSqrtD: 1 / math.Sqrt(float64(p.D)),
		y:        make([]float64, p.M),
		buckets:  make(map[uint64]bucketEntry),
	}
}

// Params returns the Accumulator's configuration.
func (a *Accumulator) Params() Params { return a.params }

// Update adds residual for the logical series named name into y. The
// series' Phi columns are computed via Buckets on first sight and cached
// under SeriesID(name) for subsequent UpdateByID calls; repeat calls to
// Update itself always re-hash name (see BenchmarkUpdateUncached) — callers
// on the steady-state path should prefer UpdateByID once a series' ID is
// known (e.g., from Tracker.Observe).
func (a *Accumulator) Update(name []byte, residual float64) {
	id := SeriesID(name)
	entry, ok := a.buckets[id]
	if !ok {
		idx, sign := Buckets(name, a.params.Seed, a.params.M, a.params.D)
		entry = bucketEntry{idx: idx, sign: sign}
		a.buckets[id] = entry
	}
	a.add(entry, residual)
}

// UpdateByID adds residual for the logical series id into y using a
// previously cached bucket assignment (see Update). If id has no cached
// entry — it was never seen via Update, or was Evict-ed — UpdateByID is a
// no-op: callers must establish the cache via at least one Update(name, …)
// call per series (typically once, at birth, per ADR-008).
func (a *Accumulator) UpdateByID(id uint64, residual float64) {
	entry, ok := a.buckets[id]
	if !ok {
		return
	}
	a.add(entry, residual)
}

func (a *Accumulator) add(entry bucketEntry, residual float64) {
	contrib := residual * a.invSqrtD
	for i, idx := range entry.idx {
		if entry.sign[i] > 0 {
			a.y[idx] += contrib
		} else {
			a.y[idx] -= contrib
		}
	}
}

// Flush returns a copy of the accumulated sketch vector y and its
// pre-quantization energy ‖y‖²/m (ADR-004's storm-detection signal), then
// zeroes y for the next window. The bucket-assignment cache is untouched
// by Flush: it persists across windows within an epoch (see Update).
func (a *Accumulator) Flush() (y []float64, energy float64) {
	y = make([]float64, len(a.y))
	copy(y, a.y)
	var sumSq float64
	for _, v := range y {
		sumSq += v * v
	}
	if len(y) > 0 {
		energy = sumSq / float64(len(y))
	}
	for i := range a.y {
		a.y[i] = 0
	}
	return y, energy
}

// Evict drops id's cached bucket assignment (ADR-008 tombstone path). It
// does not touch y; any contribution id already made to the current
// window's accumulation remains until the next Flush.
func (a *Accumulator) Evict(id uint64) {
	delete(a.buckets, id)
}
