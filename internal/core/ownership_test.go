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

	"github.com/passiveintent/Palimpsest/pkg/sketch"
)

// sharedDictHasID reports whether id is currently active in shard shardID's
// (epoch=0, view=viewID, keyVersion=0) shared union dictionary.
func sharedDictHasID(t *testing.T, eng *Engine, shardID uint64, viewID uint16, id uint64) bool {
	t.Helper()
	eng.mu.Lock()
	shard, ok := eng.shards[shardID]
	eng.mu.Unlock()
	if !ok {
		return false
	}
	shard.mu.Lock()
	defer shard.mu.Unlock()
	view, ok := shard.Views[viewKey{epoch: 0, view: viewID, keyVersion: 0}]
	if !ok {
		return false
	}
	for _, active := range view.Dict.ActiveIDs() {
		if active == id {
			return true
		}
	}
	return false
}

// TestE2E_OwnershipPreventsWrongfulEviction covers the ADR-008 amendment's
// ("Ownership & Eviction") core acceptance scenario: two emitters share a
// logical series — the routine-pod-migration case, where an old emitter
// replica and a new one both report the same shard/series name for a
// window. One emitter's tombstone must not evict the series from the
// shared dictionary while the other still owns it; only once BOTH release
// it does it actually leave, and only then does a substrate (d) absence
// event fire (and tombstones_applied count it).
func TestE2E_OwnershipPreventsWrongfulEviction(t *testing.T) {
	const (
		shardID    = uint64(8)
		emitterA   = uint64(801)
		emitterB   = uint64(802)
		viewID     = uint16(0)
		m, d, bits = 100, 6, 8
	)
	seriesTTL := 30 * time.Second

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      80.0,
		AllowedLateness:  2,
		RepairHorizon:    time.Minute,
		EmittersExpected: 2,
		FISTAIters:       100,
		FISTALambda:      0.05,
		FISTAPowerIters:  20,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, sink, _, met, clock := newTestEngine(cfg)
	// keyframeEvery huge: only window 0 (0 % anything == 0) is ever a
	// keyframe; every subsequent window is a plain RESIDUAL, so this test
	// exercises ownership via ordinary tombstone dict_deltas, independent
	// of the separate golden-keyframe completeness fix (see
	// TestE2E_GoldenKeyframeCarriesSameWindowTombstones for that).
	encA := newTestEncoder(testTenantKey, shardID, emitterA, viewID, 0, m, d, bits, 1_000_000, 1, seriesTTL)
	encB := newTestEncoder(testTenantKey, shardID, emitterB, viewID, 0, m, d, bits, 1_000_000, 1, seriesTTL)

	sharedName := "shared_metric|shard=8|agg=sum"
	sharedID := sketch.SeriesID([]byte(sharedName))
	aOnlyNames := []string{"a_only_0|shard=8|agg=sum", "a_only_1|shard=8|agg=sum"}
	bOnlyNames := []string{"b_only_0|shard=8|agg=sum", "b_only_1|shard=8|agg=sum"}

	seqA, seqB := uint32(0), uint32(0)
	stepA := func(includeShared bool) {
		v := constValues(aOnlyNames, 10.0)
		if includeShared {
			v[sharedName] = 10.0
		}
		eng.HandleFrame(ctx, encA.flush(clock.Now(), seqA, v, false))
		seqA++
	}
	stepB := func(includeShared bool) {
		v := constValues(bOnlyNames, 10.0)
		if includeShared {
			v[sharedName] = 10.0
		}
		eng.HandleFrame(ctx, encB.flush(clock.Now(), seqB, v, false))
		seqB++
	}

	// Window 0: both A and B birth the shared series.
	stepA(true)
	stepB(true)
	clock.Advance(10 * time.Second)

	if !sharedDictHasID(t, eng, shardID, viewID, sharedID) {
		t.Fatalf("shared series not active after both emitters birthed it")
	}

	// A stops observing the shared series; once its own seriesTTL elapses,
	// A's Expire() tombstones it -- but B still owns it, so it must survive
	// in the shared dictionary.
	for i := 0; i < 4; i++ {
		stepA(false)
		clock.Advance(10 * time.Second)
	}
	if !sharedDictHasID(t, eng, shardID, viewID, sharedID) {
		t.Fatalf("shared series wrongly evicted after only emitter A released it (emitter B still owns it) -- this is the eviction bug the ADR-008 amendment closes")
	}
	for _, ev := range sink.Events() {
		if ev.Substrate == "absence" && ev.SeriesID == sharedID {
			t.Fatalf("spurious absence event fired while emitter B still owns the series: %+v", ev)
		}
	}
	if got := met.TombstonesApplied(); got != 0 {
		t.Fatalf("tombstones_applied = %d, want 0 (A's tombstone released its own claim but must not have evicted anything yet)", got)
	}

	// B now also stops observing it; once ITS seriesTTL elapses, the last
	// remaining owner releases -- the series must now actually leave the
	// shared dictionary, and an absence event must fire.
	for i := 0; i < 4; i++ {
		stepB(false)
		clock.Advance(10 * time.Second)
	}
	if sharedDictHasID(t, eng, shardID, viewID, sharedID) {
		t.Fatalf("shared series still active after BOTH emitters released it")
	}
	var sawAbsence bool
	for _, ev := range sink.Events() {
		if ev.Substrate == "absence" && ev.SeriesID == sharedID {
			sawAbsence = true
		}
	}
	if !sawAbsence {
		t.Fatalf("expected an absence event once the last owner released the shared series")
	}
	if got := met.TombstonesApplied(); got != 1 {
		t.Fatalf("tombstones_applied = %d, want 1 (exactly one real eviction, once B's release emptied the owner set)", got)
	}
}

// TestE2E_GoldenKeyframeCarriesSameWindowTombstones covers the companion
// encode-side fix: a series that expires (Tracker.Expire) in the SAME
// window a golden (Full-dict) keyframe is built must still appear in that
// frame's DictDeltas as an explicit tombstone. Tracker.FullDict() alone
// only enumerates currently-active series, so without appending Expire()'s
// output, a decoder would never learn that series is gone -- FullDict
// simply stops mentioning it, forever, rather than tombstoning it.
func TestE2E_GoldenKeyframeCarriesSameWindowTombstones(t *testing.T) {
	const (
		shardID    = uint64(9)
		emitterID  = uint64(901)
		viewID     = uint16(0)
		m, d, bits = 100, 6, 8
	)
	seriesTTL := 15 * time.Second

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      80.0,
		AllowedLateness:  2,
		RepairHorizon:    time.Minute,
		EmittersExpected: 1,
		FISTAIters:       100,
		FISTALambda:      0.05,
		FISTAPowerIters:  20,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, _, _, met, clock := newTestEngine(cfg)
	// keyframeEvery=1 forces every window to be a keyframe, so the test can
	// freely force a specific window golden without disturbing tombstone
	// timing; goldenEvery huge keeps window 0 non-forced-golden-by-cadence
	// (it's still golden, being the first ever keyframe) so window 1 below
	// is golden only because forceGolden explicitly says so.
	enc := newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, m, d, bits, 1, 1_000_000, seriesTTL)

	fadingName := "fading_metric|shard=9|agg=sum"
	fadingID := sketch.SeriesID([]byte(fadingName))
	keepName := "keep_metric|shard=9|agg=sum"

	// Window 0 (golden: the first keyframe always is): births both series.
	eng.HandleFrame(ctx, enc.flush(clock.Now(), 0, map[string]float64{fadingName: 5, keepName: 5}, false))
	clock.Advance(seriesTTL + time.Second) // elapse past seriesTTL without re-observing fadingName

	// Window 1: forced golden, and fadingName is no longer observed --
	// Expire() fires inside this very flush() call, for the same window
	// buildKeyframe assembles the golden frame for.
	golden1 := enc.flush(clock.Now(), 1, map[string]float64{keepName: 5}, true)

	var sawTombstone bool
	for _, dd := range golden1.DictDeltas {
		if dd.ID == fadingID && dd.IsTombstone() {
			sawTombstone = true
		}
	}
	if !sawTombstone {
		t.Fatalf("golden keyframe's DictDeltas did not carry the same-window Expire() tombstone for %q -- regression: FullDict() alone silently drops an ID that expired this same window instead of explicitly tombstoning it", fadingName)
	}

	eng.HandleFrame(ctx, golden1)

	if sharedDictHasID(t, eng, shardID, viewID, fadingID) {
		t.Fatalf("decoder's shared dictionary still has %q active after a golden keyframe that should have tombstoned it", fadingName)
	}
	keepID := sketch.SeriesID([]byte(keepName))
	if !sharedDictHasID(t, eng, shardID, viewID, keepID) {
		t.Fatalf("decoder's shared dictionary lost %q, which was never tombstoned", keepName)
	}
	// Item 5(a) acceptance: a same-window tombstone+golden collision must
	// produce zero dict_root mismatches and never DEGRADE the view -- the
	// whole point of carrying the tombstone alongside FullDict's adds
	// (rather than the pre-fix behavior of silently dropping it) is that
	// the decoder's dictionary never diverges from the encoder's in the
	// first place.
	if got := met.DictRootMismatches(shardID); got != 0 {
		t.Fatalf("dict_root_mismatches = %d, want 0 (the golden keyframe carried the tombstone, so the decoder's dictionary never diverged)", got)
	}
	if eng.ShardDegraded(shardID) {
		t.Fatalf("view is DEGRADED after a golden keyframe that correctly carried its own window's tombstone")
	}
}

// TestE2E_RosterLivenessReapsDeadEmitter covers the ADR-008 amendment's
// roster-liveness path: an emitter that goes silent forever (crash,
// permanent partition) without ever sending an explicit tombstone must
// still eventually release its series-ownership claims, once
// Config.EmitterRosterTTL elapses since its last arrival -- driven here by
// a second, still-live emitter's ordinary frames for the same shard.
func TestE2E_RosterLivenessReapsDeadEmitter(t *testing.T) {
	const (
		shardID    = uint64(10)
		emitterA   = uint64(1001) // goes silent after its first frame
		emitterB   = uint64(1002) // stays alive, drives the roster sweep
		viewID     = uint16(0)
		m, d, bits = 100, 6, 8
	)
	rosterTTL := 60 * time.Second

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      80.0,
		AllowedLateness:  2,
		RepairHorizon:    time.Minute,
		EmittersExpected: 2,
		FISTAIters:       100,
		FISTALambda:      0.05,
		FISTAPowerIters:  20,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
		EmitterRosterTTL: rosterTTL,
	}
	eng, sink, _, met, clock := newTestEngine(cfg)
	// seriesTTL is huge on A specifically so Expire() never fires
	// encode-side -- A's silence must be caught by roster liveness alone,
	// not by it eventually tombstoning itself.
	encA := newTestEncoder(testTenantKey, shardID, emitterA, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)
	encB := newTestEncoder(testTenantKey, shardID, emitterB, viewID, 0, m, d, bits, 1_000_000, 1, time.Hour)

	aOnlyName := "a_only_metric|shard=10|agg=sum"
	aOnlyID := sketch.SeriesID([]byte(aOnlyName))
	bOnlyNames := []string{"b_metric|shard=10|agg=sum"}

	eng.HandleFrame(ctx, encA.flush(clock.Now(), 0, map[string]float64{aOnlyName: 5}, false))

	if !sharedDictHasID(t, eng, shardID, viewID, aOnlyID) {
		t.Fatalf("a_only series not active right after birth")
	}
	if got := met.EmittersReaped(); got != 0 {
		t.Fatalf("emitters_reaped = %d, want 0 before any silence has elapsed", got)
	}

	// B keeps reporting normally; wall-clock time passes well beyond
	// rosterTTL (8 * 10s = 80s > 60s) without A ever sending another frame.
	seqB := uint32(0)
	for i := 0; i < 8; i++ {
		clock.Advance(10 * time.Second)
		eng.HandleFrame(ctx, encB.flush(clock.Now(), seqB, constValues(bOnlyNames, 5), false))
		seqB++
	}

	if sharedDictHasID(t, eng, shardID, viewID, aOnlyID) {
		t.Fatalf("a_only series still active after emitter A's roster TTL expired without it ever tombstoning -- roster liveness should have released it")
	}
	if got := met.EmittersReaped(); got != 1 {
		t.Fatalf("emitters_reaped = %d, want 1", got)
	}
	var sawAbsence bool
	for _, ev := range sink.Events() {
		if ev.Substrate == "absence" && ev.SeriesID == aOnlyID {
			sawAbsence = true
		}
	}
	if !sawAbsence {
		t.Fatalf("expected an absence event for the roster-reaped series")
	}

	// A late frame from B for the SAME already-reaped series name would be
	// a fresh birth (new owner claim), not resurrection of stale state --
	// sanity-check reaping didn't wedge anything.
	eng.HandleFrame(ctx, encB.flush(clock.Now(), seqB, map[string]float64{aOnlyName: 5}, false))
	if !sharedDictHasID(t, eng, shardID, viewID, aOnlyID) {
		t.Fatalf("a fresh birth for the same series name from a different emitter after reaping should re-activate it")
	}
}

// TestE2E_DroppedTombstoneHealsAtNextGolden covers the ADR-008 amendment's
// item 3 (decoder golden reconciliation): a tombstone dict_delta lost in
// transit -- not the encoder's fault, a delivery failure -- leaves the
// decoder's per-emitter dictionary mirror (evs.dict) holding a stale ID the
// encoder itself has already forgotten and, per the amendment's non-goals,
// will never re-tombstone (Tracker only ever tombstones an ID once, when it
// expires). Incremental dict_delta application alone can never heal this:
// there is no future explicit tombstone coming. The next golden keyframe's
// authoritative-replace reconciliation must heal it instead, by noticing
// evs.dict holds an ID the golden's announced (FullDict) set doesn't
// mention.
func TestE2E_DroppedTombstoneHealsAtNextGolden(t *testing.T) {
	const (
		shardID       = uint64(11)
		emitterID     = uint64(1101)
		viewID        = uint16(0)
		m, d, bits    = 100, 6, 8
		keyframeEvery = 50 // windows 0 and 50 are keyframes; the rest RESIDUAL
	)
	seriesTTL := 15 * time.Second
	interval := 10 * time.Second

	cfg := Config{
		TenantKey:        testTenantKey,
		MaxResidual:      80.0,
		AllowedLateness:  2,
		RepairHorizon:    time.Minute,
		EmittersExpected: 1,
		FISTAIters:       100,
		FISTALambda:      0.05,
		FISTAPowerIters:  20,
		FISTAThreshold:   0.3,
		DriftThreshold:   1e9,
	}
	eng, sink, _, met, clock := newTestEngine(cfg)
	// goldenEvery huge: window 0 is still golden (the first keyframe always
	// is), but window 50 (the next natural keyframe) would otherwise be a
	// KDELTA -- forced golden explicitly below, so timing is controlled.
	enc := newTestEncoder(testTenantKey, shardID, emitterID, viewID, 0, m, d, bits, keyframeEvery, 1_000_000, seriesTTL)

	fadingName := "fading_metric|shard=11|agg=sum"
	fadingID := sketch.SeriesID([]byte(fadingName))
	keepName := "keep_metric|shard=11|agg=sum"
	keepID := sketch.SeriesID([]byte(keepName))

	// Window 0 (golden): births both series.
	eng.HandleFrame(ctx, enc.flush(clock.Now(), 0, map[string]float64{fadingName: 5, keepName: 5}, false))
	clock.Advance(seriesTTL + time.Second) // elapse past seriesTTL without re-observing fadingName

	// Window 1 (RESIDUAL): Expire() fires here, tombstoning fadingName --
	// but this frame is never delivered, simulating a dropped frame in
	// transit. The tombstone it carried is now gone forever from the
	// encoder's perspective: Tracker never re-tombstones an ID it already
	// expired once.
	dropped := enc.flush(clock.Now(), 1, map[string]float64{keepName: 5}, false)
	var droppedTombstone bool
	for _, dd := range dropped.DictDeltas {
		if dd.ID == fadingID && dd.IsTombstone() {
			droppedTombstone = true
		}
	}
	if !droppedTombstone {
		t.Fatalf("setup error: window 1's frame did not carry fadingName's Expire() tombstone")
	}
	clock.Advance(interval)

	// Windows 2..49 (RESIDUAL): ordinary traffic continues, delivered
	// normally. fadingName is never observed again and the encoder never
	// re-tombstones it (it already left the tracker's active set at window
	// 1) -- there is no future explicit tombstone for the decoder to apply.
	for seq := uint32(2); seq < keyframeEvery; seq++ {
		eng.HandleFrame(ctx, enc.flush(clock.Now(), seq, map[string]float64{keepName: 5}, false))
		clock.Advance(interval)
	}

	// Sanity check: without the dropped-tombstone-heals-via-reconcile fix,
	// the decoder's shared dictionary is still stuck holding the stale
	// series the encoder has already forgotten.
	if !sharedDictHasID(t, eng, shardID, viewID, fadingID) {
		t.Fatalf("setup error: fadingName evicted before the golden reconciliation ran -- test no longer isolates what it means to")
	}
	if got := met.TombstonesApplied(); got != 0 {
		t.Fatalf("tombstones_applied = %d, want 0 before the golden keyframe heals the dropped tombstone", got)
	}

	// Window 50: forced golden. Its DictDeltas (FullDict() ++ this window's
	// own tombstones) do NOT mention fadingID at all -- it isn't currently
	// active (nothing to add) and it isn't tombstoned THIS window (it
	// already expired at window 1). Only reconciliation against the
	// announced set heals it now.
	golden := enc.flush(clock.Now(), keyframeEvery, map[string]float64{keepName: 5}, true)
	for _, dd := range golden.DictDeltas {
		if dd.ID == fadingID {
			t.Fatalf("setup error: golden keyframe unexpectedly mentions fadingID (%+v) -- test no longer isolates reconciliation from the explicit-tombstone path", dd)
		}
	}
	eng.HandleFrame(ctx, golden)

	if sharedDictHasID(t, eng, shardID, viewID, fadingID) {
		t.Fatalf("fadingName still active in the shared dictionary after the golden keyframe reconciled evs.dict against its announced set")
	}
	if !sharedDictHasID(t, eng, shardID, viewID, keepID) {
		t.Fatalf("keepName wrongly evicted by golden reconciliation")
	}
	if got := met.TombstonesApplied(); got != 1 {
		t.Fatalf("tombstones_applied = %d, want 1 (exactly one reconcile-driven eviction)", got)
	}
	var sawAbsence bool
	for _, ev := range sink.Events() {
		if ev.Substrate == "absence" && ev.SeriesID == fadingID {
			sawAbsence = true
		}
	}
	if !sawAbsence {
		t.Fatalf("expected an absence event for the reconcile-healed series")
	}
	// Reconciliation runs BEFORE the dict_root verification (it is that
	// check's precondition, per the ADR-008 amendment), so the golden
	// keyframe's own dict_root must verify cleanly against the now-healed
	// evs.dict -- no mismatch should ever be recorded, and the view must
	// not be left DEGRADED.
	if got := met.DictRootMismatches(shardID); got != 0 {
		t.Fatalf("dict_root_mismatches = %d, want 0 (reconciliation heals evs.dict before dict_root is checked)", got)
	}
	if eng.ShardDegraded(shardID) {
		t.Fatalf("view is DEGRADED after a golden keyframe that reconciliation should have healed cleanly")
	}
}
