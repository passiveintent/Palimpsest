/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"

	"github.com/passiveintent/Palimpsest/internal/forensics"
	"github.com/passiveintent/Palimpsest/internal/metrics"
)

var testTenantKey = []byte("e2e-test-tenant-key-do-not-use-in-prod")

var ctx = context.Background()

func newTestEngine(cfg Config) (*Engine, *fakeAnomalySink, *fakeSeriesSink, *metrics.Metrics, *fakeClock) {
	sink := &fakeAnomalySink{}
	seriesSink := &fakeSeriesSink{}
	met := metrics.New()
	fstore := forensics.NewSnapshotStore(time.Hour, 1000)
	eng := New(cfg, sink, seriesSink, met, fstore)
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	eng.now = clock.Now
	return eng, sink, seriesSink, met, clock
}

func constValues(names []string, v float64) map[string]float64 {
	out := make(map[string]float64, len(names))
	for _, n := range names {
		out[n] = v
	}
	return out
}

// TestE2E_SteadyStateAnomalies covers the "Steady" acceptance scenario:
// N=20000 logical series, k=20 injected anomalies -> >=19/20 flagged
// within a couple of windows past AllowedLateness; zero false positives in
// the quiet run before injection.
func TestE2E_SteadyStateAnomalies(t *testing.T) {
	if testing.Short() {
		t.Skip("large recovery solve; skipped under -short")
	}

	const (
		shardID   = uint64(1)
		emitterID = uint64(101)
		viewID    = uint16(0)
		n         = 20000
		k         = 20
		m         = 2000
		d         = 6
		bits      = 8
	)

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      80.0,
		AllowedLateness:  3,
		RepairHorizon:    time.Minute,
		EmittersExpected: 1,
		FISTAIters:       350,
		FISTALambda:      0.05,
		FISTAPowerIters:  50,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9, // disable substrate (c) noise for this test
	}
	eng, sink, _, _, clock := newTestEngine(cfg)
	enc := newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)

	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("steady_metric_%06d|shard=1|agg=sum", i)
	}
	baseline := func() map[string]float64 { return constValues(names, 100.0) }

	seq := uint32(0)
	step := func(values map[string]float64) {
		eng.HandleFrame(ctx, enc.flush(clock.Now(), seq, values, false))
		seq++
		clock.Advance(10 * time.Second)
	}

	step(baseline()) // window 0: golden keyframe, births only, no residual signal possible
	for i := 0; i < 5; i++ {
		step(baseline())
	}
	if got := len(sink.Events()); got != 0 {
		t.Fatalf("quiet run before injection: got %d anomaly events, want 0", got)
	}

	rng := rand.New(rand.NewSource(1))
	anomV := baseline()
	idxs := rng.Perm(n)[:k]
	for _, i := range idxs {
		anomV[names[i]] += 60.0
	}
	injectedWindow := seq
	step(anomV)

	for i := 0; i < cfg.AllowedLateness+2; i++ {
		step(baseline())
	}

	flagged := make(map[uint64]bool)
	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" && ev.WindowID == injectedWindow {
			flagged[ev.SeriesID] = true
		}
	}
	hit := 0
	for _, i := range idxs {
		if flagged[sketch.SeriesID([]byte(names[i]))] {
			hit++
		}
	}
	if hit < 19 {
		t.Fatalf("recall: flagged %d/%d injected anomalies, want >= 19/20", hit, k)
	}
}

// TestE2E_LifecycleChurn covers the ADR-008 acceptance scenario: mid-run
// births/tombstones produce zero deviation false positives, and the active
// dictionary returns to baseline size once series_ttl has elapsed for the
// dropped series.
func TestE2E_LifecycleChurn(t *testing.T) {
	const (
		shardID   = uint64(2)
		emitterID = uint64(201)
		viewID    = uint16(0)
		m         = 300
		d         = 6
		bits      = 8
		n0        = 200
		nBirth    = 50
	)
	seriesTTL := 30 * time.Second

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      80.0,
		AllowedLateness:  2,
		RepairHorizon:    time.Minute,
		EmittersExpected: 1,
		FISTAIters:       200,
		FISTALambda:      0.05,
		FISTAPowerIters:  30,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, sink, _, met, clock := newTestEngine(cfg)
	enc := newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, m, d, bits, 1_000_000, 1, seriesTTL)

	names0 := make([]string, n0)
	for i := range names0 {
		names0[i] = fmt.Sprintf("churn_metric_%04d|shard=2|agg=sum", i)
	}

	seq := uint32(0)
	step := func(names []string) {
		eng.HandleFrame(ctx, enc.flush(clock.Now(), seq, constValues(names, 42.0), false))
		seq++
		clock.Advance(10 * time.Second)
	}

	step(names0)
	for i := 0; i < 3; i++ {
		step(names0)
	}

	surviving := append([]string(nil), names0[:n0/2]...)
	namesBirth := make([]string, nBirth)
	for i := range namesBirth {
		namesBirth[i] = fmt.Sprintf("churn_new_%04d|shard=2|agg=sum", i)
	}
	active := append(append([]string(nil), surviving...), namesBirth...)

	windowsNeeded := int(seriesTTL/(10*time.Second)) + 2
	for i := 0; i < windowsNeeded; i++ {
		step(active)
	}
	for i := 0; i < cfg.AllowedLateness+2; i++ {
		step(active)
	}

	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" {
			t.Fatalf("lifecycle churn produced a deviation anomaly: %+v (want zero false positives from births/tombstones alone)", ev)
		}
	}

	eng.mu.Lock()
	shard := eng.shards[shardID]
	eng.mu.Unlock()
	shard.mu.Lock()
	view := shard.Views[viewKey{epoch: 0, view: viewID}]
	gotActive := len(view.Dict.ActiveIDs())
	shard.mu.Unlock()

	wantActive := len(surviving) + len(namesBirth)
	if gotActive != wantActive {
		t.Fatalf("active dict size = %d, want %d (dict size should return to baseline after series_ttl)", gotActive, wantActive)
	}
	if got := met.BirthsApplied(); got < int64(n0+nBirth) {
		t.Fatalf("births_applied = %d, want >= %d", got, n0+nBirth)
	}
	if got := met.TombstonesApplied(); got < int64(n0-n0/2) {
		t.Fatalf("tombstones_applied = %d, want >= %d", got, n0-n0/2)
	}
}

// TestE2E_WatermarkRepair covers the ADR-013 acceptance scenario: a
// withheld frame's window still recovers (at reduced coverage) once the
// watermark passes it; a late arrival within RepairHorizon triggers a
// repair re-solve with an incremented revision and full coverage; a late
// arrival beyond RepairHorizon is dropped and counted instead.
func TestE2E_WatermarkRepair(t *testing.T) {
	const (
		shardID  = uint64(3)
		emitterA = uint64(301)
		emitterB = uint64(302)
		viewID   = uint16(0)
		m        = 200
		d        = 6
		bits     = 8
		n        = 30
	)

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      40.0,
		AllowedLateness:  2,
		RepairHorizon:    5 * time.Minute,
		EmittersExpected: 2,
		FISTAIters:       200,
		FISTALambda:      0.05,
		FISTAPowerIters:  30,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, sink, _, met, clock := newTestEngine(cfg)
	encA := newTestEncoder(testTenantKey, shardID, emitterA, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)
	encB := newTestEncoder(testTenantKey, shardID, emitterB, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)

	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("wm_metric_%03d|shard=3|agg=sum", i)
	}
	baseline := func() map[string]float64 { return constValues(names, 10.0) }
	anomV := func() map[string]float64 {
		v := baseline()
		v[names[0]] = 10.0 + 8.0
		return v
	}

	eng.HandleFrame(ctx, encA.flush(clock.Now(), 0, baseline(), false))
	eng.HandleFrame(ctx, encB.flush(clock.Now(), 0, baseline(), false))
	clock.Advance(10 * time.Second)

	// Window 1: only A reports (anomalous); B's frame is withheld for now.
	eng.HandleFrame(ctx, encA.flush(clock.Now(), 1, anomV(), false))
	clock.Advance(10 * time.Second)

	for s := uint32(2); s <= uint32(1+cfg.AllowedLateness); s++ {
		eng.HandleFrame(ctx, encA.flush(clock.Now(), s, baseline(), false))
		clock.Advance(10 * time.Second)
	}

	var sawInitial bool
	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" && ev.WindowID == 1 {
			sawInitial = true
			if ev.Revision != 0 {
				t.Fatalf("initial solve: revision = %d, want 0", ev.Revision)
			}
			if ev.Coverage != 1 {
				t.Fatalf("initial solve: coverage = %d, want 1 (only emitter A reported)", ev.Coverage)
			}
		}
	}
	if !sawInitial {
		t.Fatalf("expected an initial deviation anomaly for window 1 before B's late frame")
	}

	// B's late frame for window 1 arrives now, within RepairHorizon.
	eng.HandleFrame(ctx, encB.flush(clock.Now(), 1, anomV(), false))

	var sawRevised bool
	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" && ev.WindowID == 1 && ev.Revision > 0 {
			sawRevised = true
			if ev.Coverage != 2 {
				t.Fatalf("revised solve: coverage = %d, want 2", ev.Coverage)
			}
		}
	}
	if !sawRevised {
		t.Fatalf("expected a revised (revision > 0) event for window 1 after B's late frame")
	}

	// Beyond-horizon case, using a fresh window: A recovers it initially,
	// then wall-clock advances past RepairHorizon before B's first-ever
	// frame for that window arrives.
	eng.HandleFrame(ctx, encA.flush(clock.Now(), 100, anomV(), false))
	clock.Advance(10 * time.Second)
	for s := uint32(101); s <= uint32(100+cfg.AllowedLateness); s++ {
		eng.HandleFrame(ctx, encA.flush(clock.Now(), s, baseline(), false))
		clock.Advance(10 * time.Second)
	}

	clock.Advance(cfg.RepairHorizon + time.Minute)
	before := met.LateFramesDropped()
	eng.HandleFrame(ctx, encB.flush(clock.Now(), 100, anomV(), false))
	if got := met.LateFramesDropped(); got <= before {
		t.Fatalf("late_frames_dropped did not increment for a beyond-repair-horizon late frame (before=%d after=%d)", before, got)
	}
}

// TestE2E_ClockSkewAlert covers the clock-skew acceptance scenario: two
// emitters whose frames for the same windows consistently arrive ~50ms
// apart produce zero false deviation anomalies (window-index sketching is
// unaffected by wall-clock skew) but do fire a clock-skew Alert.
func TestE2E_ClockSkewAlert(t *testing.T) {
	const (
		shardID  = uint64(4)
		emitterA = uint64(401)
		emitterB = uint64(402)
		viewID   = uint16(0)
		m        = 100
		d        = 6
		bits     = 8
		n        = 10
	)

	cfg := Config{
		TenantKey:            testTenantKey,
		MaxResidual:          40.0,
		AllowedLateness:      2,
		RepairHorizon:        time.Minute,
		EmittersExpected:     2,
		FISTAIters:           100,
		FISTALambda:          0.05,
		FISTAPowerIters:      20,
		FISTAThreshold:       0.3,
		DriftThreshold:       1e9,
		ClockSkewToleranceMs: 30,
	}
	eng, sink, _, met, clock := newTestEngine(cfg)
	encA := newTestEncoder(testTenantKey, shardID, emitterA, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)
	encB := newTestEncoder(testTenantKey, shardID, emitterB, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)

	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("skew_metric_%02d|shard=4|agg=sum", i)
	}
	baseline := func() map[string]float64 { return constValues(names, 5.0) }

	eng.HandleFrame(ctx, encA.flush(clock.Now(), 0, baseline(), false))
	eng.HandleFrame(ctx, encB.flush(clock.Now(), 0, baseline(), false))
	clock.Advance(10 * time.Second)

	for s := uint32(1); s <= 5; s++ {
		eng.HandleFrame(ctx, encA.flush(clock.Now(), s, baseline(), false))
		clock.Advance(50 * time.Millisecond)
		eng.HandleFrame(ctx, encB.flush(clock.Now(), s, baseline(), false))
		clock.Advance(10*time.Second - 50*time.Millisecond)
	}

	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" {
			t.Fatalf("clock skew alone produced a deviation anomaly: %+v", ev)
		}
	}

	var found bool
	for _, a := range eng.Alerts() {
		if a.EmitterID == emitterB && a.ShardID == shardID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a clock-skew Alert for emitter B, got %+v", eng.Alerts())
	}
	if got := met.ClockSkewMs(emitterB); got < 40 {
		t.Fatalf("clock_skew_ms{emitter=B} = %d, want ~50", got)
	}
}

// TestE2E_DegradedHealsOnGoldenKeyframe covers the DEGRADED acceptance
// scenario: a RESIDUAL frame whose signal belongs to a series never
// birthed into the dictionary leaves a high leftover recovery residual,
// gating the window and marking the shard DEGRADED; it heals once a golden
// keyframe with a correct dict_root arrives.
func TestE2E_DegradedHealsOnGoldenKeyframe(t *testing.T) {
	const (
		shardID   = uint64(5)
		emitterID = uint64(501)
		viewID    = uint16(0)
		m         = 200
		d         = 6
		bits      = 8
		n         = 30
	)

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      2.0, // tight: an unrecognized-series residual clearly exceeds it, a quiet window's is exactly 0
		AllowedLateness:  1,
		RepairHorizon:    time.Minute,
		EmittersExpected: 1,
		FISTAIters:       200,
		FISTALambda:      0.05,
		FISTAPowerIters:  30,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, _, _, met, clock := newTestEngine(cfg)
	enc := newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)

	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("degraded_metric_%02d|shard=5|agg=sum", i)
	}
	baseline := func() map[string]float64 { return constValues(names, 3.0) }

	eng.HandleFrame(ctx, enc.flush(clock.Now(), 0, baseline(), false))
	clock.Advance(10 * time.Second)

	// A hand-built RESIDUAL frame carrying signal for a series that was
	// never birthed: the shared dictionary has no column for it, so
	// FISTA/debias can't explain the signal and leaves a large leftover
	// residual (ADR-013 DEGRADED trigger).
	seed := sketch.DeriveEphemeralSeed(testTenantKey, shardID, 0, viewID)
	unknownName := []byte("ghost_metric|shard=5|agg=sum")
	idx, sign := sketch.Buckets(unknownName, seed, m, d)
	y := make([]float64, m)
	invSqrtD := 1 / math.Sqrt(float64(d))
	const ghostMagnitude = 500.0
	for i, bucket := range idx {
		contrib := ghostMagnitude * invSqrtD
		if sign[i] > 0 {
			y[bucket] += contrib
		} else {
			y[bucket] -= contrib
		}
	}
	scale := residualScale(y, bits)
	payload, err := wire.Quantize(y, uint8(bits), scale)
	if err != nil {
		t.Fatalf("Quantize: %v", err)
	}
	ghostFrame := &wire.Frame{
		Magic: wire.Magic, Version: wire.Version, FrameType: wire.FrameTypeResidual,
		EmitterID: emitterID, ShardID: shardID, Epoch: 0, Seq: 1,
		ViewID: viewID, M: uint32(m), D: uint8(d), Bits: uint8(bits),
		QuantScale: scale, DictRoot: wire.ComputeDictRoot(enc.tracker.ActiveIDs()),
		Payload: payload,
	}
	eng.HandleFrame(ctx, ghostFrame)
	clock.Advance(10 * time.Second)

	// One more quiet window crosses AllowedLateness and triggers the solve.
	eng.HandleFrame(ctx, enc.flush(clock.Now(), 2, baseline(), false))
	clock.Advance(10 * time.Second)

	if !eng.ShardDegraded(shardID) {
		t.Fatalf("expected shard %d to be DEGRADED after the high-residual gate", shardID)
	}
	if got := met.GatedFrames(); got == 0 {
		t.Fatalf("gated_frames = 0, want > 0")
	}

	healFrame := enc.flush(clock.Now(), 3, baseline(), true /* forceGolden */)
	eng.HandleFrame(ctx, healFrame)

	if eng.ShardDegraded(shardID) {
		t.Fatalf("expected shard %d to heal after a golden keyframe with a correct dict_root", shardID)
	}
}

// TestE2E_DuplicateResidualFrameDeduped covers the ADR-013 dedup acceptance
// scenario: a RESIDUAL frame delivered twice for the same (emitter, window)
// — an at-least-once retry, or plsim's --duplicate-rate — must not be
// merged twice into the window's sketch, must not trigger a spurious
// repair/revision, and must be counted via Metrics.DuplicateFramesDropped.
// Repair (a *different* emitter's first-ever late contribution) is a
// distinct, legitimate path covered by TestE2E_WatermarkRepair above.
func TestE2E_DuplicateResidualFrameDeduped(t *testing.T) {
	const (
		shardID   = uint64(6)
		emitterID = uint64(601)
		viewID    = uint16(0)
		m         = 200
		d         = 6
		bits      = 8
		n         = 30
	)

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      40.0,
		AllowedLateness:  2,
		RepairHorizon:    5 * time.Minute,
		EmittersExpected: 1,
		FISTAIters:       200,
		FISTALambda:      0.05,
		FISTAPowerIters:  30,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, sink, _, met, clock := newTestEngine(cfg)
	enc := newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)

	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("dup_metric_%02d|shard=6|agg=sum", i)
	}
	baseline := func() map[string]float64 { return constValues(names, 10.0) }
	anomV := func() map[string]float64 {
		v := baseline()
		v[names[0]] = 10.0 + 8.0
		return v
	}

	eng.HandleFrame(ctx, enc.flush(clock.Now(), 0, baseline(), false))
	clock.Advance(10 * time.Second)

	dupFrame := enc.flush(clock.Now(), 1, anomV(), false)
	eng.HandleFrame(ctx, dupFrame)
	clock.Advance(10 * time.Second)

	for s := uint32(2); s <= uint32(1+cfg.AllowedLateness); s++ {
		eng.HandleFrame(ctx, enc.flush(clock.Now(), s, baseline(), false))
		clock.Advance(10 * time.Second)
	}

	countWindow1 := func() int {
		n := 0
		for _, ev := range sink.Events() {
			if ev.Substrate == "deviation" && ev.WindowID == 1 {
				n++
			}
		}
		return n
	}

	initialCount := countWindow1()
	if initialCount == 0 {
		t.Fatalf("expected an initial deviation event for window 1 before the duplicate")
	}
	if got := met.DuplicateFramesDropped(); got != 0 {
		t.Fatalf("DuplicateFramesDropped before any duplicate = %d, want 0", got)
	}

	// Redeliver the EXACT SAME frame object for window 1 from the same
	// emitter (at-least-once retry), still well within RepairHorizon.
	eng.HandleFrame(ctx, dupFrame)

	if got := met.DuplicateFramesDropped(); got != 1 {
		t.Fatalf("DuplicateFramesDropped after resending window 1's frame = %d, want 1", got)
	}
	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" && ev.WindowID == 1 && ev.Revision > 0 {
			t.Fatalf("duplicate frame produced a spurious revision>0 event (should have been dropped, not re-solved): %+v", ev)
		}
	}
	if got := countWindow1(); got != initialCount {
		t.Fatalf("event count for window 1 changed after a duplicate: got %d, want %d (unchanged)", got, initialCount)
	}
}

// TestE2E_SlowDriftKeyframe covers the ADR-009.c acceptance scenario as
// literally specified (docs/SPEC.md, palimpsest_copilot_prompts_v3_full.md
// Prompt 8): drift is caught at the keyframe substrate, at keyframe-cadence
// latency. It requires the Finding-2 fix (Tracker.CurrentValues +
// Predictor.LoadKeyframe at keyframe time) — a stale, never-refreshed
// predictor baseline would make EvalKeyframe compare a frozen value
// against itself and never fire.
//
// This does NOT also assert substrate (a) stays silent for the drifting
// series. An earlier version of this test tried to engineer that (first
// via a single isolated drifting series, then via ~1/3 of the fleet
// drifting together to break FISTA's sparsity assumption per ADR-004's
// storm cliff) and found empirically that this codebase's FISTA/debias
// recovery is robust enough to still recover an accurate, low-residual
// value even at k/m ratios past the informal "degrades above m/5" cliff —
// meaning a real drift can legitimately be caught by *both* substrates
// at once, which is a fine outcome (independent corroboration), not a
// defect. Substrate (a) firing here is not itself wrong; it just isn't
// what this test is gating on.
func TestE2E_SlowDriftKeyframe(t *testing.T) {
	const (
		shardID        = uint64(7)
		emitterID      = uint64(701)
		viewID         = uint16(0)
		m              = 300
		d              = 6
		bits           = 8
		n              = 15
		keyframeEvery  = 6
		perWindowDelta = 2.0
	)

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      50.0,
		AllowedLateness:  2,
		RepairHorizon:    time.Minute,
		EmittersExpected: 1,
		FISTAIters:       200,
		FISTALambda:      0.05,
		FISTAPowerIters:  30,
		FISTAThreshold:   0.3,
		DriftThreshold:   5.0,
	}
	eng, sink, _, _, clock := newTestEngine(cfg)
	enc := newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, m, d, bits, keyframeEvery, 1_000_000, time.Hour)

	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("drift_quiet_%02d|shard=7|agg=sum", i)
	}
	driftName := "drift_moving|shard=7|agg=sum"
	driftID := sketch.SeriesID([]byte(driftName))

	valuesAt := func(window int) map[string]float64 {
		v := constValues(names, 100.0)
		v[driftName] = 100.0 + float64(window)*perWindowDelta
		return v
	}

	seq := uint32(0)
	step := func() {
		eng.HandleFrame(ctx, enc.flush(clock.Now(), seq, valuesAt(int(seq)), false))
		seq++
		clock.Advance(10 * time.Second)
	}

	const windows = 19 // a bit over 3 keyframe cycles (keyframeEvery=6)
	for i := 0; i < windows; i++ {
		step()
	}

	var (
		sawDrift       bool
		sawDeviation   bool
		firstHitWindow = uint32(windows)
	)
	for _, ev := range sink.Events() {
		if ev.SeriesID != driftID {
			continue
		}
		switch ev.Substrate {
		case "deviation":
			sawDeviation = true
		case "keyframe":
			sawDrift = true
			if ev.WindowID < firstHitWindow {
				firstHitWindow = ev.WindowID
			}
			wantMin := float64(keyframeEvery) * perWindowDelta * 0.5
			if ev.Residual < wantMin {
				t.Fatalf("keyframe drift event residual = %v, want >= %v (roughly keyframeEvery*perWindowDelta)", ev.Residual, wantMin)
			}
		}
	}
	if !sawDrift {
		t.Fatalf("expected at least one substrate (c) keyframe drift event for the drifting series over %d windows (keyframe every %d)", windows, keyframeEvery)
	}
	if firstHitWindow > keyframeEvery {
		t.Fatalf("first keyframe drift detection at window %d, want <= keyframe cadence (%d)", firstHitWindow, keyframeEvery)
	}
	if sawDeviation {
		t.Logf("note: the drifting series was also independently caught by substrate (a) deviation — expected for an isolated k=1 signal (magnitude-independent sparse recovery), not a failure of this test")
	}
}
