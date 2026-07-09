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
)

func TestTruthWriterAndLoadTruthRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truth.jsonl")
	w, err := NewTruthWriter(path)
	if err != nil {
		t.Fatalf("NewTruthWriter: %v", err)
	}

	want := []TruthEvent{
		{Type: TruthAnomaly, SeriesID: 1, SeriesName: "a|agg=sum", ShardID: 1, WindowStart: 5, WindowEnd: 7, ExpectedValue: 160, Magnitude: 60, Timestamp: time.Unix(100, 0).UTC()},
		{Type: TruthBirth, SeriesID: 2, SeriesName: "b|agg=sum", ShardID: 1, WindowStart: 0, Timestamp: time.Unix(101, 0).UTC()},
		{Type: TruthTombstone, SeriesID: 2, SeriesName: "b|agg=sum", ShardID: 1, WindowStart: 50, Timestamp: time.Unix(200, 0).UTC()},
	}
	for _, ev := range want {
		if err := w.Write(ev); err != nil {
			t.Fatalf("Write(%+v): %v", ev, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := LoadTruth(path)
	if err != nil {
		t.Fatalf("LoadTruth: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("LoadTruth: got %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Timestamp.Equal(want[i].Timestamp) {
			t.Fatalf("event %d: Timestamp = %v, want %v", i, got[i].Timestamp, want[i].Timestamp)
		}
		got[i].Timestamp, want[i].Timestamp = time.Time{}, time.Time{}
		if got[i] != want[i] {
			t.Fatalf("event %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestLoadTruthMissingFileIsEmptyNotError guards a real startup-ordering
// bug: palimpsestd (a long-running service) may start --truth-configured
// before the generator process that will eventually create that file has
// written anything. LoadTruth (and therefore NewAuditor) must tolerate
// this rather than fail hard, since a periodic Reload picks the file up
// once it exists.
func TestLoadTruthMissingFileIsEmptyNotError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist-yet.jsonl")
	got, err := LoadTruth(path)
	if err != nil {
		t.Fatalf("LoadTruth(missing file): got error %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("LoadTruth(missing file) = %v, want empty", got)
	}
}

func TestTruthEventOverlaps(t *testing.T) {
	cases := []struct {
		name string
		ev   TruthEvent
		win  uint32
		want bool
	}{
		{"single-window hit", TruthEvent{WindowStart: 5}, 5, true},
		{"single-window miss", TruthEvent{WindowStart: 5}, 6, false},
		{"range start", TruthEvent{WindowStart: 5, WindowEnd: 8}, 5, true},
		{"range middle", TruthEvent{WindowStart: 5, WindowEnd: 8}, 7, true},
		{"range end", TruthEvent{WindowStart: 5, WindowEnd: 8}, 8, true},
		{"range before", TruthEvent{WindowStart: 5, WindowEnd: 8}, 4, false},
		{"range after", TruthEvent{WindowStart: 5, WindowEnd: 8}, 9, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ev.Overlaps(c.win); got != c.want {
				t.Fatalf("Overlaps(%d) = %v, want %v", c.win, got, c.want)
			}
		})
	}
}

func TestTruthEventIsAnomalyLike(t *testing.T) {
	cases := []struct {
		typ  TruthEventType
		want bool
	}{
		{TruthAnomaly, true},
		{TruthDrift, true},
		{TruthBirth, false},
		{TruthTombstone, false},
	}
	for _, c := range cases {
		if got := (TruthEvent{Type: c.typ}).IsAnomalyLike(); got != c.want {
			t.Fatalf("IsAnomalyLike(%q) = %v, want %v", c.typ, got, c.want)
		}
	}
}
