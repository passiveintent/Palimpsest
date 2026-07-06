/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package core implements palimpsestd's decode-path state machine: per-shard
// dictionary/watermark bookkeeping, sparse recovery dispatch, and the
// ADR-009 five-substrate anomaly matcher.
//
// See ADR-007 (hexagonal service), ADR-008 (ephemeral identity), ADR-009
// (alerting), ADR-010 (dimensional slicing), ADR-011 (keyframe economics),
// ADR-012 (security/tenancy), and ADR-013 (temporal alignment).
package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/recover"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"

	"github.com/passiveintent/Palimpsest/internal/forensics"
	"github.com/passiveintent/Palimpsest/internal/metrics"
	"github.com/passiveintent/Palimpsest/internal/ports"
)

// Config configures an Engine.
type Config struct {
	// TenantKey is the ADR-012 HKDF input keying material: it must match
	// whatever key the encoder(s) reporting into this daemon use, or
	// derived seeds (and therefore Phi) will silently disagree.
	TenantKey []byte

	// KeyRing is the versioned tenant-key set for ADR-012 key rotation
	// (§Addendum). When non-nil, seed derivation uses the key whose version
	// matches a frame's KeyVersion field. A nil KeyRing falls back to
	// TenantKey (single-key mode). An unknown KeyVersion gates the frame
	// and increments the keyring_miss metric; it is never silently dropped.
	KeyRing sketch.KeyRing

	// MaxResidual gates substrate (a): a recover.Result whose Residual
	// exceeds this is untrustworthy and is gated (no deviation events
	// emitted; the owning view is marked DEGRADED) rather than surfaced.
	MaxResidual float64

	// AllowedLateness (windows) and RepairHorizon (wall-clock) configure
	// the ADR-013 watermark engine (internal/core/watermark.go).
	AllowedLateness int
	RepairHorizon   time.Duration

	// EmittersExpected annotates Result.CoverageTotal; it does not gate
	// recovery (a window is solved as soon as it's watermark-ready
	// regardless of how many of the expected emitters have contributed).
	EmittersExpected int

	FISTAIters      int
	FISTALambda     float64
	FISTAPowerIters int
	FISTAThreshold  float64

	// EscalateThreshold, when > 0, enables the ADR-014 Group-OMP escalation
	// path on every Recover call this Engine makes (recommended production
	// value: 0.3). 0 disables escalation entirely — the standard
	// Recover() trigger contract this Config exposes; merged-tier views
	// (ADR-015) rely on this same contract rather than a separate
	// escalation default.
	EscalateThreshold float64

	// Grouper, if non-nil, overrides the default label-hierarchy grouper
	// used during escalation (recover.DefaultGrouper()). nil is the normal
	// case.
	Grouper recover.Grouper

	// DriftThreshold configures the Matcher's substrate (c).
	DriftThreshold float64

	// ClockSkewToleranceMs is the per-emitter arrival-time spread (relative
	// to the first emitter to report a given window) above which a clock
	// skew Alert fires. <= 0 disables skew alerting.
	ClockSkewToleranceMs int64

	// MaxBirthsPerViewPerMin, if > 0, runs a decode-side churn breaker per
	// (shard, epoch, view): a defense-in-depth check independent of
	// whatever any single encoder's own breaker decided, since multiple
	// under-limit emitters can still sum to an over-limit shard-wide birth
	// rate. 0 disables it.
	MaxBirthsPerViewPerMin int

	// MergedViews declares, by ViewID, which views are merged tier
	// (ADR-015) and their trust-guardrail policy. A ViewID absent from
	// this map is not merged tier: its Results always carry
	// recover.MergedTrustNA and the guardrail is never evaluated for it.
	MergedViews map[uint16]MergedPolicy
}

// recoverOptions builds the recover.Options this Config implies for one
// solve at the given revision.
func (c Config) recoverOptions(revision int) recover.Options {
	return recover.Options{
		Iters:             c.FISTAIters,
		Lambda:            c.FISTALambda,
		PowerIters:        c.FISTAPowerIters,
		Threshold:         c.FISTAThreshold,
		Revision:          revision,
		EscalateThreshold: c.EscalateThreshold,
		Grouper:           c.Grouper,
	}
}

// Alert is an operator-facing event (clock skew, decode-side churn
// breaker) that doesn't fit ports.AnomalyEvent's per-series shape.
type Alert struct {
	Time      time.Time
	ShardID   uint64
	EmitterID uint64
	Reason    string
}

const maxAlertsRetained = 256

// Engine is palimpsestd's decode-path core: it consumes wire.Frames (from
// any ports.FrameSource), reconstructs per-shard state, runs recovery, and
// routes anomalies/series to the configured ports.AnomalySink/SeriesSink.
//
// Engine is safe for concurrent use; HandleFrame may be called from
// multiple goroutines (e.g. multiple FrameSources) simultaneously — shard
// state is serialized per-shard, so unrelated shards process concurrently.
type Engine struct {
	cfg         Config
	anomalySink ports.AnomalySink
	seriesSink  ports.SeriesSink
	metrics     *metrics.Metrics
	forensics   *forensics.SnapshotStore
	matcher     *Matcher

	now func() time.Time

	mu     sync.Mutex
	shards map[uint64]*ShardState
	alerts []Alert
}

// New returns an Engine. seriesSink and forensics may be nil (Layer 1
// rehydration / dashcam snapshots become no-ops).
func New(cfg Config, anomalySink ports.AnomalySink, seriesSink ports.SeriesSink, m *metrics.Metrics, fs *forensics.SnapshotStore) *Engine {
	return &Engine{
		cfg:         cfg,
		anomalySink: anomalySink,
		seriesSink:  seriesSink,
		metrics:     m,
		forensics:   fs,
		matcher:     &Matcher{DriftThreshold: cfg.DriftThreshold},
		now:         time.Now,
		shards:      make(map[uint64]*ShardState),
	}
}

// viewKey identifies one (epoch, view, key-version) recovery coordinate
// system within a shard. Frames with different key_version values for the
// same (epoch, view) derive different seeds and therefore map to separate
// ViewStates; this ensures additivity is never violated across key versions
// (ADR-012 §Addendum).
type viewKey struct {
	epoch      uint64
	view       uint16
	keyVersion uint8
}

// ShardState is one shard's complete decode-path state: every emitter
// reporting into it, and the shared per-(epoch, view) recovery state their
// contributions are merged into (ADR-013: sketch additivity lets multiple
// emitters' frames for the same shard/epoch/view/window sum correctly).
type ShardState struct {
	ShardID uint64

	Emitters map[uint64]*EmitterState
	Views    map[viewKey]*ViewState

	// SeriesNames is a best-effort id->name map (from DictDelta births),
	// shard-wide, used to label AnomalyEvents/Samples for humans.
	SeriesNames map[uint64]string

	// mu guards every field above; HandleFrame holds it for the duration
	// of processing one frame, serializing this shard's state machine
	// while leaving unrelated shards free to process concurrently.
	mu sync.Mutex
}

// EmitterState is one emitter's bookkeeping within a shard: which epoch
// it's currently in, and its own per-view keyframe-stream decode state
// (each emitter's KDELTA keyframes are only decodable against *that
// emitter's own* dict_delta history, even though births/tombstones also
// feed the shard-wide recovery dictionary — see emitterViewState).
type EmitterState struct {
	EmitterID uint64

	epochSet bool
	Epoch    uint64

	Views map[uint16]*emitterViewState

	ClockSkewMs      int64
	clockSkewAlerted bool
	LastArrival      time.Time
}

// emitterViewState mirrors one emitter's own dict_delta stream for one
// view: the active-ID set and prior keyframe values a KDELTA keyframe from
// *that* emitter must be decoded against (ADR-011), and whether a golden
// (Full) keyframe has ever been seen (an emitter's first KDELTA before any
// golden keyframe is an orphaned-delta violation).
type emitterViewState struct {
	dict               *recover.Dictionary
	prevKeyframeValues map[uint64]float32
	seenGolden         bool
}

// ViewState is the shared, coverage-gated recovery state for one (shard,
// epoch, view) coordinate: the union dictionary every contributing
// emitter's dict_deltas apply to (used for BuildCSR/Recover), and one
// WindowState per in-flight window (Frame.Seq).
type ViewState struct {
	Epoch  uint64
	ViewID uint16
	Seed   uint64

	Dict      *recover.Dictionary
	Windows   map[uint32]*WindowState
	Watermark *Watermark
	Breaker   *sketch.Breaker // decode-side churn breaker, defense in depth (see Config.MaxBirthsPerViewPerMin)

	// FirstArrivalForWindow anchors clock-skew estimation: the wall-clock
	// time the first frame for a given window was observed, across every
	// emitter contributing to this view.
	FirstArrivalForWindow map[uint32]time.Time

	Degraded       bool
	breakerAlerted bool
}

// WindowState accumulates one window's (Frame.Seq's) merged sketch across
// every contributing emitter (ADR-013 additivity), plus whatever ADR-009
// speculative snapshot blobs arrived alongside it.
type WindowState struct {
	Seq  uint32
	M, D int
	Bits int

	Y            []float64
	Contributors map[uint64]bool

	SnapshotBlobs [][]byte

	Revision  int
	Recovered bool
}

func (w *WindowState) merge(emitterID uint64, y []float64) {
	if w.Y == nil {
		w.Y = append([]float64(nil), y...)
	} else {
		for i := range w.Y {
			w.Y[i] += y[i]
		}
	}
	w.Contributors[emitterID] = true
}

// HandleFrame is the Engine's sole entry point: it applies one decoded
// wire.Frame to the appropriate shard's state and reacts per its frame
// type. Intended to be passed as the handle callback to a
// ports.FrameSource's Run method.
func (e *Engine) HandleFrame(ctx context.Context, f *wire.Frame) {
	now := e.now()
	e.metrics.IncFramesTotal(frameTypeLabel(f.FrameType), f.EmitterID, f.ShardID)

	shard := e.getOrCreateShard(f.ShardID)

	shard.mu.Lock()
	defer shard.mu.Unlock()

	em := shard.getOrCreateEmitter(f.EmitterID)
	if !em.epochSet {
		em.epochSet = true
		em.Epoch = f.Epoch
	} else if f.Epoch > em.Epoch {
		em.Epoch = f.Epoch
		em.Views = make(map[uint16]*emitterViewState) // ADR-013: epochs are independent coordinate systems
	} else if f.Epoch < em.Epoch {
		e.metrics.IncLateFramesDropped()
		return
	}

	evs := em.getOrCreateEmitterView(f.ViewID)

	// Resolve the tenant key for this frame's key_version. Gate and count
	// unknown versions rather than silently corrupting recovery (ADR-012
	// §Addendum: "unknown version -> frame gated low-confidence +
	// keyring_miss metric; never silent drop").
	tenantKey, keyOK := e.resolveTenantKey(f)
	if !keyOK {
		e.metrics.IncKeyringMiss()
		return
	}

	view := shard.getOrCreateView(e.cfg, f.Epoch, f.ViewID, f.KeyVersion, tenantKey)

	e.trackClockSkew(shard, em, view, f, now)

	var tombstones []uint64
	for _, dd := range f.DictDeltas {
		evs.dict.ApplyDelta(dd)
		isNewBirth := view.Dict.ApplyDelta(dd)
		if dd.IsTombstone() {
			e.metrics.IncTombstonesApplied(1)
			tombstones = append(tombstones, dd.ID)
			continue
		}
		e.metrics.IncBirthsApplied(1)
		shard.SeriesNames[dd.ID] = string(dd.Name)
		if isNewBirth && view.Breaker != nil {
			if !view.Breaker.ObserveBirth(dd.ID, now) && !view.breakerAlerted {
				view.breakerAlerted = true
				e.metrics.IncBreakerTrips()
				e.fireAlert(shard.ShardID, f.EmitterID, "decode-side churn breaker tripped: shard-wide birth rate exceeded MaxBirthsPerViewPerMin", now)
			}
		}
	}
	if len(tombstones) > 0 {
		e.emitEvents(ctx, e.matcher.EvalAbsence(tombstones), shard, f.EmitterID, f.Epoch, f.ViewID, f.Seq, now)
	}

	switch f.FrameType {
	case wire.FrameTypeKeyframe:
		e.handleKeyframe(ctx, shard, evs, view, f, now)
	case wire.FrameTypeResidual:
		e.handleResidual(ctx, shard, view, f, now)
	case wire.FrameTypeFallback:
		e.handleFallback(ctx, shard, view, f, now)
	}

	e.gcWindows(view, now)
}

func (e *Engine) handleKeyframe(ctx context.Context, shard *ShardState, evs *emitterViewState, view *ViewState, f *wire.Frame, now time.Time) {
	kdelta := f.Flags&wire.FlagKDelta != 0
	ids := evs.dict.ActiveIDs()

	var (
		values32 map[uint64]float32
		err      error
	)
	if !kdelta {
		values32, err = wire.DecodeKeyframe(f.Payload, false, nil, nil, 0)
	} else if !evs.seenGolden {
		// ADR-013: "no orphaned deltas" — a KDELTA before this emitter's
		// first golden keyframe can't be decoded correctly.
		e.setDegraded(view, true)
		return
	} else {
		values32, err = wire.DecodeKeyframe(f.Payload, true, ids, evs.prevKeyframeValues, f.QuantScale)
	}
	if err != nil {
		e.setDegraded(view, true)
		return
	}

	if !evs.dict.VerifyKeyframe(f.DictRoot) {
		e.setDegraded(view, true)
		return
	}
	if !kdelta {
		evs.seenGolden = true
		// Heal only on a verified GOLDEN (Full) keyframe: a matching
		// KDELTA doesn't prove the encoder and decoder's full active-ID
		// sets ever agreed on more than the small delta just decoded.
		e.setDegraded(view, false)
	}

	values64 := make(map[uint64]float64, len(values32))
	for id, v := range values32 {
		values64[id] = float64(v)
	}
	evs.dict.ApplyKeyframeValues(values32)
	view.Dict.ApplyKeyframeValues(values32)

	events := e.matcher.EvalKeyframe(values32, evs.prevKeyframeValues)
	e.emitEvents(ctx, events, shard, f.EmitterID, f.Epoch, f.ViewID, f.Seq, now)

	evs.prevKeyframeValues = values32

	if e.seriesSink != nil && len(values64) > 0 {
		samples := make([]ports.Sample, 0, len(values64))
		for id, v := range values64 {
			samples = append(samples, ports.Sample{
				SeriesID:    id,
				Name:        shard.SeriesNames[id],
				ShardID:     f.ShardID,
				EmitterID:   f.EmitterID,
				ViewID:      f.ViewID,
				Value:       v,
				TimestampMs: now.UnixMilli(),
			})
		}
		_ = e.seriesSink.WriteSeries(ctx, samples)
	}
}

func (e *Engine) handleResidual(ctx context.Context, shard *ShardState, view *ViewState, f *wire.Frame, now time.Time) {
	if view.Degraded {
		e.metrics.IncGatedFrames()
		return
	}

	y, err := wire.Dequantize(f.Payload, f.M, f.Bits, f.QuantScale)
	if err != nil {
		return
	}

	ready, repair, tooLate := view.Watermark.OnFrameArrival(f.Seq, now)
	if tooLate {
		e.metrics.IncLateFramesDropped()
		return
	}

	win := view.getOrCreateWindow(f.Seq, int(f.M), int(f.D), int(f.Bits))
	if win.Contributors[f.EmitterID] {
		// ADR-013 dedup: this emitter already contributed to this window
		// (a repeated delivery — network retry, or an out-of-order replay
		// — never a legitimate second contribution: repair only ever
		// means a *different* emitter's first-ever frame for this window
		// arriving late). Merging again would double-count it.
		e.metrics.IncDuplicateFramesDropped()
	} else {
		win.merge(f.EmitterID, y)
		if len(f.SnapshotBlob) > 0 {
			win.SnapshotBlobs = append(win.SnapshotBlobs, f.SnapshotBlob)
		}
		if ready || repair {
			e.solveWindow(ctx, shard, view, win, now)
		}
	}

	// A different window may have just become ready as a side effect of
	// this frame advancing the watermark's high-water mark, even though
	// its own last direct arrival was earlier (ADR-013).
	for _, seq := range view.Watermark.ReadyWindows() {
		if w, ok := view.Windows[seq]; ok {
			e.solveWindow(ctx, shard, view, w, now)
		}
	}
}

func (e *Engine) handleFallback(ctx context.Context, shard *ShardState, view *ViewState, f *wire.Frame, now time.Time) {
	fb, err := wire.DecodeFallback(f.Payload)
	if err != nil {
		return
	}
	events := e.matcher.EvalFallback(fb)
	e.emitEvents(ctx, events, shard, f.EmitterID, f.Epoch, f.ViewID, f.Seq, now)
}

// solveWindow runs recover.Recover over win's merged sketch, annotates
// coverage/revision, gates on the residual, and (if it passes) emits
// deviation anomalies plus dashcam snapshots and Layer-1 rehydration.
func (e *Engine) solveWindow(ctx context.Context, shard *ShardState, view *ViewState, win *WindowState, now time.Time) {
	opts := e.cfg.recoverOptions(win.Revision)
	params := sketch.Params{M: win.M, D: win.D, Seed: view.Seed}

	start := e.now()
	res, err := recover.Recover(win.Y, view.Dict, params, opts)
	e.metrics.ObserveRecoverSeconds(shard.ShardID, e.now().Sub(start))
	if err != nil {
		return
	}
	res.Coverage = len(win.Contributors)
	res.CoverageTotal = e.cfg.EmittersExpected
	e.evaluateMergedTrust(&res, view)

	view.Watermark.MarkRecovered(win.Seq, now)
	win.Revision++
	win.Recovered = true

	if res.Confidence == recover.ConfidenceFallback || res.Residual > e.cfg.MaxResidual {
		e.metrics.IncGatedFrames()
		e.setDegraded(view, true)
		return
	}

	events := e.matcher.EvalDeviation(res)
	e.emitEvents(ctx, events, shard, 0, view.Epoch, view.ViewID, win.Seq, now)

	if e.forensics != nil && len(res.SupportIDs) > 0 && len(win.SnapshotBlobs) > 0 {
		for _, blob := range win.SnapshotBlobs {
			snaps, err := e.forensics.SelectAndPersist(res.SupportIDs, blob, now)
			if err != nil {
				continue
			}
			e.metrics.IncSnapshotsCaptured(len(snaps))
			for _, s := range snaps {
				e.metrics.ObserveSnapshotsLatencySeconds(s.Latency(now))
			}
		}
		win.SnapshotBlobs = nil
	}

	if e.seriesSink != nil && len(res.SupportIDs) > 0 {
		samples := make([]ports.Sample, len(res.SupportIDs))
		for i, id := range res.SupportIDs {
			samples[i] = ports.Sample{
				SeriesID:    id,
				Name:        shard.SeriesNames[id],
				ShardID:     shard.ShardID,
				ViewID:      view.ViewID,
				Value:       res.Values[i],
				TimestampMs: now.UnixMilli(),
			}
		}
		_ = e.seriesSink.WriteSeries(ctx, samples)
	}
}

// evaluateMergedTrust stamps res.MergedTrust (ADR-015): views absent from
// Config.MergedViews are not merged tier and always get MergedTrustNA. A
// declared merged-tier view's guardrail is evaluated from this solve's own
// live k (RawSupport)/A (Coverage, just set by the caller) and the view's
// current dictionary size (N) — never Config.EmittersExpected, which is a
// static ceiling, not what actually contributed this window. This runs
// before the residual/DEGRADED gate below: the two gates are independent
// and both apply regardless of order.
func (e *Engine) evaluateMergedTrust(res *recover.Result, view *ViewState) {
	policy, ok := e.cfg.MergedViews[view.ViewID]
	if !ok {
		res.MergedTrust = recover.MergedTrustNA
		return
	}
	n := len(view.Dict.ActiveIDs())
	_, trust := policy.evaluate(res.RawSupport, n, res.Coverage)
	res.MergedTrust = trust
	e.metrics.IncMergedWindows(trust)
}

func (e *Engine) setDegraded(view *ViewState, degraded bool) {
	if view.Degraded == degraded {
		return
	}
	view.Degraded = degraded
	if degraded {
		e.metrics.IncDegradedShards()
	} else {
		e.metrics.DecDegradedShards()
	}
}

// trackClockSkew estimates emitterID's clock skew for window f.Seq as the
// wall-clock gap between its arrival and the first arrival (from any
// emitter) of that same window, and fires a sticky Alert once it exceeds
// Config.ClockSkewToleranceMs.
func (e *Engine) trackClockSkew(shard *ShardState, em *EmitterState, view *ViewState, f *wire.Frame, now time.Time) {
	em.LastArrival = now

	first, ok := view.FirstArrivalForWindow[f.Seq]
	if !ok {
		view.FirstArrivalForWindow[f.Seq] = now
		em.ClockSkewMs = 0
		e.metrics.SetClockSkewMs(f.EmitterID, 0)
		return
	}
	skewMs := now.Sub(first).Milliseconds()
	em.ClockSkewMs = skewMs
	e.metrics.SetClockSkewMs(f.EmitterID, skewMs)

	if e.cfg.ClockSkewToleranceMs <= 0 {
		return
	}
	exceeded := skewMs > e.cfg.ClockSkewToleranceMs || -skewMs > e.cfg.ClockSkewToleranceMs
	switch {
	case exceeded && !em.clockSkewAlerted:
		em.clockSkewAlerted = true
		e.fireAlert(shard.ShardID, f.EmitterID, fmt.Sprintf("clock skew %dms exceeds tolerance %dms", skewMs, e.cfg.ClockSkewToleranceMs), now)
	case !exceeded:
		em.clockSkewAlerted = false
	}
}

func (e *Engine) fireAlert(shardID, emitterID uint64, reason string, now time.Time) {
	e.mu.Lock()
	e.alerts = append(e.alerts, Alert{Time: now, ShardID: shardID, EmitterID: emitterID, Reason: reason})
	if len(e.alerts) > maxAlertsRetained {
		e.alerts = e.alerts[len(e.alerts)-maxAlertsRetained:]
	}
	e.mu.Unlock()
}

// Alerts returns a copy of every Alert fired so far (clock skew, decode-side
// churn breaker).
func (e *Engine) Alerts() []Alert {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Alert, len(e.alerts))
	copy(out, e.alerts)
	return out
}

// ShardDegraded reports whether any (epoch, view) this shard currently
// tracks is DEGRADED.
func (e *Engine) ShardDegraded(shardID uint64) bool {
	e.mu.Lock()
	s, ok := e.shards[shardID]
	e.mu.Unlock()
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.Views {
		if v.Degraded {
			return true
		}
	}
	return false
}

// gcWindows drops windows that are both recovered and definitively past
// RepairHorizon, bounding memory to in-flight/recently-repairable windows.
// Caller must hold shard.mu (via HandleFrame).
func (e *Engine) gcWindows(view *ViewState, now time.Time) {
	if e.cfg.RepairHorizon <= 0 {
		return
	}
	for seq := range view.Windows {
		recoveredAt, recovered := view.Watermark.RecoveredAt(seq)
		if !recovered || now.Sub(recoveredAt) <= e.cfg.RepairHorizon {
			continue
		}
		delete(view.Windows, seq)
		delete(view.FirstArrivalForWindow, seq)
		view.Watermark.Forget(seq)
	}
}

func (e *Engine) emitEvents(ctx context.Context, events []ports.AnomalyEvent, shard *ShardState, emitterID uint64, epoch uint64, viewID uint16, windowID uint32, now time.Time) {
	for _, ev := range events {
		ev.ShardID = shard.ShardID
		ev.EmitterID = emitterID
		ev.Epoch = epoch
		ev.ViewID = viewID
		ev.WindowID = windowID
		ev.DetectedAt = now
		if ev.SeriesName == "" {
			ev.SeriesName = shard.SeriesNames[ev.SeriesID]
		}
		if e.forensics != nil {
			if snap, ok := e.forensics.Get(ev.SeriesID, now); ok {
				ev.Snapshot = snap.Entries
			}
		}
		if err := e.anomalySink.EmitAnomaly(ctx, ev); err != nil {
			continue
		}
		e.metrics.IncAnomaliesFired(ev.Substrate, ev.Confidence)
	}
}

func (e *Engine) getOrCreateShard(shardID uint64) *ShardState {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.shards[shardID]
	if !ok {
		s = &ShardState{
			ShardID:     shardID,
			Emitters:    make(map[uint64]*EmitterState),
			Views:       make(map[viewKey]*ViewState),
			SeriesNames: make(map[uint64]string),
		}
		e.shards[shardID] = s
	}
	return s
}

func (s *ShardState) getOrCreateEmitter(emitterID uint64) *EmitterState {
	em, ok := s.Emitters[emitterID]
	if !ok {
		em = &EmitterState{EmitterID: emitterID, Views: make(map[uint16]*emitterViewState)}
		s.Emitters[emitterID] = em
	}
	return em
}

func (em *EmitterState) getOrCreateEmitterView(viewID uint16) *emitterViewState {
	v, ok := em.Views[viewID]
	if !ok {
		v = &emitterViewState{dict: recover.NewDictionary()}
		em.Views[viewID] = v
	}
	return v
}

func (s *ShardState) getOrCreateView(cfg Config, epoch uint64, viewID uint16, keyVersion uint8, tenantKey []byte) *ViewState {
	key := viewKey{epoch: epoch, view: viewID, keyVersion: keyVersion}
	v, ok := s.Views[key]
	if !ok {
		seed := sketch.DeriveEphemeralSeed(tenantKey, s.ShardID, uint32(epoch), viewID)
		v = &ViewState{
			Epoch:                 epoch,
			ViewID:                viewID,
			Seed:                  seed,
			Dict:                  recover.NewDictionary(),
			Windows:               make(map[uint32]*WindowState),
			Watermark:             NewWatermark(cfg.AllowedLateness, cfg.RepairHorizon),
			FirstArrivalForWindow: make(map[uint32]time.Time),
		}
		if cfg.MaxBirthsPerViewPerMin > 0 {
			v.Breaker = sketch.NewBreaker(cfg.MaxBirthsPerViewPerMin, time.Minute)
		}
		s.Views[key] = v
	}
	return v
}

func (v *ViewState) getOrCreateWindow(seq uint32, m, d, bits int) *WindowState {
	w, ok := v.Windows[seq]
	if !ok {
		w = &WindowState{Seq: seq, M: m, D: d, Bits: bits, Contributors: make(map[uint64]bool)}
		v.Windows[seq] = w
	}
	return w
}

func frameTypeLabel(t uint8) string {
	switch t {
	case wire.FrameTypeKeyframe:
		return "keyframe"
	case wire.FrameTypeResidual:
		return "residual"
	case wire.FrameTypeFallback:
		return "fallback"
	default:
		return "unknown"
	}
}

// resolveTenantKey returns the HKDF key material that must be used to derive
// the seed for f's (shard, epoch, view) coordinate system.
// When KeyRing is nil, TenantKey is used (single-key mode).
// When KeyRing is set, it looks up f.KeyVersion; if missing, it returns
// ("", false) so HandleFrame can gate the frame and increment keyring_miss.
func (e *Engine) resolveTenantKey(f *wire.Frame) ([]byte, bool) {
	if e.cfg.KeyRing != nil {
		return e.cfg.KeyRing.Lookup(f.KeyVersion)
	}
	return e.cfg.TenantKey, true
}
