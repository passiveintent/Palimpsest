/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package audit implements Prompt 8's trust layer: comparing palimpsestd's
// emitted ports.AnomalyEvents against a synthetic workload's ground truth
// (ADR-008 lifecycle events, ADR-009 injected anomalies/drift), producing
// precision/recall/MAPE/lifecycle-false-positive/detection-latency/
// revision-accuracy metrics as both expvar and a JSON report.
//
// TruthEvent is the shared schema: cmd/plsim writes it (TruthWriter) as it
// generates a workload, and Auditor reads it (via LoadTruth/the tailer in
// audit.go) to score palimpsestd's real output against it. Keeping the type
// here (rather than duplicating it in cmd/plsim) means the writer and
// reader can never drift out of sync.
package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// TruthEventType distinguishes ground-truth record kinds. "anomaly" and
// "drift" describe an injected deviation a substrate should detect;
// "birth"/"tombstone" describe an ADR-008 lifecycle event, used to
// classify a detection as a lifecycle false positive rather than a real
// miss.
type TruthEventType string

const (
	TruthAnomaly   TruthEventType = "anomaly"
	TruthDrift     TruthEventType = "drift"
	TruthBirth     TruthEventType = "birth"
	TruthTombstone TruthEventType = "tombstone"
)

// TruthEvent is one line of truth.jsonl. WindowStart/WindowEnd (inclusive)
// bound the window range a detection must overlap to count as catching
// this event; for "birth"/"tombstone", WindowEnd is left zero (a single
// -window event) and ExpectedValue/Magnitude are unused.
type TruthEvent struct {
	Type          TruthEventType `json:"type"`
	SeriesID      uint64         `json:"series_id"`
	SeriesName    string         `json:"series_name,omitempty"`
	ShardID       uint64         `json:"shard_id"`
	ViewID        uint16         `json:"view_id"`
	WindowStart   uint32         `json:"window_start"`
	WindowEnd     uint32         `json:"window_end,omitempty"`
	ExpectedValue float64        `json:"expected_value,omitempty"`
	Magnitude     float64        `json:"magnitude,omitempty"`
	Timestamp     time.Time      `json:"timestamp"`
}

// IsAnomalyLike reports whether e is a detectable-deviation record
// ("anomaly" or "drift") as opposed to a lifecycle record.
func (e TruthEvent) IsAnomalyLike() bool {
	return e.Type == TruthAnomaly || e.Type == TruthDrift
}

// Overlaps reports whether windowID falls within e's [WindowStart,
// WindowEnd] range (WindowEnd defaults to WindowStart when zero, i.e. a
// single-window event).
func (e TruthEvent) Overlaps(windowID uint32) bool {
	end := e.WindowEnd
	if end < e.WindowStart {
		end = e.WindowStart
	}
	return windowID >= e.WindowStart && windowID <= end
}

// TruthWriter appends TruthEvents to a JSONL file, one per line,
// synchronously flushed — mirroring internal/adapters/jsonl's AnomalySink
// so a crashed/killed plsim leaves a complete, greppable truth log.
type TruthWriter struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// NewTruthWriter opens (creating/truncating) path for a fresh truth log.
func NewTruthWriter(path string) (*TruthWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("audit: opening truth-out %q: %w", path, err)
	}
	return &TruthWriter{f: f, enc: json.NewEncoder(f)}, nil
}

// Write appends one TruthEvent and fsyncs.
func (w *TruthWriter) Write(ev TruthEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(ev); err != nil {
		return fmt.Errorf("audit: encoding truth event: %w", err)
	}
	return w.f.Sync()
}

// Close closes the underlying file.
func (w *TruthWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// LoadTruth reads every TruthEvent from a complete truth.jsonl file. A
// nonexistent file is treated as zero events (not an error): palimpsestd
// is a long-running service that may start before, concurrently with, or
// after the plsim/generator process that will eventually create this
// file, and a periodic Auditor.Reload (see audit.go) picks it up once it
// appears — this must not be a startup-time hard failure.
func LoadTruth(path string) ([]TruthEvent, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit: opening truth file %q: %w", path, err)
	}
	defer f.Close()

	var out []TruthEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev TruthEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("audit: decoding truth line: %w", err)
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("audit: scanning truth file %q: %w", path, err)
	}
	return out, nil
}
