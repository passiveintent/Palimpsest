/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"math"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/predict"
	"github.com/purushpsm147/palimpsest/pkg/sketch"
	"github.com/purushpsm147/palimpsest/pkg/wire"
)

// minTestQuantScale mirrors otel/processor/csresidual's minQuantScale: it
// floors adaptive quantization scales so an all-zero (perfectly quiet)
// window never produces an invalid (zero) wire.Quantize/EncodeKeyframe
// scale.
const minTestQuantScale = 1e-6

// testEncoder is a minimal, self-contained mirror of the encode-path
// pipeline (pkg/sketch.Tracker + Accumulator + pkg/predict.Hold +
// pkg/wire), built directly against this repo's own encode-side packages
// (the same ones otel/processor/csresidual drives) so e2e_test.go can feed
// the Engine realistic frames without depending on the separate otel/
// module. One testEncoder is one emitter's one (shard, view) stream.
//
// Unlike the production pipeline, flush takes an explicit seq (window
// index) rather than auto-incrementing one internally: tests need to
// simulate withheld/late/out-of-order frames per emitter, which requires
// decoupling "which window is this" from "how many times have I called
// flush".
type testEncoder struct {
	shardID, emitterID uint64
	viewID             uint16
	epoch              uint64
	m, d, bits         int
	seed               uint64

	tracker *sketch.Tracker
	acc     *sketch.Accumulator
	pred    predict.Predictor

	keyframeEvery      int
	goldenEvery        int
	keyframeCount      int
	prevKeyframeValues map[uint64]float32

	seriesTTL time.Duration
}

func newTestEncoder(tenantKey []byte, shardID, emitterID uint64, viewID uint16, epoch uint64, m, d, bits int, keyframeEvery, goldenEvery int, seriesTTL time.Duration) *testEncoder {
	seed := sketch.DeriveEphemeralSeed(tenantKey, shardID, uint32(epoch), viewID)
	pred := predict.NewHold()
	return &testEncoder{
		shardID: shardID, emitterID: emitterID, viewID: viewID, epoch: epoch,
		m: m, d: d, bits: bits, seed: seed,
		tracker:       sketch.NewTracker(pred, nil),
		acc:           sketch.NewAccumulator(sketch.Params{M: m, D: d, Seed: seed, Bits: bits}),
		pred:          pred,
		keyframeEvery: keyframeEvery,
		goldenEvery:   goldenEvery,
		seriesTTL:     seriesTTL,
	}
}

// flush observes one window's raw values (name -> value) through this
// encoder's Tracker/Accumulator/Predictor, then builds the resulting
// wire.Frame for window seq: a KEYFRAME on cadence (or forceGolden), else
// a RESIDUAL.
func (e *testEncoder) flush(now time.Time, seq uint32, values map[string]float64, forceGolden bool) *wire.Frame {
	for name, v := range values {
		_, isNew, residual := e.tracker.Observe([]byte(name), v, now)
		if !isNew {
			e.acc.Update([]byte(name), residual)
		}
	}
	tombstones := e.tracker.Expire(now, e.seriesTTL)
	births := e.tracker.DrainDeltas()
	dictDeltas := append(births, tombstones...)

	y, energy := e.acc.Flush()

	f := &wire.Frame{
		Magic:     wire.Magic,
		Version:   wire.Version,
		EmitterID: e.emitterID,
		ShardID:   e.shardID,
		Epoch:     e.epoch,
		Seq:       seq,
		ViewID:    e.viewID,
		M:         uint32(e.m),
		D:         uint8(e.d),
		Predictor: uint8(e.pred.ID()),
		Bits:      uint8(e.bits),
		Energy:    float32(energy),
	}

	keyframeWindow := seq%uint32(e.keyframeEvery) == 0
	if forceGolden || keyframeWindow {
		golden := forceGolden || e.keyframeCount%e.goldenEvery == 0
		e.keyframeCount++

		ids := e.tracker.ActiveIDs()
		values64 := e.tracker.CurrentValues()
		values32 := make(map[uint64]float32, len(values64))
		for id, v := range values64 {
			values32[id] = float32(v)
		}
		scale := kdeltaScale(values32, e.prevKeyframeValues)
		payload, flags, err := wire.EncodeKeyframe(ids, values32, e.prevKeyframeValues, golden, scale)
		if err != nil {
			panic(err)
		}
		f.FrameType = wire.FrameTypeKeyframe
		f.Flags |= flags
		f.Payload = payload
		f.QuantScale = scale
		f.DictRoot = wire.ComputeDictRoot(ids)
		if golden {
			f.DictDeltas = e.tracker.FullDict()
		} else {
			f.DictDeltas = dictDeltas
		}
		e.prevKeyframeValues = values32
		// ADR-003: a keyframe is the only steady-state point the open-loop
		// predictor baseline is allowed to move. Refresh it to the true
		// current value here, or residuals accumulate unbounded from birth
		// forever instead of resetting each keyframe.
		e.pred.LoadKeyframe(values64)
	} else {
		scale := residualScale(y, e.bits)
		payload, err := wire.Quantize(y, uint8(e.bits), scale)
		if err != nil {
			panic(err)
		}
		f.FrameType = wire.FrameTypeResidual
		f.Payload = payload
		f.QuantScale = scale
		f.DictDeltas = dictDeltas
		f.DictRoot = wire.ComputeDictRoot(e.tracker.ActiveIDs())
	}

	e.tracker.ResetWindow()
	return f
}

// residualScale mirrors otel/processor/csresidual's adaptiveScale: the
// smallest RESIDUAL quantization scale that keeps the window's
// largest-magnitude sketch value from clamping.
func residualScale(y []float64, bits int) float32 {
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

// kdeltaScale mirrors otel/processor/csresidual's adaptiveKDeltaScale.
func kdeltaScale(cur, prev map[uint64]float32) float32 {
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
		return minTestQuantScale
	}
	s := float32(maxAbs / steps)
	if s <= 0 {
		return minTestQuantScale
	}
	return s
}
