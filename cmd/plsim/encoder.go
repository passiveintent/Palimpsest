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

	"github.com/passiveintent/Palimpsest/pkg/predict"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

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

	// Codec compresses the KEYFRAME payload and the snapshot blob
	// (ADR-006 §Addendum); the zero value, wire.CodecNone, is a no-op and
	// matches this tool's pre-existing (uncompressed) behavior.
	Codec wire.Codec
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
		Codec:     p.cfg.Codec,
	}

	keyframeWindow := p.cfg.KeyframeEvery > 0 && seq%uint32(p.cfg.KeyframeEvery) == 0
	if keyframeWindow {
		p.buildKeyframe(f, dictDeltas, tombstones)
	} else {
		res, err := wire.BuildResidual(wire.ResidualBuildInput{
			Y:          y,
			Bits:       uint8(p.cfg.Bits),
			DictDeltas: dictDeltas,
			IDs:        p.tracker.ActiveIDs(),
		})
		if err != nil {
			panic(err)
		}
		f.FrameType = wire.FrameTypeResidual
		f.Payload = res.Payload
		f.QuantScale = res.QuantScale
		f.DictDeltas = res.DictDeltas
		f.DictRoot = res.DictRoot
	}

	if p.cfg.SnapshotThreshold > 0 && haveCandidate && maxAbsResidual > p.cfg.SnapshotThreshold {
		if rb, ok := p.ringBuffers[maxAbsID]; ok {
			// RingBuffer.Snapshot() always serializes uncompressed
			// (sketch.RingBuffer has no Codec concept of its own); decode
			// then re-encode under this pipeline's configured Codec so the
			// frame's SnapshotBlob honors it end-to-end (ADR-006 §Addendum).
			entries, err := wire.DecodeSnapshot(rb.Snapshot(), wire.CodecNone)
			if err != nil {
				panic(err)
			}
			blob, err := wire.EncodeSnapshot(entries, p.cfg.Codec)
			if err != nil {
				panic(err)
			}
			f.SnapshotBlob = blob
		}
	}

	p.tracker.ResetWindow()
	return f
}

// buildKeyframe builds f as a KEYFRAME frame. tombstones is this same
// window's Expire() output (a subset of dictDeltas, which also mixes in
// births): on a golden keyframe, FullDict() alone only lists currently
// active series, so any series that expired in this exact window must be
// appended explicitly (ADR-008 amendment) — omission is not the same as an
// explicit tombstone, and without it a decoder would never learn that
// series is gone (FullDict simply stops mentioning it, forever).
func (p *pipeline) buildKeyframe(f *wire.Frame, dictDeltas, tombstones []wire.DictDelta) {
	golden := p.cfg.GoldenEvery <= 0 || p.keyframeCount%p.cfg.GoldenEvery == 0
	p.keyframeCount++

	ids := p.tracker.ActiveIDs()
	values64 := p.tracker.CurrentValues()

	res, err := wire.BuildKeyframe(wire.KeyframeBuildInput{
		Golden:             golden,
		Codec:              p.cfg.Codec,
		IDs:                ids,
		Values:             values64,
		PrevKeyframeValues: p.prevKeyframeValues,
		DictDeltas:         dictDeltas,
		FullDict:           p.tracker.FullDict(),
		Tombstones:         tombstones,
	})
	if err != nil {
		panic(err)
	}

	f.FrameType = wire.FrameTypeKeyframe
	f.Flags |= res.Flags
	f.Payload = res.Payload
	f.QuantScale = res.QuantScale
	f.DictRoot = res.DictRoot
	f.DictDeltas = res.DictDeltas
	p.prevKeyframeValues = res.Values32
	// ADR-003: refresh the open-loop baseline to the true current value at
	// every keyframe (see pkg/sketch/lifecycle.go's Tracker.CurrentValues
	// doc comment) — otherwise residuals accumulate unbounded from birth
	// and substrate (c) never sees a nonzero keyframe-to-keyframe delta.
	p.pred.LoadKeyframe(values64)
}
