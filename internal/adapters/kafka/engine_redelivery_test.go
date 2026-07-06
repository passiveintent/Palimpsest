/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package kafka

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/predict"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"

	"github.com/passiveintent/Palimpsest/internal/core"
	"github.com/passiveintent/Palimpsest/internal/forensics"
	"github.com/passiveintent/Palimpsest/internal/metrics"
	"github.com/passiveintent/Palimpsest/internal/ports"
)

// fakeAnomalySink collects ports.AnomalyEvents; safe for concurrent use
// since Source delivers to Engine.HandleFrame from its own goroutine while
// the test reads back events from the main goroutine.
type fakeAnomalySink struct {
	mu     sync.Mutex
	events []ports.AnomalyEvent
}

func (s *fakeAnomalySink) EmitAnomaly(_ context.Context, ev ports.AnomalyEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *fakeAnomalySink) Events() []ports.AnomalyEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ports.AnomalyEvent(nil), s.events...)
}

// kafkaTestEncoder is a minimal mirror of internal/core's own (unexported,
// test-only) testEncoder: the real Tracker+Accumulator+Hold pipeline,
// self-contained here since a different package can't reach into
// internal/core's _test.go helpers. Only what this file's scenario needs
// (one golden keyframe, then plain RESIDUAL windows) is implemented.
type kafkaTestEncoder struct {
	shardID, emitterID uint64
	viewID             uint16
	m, d, bits         int
	seed               uint64

	tracker *sketch.Tracker
	acc     *sketch.Accumulator
}

func newKafkaTestEncoder(tenantKey []byte, shardID, emitterID uint64, viewID uint16, m, d, bits int) *kafkaTestEncoder {
	seed := sketch.DeriveEphemeralSeed(tenantKey, shardID, 0, viewID)
	return &kafkaTestEncoder{
		shardID: shardID, emitterID: emitterID, viewID: viewID,
		m: m, d: d, bits: bits, seed: seed,
		tracker: sketch.NewTracker(predict.NewHold(), nil),
		acc:     sketch.NewAccumulator(sketch.Params{M: m, D: d, Seed: seed, Bits: bits}),
	}
}

func (e *kafkaTestEncoder) frameHeader(seq uint32) *wire.Frame {
	return &wire.Frame{
		Magic: wire.Magic, Version: wire.Version,
		EmitterID: e.emitterID, ShardID: e.shardID, Seq: seq, ViewID: e.viewID,
		M: uint32(e.m), D: uint8(e.d), Bits: uint8(e.bits),
	}
}

// golden observes values and emits a golden (full-dict) KEYFRAME for seq.
func (e *kafkaTestEncoder) golden(seq uint32, values map[string]float64, now time.Time) *wire.Frame {
	for name, v := range values {
		e.tracker.Observe([]byte(name), v, now)
	}
	e.tracker.DrainDeltas()
	ids := e.tracker.ActiveIDs()
	values64 := e.tracker.CurrentValues()
	values32 := make(map[uint64]float32, len(values64))
	for id, v := range values64 {
		values32[id] = float32(v)
	}
	payload, flags, err := wire.EncodeKeyframe(ids, values32, nil, true, 1e-3)
	if err != nil {
		panic(err)
	}
	f := e.frameHeader(seq)
	f.FrameType = wire.FrameTypeKeyframe
	f.Flags |= flags
	f.Payload = payload
	f.QuantScale = 1e-3
	f.DictRoot = wire.ComputeDictRoot(ids)
	f.DictDeltas = e.tracker.FullDict()
	e.tracker.ResetWindow()
	return f
}

// residual observes values (already-known series only) and emits a RESIDUAL
// frame for seq.
func (e *kafkaTestEncoder) residual(seq uint32, values map[string]float64, now time.Time) *wire.Frame {
	for name, v := range values {
		_, isNew, res := e.tracker.Observe([]byte(name), v, now)
		if !isNew {
			e.acc.Update([]byte(name), res)
		}
	}
	y, energy := e.acc.Flush()
	var maxAbs float64
	for _, v := range y {
		if a := math.Abs(v); a > maxAbs {
			maxAbs = a
		}
	}
	scale := float32(1e-6)
	if maxAbs > 0 {
		scale = float32(maxAbs / 127.0)
	}
	payload, err := wire.Quantize(y, 8, scale)
	if err != nil {
		panic(err)
	}
	f := e.frameHeader(seq)
	f.FrameType = wire.FrameTypeResidual
	f.Payload = payload
	f.QuantScale = scale
	f.Energy = float32(energy)
	f.DictRoot = wire.ComputeDictRoot(e.tracker.ActiveIDs())
	e.tracker.ResetWindow()
	return f
}

// TestEngine_KafkaTransportRedeliveryIsDeduped is the Kafka-transport
// version of internal/core's TestE2E_DuplicateResidualFrameDeduped: it wires
// a real kafka.Sink -> (fake broker) -> kafka.Source into a real
// core.Engine and shows that an at-least-once redelivery of the same
// RESIDUAL frame (a producer retry, or a consumer resuming from an
// uncommitted offset) is absorbed by ADR-013's per-window contributor
// ledger — counted via Metrics.DuplicateFramesDropped, never merged twice
// into the window's sketch, and never producing a spurious revision>0
// event.
func TestEngine_KafkaTransportRedeliveryIsDeduped(t *testing.T) {
	const (
		shardID         = uint64(77)
		emitterID       = uint64(9001)
		viewID          = uint16(0)
		m, d, bits      = 200, 6, 8
		allowedLateness = 2
	)
	tenantKey := []byte("kafka-adapter-redelivery-test-key")

	sink := &fakeAnomalySink{}
	met := metrics.New()
	eng := core.New(core.Config{
		TenantKey:        tenantKey,
		MaxResidual:      40.0,
		AllowedLateness:  allowedLateness,
		RepairHorizon:    5 * time.Minute,
		EmittersExpected: 1,
		FISTAIters:       200,
		FISTALambda:      0.05,
		FISTAPowerIters:  30,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}, sink, nil, met, forensics.NewSnapshotStore(time.Hour, 100))

	names := make([]string, 30)
	for i := range names {
		names[i] = fmt.Sprintf("kafka_redelivery_metric_%02d|shard=77|agg=sum", i)
	}
	baseline := func() map[string]float64 {
		v := make(map[string]float64, len(names))
		for _, n := range names {
			v[n] = 10.0
		}
		return v
	}
	anomalous := func() map[string]float64 {
		v := baseline()
		v[names[0]] = 10.0 + 8.0
		return v
	}

	enc := newKafkaTestEncoder(tenantKey, shardID, emitterID, viewID, m, d, bits)
	now := time.Now()

	brokers := newFakeCluster(t, 1)
	kSink, err := NewSink(SinkConfig{Brokers: brokers, Topic: testTopic, TenantID: "redelivery-tenant"})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	defer kSink.Close()

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer sendCancel()

	send := func(f *wire.Frame) {
		t.Helper()
		if err := kSink.Send(sendCtx, f); err != nil {
			t.Fatalf("kSink.Send seq=%d: %v", f.Seq, err)
		}
	}

	send(enc.golden(0, baseline(), now))
	dupFrame := enc.residual(1, anomalous(), now)
	send(dupFrame)
	for s := uint32(2); s <= uint32(1+allowedLateness); s++ {
		send(enc.residual(s, baseline(), now))
	}

	src, err := NewSource(SourceConfig{Brokers: brokers, Topic: testTopic, Group: "redelivery-engine-group"})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	runDone := make(chan error, 1)
	go func() { runDone <- src.Run(runCtx, func(f *wire.Frame) { eng.HandleFrame(context.Background(), f) }) }()

	waitFor := func(desc string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for: %s", desc)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	countWindow1 := func() int {
		n := 0
		for _, ev := range sink.Events() {
			if ev.Substrate == "deviation" && ev.WindowID == 1 {
				n++
			}
		}
		return n
	}
	waitFor("initial window-1 deviation event", func() bool { return countWindow1() > 0 })
	initialCount := countWindow1()

	// Resend the EXACT SAME window-1 frame (an at-least-once transport
	// redelivery: a producer retry upstream, or a consumer resuming from an
	// uncommitted offset — see TestSource_AtLeastOnceRedelivery for that
	// half in isolation). The still-running Source will fetch and deliver
	// it a second time.
	send(dupFrame)

	waitFor("duplicate_frames_dropped to increment", func() bool { return met.DuplicateFramesDropped() > 0 })

	runCancel()
	<-runDone

	if got := countWindow1(); got != initialCount {
		t.Fatalf("window-1 deviation event count changed after redelivered duplicate: got %d, want %d (unchanged)", got, initialCount)
	}
	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" && ev.WindowID == 1 && ev.Revision > 0 {
			t.Fatalf("redelivered duplicate produced a spurious revision>0 event (should have been deduped, not re-solved): %+v", ev)
		}
	}
}
