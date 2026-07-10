/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"testing"
	"time"
)

// TestE2E_UnregisteredCodecGatesKeyframe covers the ADR-006 §Addendum
// guardrail: a frame naming a Codec with no registered Compressor (e.g. a
// peer that encoded with zstd while this decoder never registered
// zstdcodec) must never be silently dropped or crash the engine — it gates
// the stream at low confidence (mirroring IncKeyringMiss's identical policy
// for an unknown key_version) and is counted via Metrics.UnregisteredCodec.
func TestE2E_UnregisteredCodecGatesKeyframe(t *testing.T) {
	const (
		shardID   = uint64(9)
		emitterID = uint64(901)
		viewID    = uint16(0)
		m         = 64
		d         = 4
		bits      = 8
	)

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      5.0,
		AllowedLateness:  1,
		RepairHorizon:    time.Minute,
		EmittersExpected: 1,
		FISTAIters:       100,
		FISTALambda:      0.05,
		FISTAPowerIters:  30,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, _, _, met, clock := newTestEngine(cfg)
	enc := newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)

	names := []string{"unregistered_codec_metric|shard=9|agg=sum"}
	f := enc.flush(clock.Now(), 0, constValues(names, 1.0), true /* forceGolden */)

	const unregisteredCodec = 99 // never Register-ed in this test binary
	f.Codec = unregisteredCodec

	before := met.UnregisteredCodec()
	eng.HandleFrame(ctx, f)

	if !eng.ShardDegraded(shardID) {
		t.Fatalf("expected shard %d to be DEGRADED after a KEYFRAME with an unregistered codec", shardID)
	}
	if got := met.UnregisteredCodec(); got != before+1 {
		t.Fatalf("UnregisteredCodec() = %d, want %d", got, before+1)
	}
}
