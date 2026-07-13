/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"math"
	"strings"
	"testing"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
)

// testDeadzoneConfig is a scaled-down sweep (small sketch, few iterations,
// 3 trials) that still spans both regimes: a large anomaly on a
// near-quiet fleet, and a small anomaly drowned in heavy per-series noise.
func testDeadzoneConfig() deadzoneConfig {
	return deadzoneConfig{
		M: 512, D: 6, Bits: 8, Seed: 1,
		SketchSeed: sketch.DeriveEphemeralSeed([]byte("deadzone-test-key"), 1, 0, deadzoneViewID),
		Background: 200,
		Magnitudes: []float64{0.5, 20},
		Noises:     []float64{0.05, 2.0},
		Trials:     3,

		StormWindow:     10,
		StormMultiplier: 25,
		FallbackTopK:    100,

		FISTAIters:      120,
		FISTALambda:     0.05,
		FISTAPowerIters: 30,
		FISTAThreshold:  0.3,
		MaxResidual:     5.0,
	}
}

// TestDeadzoneSweepFindsDeadZone pins the dead zone's existence: the
// production-shaped pipeline recovers a large anomaly out of a near-quiet
// fleet, and loses a small anomaly diluted in heavy noise — with the storm
// breaker silent, so nothing else catches it either. This is the boundary
// docs/DEADZONE.md documents; if recovery or storm behavior shifts enough
// to close (or silently widen) the zone, this fails and the docs need
// re-measuring.
func TestDeadzoneSweepFindsDeadZone(t *testing.T) {
	cfg := testDeadzoneConfig()
	cells, err := runDeadzoneSweep(cfg)
	if err != nil {
		t.Fatalf("runDeadzoneSweep: %v", err)
	}
	if len(cells) != len(cfg.Noises) || len(cells[0]) != len(cfg.Magnitudes) {
		t.Fatalf("grid shape = %dx%d, want %dx%d", len(cells), len(cells[0]), len(cfg.Noises), len(cfg.Magnitudes))
	}

	// σ=0.05, ε=20: far above the support threshold, negligible dilution —
	// every trial must recover and detect it.
	quietBig := cells[0][1]
	if quietBig.Recovered != cfg.Trials || quietBig.Detected != cfg.Trials {
		t.Errorf("quiet/big cell: Recovered=%d Detected=%d, want both %d (residual sum %v)",
			quietBig.Recovered, quietBig.Detected, cfg.Trials, quietBig.ResidualSum)
	}

	// σ=2.0, ε=0.5: the dead zone. The anomaly is ~0.2% of the fleet's
	// noise energy (storm bar ≈ σ·sqrt(24·n) ≈ 138, far above 0.5) so the
	// breaker must stay silent, and the solve is noise-swamped (residual
	// ≈ sqrt(n)·σ ≫ MaxResidual) so no deviation event may emit.
	dead := cells[1][0]
	if dead.Storms != 0 {
		t.Errorf("dead cell: storm tripped %d times, want 0 — a small anomaly must not spike global energy", dead.Storms)
	}
	if dead.Recovered != 0 || dead.Detected != 0 {
		t.Errorf("dead cell: Recovered=%d Detected=%d, want 0/0 (support=%d gated=%d)",
			dead.Recovered, dead.Detected, dead.SupportHits, dead.Gated)
	}

	// The outputs must render for any sweep result.
	csv := deadzoneCSV(cells)
	if !strings.HasPrefix(csv, "noise,magnitude,") || strings.Count(csv, "\n") != 1+len(cfg.Noises)*len(cfg.Magnitudes) {
		t.Errorf("deadzoneCSV: unexpected shape:\n%s", csv)
	}
	svg := renderDeadzoneSVG(cfg, cells)
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "dead zone") {
		t.Errorf("renderDeadzoneSVG: missing expected content")
	}
	md := deadzoneSummaryMD(cfg, cells)
	if !strings.Contains(md, "| noise σ |") {
		t.Errorf("deadzoneSummaryMD: missing boundary table:\n%s", md)
	}
}

func TestParseDeadzoneGrid(t *testing.T) {
	got, err := parseDeadzoneGrid(" 3.2,0.1, 0.8 ,0.1", false)
	if err != nil {
		t.Fatalf("parseDeadzoneGrid: %v", err)
	}
	want := []float64{0.1, 0.8, 3.2}
	if len(got) != len(want) {
		t.Fatalf("grid = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("grid = %v, want %v", got, want)
		}
	}

	if _, err := parseDeadzoneGrid("0,1", false); err == nil {
		t.Error("zero magnitude accepted, want error")
	}
	if _, err := parseDeadzoneGrid("0,1", true); err != nil {
		t.Errorf("zero noise rejected: %v", err)
	}
	if _, err := parseDeadzoneGrid("-1", true); err == nil {
		t.Error("negative value accepted, want error")
	}
	if _, err := parseDeadzoneGrid(" , ", true); err == nil {
		t.Error("empty grid accepted, want error")
	}
}

func TestDeadzoneFloorAndCeiling(t *testing.T) {
	col := []*deadzoneCell{
		{Magnitude: 1, Trials: 10, Recovered: 0},
		{Magnitude: 2, Trials: 10, Recovered: 10},
		{Magnitude: 4, Trials: 10, Recovered: 3}, // dip: gate closed here
		{Magnitude: 8, Trials: 10, Recovered: 10},
	}
	// The floor must not report 2: the dip at 4 breaks the monotone
	// envelope, so only 8 is reliably recovered "from here up".
	if got := deadzoneFloor(col, (*deadzoneCell).RecoveryRate, 0.9); got != 8 {
		t.Errorf("deadzoneFloor = %v, want 8", got)
	}
	if got := deadzoneCeiling(col, (*deadzoneCell).RecoveryRate, 0.5); got != 4 {
		t.Errorf("deadzoneCeiling = %v, want 4", got)
	}
	none := []*deadzoneCell{{Magnitude: 1, Trials: 10, Recovered: 0}}
	if got := deadzoneFloor(none, (*deadzoneCell).RecoveryRate, 0.9); !math.IsNaN(got) {
		t.Errorf("deadzoneFloor on never-recovered column = %v, want NaN", got)
	}
	all := []*deadzoneCell{{Magnitude: 1, Trials: 10, Recovered: 10}}
	if got := deadzoneCeiling(all, (*deadzoneCell).RecoveryRate, 0.5); !math.IsNaN(got) {
		t.Errorf("deadzoneCeiling on always-recovered column = %v, want NaN", got)
	}
}
