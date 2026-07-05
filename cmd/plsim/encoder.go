/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"math"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/predict"
	"github.com/purushpsm147/palimpsest/pkg/sketch"
	"github.com/purushpsm147/palimpsest/pkg/wire"
)

// minQuantScale floors adaptive quantization scales so an all-zero (quiet)
// window never produces an invalid (zero) wire.Quantize/EncodeKeyframe
// scale, mirroring otel/processor/csresidual's and
// internal/core/testencoder_test.go's identical constant.
const minQuantScale = 1e-6

// pipelineConfig configures one encoder pipeline's static parameters.
type pipelineConfig struct {
	TenantKey []byte
	ShardID   uint64
	EmitterID uint64
	ViewID    uint16
	Epoch     uint64

	M, D, Bits int

	KeyframeEvery int
	GoldenEvery   int
	SeriesTTL     time.Duration

	SnapshotThreshold float64 // <=0 disables speculative snapshot push
	RingWindow        time.Duration
}

// pipeline is one (emitter, view)'s complete encode-side state: Tracker +
// Accumulator + Hold predictor + per-series ring buffers, driving lifecycle
// (ADR-008), KDELTA/golden keyframe cadence (ADR-011), and speculative
// snapshot push (ADR-009, ADR-012) exactly as
// otel/processor/csresidual/processor.go and
// internal/core/testencoder_test.go do — this is plsim's own copy since
// neither of those is importable from cmd/plsim (one is a separate module,
// the other is test-only).
type pipeline struct {
	cfg  pipelineConfig
	seed uint64

	tracker *sketch.Tracker
	acc     *sketch.Accumulator
	pred    predict.Predictor

	keyframeCount      int
	prevKeyframeValues map[uint64]float32

	ringBuffers map[uint64]*sketch.RingBuffer
}

func newPipeline(cfg pipelineConfig) *pipeline {
	seed := sketch.DeriveEphemeralSeed(cfg.TenantKey, cfg.ShardID, uint32(cfg.Epoch), cfg.ViewID)
	pred := predict.NewHold()
	return &pipeline{
		cfg:         cfg,
		seed:        seed,
		tracker:     sketch.NewTracker(pred, nil),
		acc:         sketch.NewAccumulator(sketch.Params{M: cfg.M, D: cfg.D, Seed: seed, Bits: cfg.Bits}),
		pred:        pred,
		ringBuffers: make(map[uint64]*sketch.RingBuffer),
	}
}

// flush observes one window's raw values (name -> value), then builds the
// resulting wire.Frame for window seq: a KEYFRAME on cadence, else a
// RESIDUAL, with a speculative snapshot blob attached when the window's
// largest-magnitude residual crosses cfg.SnapshotThreshold.
func (p *pipeline) flush(now time.Time, seq uint32, values map[string]float64) *wire.Frame {
	var (
		maxAbsResidual float64
		maxAbsID       uint64
		haveCandidate  bool
	)
	for name, v := range values {
		id, isNew, residual := p.tracker.Observe([]byte(name), v, now)
		if !isNew {
			p.acc.Update([]byte(name), residual)
		}
		if p.cfg.SnapshotThreshold > 0 {
			rb, ok := p.ringBuffers[id]
			if !ok {
				rb = sketch.NewRingBuffer(id, p.cfg.RingWindow)
				p.ringBuffers[id] = rb
			}
			rb.Push(uint64(now.UnixMilli()), v)
			if a := math.Abs(residual); a > maxAbsResidual {
				maxAbsResidual, maxAbsID, haveCandidate = a, id, true
			}
		}
	}

	tombstones := p.tracker.Expire(now, p.cfg.SeriesTTL)
	births := p.tracker.DrainDeltas()
	dictDeltas := append(births, tombstones...)
	for _, ts := range tombstones {
		delete(p.ringBuffers, ts.ID)
	}

	y, energy := p.acc.Flush()

	f := &wire.Frame{
		Magic:     wire.Magic,
		Version:   wire.Version,
		EmitterID: p.cfg.EmitterID,
		ShardID:   p.cfg.ShardID,
		Epoch:     p.cfg.Epoch,
		Seq:       seq,
		ViewID:    p.cfg.ViewID,
		M:         uint32(p.cfg.M),
		D:         uint8(p.cfg.D),
		Predictor: uint8(p.pred.ID()),
		Bits:      uint8(p.cfg.Bits),
		Energy:    float32(energy),
	}

	keyframeWindow := p.cfg.KeyframeEvery > 0 && seq%uint32(p.cfg.KeyframeEvery) == 0
	if keyframeWindow {
		p.buildKeyframe(f, dictDeltas)
	} else {
		scale := adaptiveScale(y, p.cfg.Bits)
		payload, err := wire.Quantize(y, uint8(p.cfg.Bits), scale)
		if err != nil {
			panic(err)
		}
		f.FrameType = wire.FrameTypeResidual
		f.Payload = payload
		f.QuantScale = scale
		f.DictDeltas = dictDeltas
		f.DictRoot = wire.ComputeDictRoot(p.tracker.ActiveIDs())
	}

	if p.cfg.SnapshotThreshold > 0 && haveCandidate && maxAbsResidual > p.cfg.SnapshotThreshold {
		if rb, ok := p.ringBuffers[maxAbsID]; ok {
			f.SnapshotBlob = rb.Snapshot()
		}
	}

	p.tracker.ResetWindow()
	return f
}

func (p *pipeline) buildKeyframe(f *wire.Frame, dictDeltas []wire.DictDelta) {
	golden := p.cfg.GoldenEvery <= 0 || p.keyframeCount%p.cfg.GoldenEvery == 0
	p.keyframeCount++

	ids := p.tracker.ActiveIDs()
	values64 := p.tracker.CurrentValues()
	values32 := make(map[uint64]float32, len(values64))
	for id, v := range values64 {
		values32[id] = float32(v)
	}

	scale := adaptiveKDeltaScale(values32, p.prevKeyframeValues)
	payload, flags, err := wire.EncodeKeyframe(ids, values32, p.prevKeyframeValues, golden, scale)
	if err != nil {
		panic(err)
	}

	f.FrameType = wire.FrameTypeKeyframe
	f.Flags |= flags
	f.Payload = payload
	f.QuantScale = scale
	f.DictRoot = wire.ComputeDictRoot(ids)
	if golden {
		f.DictDeltas = p.tracker.FullDict()
	} else {
		f.DictDeltas = dictDeltas
	}
	p.prevKeyframeValues = values32
	// ADR-003: refresh the open-loop baseline to the true current value at
	// every keyframe (see pkg/sketch/lifecycle.go's Tracker.CurrentValues
	// doc comment) — otherwise residuals accumulate unbounded from birth
	// and substrate (c) never sees a nonzero keyframe-to-keyframe delta.
	p.pred.LoadKeyframe(values64)
}

func adaptiveScale(y []float64, bits int) float32 {
	var maxAbs float64
	for _, v := range y {
		if a := math.Abs(v); a > maxAbs {
			maxAbs = a
		}
	}
	limit := 127.0
	if bits == 16 {
		limit = 32767.0
	}
	return scaleFor(maxAbs, limit)
}

func adaptiveKDeltaScale(cur, prev map[uint64]float32) float32 {
	var maxAbs float64
	for id, v := range cur {
		d := math.Abs(float64(v - prev[id]))
		if d > maxAbs {
			maxAbs = d
		}
	}
	return scaleFor(maxAbs, 32767.0)
}

func scaleFor(maxAbs, steps float64) float32 {
	if maxAbs <= 0 {
		return minQuantScale
	}
	s := float32(maxAbs / steps)
	if s <= 0 {
		return minQuantScale
	}
	return s
}
