/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package jsonl

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/internal/ports"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

func TestAnomalySink_AppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anomalies.jsonl")
	sink, err := NewAnomalySink(path)
	if err != nil {
		t.Fatalf("NewAnomalySink: %v", err)
	}
	defer sink.Close()

	ev := ports.AnomalyEvent{Substrate: "deviation", Confidence: "recovered", SeriesID: 42, Value: 1.5, DetectedAt: time.Unix(100, 0)}
	if err := sink.EmitAnomaly(context.Background(), ev); err != nil {
		t.Fatalf("EmitAnomaly: %v", err)
	}
	if err := sink.EmitAnomaly(context.Background(), ev); err != nil {
		t.Fatalf("EmitAnomaly: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(lines))
	}
	var got ports.AnomalyEvent
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SeriesID != 42 || got.Substrate != "deviation" {
		t.Fatalf("got = %+v, want SeriesID=42 Substrate=deviation", got)
	}
}

func TestSeriesSink_AppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series.jsonl")
	sink, err := NewSeriesSink(path)
	if err != nil {
		t.Fatalf("NewSeriesSink: %v", err)
	}
	defer sink.Close()

	samples := []ports.Sample{
		{SeriesID: 1, Name: "a", Value: 1.0, TimestampMs: 1000},
		{SeriesID: 2, Name: "b", Value: 2.0, TimestampMs: 2000},
	}
	if err := sink.WriteSeries(context.Background(), samples); err != nil {
		t.Fatalf("WriteSeries: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(lines))
	}
	var got ports.Sample
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SeriesID != 2 || got.Name != "b" {
		t.Fatalf("got = %+v, want SeriesID=2 Name=b", got)
	}
}
