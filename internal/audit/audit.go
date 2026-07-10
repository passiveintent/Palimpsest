/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package audit

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/passiveintent/Palimpsest/internal/ports"
)

// revisionTolerance is how close a repaired detection's final value must
// land to a truth event's ExpectedValue (as a fraction of |ExpectedValue|)
// to count as an accurate revision.
const revisionTolerance = 0.10

// groupKey canonicalizes repeated emissions of the same underlying
// detection: solveWindow emits a fresh AnomalyEvent per SupportID on
// *every* ADR-013 watermark revision, so the same real detection can
// appear more than once for a repaired window. Scoring raw events instead
// of this canonical key would double- (or triple-) count precision/recall
// for exactly the runs (partition/duplicate/reorder) this project most
// wants to demonstrate correct handling of.
type groupKey struct {
	ShardID  uint64
	ViewID   uint16
	SeriesID uint64
	WindowID uint32
}

// detectionGroup is one canonical detection's accumulated state across
// every AnomalyEvent Observe has seen for its groupKey.
type detectionGroup struct {
	firstDetectedAt    time.Time
	firstSubstrate     string
	maxRevision        int
	valueAtMaxRevision float64
}

// Auditor scores palimpsestd's emitted ports.AnomalyEvents against a
// synthetic workload's ground truth (see TruthEvent). It is safe for
// concurrent use: Observe is called from the anomaly-sink path, Report and
// Reload from a separate reporting goroutine.
type Auditor struct {
	mu sync.Mutex

	truth      []TruthEvent
	bySeriesID map[uint64][]int // index into truth, by SeriesID

	detections map[groupKey]*detectionGroup

	publishOnce sync.Once
}

// NewAuditor loads truthPath and returns an Auditor ready to score events
// against it. Call Reload periodically if the truth file may still be
// growing (e.g. a concurrently-running plsim).
func NewAuditor(truthPath string) (*Auditor, error) {
	a := &Auditor{detections: make(map[groupKey]*detectionGroup)}
	if err := a.Reload(truthPath); err != nil {
		return nil, err
	}
	return a, nil
}

// Reload re-reads truthPath and atomically replaces the truth set. Safe to
// call concurrently with Observe/Report.
func (a *Auditor) Reload(truthPath string) error {
	events, err := LoadTruth(truthPath)
	if err != nil {
		return err
	}
	bySeriesID := make(map[uint64][]int, len(events))
	for i, ev := range events {
		bySeriesID[ev.SeriesID] = append(bySeriesID[ev.SeriesID], i)
	}

	a.mu.Lock()
	a.truth = events
	a.bySeriesID = bySeriesID
	a.mu.Unlock()
	return nil
}

// Observe scores one emitted AnomalyEvent, folding it into its canonical
// detection group. "absence" events (ADR-008 tombstone confirmations) are
// intentionally excluded: they are not deviation-style detections, and are
// only ever used (via the truth log's own birth/tombstone records, not
// these events) to explain away a lifecycle false positive.
func (a *Auditor) Observe(ev ports.AnomalyEvent) {
	if ev.Substrate == "absence" {
		return
	}

	key := groupKey{ShardID: ev.ShardID, ViewID: ev.ViewID, SeriesID: ev.SeriesID, WindowID: ev.WindowID}

	a.mu.Lock()
	defer a.mu.Unlock()

	g, ok := a.detections[key]
	if !ok {
		a.detections[key] = &detectionGroup{
			firstDetectedAt:    ev.DetectedAt,
			firstSubstrate:     ev.Substrate,
			maxRevision:        ev.Revision,
			valueAtMaxRevision: ev.Value,
		}
		return
	}
	if ev.DetectedAt.Before(g.firstDetectedAt) {
		g.firstDetectedAt = ev.DetectedAt
		g.firstSubstrate = ev.Substrate
	}
	if ev.Revision >= g.maxRevision {
		g.maxRevision = ev.Revision
		g.valueAtMaxRevision = ev.Value
	}
}

// Report computes a fresh scoring snapshot from every detection group
// observed so far against the current truth set.
func (a *Auditor) Report() Report {
	a.mu.Lock()
	defer a.mu.Unlock()

	rpt := Report{
		GeneratedAt:                 time.Now(),
		RecallByType:                make(map[string]TypeRecall),
		DetectionLatencyBySubstrate: make(map[string]SubstrateLatency),
	}

	recalledTruth := make(map[int]bool)
	latencySamples := make(map[string][]float64)

	var (
		tp, fp                     int
		lifecycleFP                int
		mapeSum                    float64
		mapeCount                  int
		revisionMatches, revisionN int
	)

	for key, g := range a.detections {
		idx, matched := a.matchAnomalyTruth(key.SeriesID, key.WindowID)
		if !matched {
			fp++
			if g.firstSubstrate == "deviation" && a.nearLifecycleEvent(key.SeriesID, key.WindowID) {
				lifecycleFP++
			}
			continue
		}

		tp++
		recalledTruth[idx] = true
		t := a.truth[idx]

		latencySamples[g.firstSubstrate] = append(latencySamples[g.firstSubstrate], g.firstDetectedAt.Sub(t.Timestamp).Seconds())

		// MAPE and revision accuracy both need a single well-defined
		// "true value at the time of this detection" to compare against.
		// That holds for "anomaly" truth (a constant magnitude for its
		// whole, short window range) but not "drift": a drift truth
		// record's ExpectedValue/Magnitude describe the *cumulative*
		// change by the run's final window, while a real detection can
		// land at any window along that continuously-growing trend —
		// comparing an early-window value against the final target
		// produces a meaningless error. Drift still counts fully for
		// recall/latency above; it just isn't scored for value accuracy.
		if t.Type != TruthAnomaly {
			continue
		}

		expected := expectedValueFor(g.firstSubstrate, t)
		if expected != 0 {
			mapeSum += math.Abs(g.valueAtMaxRevision-expected) / math.Abs(expected)
			mapeCount++
		}

		if g.maxRevision > 0 {
			revisionN++
			tol := math.Abs(expected) * revisionTolerance
			if tol == 0 {
				tol = 1e-9
			}
			if math.Abs(g.valueAtMaxRevision-expected) <= tol {
				revisionMatches++
			}
		}
	}

	rpt.Detections = len(a.detections)
	rpt.TruePositives = tp
	rpt.FalsePositives = fp
	rpt.LifecycleFalsePositives = lifecycleFP
	if tp+fp > 0 {
		v := float64(tp) / float64(tp+fp)
		rpt.Precision = &v
	}

	type typeTotals struct{ truth, recalled int }
	byType := make(map[TruthEventType]*typeTotals)
	var totalTruth, totalRecalled int
	for i, t := range a.truth {
		if !t.IsAnomalyLike() {
			continue
		}
		tt, ok := byType[t.Type]
		if !ok {
			tt = &typeTotals{}
			byType[t.Type] = tt
		}
		tt.truth++
		totalTruth++
		if recalledTruth[i] {
			tt.recalled++
			totalRecalled++
		}
	}
	for typ, tt := range byType {
		tr := TypeRecall{Truth: tt.truth, Recalled: tt.recalled}
		if tt.truth > 0 {
			v := float64(tt.recalled) / float64(tt.truth)
			tr.Recall = &v
		}
		rpt.RecallByType[string(typ)] = tr
	}
	rpt.TruthAnomalies = totalTruth
	if totalTruth > 0 {
		v := float64(totalRecalled) / float64(totalTruth)
		rpt.Recall = &v
	}

	if mapeCount > 0 {
		v := mapeSum / float64(mapeCount)
		rpt.ValueMAPE = &v
	}
	rpt.ValueMAPESamples = mapeCount

	for substrate, samples := range latencySamples {
		rpt.DetectionLatencyBySubstrate[substrate] = latencyStats(samples)
	}

	if revisionN > 0 {
		v := float64(revisionMatches) / float64(revisionN)
		rpt.RevisionAccuracy = &v
	}
	rpt.RevisionAccuracySamples = revisionN

	return rpt
}

// expectedValueFor picks which of t's two magnitude fields is comparable
// to an AnomalyEvent.Value from substrate, since AnomalyEvent.Value's
// shape differs by substrate: Matcher.EvalDeviation reports a recovered
// *residual* (value minus predicted baseline — the sketch domain is
// residuals, per ADR-003), while Matcher.EvalKeyframe reports the
// keyframe's *absolute* exact value. Comparing a residual against
// t.ExpectedValue (an absolute value) would produce a meaninglessly large
// MAPE — t.Magnitude (the injected delta, which is what a residual
// approximates) is the correct comparison for anything but "keyframe".
func expectedValueFor(substrate string, t TruthEvent) float64 {
	if substrate == "keyframe" {
		return t.ExpectedValue
	}
	return t.Magnitude
}

// matchAnomalyTruth reports the first anomaly/drift truth record (by
// index into a.truth) overlapping windowID for seriesID, if any. Caller
// must hold a.mu.
func (a *Auditor) matchAnomalyTruth(seriesID uint64, windowID uint32) (int, bool) {
	for _, idx := range a.bySeriesID[seriesID] {
		t := a.truth[idx]
		if t.IsAnomalyLike() && t.Overlaps(windowID) {
			return idx, true
		}
	}
	return 0, false
}

// nearLifecycleEvent reports whether seriesID has a birth/tombstone truth
// record within one window of windowID. Caller must hold a.mu.
func (a *Auditor) nearLifecycleEvent(seriesID uint64, windowID uint32) bool {
	for _, idx := range a.bySeriesID[seriesID] {
		t := a.truth[idx]
		if t.Type != TruthBirth && t.Type != TruthTombstone {
			continue
		}
		d := int64(windowID) - int64(t.WindowStart)
		if d >= -1 && d <= 1 {
			return true
		}
	}
	return false
}

// latencyStats computes count/mean/p95 over samples (wall-clock seconds).
func latencyStats(samples []float64) SubstrateLatency {
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)

	sum := 0.0
	for _, s := range sorted {
		sum += s
	}
	p95 := sorted[len(sorted)-1]
	if idx := int(float64(len(sorted)) * 0.95); idx < len(sorted) {
		p95 = sorted[idx]
	}
	return SubstrateLatency{
		Count:       len(sorted),
		MeanSeconds: sum / float64(len(sorted)),
		P95Seconds:  p95,
	}
}
