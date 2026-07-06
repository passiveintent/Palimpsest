/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package ports defines the hexagonal boundary between palimpsestd's core
// (internal/core) and the outside world (internal/adapters): where frames
// come from, where anomalies and recovered series go, and where engine
// state is persisted (ADR-007: "palimpsestd is ports-and-adapters").
//
// Every port here has an in-memory fake usable from tests without pulling
// in a real filesystem, HTTP server, or external service.
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package ports

import (
	"context"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// FrameSource supplies decoded wire frames to the engine (e.g.
// internal/adapters/fswatch polling a directory, or internal/adapters/httpsrc
// serving speculative pushes per ADR-012's "zero listening sockets by
// default" — httpsrc is the one adapter that opts into a listening socket).
type FrameSource interface {
	// Run blocks, invoking handle for every frame decoded from the source,
	// until ctx is canceled or a fatal, non-recoverable error occurs (a
	// per-frame decode error is not fatal: implementations should log/count
	// it and continue). Run must return promptly once ctx is canceled
	// (ctx-owned goroutines, no leaks).
	Run(ctx context.Context, handle func(*wire.Frame)) error
}

// FrameSink publishes wire frames to a transport, the encode-side mirror of
// FrameSource (e.g. internal/adapters/kafka.Sink producing to a Kafka
// topic). Most encoders (otel/processor/csresidual's default file sink)
// write directly to disk instead and never need this port; it exists for
// transports where publishing can fail or block (a network producer) and
// callers need an explicit error rather than a best-effort file write.
type FrameSink interface {
	Send(ctx context.Context, f *wire.Frame) error
	// Close releases any resources (e.g. the underlying client connection).
	Close() error
}

// AnomalyEvent is one flagged deviation, produced by any of ADR-009's five
// recovery substrates and routed to an AnomalySink.
type AnomalyEvent struct {
	// Substrate identifies which of ADR-009's five evaluation paths
	// produced this event: "deviation", "pointquery", "keyframe",
	// "absence", or "exact".
	Substrate string `json:"substrate"`

	// Confidence mirrors recover.Confidence* for substrates backed by the
	// recovery solver, plus "absence" and "exact" for the substrates that
	// aren't.
	Confidence string `json:"confidence"`

	ShardID   uint64 `json:"shard_id"`
	EmitterID uint64 `json:"emitter_id"` // 0 when the event is attributed to a merged, multi-emitter window rather than one emitter's stream
	Epoch     uint64 `json:"epoch"`
	ViewID    uint16 `json:"view_id"`
	WindowID  uint32 `json:"window_id"` // Frame.Seq

	SeriesID   uint64 `json:"series_id"`
	SeriesName string `json:"series_name,omitempty"` // best-effort (from a DictDelta birth); "" if never observed

	Value    float64 `json:"value"`
	Residual float64 `json:"residual"`

	Coverage      int `json:"coverage"`       // emitters present for this window (ADR-013)
	CoverageTotal int `json:"coverage_total"` // emitters expected
	Revision      int `json:"revision"`       // 0 for initial, incremented on watermark repair

	// MergedTrust is "" | "na" | "unproven" | "proven" (ADR-015): the
	// scaling-law guardrail verdict for a merged-tier view's cross-emitter
	// fusion, independent of Confidence (which reports recovery mode
	// regardless of this verdict). "na" for non-merged-tier events.
	MergedTrust string `json:"merged_trust,omitempty"`

	DetectedAt time.Time `json:"detected_at"`

	// Snapshot is the ADR-009 "dashcam" forensic ring-buffer window for
	// SeriesID, if one was captured and selected for this event.
	Snapshot []wire.SnapshotEntry `json:"snapshot,omitempty"`
}

// AnomalySink routes flagged AnomalyEvents out of the engine (e.g. to a
// JSON-lines file or a webhook).
type AnomalySink interface {
	EmitAnomaly(ctx context.Context, ev AnomalyEvent) error
}

// Sample is one exact, named data point: a keyframe value (ADR-010 Layer 1,
// forever-queryable PromQL layer) or a rehydrated anomalous series value.
type Sample struct {
	SeriesID    uint64  `json:"series_id"`
	Name        string  `json:"name,omitempty"`
	ShardID     uint64  `json:"shard_id"`
	EmitterID   uint64  `json:"emitter_id,omitempty"`
	ViewID      uint16  `json:"view_id"`
	Value       float64 `json:"value"`
	TimestampMs int64   `json:"timestamp_ms"`
}

// SeriesSink receives exact series values (e.g. to Prometheus remote-write,
// or to a JSON-lines file for offline inspection).
type SeriesSink interface {
	WriteSeries(ctx context.Context, samples []Sample) error
}

// StateStore persists and recovers engine state across restarts (e.g. the
// in-memory adapter's periodic gob snapshot).
type StateStore interface {
	Save(ctx context.Context, key string, data []byte) error
	// Load returns ok=false (no error) if key has never been saved.
	Load(ctx context.Context, key string) (data []byte, ok bool, err error)
}
