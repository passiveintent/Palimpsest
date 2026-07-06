/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
)

// TestE2E_JitteredEpochRotationZeroErrors is the ADR-013 §Addendum "herd
// jitter" acceptance test's decode-side half (the encode-side distribution
// claim — 1000 emitters, max per-second arrival count < 5% of the fleet —
// is pkg/sketch.TestEpochJitterSpread): a fleet of emitters sharing one
// epoch boundary, staggered across a jitter window by
// sketch.EpochJitter(emitterID, ...) so they don't all rotate in the same
// instant, must decode with zero gated/dropped/degraded outcomes
// attributable to the transition itself.
//
// Each emitter reports the same single fleet-wide series (additive
// ADR-013 summation across observers), at a constant value, for the whole
// run: with no injected change, both the pre- and post-rotation epoch's
// recovered residual should stay ~0 throughout, so any gate/degrade/drop
// firing would be a bug in how the decode path handles a fleet fragmented
// across two concurrent (epoch, view) coordinate systems mid-transition,
// not an artifact of this scenario's data.
func TestE2E_JitteredEpochRotationZeroErrors(t *testing.T) {
	const (
		shardID      = uint64(42)
		viewID       = uint16(0)
		m            = 256
		d            = 6
		bits         = 8
		numEmitters  = 200
		jitterWindow = 60 * time.Second
		seriesName   = "fleet_cpu|shard=42|agg=sum"
		baselineVal  = 3.0
	)

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      2.0,
		AllowedLateness:  1,
		RepairHorizon:    time.Minute,
		EmittersExpected: numEmitters,
		FISTAIters:       100,
		FISTALambda:      0.05,
		FISTAPowerIters:  30,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, sink, _, met, clock := newTestEngine(cfg)

	encoders := make([]*testEncoder, numEmitters)
	jitter := make([]time.Duration, numEmitters)
	epoch0Seq := make([]uint32, numEmitters)
	epoch1Seq := make([]uint32, numEmitters)
	rotated := make([]bool, numEmitters)
	for i := 0; i < numEmitters; i++ {
		emitterID := uint64(1000 + i)
		encoders[i] = newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)
		jitter[i] = sketch.EpochJitter(emitterID, jitterWindow)
	}

	values := constValues([]string{seriesName}, baselineVal)

	// Pre-boundary: everyone opens epoch 0 with a golden keyframe, then
	// flushes two quiet windows so the epoch-0 view is already solving
	// normally before rotation begins.
	for seq := uint32(0); seq < 3; seq++ {
		for i := range encoders {
			eng.HandleFrame(ctx, encoders[i].flush(clock.Now(), seq, values, seq == 0))
			epoch0Seq[i] = seq
		}
		clock.Advance(10 * time.Second)
	}

	// Boundary region: 7 ticks spanning [0, jitterWindow] (60s at 10s
	// steps). At each tick, any not-yet-rotated emitter whose jitter offset
	// has elapsed rotates now (fresh Accumulator/Tracker under epoch 1,
	// opening with a golden keyframe per ADR-013); every other emitter
	// (on either side of its own rotation) keeps flushing quiet windows in
	// whichever epoch it currently belongs to, so both epoch-0 and epoch-1
	// views keep advancing and solving throughout the transition instead of
	// leaving orphaned unsolved tail windows.
	boundaryStart := clock.Now()
	for tick := 0; tick <= 6; tick++ {
		elapsed := time.Duration(tick) * 10 * time.Second
		for i := range encoders {
			if !rotated[i] && jitter[i] <= elapsed {
				encoders[i].rotateEpoch(boundaryStart.Add(elapsed), 1)
				rotated[i] = true
				eng.HandleFrame(ctx, encoders[i].flush(clock.Now(), 0, values, true))
				epoch1Seq[i] = 0
				continue
			}
			if rotated[i] {
				epoch1Seq[i]++
				eng.HandleFrame(ctx, encoders[i].flush(clock.Now(), epoch1Seq[i], values, false))
			} else {
				epoch0Seq[i]++
				eng.HandleFrame(ctx, encoders[i].flush(clock.Now(), epoch0Seq[i], values, false))
			}
		}
		clock.Advance(10 * time.Second)
	}

	for i, r := range rotated {
		if !r {
			t.Fatalf("emitter %d never rotated to epoch 1 by the end of the jitter window (jitter=%v)", i, jitter[i])
		}
	}

	// Post-boundary settle: everyone is now on epoch 1; a couple more
	// quiet windows let its last windows reach the watermark and solve.
	for extra := 0; extra < 2; extra++ {
		for i := range encoders {
			epoch1Seq[i]++
			eng.HandleFrame(ctx, encoders[i].flush(clock.Now(), epoch1Seq[i], values, false))
		}
		clock.Advance(10 * time.Second)
	}

	if got := met.GatedFrames(); got != 0 {
		t.Errorf("GatedFrames() = %d, want 0", got)
	}
	if got := met.LateFramesDropped(); got != 0 {
		t.Errorf("LateFramesDropped() = %d, want 0", got)
	}
	if got := met.DuplicateFramesDropped(); got != 0 {
		t.Errorf("DuplicateFramesDropped() = %d, want 0", got)
	}
	if got := met.KeyringMisses(); got != 0 {
		t.Errorf("KeyringMisses() = %d, want 0", got)
	}
	if got := met.UnregisteredCodec(); got != 0 {
		t.Errorf("UnregisteredCodec() = %d, want 0", got)
	}
	if eng.ShardDegraded(shardID) {
		t.Errorf("shard %d DEGRADED after a quiet jittered epoch rotation, want healthy", shardID)
	}
	if events := sink.Events(); len(events) != 0 {
		t.Errorf("got %d anomaly events from a perfectly quiet fleet, want 0: %+v", len(events), events)
	}
}
