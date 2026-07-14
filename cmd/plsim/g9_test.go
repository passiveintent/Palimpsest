/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
)

// TestPipelineFreezeBlocksRebasing pins G9 fix F3's encode-side semantics:
// without a freeze, a sustained shift is re-based into the baseline at the
// next keyframe (residual returns to ~0, ADR-003); with the series frozen,
// the shift keeps producing its full residual after the keyframe, and
// un-freezing re-bases it away again at the following keyframe.
func TestPipelineFreezeBlocksRebasing(t *testing.T) {
	newPipe := func() *pipeline {
		return newPipeline(pipelineConfig{
			TenantKey: []byte("g9-test-key"), ShardID: 1, EmitterID: 1, ViewID: 0,
			M: 256, D: 4, Bits: 8, KeyframeEvery: 3, GoldenEvery: 10,
			SeriesTTL: time.Hour, CaptureResiduals: true,
		})
	}
	const name = "g9_freeze_target|agg=sum"
	id := sketch.SeriesID([]byte(name))
	values := func(v float64) map[string]float64 {
		return map[string]float64{name: v, "g9_freeze_other|agg=sum": 10}
	}
	run := func(freeze bool) (afterKeyframe float64) {
		p := newPipe()
		now := time.Unix(1_700_000_000, 0)
		if freeze {
			p.SetFrozen(map[uint64]struct{}{id: {}})
		}
		// w0 keyframe (birth), w1-w2 shifted, w3 keyframe (re-base or not),
		// w4 still shifted: the residual at w4 is the probe.
		vals := []float64{100, 130, 130, 130, 130}
		for w, v := range vals {
			p.flush(now.Add(time.Duration(w)*10*time.Second), uint32(w), values(v))
		}
		return p.lastResiduals[id]
	}

	if r := run(false); math.Abs(r) > 1e-6 {
		t.Fatalf("unfrozen residual after keyframe = %v, want ~0 (ADR-003 re-basing)", r)
	}
	if r := run(true); math.Abs(r-30) > 1e-6 {
		t.Fatalf("frozen residual after keyframe = %v, want 30 (freeze blocks re-basing)", r)
	}
}

// TestG9WorldDeterministicAcrossConfigs pins the A/B guarantee: the world
// draws depend only on the seed, never on which fixes are enabled.
func TestG9WorldDeterministicAcrossConfigs(t *testing.T) {
	mk := func(f1, f2, f3 bool) []float64 {
		cfg := g9Config{Seed: 41, Series: 25, Noise: 0.2, SeasonalAmp: 0.25,
			F1: f1, F2: f2, F3: f3, CGate: 1.2, Kappa: 0.7}
		w := newG9World(&cfg)
		dst := make([]float64, len(w.names))
		w.valuesAt(1234, w.rng, dst)
		return dst
	}
	a, b := mk(false, false, false), mk(true, true, true)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("world draw %d differs across fix configs: %v vs %v", i, a[i], b[i])
		}
	}
	if fmt.Sprint(a) == fmt.Sprint(make([]float64, len(a))) {
		t.Fatal("world produced all zeros")
	}
}
