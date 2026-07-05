/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package metrics

import (
	"testing"
	"time"
)

func TestMetrics_Counters(t *testing.T) {
	m := New()

	m.IncFramesTotal("residual", 1, 2)
	m.IncFramesTotal("residual", 1, 2)
	m.IncFramesTotal("keyframe", 1, 2)
	if got := m.FramesTotal("residual", 1, 2); got != 2 {
		t.Fatalf("FramesTotal(residual,1,2) = %d, want 2", got)
	}
	if got := m.FramesTotal("keyframe", 1, 2); got != 1 {
		t.Fatalf("FramesTotal(keyframe,1,2) = %d, want 1", got)
	}

	m.IncGatedFrames()
	m.IncGatedFrames()
	if got := m.GatedFrames(); got != 2 {
		t.Fatalf("GatedFrames() = %d, want 2", got)
	}

	m.IncDegradedShards()
	m.IncDegradedShards()
	m.DecDegradedShards()
	if got := m.DegradedShards(); got != 1 {
		t.Fatalf("DegradedShards() = %d, want 1", got)
	}

	m.IncTombstonesApplied(3)
	m.IncBirthsApplied(5)
	if got := m.TombstonesApplied(); got != 3 {
		t.Fatalf("TombstonesApplied() = %d, want 3", got)
	}
	if got := m.BirthsApplied(); got != 5 {
		t.Fatalf("BirthsApplied() = %d, want 5", got)
	}

	m.IncAnomaliesFired("deviation", "recovered")
	m.IncAnomaliesFired("deviation", "recovered")
	m.IncAnomaliesFired("absence", "absence")
	if got := m.AnomaliesFired("deviation", "recovered"); got != 2 {
		t.Fatalf("AnomaliesFired(deviation,recovered) = %d, want 2", got)
	}

	m.IncSnapshotsCaptured(4)
	if got := m.SnapshotsCaptured(); got != 4 {
		t.Fatalf("SnapshotsCaptured() = %d, want 4", got)
	}

	m.IncBreakerTrips()
	if got := m.BreakerTrips(); got != 1 {
		t.Fatalf("BreakerTrips() = %d, want 1", got)
	}

	m.SetClockSkewMs(7, 42)
	m.SetClockSkewMs(7, -10)
	if got := m.ClockSkewMs(7); got != -10 {
		t.Fatalf("ClockSkewMs(7) = %d, want -10 (gauge, last write wins)", got)
	}

	m.IncLateFramesDropped()
	m.IncLateFramesDropped()
	m.IncLateFramesDropped()
	if got := m.LateFramesDropped(); got != 3 {
		t.Fatalf("LateFramesDropped() = %d, want 3", got)
	}
}

func TestMetrics_Averages(t *testing.T) {
	m := New()
	m.ObserveRecoverSeconds(1, 100*time.Millisecond)
	m.ObserveRecoverSeconds(1, 300*time.Millisecond)
	if got, want := m.RecoverSecondsAvg(1), 200*time.Millisecond; got != want {
		t.Fatalf("RecoverSecondsAvg(1) = %v, want %v", got, want)
	}
	if got := m.RecoverSecondsAvg(999); got != 0 {
		t.Fatalf("RecoverSecondsAvg(unseen) = %v, want 0", got)
	}

	m.ObserveSnapshotsLatencySeconds(2 * time.Second)
	m.ObserveSnapshotsLatencySeconds(4 * time.Second)
	if got, want := m.SnapshotsLatencySecondsAvg(), 3*time.Second; got != want {
		t.Fatalf("SnapshotsLatencySecondsAvg() = %v, want %v", got, want)
	}
}

func TestMetrics_SnapshotAndPublish(t *testing.T) {
	m := New()
	m.IncGatedFrames()
	snap := m.Snapshot()
	if snap["gated_frames"].(int64) != 1 {
		t.Fatalf("Snapshot()[gated_frames] = %v, want 1", snap["gated_frames"])
	}

	// Publish must not panic even if called more than once (sync.Once
	// guard), and must not panic across multiple *distinct* Metrics
	// instances publishing under different names.
	m.Publish("palimpsest_metrics_test_a")
	m.Publish("palimpsest_metrics_test_a")

	m2 := New()
	m2.Publish("palimpsest_metrics_test_b")
}
