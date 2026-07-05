/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"math/rand"
	"time"

	"github.com/purushpsm147/palimpsest/internal/audit"
	"github.com/purushpsm147/palimpsest/pkg/sketch"
)

// anomalyDurationWindows is how many consecutive windows one injected
// anomaly holds its perturbed value, mirroring
// internal/core/e2e_test.go's TestE2E_SteadyStateAnomalies-style sustained
// (not single-sample) spikes.
const anomalyDurationWindows = 2

// injectedAnomaly is one scheduled deviation: group g's agg aggregate is
// perturbed by +magnitude for [startWindow, startWindow+anomalyDurationWindows-1].
type injectedAnomaly struct {
	g           *group
	agg         string
	startWindow uint32
	magnitude   float64
}

// Scenario holds the anomaly-injection schedule, computed once at startup
// so ground truth (truth.jsonl) can be written before the run begins.
type Scenario struct {
	shardID           uint64
	baseline          float64
	instancesPerGroup int
	anomalies         []injectedAnomaly
}

// NewScenario schedules `count` anomalies (clamped to however many
// candidate groups World.ReserveForAnomaly can provide), spread evenly
// across [startFrac, endFrac] of totalWindows so there's a quiet baseline
// period up front and enough trailing windows for detection (and, for the
// windows watermark scenario, repair) to complete before the run ends.
// magnitude is an absolute delta added on top of whatever value the
// targeted aggregate would otherwise have reported.
func NewScenario(w *World, count int, totalWindows uint32, magnitude float64, rnd *rand.Rand) *Scenario {
	groups := w.ReserveForAnomaly(count)
	s := &Scenario{shardID: w.cfg.ShardID, baseline: w.cfg.Baseline, instancesPerGroup: w.cfg.InstancesPerGroup}
	if len(groups) == 0 || totalWindows == 0 {
		return s
	}

	const (
		startFrac = 0.15
		endFrac   = 0.75
	)
	first := uint32(float64(totalWindows) * startFrac)
	last := uint32(float64(totalWindows) * endFrac)
	if last <= first {
		last = first + 1
	}
	span := last - first

	aggChoices := [2]string{"sum", "max"}
	for i, g := range groups {
		var offset uint32
		if len(groups) > 1 {
			offset = uint32(uint64(i) * uint64(span) / uint64(len(groups)-1))
		}
		// Jitter within +/- an eighth of the per-anomaly spacing so
		// anomalies don't land on suspiciously round window boundaries.
		jitter := int(span) / (len(groups) + 1)
		if jitter > 0 {
			offset = uint32(int(offset) + rnd.Intn(2*jitter+1) - jitter)
		}
		start := first + offset
		agg := aggChoices[rnd.Intn(len(aggChoices))]
		s.anomalies = append(s.anomalies, injectedAnomaly{
			g:           g,
			agg:         agg,
			startWindow: start,
			magnitude:   magnitude,
		})
	}
	return s
}

// TruthEvents returns one TruthAnomaly record per scheduled anomaly,
// intended to be written to truth.jsonl once at startup (the run's ground
// truth is fixed in advance, not discovered as it plays out). Timestamp is
// the *predicted* real wall-clock time the anomaly's window will actually
// fire (runStart + startWindow*interval), not the write time — otherwise
// internal/audit's detection_latency would include the entire wait from
// process startup to the anomaly, not just detection delay.
func (s *Scenario) TruthEvents(runStart time.Time, interval time.Duration) []audit.TruthEvent {
	events := make([]audit.TruthEvent, 0, len(s.anomalies))
	for _, a := range s.anomalies {
		name := a.g.logicalName(s.shardID, a.agg)
		events = append(events, audit.TruthEvent{
			Type:          audit.TruthAnomaly,
			SeriesID:      sketch.SeriesID([]byte(name)),
			SeriesName:    name,
			ShardID:       s.shardID,
			WindowStart:   a.startWindow,
			WindowEnd:     a.startWindow + anomalyDurationWindows - 1,
			ExpectedValue: s.aggregateBaseline(a.agg) + a.magnitude,
			Magnitude:     a.magnitude,
			Timestamp:     runStart.Add(time.Duration(a.startWindow) * interval),
		})
	}
	return events
}

// aggregateBaseline is the steady-state value of agg before any
// anomaly/drift perturbation: sum scales with instance count, count/max do
// not (per ADR-008, count is the instance count itself and max is a single
// instance's value regardless of how many others exist).
func (s *Scenario) aggregateBaseline(agg string) float64 {
	if agg == "sum" {
		return s.baseline * float64(s.instancesPerGroup)
	}
	return s.baseline
}

// Apply perturbs values in place for whichever scheduled anomalies are
// active at window (additive: it does not replace the underlying
// sum/count/max computation, it overlays on top of it, matching how a real
// anomaly adds to — rather than substitutes for — normal traffic).
func (s *Scenario) Apply(window uint32, values map[*group]map[string]float64) {
	for _, a := range s.anomalies {
		if window < a.startWindow || window >= a.startWindow+anomalyDurationWindows {
			continue
		}
		agg, ok := values[a.g]
		if !ok {
			continue
		}
		agg[a.agg] += a.magnitude
	}
}
