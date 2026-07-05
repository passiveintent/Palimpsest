/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"math"

	"github.com/passiveintent/Palimpsest/pkg/recover"
	"github.com/passiveintent/Palimpsest/pkg/wire"

	"github.com/passiveintent/Palimpsest/internal/ports"
)

// Matcher implements ADR-009's five recovery substrates, translating
// decoded/recovered state into ports.AnomalyEvent values. Matcher itself is
// stateless (pure functions of its inputs) except for the drift baseline
// EvalKeyframe needs, so a single Matcher can be shared across every shard
// engine.go drives.
//
// Engine fills in the context fields (ShardID, EmitterID, Epoch, ViewID,
// WindowID, SeriesName, DetectedAt) after a Matcher call returns: Matcher's
// job is purely "is this deviating, and by how much", not "whose stream is
// this".
type Matcher struct {
	// DriftThreshold gates substrate (c): a keyframe-to-keyframe exact
	// value change with |cur-prev| > DriftThreshold is reported as slow
	// drift.
	DriftThreshold float64
}

// EvalDeviation implements substrate (a): every entry of an already
// residual-gated recover.Result's support is itself the anomaly signal (the
// support the solver believes were the true perturbed columns). Callers
// must not invoke this for a Result whose Residual failed the engine's
// max-residual gate, nor for ConfidenceFallback results (see EvalFallback).
func (m *Matcher) EvalDeviation(res recover.Result) []ports.AnomalyEvent {
	if res.Confidence == recover.ConfidenceFallback {
		return nil
	}
	events := make([]ports.AnomalyEvent, 0, len(res.SupportIDs))
	for i, id := range res.SupportIDs {
		events = append(events, ports.AnomalyEvent{
			Substrate:     "deviation",
			Confidence:    res.Confidence,
			SeriesID:      id,
			Value:         res.Values[i],
			Residual:      res.Residual,
			Coverage:      res.Coverage,
			CoverageTotal: res.CoverageTotal,
			Revision:      res.Revision,
		})
	}
	return events
}

// EvalNamedSeries implements substrate (b): the count-sketch point-query
// fast path for a single known series, bypassing full FISTA recovery.
// Returns the recovered value and its conservative error estimate
// (recover.PointQuery); callers decide their own significance threshold on
// the returned value.
func (m *Matcher) EvalNamedSeries(y []float64, idx []uint32, sign []int8) (value, errEstimate float64) {
	return recover.PointQuery(y, idx, sign)
}

// EvalKeyframe implements substrate (c): slow drift caught by comparing a
// freshly decoded keyframe's exact values against the immediately prior
// keyframe's exact values for the same IDs (ADR-011's "prev" map), flagging
// any whose absolute change exceeds DriftThreshold. This catches
// steady-state trends too gradual for any single window's residual to gate
// on, at keyframe-cadence latency (docs/SPEC.md).
func (m *Matcher) EvalKeyframe(cur, prev map[uint64]float32) []ports.AnomalyEvent {
	var events []ports.AnomalyEvent
	for id, v := range cur {
		pv, ok := prev[id]
		if !ok {
			continue // birth or first golden keyframe: no prior value to drift from
		}
		delta := math.Abs(float64(v - pv))
		if delta > m.DriftThreshold {
			events = append(events, ports.AnomalyEvent{
				Substrate:  "keyframe",
				Confidence: recover.ConfidenceKeyframe,
				SeriesID:   id,
				Value:      float64(v),
				Residual:   delta,
			})
		}
	}
	return events
}

// EvalAbsence implements substrate (d): a series' tombstoning (ADR-008) is
// itself reported as a distinct, informational absence event — separate
// from (and never counted as) a deviation false positive, since routine
// lifecycle churn (a pod dying on schedule) tombstones series constantly.
// Callers that only care about "this series went away unexpectedly" should
// filter/alert on these downstream rather than expect Matcher to know which
// absences are routine.
func (m *Matcher) EvalAbsence(deletedIDs []uint64) []ports.AnomalyEvent {
	events := make([]ports.AnomalyEvent, len(deletedIDs))
	for i, id := range deletedIDs {
		events[i] = ports.AnomalyEvent{
			Substrate:  "absence",
			Confidence: "absence",
			SeriesID:   id,
		}
	}
	return events
}

// EvalFallback implements the FALLBACK-frame path (ADR-004 storm): heavy
// hitters reported at low confidence rather than run through recovery,
// since a FALLBACK frame is emitted precisely when pre-quantization energy
// made recovery untrustworthy.
func (m *Matcher) EvalFallback(fb *wire.FallbackResult) []ports.AnomalyEvent {
	events := make([]ports.AnomalyEvent, 0, len(fb.Values))
	for id, v := range fb.Values {
		events = append(events, ports.AnomalyEvent{
			Substrate:  "fallback",
			Confidence: recover.ConfidenceFallback,
			SeriesID:   id,
			Value:      float64(v),
		})
	}
	return events
}
