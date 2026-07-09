// Package jsonl implements ports.AnomalySink and ports.SeriesSink as
// append-only JSON-lines files: one JSON object per line, flushed
// synchronously so a killed process never loses a buffered-but-unwritten
// line.
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package jsonl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/passiveintent/Palimpsest/internal/ports"
)

// AnomalySink appends one JSON object per ports.AnomalyEvent to a file.
type AnomalySink struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// NewAnomalySink opens (creating/appending) path.
func NewAnomalySink(path string) (*AnomalySink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("jsonl: opening %s: %w", path, err)
	}
	return &AnomalySink{f: f, enc: json.NewEncoder(f)}, nil
}

// EmitAnomaly implements ports.AnomalySink.
func (s *AnomalySink) EmitAnomaly(_ context.Context, ev ports.AnomalyEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(ev); err != nil {
		return fmt.Errorf("jsonl: encoding anomaly: %w", err)
	}
	return s.f.Sync()
}

// Close closes the underlying file.
func (s *AnomalySink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// SeriesSink appends one JSON object per ports.Sample to a file.
type SeriesSink struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// NewSeriesSink opens (creating/appending) path.
func NewSeriesSink(path string) (*SeriesSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("jsonl: opening %s: %w", path, err)
	}
	return &SeriesSink{f: f, enc: json.NewEncoder(f)}, nil
}

// WriteSeries implements ports.SeriesSink.
func (s *SeriesSink) WriteSeries(_ context.Context, samples []ports.Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, smp := range samples {
		if err := s.enc.Encode(smp); err != nil {
			return fmt.Errorf("jsonl: encoding sample: %w", err)
		}
	}
	return s.f.Sync()
}

// Close closes the underlying file.
func (s *SeriesSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
