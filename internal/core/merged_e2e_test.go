/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/recover"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"

	"github.com/passiveintent/Palimpsest/internal/metrics"
	"github.com/passiveintent/Palimpsest/internal/ports"
)

// ADR-015 merged-tier E2E scenario: a correlated 500-member group (AZ-outage
// shape, matching ADR-014's own reference scenario scale) plus 20 scattered
// singletons, whose TRUE per-series shift is diluted across `emitters` weak
// observers at per-agent SNR ~1.0 and fused via the engine's existing
// ADR-013 cross-emitter merge. See docs/adr/ADR-015-merged-tier.md.
const (
	mergedN         = 50000
	mergedGroupSize = 500
	mergedScattered = 20
	mergedM         = 2000
	mergedD         = 6
	mergedBits      = 8
	mergedViewID    = uint16(1)
)

var mergedGroupLabel = "region=merged-az-east-1,cluster=prod"

type mergedNames struct {
	all       []string
	group     []string
	scattered []string
}

func buildMergedNames() mergedNames {
	var s mergedNames
	s.all = make([]string, 0, mergedN)
	for i := 0; i < mergedGroupSize; i++ {
		name := fmt.Sprintf("merged_az_%04d|%s|agg=sum", i, mergedGroupLabel)
		s.group = append(s.group, name)
		s.all = append(s.all, name)
	}
	for i := 0; i < mergedScattered; i++ {
		name := fmt.Sprintf("merged_scattered_%03d|uid=%d,cluster=prod|agg=sum", i, i)
		s.scattered = append(s.scattered, name)
		s.all = append(s.all, name)
	}
	for i := 0; i < mergedN-mergedGroupSize-mergedScattered; i++ {
		s.all = append(s.all, fmt.Sprintf("merged_quiet_%05d|shard=merged|agg=sum", i))
	}
	return s
}

// mergedTrueShifts draws each group/scattered series' TRUE combined
// (post-fusion, i.e. as if summed across however many agents observe it)
// shift once, deterministically, so the 2000- and 100-emitter runs fuse the
// SAME underlying scenario — only the live emitter coverage (A) differs
// between them, matching the ADR-015 acceptance criteria's "same scenario,
// fewer emitters" framing.
func mergedTrueShifts(s mergedNames) map[string]float64 {
	rng := rand.New(rand.NewSource(99))
	shifts := make(map[string]float64, len(s.group)+len(s.scattered))
	for _, name := range s.group {
		shifts[name] = 0.5 + rng.Float64()*1.5 // [0.5, 2.0) -- ADR-014's AZ-group amplitude range
	}
	for _, name := range s.scattered {
		shifts[name] = 2.0 + rng.Float64()*6.0 // [2.0, 8.0) -- ADR-014's scattered amplitude range
	}
	return shifts
}

// buildResidualFrame hand-builds a minimal RESIDUAL wire.Frame carrying y
// directly (bypassing Tracker/Accumulator dictionary-lifecycle bookkeeping,
// which a merged-tier weak observer has no need of: its only job is to
// contribute one window's worth of signal for an already-known set of
// series). Mirrors e2e_test.go's TestE2E_DegradedHealsOnGoldenKeyframe
// ghost-frame construction.
func buildResidualFrame(shardID, emitterID uint64, viewID uint16, seq uint32, m, d, bits int, y []float64) *wire.Frame {
	scale := wire.AdaptiveScale(y, bits)
	payload, err := wire.Quantize(y, uint8(bits), scale)
	if err != nil {
		panic(err)
	}
	return &wire.Frame{
		Magic: wire.Magic, Version: wire.Version, FrameType: wire.FrameTypeResidual,
		EmitterID: emitterID, ShardID: shardID, Epoch: 0, Seq: seq,
		ViewID: viewID, M: uint32(m), D: uint8(d), Bits: uint8(bits),
		QuantScale: scale, Payload: payload,
	}
}

// mergedScenarioResult bundles what the guardrail E2E tests assert against
// after driving `emitters` weak-observer frames for one injected window
// through a real Engine.
type mergedScenarioResult struct {
	events         []ports.AnomalyEvent
	met            *metrics.Metrics
	injectedWindow uint32
	groupID        uint64
	res            recover.Result // directly re-solved for Iters/GroupIDs/wall-time inspection
	solveDur       time.Duration

	// y/dict/params/opts are the exact inputs the direct re-solve above
	// used, kept around so a caller can also compute recover.GroupZScore
	// against them without re-deriving anything.
	y      []float64
	dict   *recover.Dictionary
	params sketch.Params
	opts   recover.Options
}

// runMergedTierScenario drives the ADR-015 correlated-group-across-many-
// weak-emitters scenario through a real Engine (genesis emitter births the
// full mergedN-series dictionary; `emitters` weak observers each contribute
// one window's worth of per-agent-SNR~1.0 signal, fused via the engine's
// existing ADR-013 merge), then directly re-solves the merged window (same
// y, same dictionary) to inspect Result.Iters/GroupIDs and measure wall
// time in isolation from the surrounding per-window bookkeeping cost.
func runMergedTierScenario(t *testing.T, shardID uint64, emitters int, policy MergedPolicy) mergedScenarioResult {
	t.Helper()
	const (
		genesisEmitter  = uint64(999)
		weakEmitterBase = uint64(1_000_000)
	)

	cfg := Config{
		TenantKey:         testTenantKey,
		MaxResidual:       500.0, // generous: this scenario tests the ADR-015 guardrail label, not the residual gate
		AllowedLateness:   2,
		RepairHorizon:     time.Minute,
		EmittersExpected:  emitters,
		FISTAIters:        350,
		FISTALambda:       0.05,
		FISTAPowerIters:   50,
		FISTAThreshold:    0.3,
		EscalateThreshold: 0.3,
		DriftThreshold:    1e9,
		MergedViews:       map[uint16]MergedPolicy{mergedViewID: policy},
	}
	eng, sink, _, met, clock := newTestEngine(cfg)
	genesis := newTestEncoder(testTenantKey, shardID, genesisEmitter, mergedViewID, 0, mergedM, mergedD, mergedBits, 1_000_000, 1, time.Hour)

	names := buildMergedNames()
	shifts := mergedTrueShifts(names)
	baseline := constValues(names.all, 100.0)

	seq := uint32(0)
	step := func() {
		eng.HandleFrame(ctx, genesis.flush(clock.Now(), seq, baseline, false))
		seq++
		clock.Advance(10 * time.Second)
	}
	step() // window 0: golden keyframe, births all mergedN names
	for i := 0; i < 2; i++ {
		step()
	}

	injectedWindow := seq
	seed := sketch.DeriveEphemeralSeed(testTenantKey, shardID, 0, mergedViewID)
	rng := rand.New(rand.NewSource(1))
	for agent := 0; agent < emitters; agent++ {
		acc := sketch.NewAccumulator(sketch.Params{M: mergedM, D: mergedD, Seed: seed, Bits: mergedBits})
		for name, trueShift := range shifts {
			sigmaAgent := trueShift / float64(emitters) // per-agent SNR ~1.0, see docs/adr/ADR-015
			v := trueShift/float64(emitters) + rng.NormFloat64()*sigmaAgent
			acc.Update([]byte(name), v)
		}
		y, _ := acc.Flush()
		frame := buildResidualFrame(shardID, weakEmitterBase+uint64(agent), mergedViewID, injectedWindow, mergedM, mergedD, mergedBits, y)
		eng.HandleFrame(ctx, frame)
	}
	seq++
	clock.Advance(10 * time.Second)
	for i := 0; i < cfg.AllowedLateness+2; i++ { // cross the watermark with margin
		step()
	}

	eng.mu.Lock()
	shard := eng.shards[shardID]
	eng.mu.Unlock()
	shard.mu.Lock()
	view := shard.Views[viewKey{epoch: 0, view: mergedViewID, keyVersion: 0}]
	win := view.Windows[injectedWindow]
	y := append([]float64(nil), win.Y...)
	dict := view.Dict
	shard.mu.Unlock()

	groupID := recover.DefaultGrouper()(sketch.SeriesID([]byte(names.group[0])), dict)

	params := sketch.Params{M: mergedM, D: mergedD, Seed: seed}
	opts := cfg.recoverOptions(0)
	start := time.Now()
	res, err := recover.Recover(y, dict, params, opts)
	solveDur := time.Since(start)
	if err != nil {
		t.Fatalf("direct re-solve of the merged window: %v", err)
	}

	return mergedScenarioResult{
		events:         sink.Events(),
		met:            met,
		injectedWindow: injectedWindow,
		groupID:        groupID,
		res:            res,
		solveDur:       solveDur,
		y:              y,
		dict:           dict,
		params:         params,
		opts:           opts,
	}
}

// TestE2E_MergedTierProvenEscalation covers the ADR-015 "proven" acceptance
// scenario: a correlated 500-member group fused across 2000 weak observers
// at per-agent SNR ~1.0 escalates via the standard Recover() trigger
// contract, Group-OMP activates the group, group recall >= 0.90, and the
// scaling-law guardrail's live A=2000 clears the configured threshold.
//
// Guardrail arithmetic (documented per the acceptance criteria): with
// SigmaEstimate=1.0 (config estimate matching this scenario's true
// per-agent SNR) and N=50000 (mergedN, the dictionary size), MinSignalRatio
// = 3.0 requires k/N*A >= 3, i.e. k >= 3*50000/2000 = 75. The group alone
// (k~520 once activated) clears this by a wide margin -- see the logged
// ratio for the actual measured k.
func TestE2E_MergedTierProvenEscalation(t *testing.T) {
	if testing.Short() {
		t.Skip("large recovery solve; skipped under -short")
	}
	const (
		shardID  = uint64(900)
		emitters = 2000
	)
	policy := MergedPolicy{MinSignalRatio: 3.0, SigmaEstimate: 1.0}
	r := runMergedTierScenario(t, shardID, emitters, policy)

	if r.res.Confidence != recover.ConfidenceRecoveredGroup {
		t.Fatalf("Confidence = %q, want %q (escalation should have fired: residual=%.4f raw_support=%d)",
			r.res.Confidence, recover.ConfidenceRecoveredGroup, r.res.Residual, r.res.RawSupport)
	}
	azFlagged := false
	for _, gid := range r.res.GroupIDs {
		if gid == r.groupID {
			azFlagged = true
			break
		}
	}
	if !azFlagged {
		t.Fatalf("group ID %d not in GroupIDs (len=%d)", r.groupID, len(r.res.GroupIDs))
	}

	recovered := make(map[uint64]struct{}, len(r.res.SupportIDs))
	for _, id := range r.res.SupportIDs {
		recovered[id] = struct{}{}
	}
	names := buildMergedNames()
	var hit int
	for _, name := range names.group {
		if _, ok := recovered[sketch.SeriesID([]byte(name))]; ok {
			hit++
		}
	}
	recall := float64(hit) / float64(mergedGroupSize)
	if recall < 0.90 {
		t.Fatalf("group recall = %.2f (%d/%d), want >= 0.90", recall, hit, mergedGroupSize)
	}

	// ADR-015: MergedTrust is carried on its own field, independent of
	// Confidence, and must reach every emitted deviation event for this
	// window.
	var sawTrust bool
	for _, ev := range r.events {
		if ev.Substrate == "deviation" && ev.WindowID == r.injectedWindow {
			sawTrust = true
			if ev.MergedTrust != recover.MergedTrustProven {
				t.Fatalf("event MergedTrust = %q, want %q", ev.MergedTrust, recover.MergedTrustProven)
			}
		}
	}
	if !sawTrust {
		t.Fatalf("expected at least one deviation event for the injected window %d", r.injectedWindow)
	}

	// ADR-014/Prompt 11c acceptance criterion (c): log the H0 z-score of the
	// activated group against the same (y, dict, params, opts) the direct
	// re-solve above used.
	zScore := recover.GroupZScore(r.y, r.dict, r.params, nil, r.groupID, r.opts)

	t.Logf("proven scenario: emitters=%d confidence=%q residual=%.4f raw_support=%d groups=%d recall=%.2f (%d/%d) iters=%d restarts=%d z_score=%.1f merged_windows{proven}=%d solve_time=%s",
		emitters, r.res.Confidence, r.res.Residual, r.res.RawSupport, len(r.res.GroupIDs), recall, hit, mergedGroupSize, r.res.Iters, r.res.Restarts, zScore, r.met.MergedWindows(recover.MergedTrustProven), r.solveDur)

	// ADR-014's existing decode budget (testGroupCaseWallTime): >=1.5s absolute.
	if r.solveDur > 1500*time.Millisecond {
		t.Fatalf("escalated solve wall time %s exceeds the 1.5s decode budget (ADR-014)", r.solveDur)
	}
}

// TestE2E_MergedTierUnprovenGuardrail covers the ADR-015 "unproven"
// acceptance scenario: the SAME scenario as TestE2E_MergedTierProvenEscalation
// (same true group/scattered shifts, same MergedPolicy), fused across only
// 100 weak observers instead of 2000. The guardrail must fire on
// MergedTrust alone -- this test does NOT assert anything about recovery
// quality/Confidence, per the acceptance criteria's explicit instruction
// that Confidence keeps reporting whatever recovery mode actually occurred.
//
// Guardrail arithmetic (documented per the acceptance criteria): with
// SigmaEstimate=1.0, N=50000, MinSignalRatio=3.0, A=100 requires
// k >= 3*50000/100 = 1500 to reach "proven". The union support Group-OMP
// can possibly produce here is bounded well below that (500-member group +
// 20 scattered + a <=M/5=400 capped plain-path support, all <= ~920), so
// the guardrail is unproven regardless of whether escalation itself
// succeeds or fails this run -- exactly the point of keeping the two
// signals independent.
func TestE2E_MergedTierUnprovenGuardrail(t *testing.T) {
	if testing.Short() {
		t.Skip("large recovery solve; skipped under -short")
	}
	const (
		shardID  = uint64(901)
		emitters = 100
	)
	policy := MergedPolicy{MinSignalRatio: 3.0, SigmaEstimate: 1.0}
	r := runMergedTierScenario(t, shardID, emitters, policy)

	var sawEvent bool
	for _, ev := range r.events {
		if ev.Substrate == "deviation" && ev.WindowID == r.injectedWindow {
			sawEvent = true
			if ev.MergedTrust != recover.MergedTrustUnproven {
				t.Fatalf("event MergedTrust = %q, want %q (ratio should be well under MinSignalRatio at A=%d)",
					ev.MergedTrust, recover.MergedTrustUnproven, emitters)
			}
		}
	}
	// A window that failed to recover anything emits no deviation events at
	// all (empty SupportIDs) -- that's still a valid outcome here, since
	// this test does not assert recovery quality. Confirm via the metric
	// instead, which evaluateMergedTrust increments unconditionally for
	// every merged-tier window solve, whether or not any event followed.
	if !sawEvent && r.met.MergedWindows(recover.MergedTrustUnproven) == 0 {
		t.Fatalf("expected merged_windows{unproven} to have incremented for the injected window")
	}

	t.Logf("unproven scenario: emitters=%d confidence=%q residual=%.4f raw_support=%d groups=%d iters=%d merged_windows{unproven}=%d solve_time=%s",
		emitters, r.res.Confidence, r.res.Residual, r.res.RawSupport, len(r.res.GroupIDs), r.res.Iters, r.met.MergedWindows(recover.MergedTrustUnproven), r.solveDur)
}

// TestE2E_CrossTenantFramesNeverSummedTrusted is the ADR-015 §1 / ADR-012
// regression test: a frame produced under a genuinely different tenant's
// key material, delivered into another tenant's Engine (a misroute -- the
// wire format carries no tenant identity to check against by design, see
// ADR-012), must never contribute to a trusted "recovered" anomaly. Either
// its key_version doesn't resolve in the receiving tenant's KeyRing (gated
// pre-merge, keyring_miss increments, zero summation), or -- the case this
// test actually exercises, since key_version collisions across tenants are
// entirely plausible (0 is the common default) -- the cryptographic seed
// mismatch makes its contribution inexplicable under the receiving
// tenant's own dictionary, and the existing high-residual/DEGRADED gate
// (Config.MaxResidual) catches it before any anomaly is emitted.
func TestE2E_CrossTenantFramesNeverSummedTrusted(t *testing.T) {
	const (
		shardID   = uint64(902)
		emitterA  = uint64(9001) // tenant A's own legitimate emitter
		emitterB  = uint64(9002) // a genuinely different tenant's emitter, misrouted here
		viewID    = uint16(0)
		m         = 200
		d         = 6
		bits      = 8
		n         = 30
	)
	tenantA := []byte("cross-tenant-e2e-tenant-A-key")
	tenantB := []byte("cross-tenant-e2e-tenant-B-key-totally-different")

	cfg := Config{
		TenantKey:        tenantA,
		MaxResidual:      40.0,
		AllowedLateness:  2,
		RepairHorizon:    time.Minute,
		EmittersExpected: 2,
		FISTAIters:       200,
		FISTALambda:      0.05,
		FISTAPowerIters:  30,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, sink, _, met, clock := newTestEngine(cfg)
	encA := newTestEncoder(tenantA, shardID, emitterA, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)
	// encB derives its seed from tenantB, exactly as a genuinely different
	// tenant's own encoder would -- its frames are only ever meaningful
	// decoded against tenantB's own seed, never tenantA's.
	encB := newTestEncoder(tenantB, shardID, emitterB, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)

	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("xtenant_metric_%02d|shard=902|agg=sum", i)
	}
	baseline := constValues(names, 10.0)
	// encB's own baseline reading, offset so its (cryptographically
	// meaningless, from tenant A's perspective) contribution to tenant A's
	// window is unambiguously large -- mirroring
	// TestE2E_DegradedHealsOnGoldenKeyframe's unrecognized-series-residual
	// construction, just via a second tenant's encoder instead of a
	// hand-built frame.
	contaminated := constValues(names, 10.0)
	for k := range contaminated {
		contaminated[k] += 80.0
	}

	// Window 0: both encoders warm up at baseline, so window 1's reading is
	// a real residual for encB (not a birth, which contributes zero to the
	// sketch per ADR-008 -- a fresh encoder's very first observation of
	// every series would otherwise silently carry no signal at all).
	eng.HandleFrame(ctx, encA.flush(clock.Now(), 0, baseline, false))
	eng.HandleFrame(ctx, encB.flush(clock.Now(), 0, baseline, false))
	clock.Advance(10 * time.Second)

	gatedBefore := met.GatedFrames()

	// Window 1: tenant A's own (quiet) frame, plus tenant B's misrouted
	// frame for the SAME (shard, epoch, view, seq) coordinate.
	eng.HandleFrame(ctx, encA.flush(clock.Now(), 1, baseline, false))
	eng.HandleFrame(ctx, encB.flush(clock.Now(), 1, contaminated, false))
	clock.Advance(10 * time.Second)

	for s := uint32(2); s <= uint32(1+cfg.AllowedLateness); s++ {
		eng.HandleFrame(ctx, encA.flush(clock.Now(), s, baseline, false))
		clock.Advance(10 * time.Second)
	}

	if got := met.GatedFrames(); got <= gatedBefore {
		t.Fatalf("gated_frames did not increment for the cross-tenant-contaminated window (before=%d after=%d)", gatedBefore, got)
	}
	if !eng.ShardDegraded(shardID) {
		t.Fatalf("expected shard %d to be DEGRADED after cross-tenant contamination, not silently trusted", shardID)
	}
	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" && ev.WindowID == 1 {
			t.Fatalf("cross-tenant-contaminated window produced a trusted deviation event: %+v (ADR-012 requires zero trusted summation across tenants)", ev)
		}
	}
	t.Logf("cross-tenant regression: gated_frames before=%d after=%d shard_degraded=%v", gatedBefore, met.GatedFrames(), eng.ShardDegraded(shardID))
}
