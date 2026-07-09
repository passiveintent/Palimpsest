/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package audit

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/internal/ports"
)

func newTestAuditor(t *testing.T, events []TruthEvent) *Auditor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "truth.jsonl")
	w, err := NewTruthWriter(path)
	if err != nil {
		t.Fatalf("NewTruthWriter: %v", err)
	}
	for _, ev := range events {
		if err := w.Write(ev); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	a, err := NewAuditor(path)
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	return a
}

func TestAuditor_EmptyReportHasNoDataNotZero(t *testing.T) {
	a := newTestAuditor(t, nil)
	rpt := a.Report()
	if rpt.Precision != nil {
		t.Fatalf("Precision = %v, want nil (no detections)", *rpt.Precision)
	}
	if rpt.Recall != nil {
		t.Fatalf("Recall = %v, want nil (no truth)", *rpt.Recall)
	}
	if rpt.ValueMAPE != nil {
		t.Fatalf("ValueMAPE = %v, want nil", *rpt.ValueMAPE)
	}
	if rpt.RevisionAccuracy != nil {
		t.Fatalf("RevisionAccuracy = %v, want nil", *rpt.RevisionAccuracy)
	}
	if rpt.LifecycleFalsePositives != 0 {
		t.Fatalf("LifecycleFalsePositives = %d, want 0", rpt.LifecycleFalsePositives)
	}
}

func TestAuditor_BasicPrecisionAndRecall(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	truth := []TruthEvent{
		{Type: TruthAnomaly, SeriesID: 1, ShardID: 9, WindowStart: 10, WindowEnd: 11, ExpectedValue: 160, Magnitude: 60, Timestamp: base},
		{Type: TruthAnomaly, SeriesID: 2, ShardID: 9, WindowStart: 20, WindowEnd: 21, ExpectedValue: 160, Magnitude: 60, Timestamp: base},
	}
	a := newTestAuditor(t, truth)

	// TP: series 1 detected inside its truth window. A "deviation" event's
	// Value is a recovered *residual* (EvalDeviation, ADR-003's sketch
	// domain), comparable to Magnitude (the injected delta), not
	// ExpectedValue (the absolute post-anomaly value) — see
	// expectedValueFor's doc comment.
	a.Observe(ports.AnomalyEvent{Substrate: "deviation", ShardID: 9, SeriesID: 1, WindowID: 10, Value: 58, DetectedAt: base.Add(11 * time.Second)})
	// FP: series 3 has no truth record at all.
	a.Observe(ports.AnomalyEvent{Substrate: "deviation", ShardID: 9, SeriesID: 3, WindowID: 5, Value: 50, DetectedAt: base})
	// series 2's truth anomaly is never detected -> recall miss.

	rpt := a.Report()
	if rpt.Detections != 2 {
		t.Fatalf("Detections = %d, want 2", rpt.Detections)
	}
	if rpt.TruePositives != 1 || rpt.FalsePositives != 1 {
		t.Fatalf("TP/FP = %d/%d, want 1/1", rpt.TruePositives, rpt.FalsePositives)
	}
	if rpt.Precision == nil || *rpt.Precision != 0.5 {
		t.Fatalf("Precision = %v, want 0.5", rpt.Precision)
	}
	if rpt.Recall == nil || *rpt.Recall != 0.5 {
		t.Fatalf("Recall = %v, want 0.5 (1 of 2 truth anomalies caught)", rpt.Recall)
	}
	if rpt.ValueMAPE == nil {
		t.Fatalf("ValueMAPE = nil, want a value (one TP with nonzero Magnitude)")
	}
	wantMAPE := (60.0 - 58.0) / 60.0
	if diff := *rpt.ValueMAPE - wantMAPE; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("ValueMAPE = %v, want %v", *rpt.ValueMAPE, wantMAPE)
	}
}

// TestAuditor_CanonicalizesAcrossRevisions is the regression test for the
// Plan-agent-identified gap: solveWindow emits a fresh AnomalyEvent per
// SupportID on every ADR-013 watermark revision, so the same real
// detection can appear 2+ times for a repaired window. Scoring raw events
// would double-count precision/recall/latency for exactly the
// partition/duplicate/reorder scenarios this project needs to get right.
func TestAuditor_CanonicalizesAcrossRevisions(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	truth := []TruthEvent{
		{Type: TruthAnomaly, SeriesID: 1, ShardID: 9, WindowStart: 10, WindowEnd: 10, ExpectedValue: 160, Magnitude: 60, Timestamp: base},
	}
	a := newTestAuditor(t, truth)

	// Initial solve (revision 0), arrives first. A "deviation" event's
	// Value is a recovered residual, comparable to Magnitude (60), not
	// ExpectedValue (160) — see expectedValueFor's doc comment.
	a.Observe(ports.AnomalyEvent{
		Substrate: "deviation", ShardID: 9, SeriesID: 1, WindowID: 10,
		Value: 40, Revision: 0, DetectedAt: base.Add(15 * time.Second),
	})
	// Repair (revision 1) for the SAME (shard, view, series, window), a
	// more accurate value, arriving later.
	a.Observe(ports.AnomalyEvent{
		Substrate: "deviation", ShardID: 9, SeriesID: 1, WindowID: 10,
		Value: 59, Revision: 1, DetectedAt: base.Add(30 * time.Second),
	})

	rpt := a.Report()
	if rpt.Detections != 1 {
		t.Fatalf("Detections = %d, want 1 (both events are the same underlying detection)", rpt.Detections)
	}
	if rpt.TruePositives != 1 {
		t.Fatalf("TruePositives = %d, want 1 (not 2)", rpt.TruePositives)
	}
	// MAPE must use the LATEST (highest-revision) value, 59, not the
	// stale initial 40.
	wantMAPE := (60.0 - 59.0) / 60.0
	if rpt.ValueMAPE == nil || *rpt.ValueMAPE != wantMAPE {
		t.Fatalf("ValueMAPE = %v, want %v (must use revision 1's value)", rpt.ValueMAPE, wantMAPE)
	}
	// Latency must use the EARLIEST detection time (revision 0's arrival),
	// not the later repair.
	lat, ok := rpt.DetectionLatencyBySubstrate["deviation"]
	if !ok || lat.Count != 1 {
		t.Fatalf("DetectionLatencyBySubstrate[deviation] = %+v, want one sample", lat)
	}
	if wantLatency := 15.0; lat.MeanSeconds != wantLatency {
		t.Fatalf("latency = %v, want %v (from revision 0's earlier DetectedAt)", lat.MeanSeconds, wantLatency)
	}
	// Revision accuracy: this group did receive a repair (maxRevision=1)
	// and its final value (159) is well within tolerance of 160.
	if rpt.RevisionAccuracySamples != 1 {
		t.Fatalf("RevisionAccuracySamples = %d, want 1", rpt.RevisionAccuracySamples)
	}
	if rpt.RevisionAccuracy == nil || *rpt.RevisionAccuracy != 1.0 {
		t.Fatalf("RevisionAccuracy = %v, want 1.0", rpt.RevisionAccuracy)
	}
}

func TestAuditor_LifecycleFalsePositive(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	truth := []TruthEvent{
		{Type: TruthBirth, SeriesID: 42, ShardID: 1, WindowStart: 5, Timestamp: base},
	}
	a := newTestAuditor(t, truth)

	// A spurious deviation for the same series, one window after its birth.
	a.Observe(ports.AnomalyEvent{Substrate: "deviation", ShardID: 1, SeriesID: 42, WindowID: 6, Value: 1, DetectedAt: base})

	rpt := a.Report()
	if rpt.FalsePositives != 1 {
		t.Fatalf("FalsePositives = %d, want 1", rpt.FalsePositives)
	}
	if rpt.LifecycleFalsePositives != 1 {
		t.Fatalf("LifecycleFalsePositives = %d, want 1", rpt.LifecycleFalsePositives)
	}
}

func TestAuditor_FarFromLifecycleEventIsOrdinaryFalsePositive(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	truth := []TruthEvent{
		{Type: TruthBirth, SeriesID: 42, ShardID: 1, WindowStart: 5, Timestamp: base},
	}
	a := newTestAuditor(t, truth)

	// Same series, but far (in window terms) from its birth: an ordinary
	// false positive, not a lifecycle one.
	a.Observe(ports.AnomalyEvent{Substrate: "deviation", ShardID: 1, SeriesID: 42, WindowID: 50, Value: 1, DetectedAt: base})

	rpt := a.Report()
	if rpt.FalsePositives != 1 {
		t.Fatalf("FalsePositives = %d, want 1", rpt.FalsePositives)
	}
	if rpt.LifecycleFalsePositives != 0 {
		t.Fatalf("LifecycleFalsePositives = %d, want 0 (window 50 is far from birth at window 5)", rpt.LifecycleFalsePositives)
	}
}

func TestAuditor_AbsenceEventsExcluded(t *testing.T) {
	a := newTestAuditor(t, nil)
	a.Observe(ports.AnomalyEvent{Substrate: "absence", SeriesID: 1, WindowID: 1, DetectedAt: time.Now()})

	rpt := a.Report()
	if rpt.Detections != 0 {
		t.Fatalf("Detections = %d, want 0 (absence events are not scored detections)", rpt.Detections)
	}
}

// TestAuditor_DriftExcludedFromMAPEAndRevisionAccuracy guards the fix for a
// real bug found while running plsim end-to-end: a drift truth record's
// ExpectedValue/Magnitude describe the *cumulative* change by the run's
// final window, but a real "keyframe" detection can land at any
// intermediate window along that continuously-growing trend. Scoring MAPE
// against the final target for an early-window detection produced
// nonsensically large errors (~90% MAPE on an otherwise-perfect run).
// Drift must still count fully for recall (checked elsewhere) and latency,
// just not value accuracy.
func TestAuditor_DriftExcludedFromMAPEAndRevisionAccuracy(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	truth := []TruthEvent{
		// Cumulative-by-end-of-run target of 175, but the detection below
		// lands early, where the true accumulated value is only ~106.
		{Type: TruthDrift, SeriesID: 1, WindowStart: 0, WindowEnd: 100, ExpectedValue: 175, Magnitude: 75, Timestamp: base},
	}
	a := newTestAuditor(t, truth)
	a.Observe(ports.AnomalyEvent{Substrate: "keyframe", SeriesID: 1, WindowID: 6, Value: 106, Revision: 1, DetectedAt: base.Add(3 * time.Second)})

	rpt := a.Report()
	if rpt.TruePositives != 1 {
		t.Fatalf("TruePositives = %d, want 1 (drift still counts for recall)", rpt.TruePositives)
	}
	if lat, ok := rpt.DetectionLatencyBySubstrate["keyframe"]; !ok || lat.Count != 1 {
		t.Fatalf("DetectionLatencyBySubstrate[keyframe] = %+v, want one sample (drift still counts for latency)", lat)
	}
	if rpt.ValueMAPE != nil {
		t.Fatalf("ValueMAPE = %v, want nil (drift must not be scored for value accuracy)", *rpt.ValueMAPE)
	}
	if rpt.ValueMAPESamples != 0 {
		t.Fatalf("ValueMAPESamples = %d, want 0", rpt.ValueMAPESamples)
	}
	if rpt.RevisionAccuracySamples != 0 {
		t.Fatalf("RevisionAccuracySamples = %d, want 0 (drift excluded despite Revision=1)", rpt.RevisionAccuracySamples)
	}
}

func TestAuditor_RecallByType(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	truth := []TruthEvent{
		{Type: TruthAnomaly, SeriesID: 1, WindowStart: 1, WindowEnd: 1, ExpectedValue: 100, Timestamp: base},
		{Type: TruthDrift, SeriesID: 2, WindowStart: 0, WindowEnd: 100, ExpectedValue: 200, Timestamp: base},
	}
	a := newTestAuditor(t, truth)
	a.Observe(ports.AnomalyEvent{Substrate: "keyframe", SeriesID: 2, WindowID: 6, Value: 199, DetectedAt: base})

	rpt := a.Report()
	anomaly := rpt.RecallByType["anomaly"]
	if anomaly.Truth != 1 || anomaly.Recalled != 0 {
		t.Fatalf("RecallByType[anomaly] = %+v, want Truth=1 Recalled=0", anomaly)
	}
	drift := rpt.RecallByType["drift"]
	if drift.Truth != 1 || drift.Recalled != 1 {
		t.Fatalf("RecallByType[drift] = %+v, want Truth=1 Recalled=1", drift)
	}
}

func TestAuditor_Reload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truth.jsonl")
	w, err := NewTruthWriter(path)
	if err != nil {
		t.Fatalf("NewTruthWriter: %v", err)
	}
	defer w.Close()
	base := time.Unix(1_700_000_000, 0)
	if err := w.Write(TruthEvent{Type: TruthAnomaly, SeriesID: 1, WindowStart: 1, WindowEnd: 1, ExpectedValue: 100, Timestamp: base}); err != nil {
		t.Fatal(err)
	}

	a, err := NewAuditor(path)
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	if rpt := a.Report(); rpt.TruthAnomalies != 1 {
		t.Fatalf("TruthAnomalies = %d, want 1", rpt.TruthAnomalies)
	}

	if err := w.Write(TruthEvent{Type: TruthAnomaly, SeriesID: 2, WindowStart: 2, WindowEnd: 2, ExpectedValue: 100, Timestamp: base}); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(path); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if rpt := a.Report(); rpt.TruthAnomalies != 2 {
		t.Fatalf("TruthAnomalies after Reload = %d, want 2", rpt.TruthAnomalies)
	}
}
