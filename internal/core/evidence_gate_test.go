/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"fmt"
	"testing"
	"time"
)

// TestDictRootEvidenceGateMeasurement is the ADR-017 dict_root Merkle/resync
// evidence-gate byte measurement (docs/generated/palimpsest_prod_wave_prompts_9-16.md
// PROMPT 16, Part A, condition 1). It drives the real encode path
// (pkg/sketch, pkg/wire, via this package's testEncoder, which mirrors
// cmd/plsim/encoder.go exactly) and the real decode path (this package's
// Engine) through cmd/plsim/world.go's own churn formula
// (perWindow = (churnPerMinute/2)/60*intervalSeconds) at --churn-rate
// {10,60,300}/min, for a 1h-equivalent run: 360 windows at a realistic 10s
// flush interval (plsim's own --interval default), at the same scale
// docs/PERF.md's "Metadata overhead at churn-rate 60/min" section already
// established (300 logical series, plsim's default m/d/bits/keyframe
// cadence). It reports full-dict-keyframe bytes as a fraction of total wire
// bytes per shard -- the ADR-017 decision rule's first condition. See
// TestDictRootHealTimeMeasurement for the second condition (time-to-heal).
func TestDictRootEvidenceGateMeasurement(t *testing.T) {
	if testing.Short() {
		t.Skip("long-running evidence-gate measurement; run explicitly (go test -run TestDictRootEvidenceGateMeasurement -v) to regenerate docs/PERF.md numbers")
	}

	for _, churnPerMin := range []float64{10, 60, 300} {
		t.Run(fmt.Sprintf("churn_%d_per_min", int(churnPerMin)), func(t *testing.T) {
			runByteFractionScenario(t, churnPerMin)
		})
	}
}

const (
	evidenceShardID       = uint64(9001)
	evidenceEmitterID     = uint64(1)
	evidenceViewID        = uint16(0)
	evidenceM             = 2000 // plsim/demo default sketch width
	evidenceD             = 6
	evidenceBits          = 8
	evidenceKeyframeEvery = 6  // plsim default: KEYFRAME every 6th window
	evidenceGoldenEvery   = 10 // plsim default: golden (Full) every 10th keyframe
	evidenceLogicalSeries = 300
	evidenceInterval      = 10 * time.Second // plsim's own --interval default
	evidenceWindows       = 360              // 1h-equivalent at a 10s flush interval
	evidenceSeriesTTL     = 90 * time.Second // plsim's own --series-ttl default
	evidenceBaseline      = 100.0
)

// runByteFractionScenario runs one churn-rate scenario end to end (lossless
// delivery -- no induced drop) and logs the byte-fraction number the
// ADR-017 decision rule's first condition is evaluated against.
func runByteFractionScenario(t *testing.T, churnPerMin float64) {
	t.Helper()

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      1e9, // steady-state/no injected anomalies: never gate on residual here
		AllowedLateness:  1,
		RepairHorizon:    time.Minute,
		EmittersExpected: 1,
		FISTAIters:       100,
		FISTALambda:      0.05,
		FISTAPowerIters:  20,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, _, _, met, clock := newTestEngine(cfg)
	enc := newTestEncoder(testTenantKey, evidenceShardID, evidenceEmitterID, evidenceViewID, 0,
		evidenceM, evidenceD, evidenceBits, evidenceKeyframeEvery, evidenceGoldenEvery, evidenceSeriesTTL)

	aggs := [3]string{"sum", "count", "max"}
	nameFor := func(idx int, agg string) string {
		return fmt.Sprintf("evidence_metric_%05d|shard=%d|agg=%s", idx, evidenceShardID, agg)
	}

	// active holds currently-live group indices in birth order; retiring the
	// oldest (FIFO) is a deterministic stand-in for cmd/plsim/world.go's
	// random victim pick -- which specific group churns doesn't matter for a
	// byte measurement, only how many groups churn and when.
	active := make([]int, evidenceLogicalSeries)
	for i := range active {
		active[i] = i
	}
	nextIdx := evidenceLogicalSeries

	// perWindowChurn mirrors cmd/plsim/world.go's stepChurn exactly: each
	// replacement is one death + one birth (2 lifecycle events), and
	// ChurnPerMinute already counts both, so a replacement's rate is half.
	perWindowChurn := (churnPerMin / 2) / 60 * evidenceInterval.Seconds()
	var churnBudget float64

	for seq := uint32(0); seq < evidenceWindows; seq++ {
		churnBudget += perWindowChurn
		for churnBudget >= 1 && len(active) > 0 {
			churnBudget--
			active = active[1:]
			active = append(active, nextIdx)
			nextIdx++
		}

		values := make(map[string]float64, len(active)*3)
		for _, idx := range active {
			for _, agg := range aggs {
				v := evidenceBaseline
				if agg == "count" {
					v = 1
				}
				values[nameFor(idx, agg)] = v
			}
		}

		frame := enc.flush(clock.Now(), seq, values, false)
		eng.HandleFrame(ctx, frame)
		clock.Advance(evidenceInterval)
	}

	total := met.WireBytesTotal(evidenceShardID)
	full := met.FullDictKeyframeBytes(evidenceShardID)

	var pct float64
	if total > 0 {
		pct = 100 * float64(full) / float64(total)
	}

	t.Logf("churn=%v/min windows=%d interval=%s logical_series=%d->%d(end) total_wire_bytes=%d full_dict_keyframe_bytes=%d (%.3f%%) dict_root_mismatches=%d degraded_at_end=%v",
		churnPerMin, evidenceWindows, evidenceInterval, evidenceLogicalSeries, nextIdx, total, full, pct, met.DictRootMismatches(evidenceShardID), eng.ShardDegraded(evidenceShardID))

	if total == 0 {
		t.Fatalf("total_wire_bytes = 0, scenario produced no frames")
	}
	// This scenario is lossless (no dropped frames), so a naive expectation
	// would be dict_root_mismatches == 0 and degraded_at_end == false. In
	// practice, at realistic churn rates over an hour, a tombstone reliably
	// happens to expire in the very same window as a golden keyframe at
	// least once -- and that tombstone is silently dropped from the wire
	// (the "Discovered defect" in docs/adr/ADR-017-merkle-decision.md):
	// cmd/plsim/encoder.go, otel/processor/csresidual/processor.go, and this
	// package's testEncoder all replace that window's dict_delta list with
	// tracker.FullDict() wholesale on a golden keyframe, discarding that
	// same call's Expire() tombstones. Unlike a dropped birth (which the
	// *next* golden's full-dict resync repairs, see
	// TestDictRootHealTimeMeasurement), a lost tombstone leaves a stale ID
	// in the decoder's dictionary that the encoder never re-announces --
	// nothing ever removes it, so the view stays degraded for the rest of
	// the run. This assertion pins that known, out-of-scope behavior rather
	// than hiding it; it is not this test's job to fix it (GUARDRAILS: no
	// protocol changes; discovered defects become a v3 proposal appendix).
	if got := met.DictRootMismatches(evidenceShardID); got > 1 {
		t.Fatalf("dict_root_mismatches = %d, want 0 or 1 (see the Discovered defect note above)", got)
	}
	if !eng.ShardDegraded(evidenceShardID) {
		t.Logf("note: shard healed by end of run (the golden/tombstone collision did not occur this run, or a later event happened to repair it)")
	}
}

// TestDictRootHealTimeMeasurement is the ADR-017 evidence gate's second
// condition (docs/generated/palimpsest_prod_wave_prompts_9-16.md PROMPT 16,
// Part A, "time-to-heal (mismatch -> verified root)"). Lossless delivery
// never produces a mismatch (confirmed above), so this test induces exactly
// one: it drops a single dict_delta-bearing frame shortly after the run's
// opening golden keyframe -- simulating one lost frame (a dropped delivery,
// or a crashed/restarted agent's gap) -- and measures how long the
// resulting dict_root divergence takes to heal at the next golden keyframe.
//
// seriesTTL here is set longer than the observation window so no series
// tombstones during the run: this isolates the mechanism's designed
// behavior (a birth missed once, healed by the next golden's full-dict
// resync) from a separate, already-discovered defect (see
// docs/adr/ADR-017-merkle-decision.md's "Discovered defect" section): a
// tombstone whose TTL expires in the *same* window as a golden keyframe is
// silently dropped from the wire (cmd/plsim/encoder.go, otel/processor/
// csresidual/processor.go, and this package's testEncoder all replace
// dictDeltas with tracker.FullDict() wholesale on a golden window, discarding
// that window's own Expire() tombstones), which can make a divergence
// permanent rather than bounded. That defect is out of scope for this RFC
// prompt (no protocol changes) and is not what this test measures.
func TestDictRootHealTimeMeasurement(t *testing.T) {
	const (
		shardID       = uint64(9002)
		emitterID     = uint64(1)
		viewID        = uint16(0)
		keyframeEvery = 6
		goldenEvery   = 10
		interval      = 10 * time.Second
		windows       = 61        // through the second golden keyframe (seq 60)
		longSeriesTTL = time.Hour // long enough that nothing tombstones during the run
	)

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      1e9,
		AllowedLateness:  1,
		RepairHorizon:    time.Minute,
		EmittersExpected: 1,
		FISTAIters:       50,
		FISTALambda:      0.05,
		FISTAPowerIters:  10,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, _, _, met, clock := newTestEngine(cfg)
	enc := newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, 200, 6, 8, keyframeEvery, goldenEvery, longSeriesTTL)

	names := []string{"heal_a|shard=9002|agg=sum", "heal_b|shard=9002|agg=sum"}
	values := map[string]float64{names[0]: 1, names[1]: 1}

	droppedAt := -1
	for seq := uint32(0); seq < windows; seq++ {
		if seq == 3 {
			values["heal_c|shard=9002|agg=sum"] = 5 // triggers a birth delta to drop
		}
		f := enc.flush(clock.Now(), seq, values, false)
		if seq == 3 {
			droppedAt = int(seq)
			// Simulate one lost frame: never deliver it.
		} else {
			eng.HandleFrame(ctx, f)
		}
		clock.Advance(interval)
	}

	mismatches := met.DictRootMismatches(shardID)
	healMax := met.DictRootHealSecondsMax(shardID)
	goldenCycle := time.Duration(keyframeEvery*goldenEvery) * interval

	t.Logf("dropped_at_seq=%d keyframe_every=%d golden_every=%d golden_cycle=%s dict_root_mismatches=%d heal_max=%s degraded_at_end=%v",
		droppedAt, keyframeEvery, goldenEvery, goldenCycle, mismatches, healMax, eng.ShardDegraded(shardID))

	if mismatches != 1 {
		t.Fatalf("dict_root_mismatches = %d, want exactly 1 (one induced drop)", mismatches)
	}
	if healMax == 0 {
		t.Fatalf("heal_max = 0, want > 0 (the induced mismatch should heal at the next golden keyframe)")
	}
	if eng.ShardDegraded(shardID) {
		t.Fatalf("shard still degraded at end of run; expected the golden keyframe at seq=60 to heal it")
	}
	if healMax > 2*goldenCycle {
		t.Fatalf("heal_max = %s exceeds 2x the golden cycle (%s); ADR-017 decision rule would trip", healMax, 2*goldenCycle)
	}
}
