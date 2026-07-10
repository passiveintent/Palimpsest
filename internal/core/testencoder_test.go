/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"time"

	"github.com/passiveintent/Palimpsest/pkg/predict"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

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
	tenantKey          []byte
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
		tenantKey: tenantKey,
		shardID:   shardID, emitterID: emitterID, viewID: viewID, epoch: epoch,
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

		res, err := wire.BuildKeyframe(wire.KeyframeBuildInput{
			Golden:             golden,
			IDs:                ids,
			Values:             values64,
			PrevKeyframeValues: e.prevKeyframeValues,
			DictDeltas:         dictDeltas,
			FullDict:           e.tracker.FullDict(),
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
		e.prevKeyframeValues = res.Values32
		// ADR-003: a keyframe is the only steady-state point the open-loop
		// predictor baseline is allowed to move. Refresh it to the true
		// current value here, or residuals accumulate unbounded from birth
		// forever instead of resetting each keyframe.
		e.pred.LoadKeyframe(values64)
	} else {
		res, err := wire.BuildResidual(wire.ResidualBuildInput{
			Y:          y,
			Bits:       uint8(e.bits),
			DictDeltas: dictDeltas,
			IDs:        e.tracker.ActiveIDs(),
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

	e.tracker.ResetWindow()
	return f
}

// rotateEpoch mirrors otel/processor/csresidual's (*metricsProcessor).
// rotateEpoch: it starts a fresh (epoch-keyed) Accumulator/Tracker/
// Predictor under newEpoch, re-birthing every series active just before
// rotation at its last known value so the new epoch's opening golden
// keyframe (ADR-013: "every emitter MUST open each epoch stream with
// golden keyframe + full dict") carries it forward rather than losing it.
func (e *testEncoder) rotateEpoch(now time.Time, newEpoch uint64) {
	prevActive := e.tracker.FullDict()

	seed := sketch.DeriveEphemeralSeed(e.tenantKey, e.shardID, uint32(newEpoch), e.viewID)
	pred := predict.NewHold()
	tracker := sketch.NewTracker(pred, nil)
	acc := sketch.NewAccumulator(sketch.Params{M: e.m, D: e.d, Seed: seed, Bits: e.bits})

	for _, dd := range prevActive {
		tracker.Observe(dd.Name, float64(dd.InitValue), now)
	}

	e.epoch = newEpoch
	e.tracker = tracker
	e.acc = acc
	e.pred = pred
	e.prevKeyframeValues = nil
	e.keyframeCount = 0
}

