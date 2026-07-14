/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// testMonthConfig is a ~600-window micro-month: 40 groups × 3 pods across
// 2 emitters, heavy per-instance noise (so the tiny fleet is not in the
// storm detector's σ→0 hair-trigger regime), and all three incident kinds
// at explicitly mid-keyframe-cycle onsets.
func testMonthConfig() monthConfig {
	return monthConfig{
		M: 512, D: 6, Bits: 8, Seed: 1, ShardID: 1,
		TenantKey: []byte("month-test-key"), Codec: wire.CodecNone,
		Interval: 10 * time.Second, TotalWindows: 600, WindowsPerDay: 600,
		Groups: 40, InstancesPerLogical: 3, Emitters: 2,
		KeyframeEvery: 6, GoldenEvery: 10, SeriesTTL: 90 * time.Second,
		SnapshotThreshold: 20, RingWindow: 15 * time.Minute,
		Noise: 1.5, SeasonalAmp: 0.25,
		PodChurnPerMin: 2, LogicalChurnPerMin: 1,
		StormWindow: 30, StormMultiplier: 25, FallbackTopK: 100,
		BaselineEvery: 50, SolveBudget: 40,
		MaxResidual: 5.0, DriftThresh: 10.0,
		Incidents: []monthIncident{
			{Name: "spike", Kind: "spike", StartWin: 100, DurWin: 12, Magnitude: 50, Groups: 2},
			{Name: "leak", Kind: "ramp", StartWin: 250, DurWin: 60, Magnitude: 3, Groups: 1},
			{Name: "azdown", Kind: "mass", StartWin: 400, DurWin: 18, Magnitude: 100, GroupFrac: 0.3},
		},
	}
}

func TestMonthBench(t *testing.T) {
	cfg := testMonthConfig()
	res, err := runMonthBench(cfg)
	if err != nil {
		t.Fatalf("runMonthBench: %v", err)
	}

	total := monthTotals(res)
	if total.PLTotal <= 0 || total.PLGolden <= 0 || total.PLKDelta <= 0 || total.PLResidual <= 0 {
		t.Fatalf("ledger missing frame classes: %+v", total)
	}
	if total.BaseRaw <= total.PLTotal {
		t.Errorf("uncompressed baseline (%d) should exceed palimpsest total (%d) even at this tiny scale", total.BaseRaw, total.PLTotal)
	}
	if total.BaseSnappy <= 0 || total.BaseZstd <= 0 || total.BaseSnappy >= total.BaseRaw || total.BaseZstd >= total.BaseSnappy {
		t.Errorf("baseline compression ordering violated: raw=%d snappy=%d zstd=%d", total.BaseRaw, total.BaseSnappy, total.BaseZstd)
	}

	byName := map[string]monthIncidentReport{}
	for _, rep := range res.Incidents {
		byName[rep.Inc.Name] = rep
	}

	// The spike is far above the solver's floor: FISTA must find it.
	// (Whether it also clears the residual gate is scale-dependent — at
	// this toy n/m the gate is mostly open; at the default 10k-series
	// scale it is mostly shut. Both are honest; neither is asserted.)
	spike := byName["spike"]
	if spike.SupportLagWin < 0 {
		t.Errorf("spike: solver never put the target in support (rate %.2f)", spike.SupportRate)
	}

	// The leak's climb must be invisible to the two escape hatches with
	// hard thresholds: the storm breaker (its energy contribution is
	// negligible) and substrate (c) (its end-of-ramp cliff is 3.0, below
	// the 10.0 drift threshold). Substrate (a) support at this noise
	// level is indistinguishable from the fleet's own jitter, so it is
	// reported but not asserted.
	leak := byName["leak"]
	if leak.StormWindows > 0 || leak.DriftLagWin >= 0 {
		t.Errorf("leak escaped the dead zone: storm=%d drift=%d (max Δ %.2f)", leak.StormWindows, leak.DriftLagWin, leak.MaxDrift)
	}

	// The AZ collapse must trip the breaker, ship FALLBACK frames, and
	// account their bytes; top-K coverage must be a sane fraction.
	az := byName["azdown"]
	if az.StormWindows == 0 || az.FallbackTotal == 0 {
		t.Errorf("azdown: breaker never fired (storm=%d fallbackTotal=%d)", az.StormWindows, az.FallbackTotal)
	}
	if az.FallbackSeen > az.FallbackTotal {
		t.Errorf("azdown: fallback coverage %d/%d exceeds its denominator", az.FallbackSeen, az.FallbackTotal)
	}
	if total.PLFallbk <= 0 {
		t.Errorf("ledger shows no FALLBACK bytes despite %d storm windows", az.StormWindows)
	}

	if res.QuietSolve.Solves == 0 {
		t.Error("no quiet-window solves ran")
	}

	// Reports must render.
	if csv := monthCSV(res); !strings.HasPrefix(csv, "day,base_raw,") {
		t.Errorf("monthCSV header missing:\n%s", csv)
	}
	if svg := renderMonthSVG(cfg, res); !strings.Contains(svg, "<svg") || !strings.Contains(svg, "Palimpsest") {
		t.Errorf("renderMonthSVG missing expected content")
	}
	if md := monthSummaryMD(cfg, res); !strings.Contains(md, "## Incidents") {
		t.Errorf("monthSummaryMD missing incidents table")
	}
}

// TestDefaultMonthIncidentsAvoidKeyframeAlignment pins the ADR-003 nudge:
// an incident starting exactly on a keyframe window is re-based away
// before producing a single residual, so the default script must never
// schedule one there.
func TestDefaultMonthIncidentsAvoidKeyframeAlignment(t *testing.T) {
	for _, total := range []uint32{8640, 259200, 1000} {
		for _, inc := range defaultMonthIncidents(total, 6) {
			if inc.StartWin%6 == 0 {
				t.Errorf("total=%d: incident %s starts on a keyframe window (%d)", total, inc.Name, inc.StartWin)
			}
			if inc.DurWin < 1 {
				t.Errorf("total=%d: incident %s has zero duration", total, inc.Name)
			}
		}
	}
}
