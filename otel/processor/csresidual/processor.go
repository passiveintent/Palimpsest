/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"

	"github.com/passiveintent/Palimpsest/pkg/predict"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/tier"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// stormHistoryWindow is the rolling-median window (in flush windows)
// sketch.StormDetector tracks. Not exposed in config (only
// storm.energy_multiplier and storm.fallback_topk are); 30 windows gives a
// reasonable baseline at the default 10s flush_interval (~5 minutes)
// without needing another knob.
const stormHistoryWindow = 30

// minQuantScale floors adaptive quantization scales so an all-zero (or
// all-identical) window never produces an invalid (zero) wire.Quantize /
// wire.EncodeKeyframe scale.
const minQuantScale = 1e-6

// pipelineKey identifies one independent (shard, view) encode pipeline:
// sketches never mix across shards (ADR-002/ADR-012 seed derivation keys
// on shard_id) or across views (ADR-010 declared sketch-cube views).
type pipelineKey struct {
	shard uint64
	view  uint16
}

// pipeline is one (shard, view)'s complete encode-path state: the current
// window's instance fold, the ADR-008 dictionary lifecycle (Tracker +
// Predictor), the ADR-002 sketch (Accumulator), churn breaker, storm
// detector, instance ring buffer, and snapshot stager, plus the
// bookkeeping (epoch/seq/keyframe cadence) needed to emit a coherent
// wire.Frame stream.
//
// All fields are guarded by mu; pipeline is otherwise not safe for
// concurrent use (mirroring the NOT-safe-for-concurrent-use contract of
// sketch.Tracker/Accumulator themselves).
type pipeline struct {
	shardID uint64
	viewID  uint16

	mu sync.Mutex

	fold    *windowFold
	tracker *sketch.Tracker
	acc     *sketch.Accumulator
	pred    predict.Predictor
	breaker *sketch.Breaker
	storm   *sketch.StormDetector
	rings   *instanceBuffers
	snap    *snapshotStager

	epoch              uint64
	seq                uint32
	keyframeCount      int // keyframes emitted so far this epoch (golden cadence)
	prevKeyframeValues map[uint64]float32

	degraded       bool
	breakerAlerted bool
}

// frameSink is where flushed frames go. The production implementation
// (fileFrameSink) writes one file per frame under output.dir; tests
// substitute an in-memory sink so assertions don't need to parse the
// filesystem.
type frameSink interface {
	writeFrame(f *wire.Frame) error
}

// fileFrameSink implements frameSink by writing each frame, wire-encoded,
// to its own file under dir (config's output.dir).
type fileFrameSink struct {
	dir string
}

func (s *fileFrameSink) writeFrame(f *wire.Frame) error {
	b, err := wire.Marshal(f)
	if err != nil {
		return fmt.Errorf("csresidual: marshal frame: %w", err)
	}
	name := fmt.Sprintf("emitter%016x-shard%016x-epoch%016x-view%04x-seq%08x.plmp",
		f.EmitterID, f.ShardID, f.Epoch, f.ViewID, f.Seq)
	return os.WriteFile(filepath.Join(s.dir, name), b, 0o644)
}

// metricsProcessor is the csresidual processor's internal implementation:
// its consumeMetrics/start/shutdown methods are wired into a
// processor.Metrics by factory.go via processorhelper.NewMetrics.
// Constructing it directly (bypassing the factory) is how processor_test.go
// drives it deterministically, with an explicit clock instead of a real
// ticker and an in-memory frameSink instead of the filesystem.
type metricsProcessor struct {
	cfg    *Config
	logger *zap.Logger

	tenantKey []byte
	// keyRing is non-nil when tenant_keys rotation is configured (ADR-012
	// §Addendum). When set, deriveSeed uses the key for each frame's
	// key_version rather than the single tenantKey.
	keyRing sketch.KeyRing
	// activeKeyVersion is stamped on every newly-encoded frame (wire v2).
	activeKeyVersion uint8
	emitterID        uint64
	views            []viewSpec
	tierRules        tier.CompiledRules
	goldenEvery      int

	sink frameSink

	mu        sync.Mutex
	pipelines map[pipelineKey]*pipeline
	alerts    []Alert

	pullSrv *http.Server

	// onObserve, if non-nil, is called for every Tracker.Observe result
	// during a flush, before ResetWindow clears the window's transient
	// residual state. It exists purely as a test seam (processor_test.go
	// asserts per-series residual magnitude, which Tracker does not expose
	// after a flush completes) and has no effect on production behavior.
	onObserve func(pk pipelineKey, groupKey, agg string, id uint64, value, residual float64, isNew bool)

	// now returns the current time; defaults to time.Now, overridable so
	// processor_test.go can drive consumeMetrics/flushAll from the same
	// explicit, synthetic clock instead of coupling every test to real
	// wall-clock time (important since epoch/keyframe/TTL cadence all key
	// off "now").
	now func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newMetricsProcessor builds a metricsProcessor from cfg. sink may be nil,
// in which case cfg.Output.Type selects the default: a fileFrameSink rooted
// at cfg.Output.Dir (the "file" default, preserving pre-existing behavior),
// or a kafkaFrameSink built from cfg.Output.Kafka ("kafka").
func newMetricsProcessor(cfg *Config, logger *zap.Logger, sink frameSink) (*metricsProcessor, error) {
	var tenantKey []byte
	var keyRing sketch.KeyRing
	var activeKeyVersion uint8

	if len(cfg.TenantKeys) > 0 {
		// Multi-key rotation mode (ADR-012 §Addendum).
		ring, err := loadKeyRing(cfg.TenantKeys)
		if err != nil {
			return nil, err
		}
		keyRing = ring
		activeKeyVersion = cfg.ActiveKeyVersion
		// tenantKey for newPipeline uses the active version's key.
		tenantKey = ring[activeKeyVersion]
	} else {
		k, err := loadTenantKey(cfg.TenantKeyEnv)
		if err != nil {
			return nil, err
		}
		tenantKey = k
	}

	tierRules, err := compileTierRules(cfg.Tiers)
	if err != nil {
		return nil, err
	}
	goldenEvery, err := cfg.resolveGoldenEvery()
	if err != nil {
		return nil, err
	}
	if sink == nil {
		switch resolveOutputType(cfg) {
		case outputTypeKafka:
			ks, err := newKafkaFrameSink(cfg.Output.Kafka, cfg.TenantID)
			if err != nil {
				return nil, err
			}
			sink = ks
		default:
			sink = &fileFrameSink{dir: cfg.Output.Dir}
		}
	}
	return &metricsProcessor{
		cfg:              cfg,
		logger:           logger,
		tenantKey:        tenantKey,
		keyRing:          keyRing,
		activeKeyVersion: activeKeyVersion,
		emitterID:        randomEmitterID(),
		views:            resolveViews(cfg),
		tierRules:        tierRules,
		goldenEvery:      goldenEvery,
		sink:             sink,
		pipelines:        make(map[pipelineKey]*pipeline),
		now:              time.Now,
	}, nil
}

func randomEmitterID() uint64 {
	var b [8]byte
	_, _ = crand.Read(b[:]) // ADR-013: random per agent process; a zero fallback on the (practically unreachable) read-error path is still a valid, if unlucky, emitter id
	return binary.LittleEndian.Uint64(b[:])
}

// start implements component.StartFunc, wired in by factory.go via
// processorhelper.WithStart. It creates output.dir (for the default file
// sink), starts the optional ADR-012 pull endpoint, and starts the
// flush-timer goroutine — the only background goroutine this processor
// owns, cancelled cleanly in shutdown.
func (p *metricsProcessor) start(_ context.Context, _ component.Host) error {
	if fs, ok := p.sink.(*fileFrameSink); ok {
		if err := os.MkdirAll(fs.dir, 0o755); err != nil {
			return fmt.Errorf("csresidual: creating output.dir %q: %w", fs.dir, err)
		}
	}
	if ks, ok := p.sink.(*kafkaFrameSink); ok {
		ks.startDrainLoop(p.cfg.Output.Kafka.DrainInterval)
	}

	if p.cfg.Pull.Enabled {
		srv, err := newPullServer(p.cfg.Pull, p.pullLookup)
		if err != nil {
			return err
		}
		p.pullSrv = srv
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			var serveErr error
			if p.cfg.Pull.MTLS {
				serveErr = srv.ListenAndServeTLS("", "")
			} else {
				serveErr = srv.ListenAndServe()
			}
			if serveErr != nil && serveErr != http.ErrServerClosed {
				p.logger.Error("csresidual: pull endpoint stopped", zap.Error(serveErr))
			}
		}()
	}

	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.wg.Add(1)
	go p.flushLoop(runCtx)
	return nil
}

// shutdown implements component.ShutdownFunc: it stops the flush loop and
// pull endpoint, waits for both to exit, then performs one best-effort
// final flush so a partially-filled window isn't silently dropped.
func (p *metricsProcessor) shutdown(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.pullSrv != nil {
		_ = p.pullSrv.Shutdown(ctx)
	}
	p.wg.Wait()
	p.flushAll(p.now())
	if ks, ok := p.sink.(*kafkaFrameSink); ok {
		if err := ks.close(); err != nil {
			p.logger.Error("csresidual: closing kafka output", zap.Error(err))
		}
	}
	return nil
}

func (p *metricsProcessor) flushLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			p.flushAll(now)
		}
	}
}

// pullLookup answers the ADR-012 pull endpoint by merging matching
// instance samples across every pipeline (shard x view) this processor
// runs. instanceBuffers is internally synchronized, so this only needs to
// take p.mu long enough to snapshot the current pipeline set.
func (p *metricsProcessor) pullLookup(logical string) []wire.SnapshotEntry {
	p.mu.Lock()
	pls := make([]*pipeline, 0, len(p.pipelines))
	for _, pl := range p.pipelines {
		pls = append(pls, pl)
	}
	p.mu.Unlock()

	var out []wire.SnapshotEntry
	for _, pl := range pls {
		out = append(out, pl.rings.snapshotGroup(logical)...)
	}
	return out
}

// consumeMetrics is this processor's processorhelper.ProcessMetricsFunc:
// exact-tier metrics (ADR-005) pass through untouched; sketched-tier
// metrics are folded into the Palimpsest pipeline and, unless
// Config.Shadow is set, removed from md before it continues downstream
// (they are now represented by emitted frames instead).
func (p *metricsProcessor) consumeMetrics(_ context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	now := p.now()
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		resAttrs := rm.Resource().Attributes()
		sms := rm.ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			metrics := sms.At(j).Metrics()
			for k := 0; k < metrics.Len(); k++ {
				p.consumeMetric(metrics.At(k), resAttrs, now)
			}
		}
	}

	if !p.cfg.Shadow {
		removeSketchedDatapoints(md, p.tierRules)
	}
	return md, nil
}

func (p *metricsProcessor) consumeMetric(m pmetric.Metric, resAttrs pcommon.Map, now time.Time) {
	isSummary := m.Type() == pmetric.MetricTypeSummary
	tier := matchTier(p.tierRules, m.Name(), isSummary)

	if tier == tierExact {
		if p.cfg.DrilldownHint {
			p.tagDrilldownHint(m, resAttrs)
		}
		return
	}

	switch m.Type() {
	case pmetric.MetricTypeGauge:
		p.consumeNumberPoints(m.Name(), m.Gauge().DataPoints(), resAttrs, now, nil)
	case pmetric.MetricTypeSum:
		p.consumeNumberPoints(m.Name(), m.Sum().DataPoints(), resAttrs, now, nil)
	case pmetric.MetricTypeHistogram:
		p.consumeHistogramPoints(m.Name(), m.Histogram().DataPoints(), resAttrs, now)
	}

	// Shadow mode mirrors sketched data downstream unchanged (alongside
	// sketching it), so the drilldown hint is only useful to attach here;
	// otherwise consumeMetrics removes these points before they'd be seen.
	if p.cfg.Shadow && p.cfg.DrilldownHint {
		p.tagDrilldownHint(m, resAttrs)
	}
}

func (p *metricsProcessor) tagDrilldownHint(m pmetric.Metric, resAttrs pcommon.Map) {
	tag := func(attrs pcommon.Map) {
		pairs := resolvePairs(p.cfg.LogicalKey, attrs, resAttrs)
		attrs.PutStr("palimpsest.logical_series", groupKey(m.Name(), pairs))
	}
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			tag(dps.At(i).Attributes())
		}
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			tag(dps.At(i).Attributes())
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			tag(dps.At(i).Attributes())
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			tag(dps.At(i).Attributes())
		}
	}
}

func (p *metricsProcessor) consumeNumberPoints(metric string, dps pmetric.NumberDataPointSlice, resAttrs pcommon.Map, now time.Time, extra *kv) {
	for i := 0; i < dps.Len(); i++ {
		dp := dps.At(i)
		value := dp.DoubleValue()
		if dp.ValueType() == pmetric.NumberDataPointValueTypeInt {
			value = float64(dp.IntValue())
		}
		p.observeSample(now, metric, dp.Attributes(), resAttrs, value, extra)
	}
}

func (p *metricsProcessor) consumeHistogramPoints(metric string, dps pmetric.HistogramDataPointSlice, resAttrs pcommon.Map, now time.Time) {
	for i := 0; i < dps.Len(); i++ {
		dp := dps.At(i)
		counts := dp.BucketCounts()
		bounds := dp.ExplicitBounds()
		for b := 0; b < counts.Len(); b++ {
			bound := math.Inf(1)
			if b < bounds.Len() {
				bound = bounds.At(b)
			}
			le := leKV(bound)
			p.observeSample(now, metric, dp.Attributes(), resAttrs, float64(counts.At(b)), &le)
		}
	}
}

// observeSample routes one raw sample into every declared view's pipeline
// for the sample's shard, accumulating it into this window's fold and
// (unless the pipeline is breaker-degraded) its instance ring buffer.
// extra, if non-nil, is prepended to the view's own label pairs (histogram
// bucket identity's synthetic "le" pair).
func (p *metricsProcessor) observeSample(now time.Time, metric string, dpAttrs, resAttrs pcommon.Map, value float64, extra *kv) {
	shardID := computeShardID(p.cfg.ShardBy, dpAttrs, resAttrs)
	instKey := joinKV(resolvePairs(p.cfg.InstanceKey, dpAttrs, resAttrs))
	instID := sketch.SeriesID([]byte(instKey))
	tsMs := uint64(now.UnixMilli())

	for _, v := range p.views {
		pairs := resolvePairs(v.labels, dpAttrs, resAttrs)
		if extra != nil {
			pairs = append([]kv{*extra}, pairs...)
		}
		gk := groupKey(metric, pairs)

		pl := p.getOrCreatePipeline(shardID, v, now)
		pl.mu.Lock()
		pl.fold.observe(gk, instKey, value)
		if !pl.degraded {
			pl.rings.push(gk, instKey, instID, tsMs, value)
		}
		pl.mu.Unlock()
	}
}

func computeShardID(keys []string, dpAttrs, resAttrs pcommon.Map) uint64 {
	if len(keys) == 0 {
		return 0
	}
	return sketch.SeriesID([]byte(joinKV(resolvePairs(keys, dpAttrs, resAttrs))))
}

func (p *metricsProcessor) getOrCreatePipeline(shardID uint64, v viewSpec, now time.Time) *pipeline {
	key := pipelineKey{shard: shardID, view: v.id}

	p.mu.Lock()
	defer p.mu.Unlock()
	pl, ok := p.pipelines[key]
	if !ok {
		pl = p.newPipeline(shardID, v, now)
		p.pipelines[key] = pl
	}
	return pl
}

func (p *metricsProcessor) newPipeline(shardID uint64, v viewSpec, now time.Time) *pipeline {
	epoch := epochIndex(now, p.cfg.EpochRotate)
	seed := deriveSeed(p.tenantKey, shardID, uint32(epoch), v.id)
	pred := predict.NewHold()
	breaker := newBreaker(p.cfg.Churn)
	maxBytes, _ := parseByteSize(p.cfg.RingBuffer.MaxTotalBytes) // shape already validated by Config.Validate

	return &pipeline{
		shardID: shardID,
		viewID:  v.id,
		fold:    newWindowFold(),
		tracker: sketch.NewTracker(pred, breaker),
		acc:     sketch.NewAccumulator(sketch.Params{M: p.cfg.M, D: p.cfg.D, Seed: seed, Bits: p.cfg.Bits}),
		pred:    pred,
		breaker: breaker,
		storm:   sketch.NewStormDetector(stormHistoryWindow, p.cfg.Storm.EnergyMultiplier),
		rings:   newInstanceBuffers(p.cfg.RingBuffer.Window, p.cfg.RingBuffer.MaxInstancesPerLogical, maxBytes),
		snap:    newSnapshotStager(p.cfg.Snapshot),
		epoch:   epoch,
	}
}

// removeSketchedDatapoints drops every datapoint of every sketched-tier
// metric from md (Config.Shadow == false path): that data is now
// represented by emitted frames instead of flowing through the rest of
// the OTel pipeline. Exact-tier metrics (and, for shadow mode, all of it)
// are left untouched by the caller.
func removeSketchedDatapoints(md pmetric.Metrics, rules tier.CompiledRules) {
	rms := md.ResourceMetrics()
	rms.RemoveIf(func(rm pmetric.ResourceMetrics) bool {
		sms := rm.ScopeMetrics()
		sms.RemoveIf(func(sm pmetric.ScopeMetrics) bool {
			ms := sm.Metrics()
			ms.RemoveIf(func(m pmetric.Metric) bool {
				isSummary := m.Type() == pmetric.MetricTypeSummary
				if matchTier(rules, m.Name(), isSummary) != tierSketched {
					return false
				}
				switch m.Type() {
				case pmetric.MetricTypeGauge:
					dps := m.Gauge().DataPoints()
					dps.RemoveIf(func(pmetric.NumberDataPoint) bool { return true })
					return dps.Len() == 0
				case pmetric.MetricTypeSum:
					dps := m.Sum().DataPoints()
					dps.RemoveIf(func(pmetric.NumberDataPoint) bool { return true })
					return dps.Len() == 0
				case pmetric.MetricTypeHistogram:
					dps := m.Histogram().DataPoints()
					dps.RemoveIf(func(pmetric.HistogramDataPoint) bool { return true })
					return dps.Len() == 0
				default:
					return false
				}
			})
			return ms.Len() == 0
		})
		return sms.Len() == 0
	})
}

// flushAll runs one flush cycle across every pipeline that currently
// exists, each independently (ADR-010 multi-view: "state remains
// independent").
func (p *metricsProcessor) flushAll(now time.Time) {
	p.mu.Lock()
	pls := make([]*pipeline, 0, len(p.pipelines))
	for _, pl := range p.pipelines {
		pls = append(pls, pl)
	}
	p.mu.Unlock()

	for _, pl := range pls {
		pl.mu.Lock()
		p.flushPipeline(pl, now)
		pl.mu.Unlock()
	}
}

// flushPipeline ends pl's current window: it rotates epoch if due, folds
// and observes every group's configured aggregates through the dictionary
// lifecycle and sketch, expires/drains dictionary deltas, decides this
// window's frame type (KEYFRAME on cadence, else FALLBACK on a storm
// trigger, else RESIDUAL), attaches any due snapshot blob, and writes the
// resulting frame. Caller must hold pl.mu.
func (p *metricsProcessor) flushPipeline(pl *pipeline, now time.Time) {
	forceGolden := false
	if newEpoch := epochIndex(now, p.cfg.EpochRotate); newEpoch != pl.epoch {
		p.rotateEpoch(pl, newEpoch, now)
		forceGolden = true
	}

	folded := pl.fold.fold()
	pl.fold.reset()

	for gk, fr := range folded {
		for _, agg := range p.cfg.Aggregates {
			name := logicalName(gk, agg)
			val := aggregateValue(fr, agg)
			id, isNew, residual := pl.tracker.Observe([]byte(name), val, now)
			if p.onObserve != nil {
				p.onObserve(pipelineKey{shard: pl.shardID, view: pl.viewID}, gk, agg, id, val, residual, isNew)
			}
			if isNew {
				continue // birth: no sketch contribution (ADR-008)
			}
			pl.acc.Update([]byte(name), residual)
			pl.snap.observe(id, gk, residual, now, pl.rings.snapshotGroup)
		}
	}

	tombstones := pl.tracker.Expire(now, p.cfg.SeriesTTL)
	births := pl.tracker.DrainDeltas()
	dictDeltas := append(births, tombstones...)

	y, energy := pl.acc.Flush()
	p.checkBreaker(pl, now)

	var f wire.Frame
	f.Magic = wire.Magic
	f.Version = wire.Version
	f.EmitterID = p.emitterID
	f.ShardID = pl.shardID
	f.Epoch = pl.epoch
	f.Seq = pl.seq
	f.ViewID = pl.viewID
	f.M = uint32(p.cfg.M)
	f.D = uint8(p.cfg.D)
	f.Predictor = uint8(pl.pred.ID())
	f.Bits = uint8(p.cfg.Bits)
	f.Energy = float32(energy)
	f.KeyVersion = p.activeKeyVersion // ADR-012 §Addendum: stamp the active key version

	keyframeWindow := pl.seq%uint32(p.cfg.KeyframeEvery) == 0
	switch {
	case forceGolden || keyframeWindow:
		p.buildKeyframe(pl, &f, forceGolden, dictDeltas)
	case pl.storm.Trigger(energy):
		p.buildFallback(pl, &f, dictDeltas)
	default:
		scale := adaptiveScale(y, p.cfg.Bits)
		p.buildResidual(&f, y, scale, dictDeltas, pl.tracker.ActiveIDs())
	}

	if entries := pl.snap.drain(now); len(entries) > 0 {
		blob, err := wire.EncodeSnapshot(entries, true)
		if err != nil {
			p.logger.Error("csresidual: EncodeSnapshot", zap.Error(err))
		} else {
			f.SnapshotBlob = blob
			// v2: use the Codec field rather than the deprecated FlagGzip bit.
			f.Codec = wire.CodecGzip
		}
	}

	if err := p.sink.writeFrame(&f); err != nil {
		p.logger.Error("csresidual: writing frame", zap.Error(err), zap.Uint64("shard_id", pl.shardID), zap.Uint16("view_id", pl.viewID))
	}

	pl.seq++
	pl.tracker.ResetWindow()
	pl.rings.drain(p.cfg.RingBuffer.Window)
}

// rotateEpoch starts a fresh coordinate system for pl (ADR-013: "Epochs
// are independent coordinate systems"): the Accumulator's implicit Phi
// seed is keyed by epoch index, so a rotation needs a brand new
// Accumulator (and, since the dictionary and the sketch must agree on
// which IDs exist, a brand new Tracker/Predictor too). Every series active
// just before rotation is re-birthed into the new tracker at its last
// known value, so the new epoch's golden keyframe carries it forward
// rather than silently losing it. Caller must hold pl.mu.
func (p *metricsProcessor) rotateEpoch(pl *pipeline, newEpoch uint64, now time.Time) {
	prevActive := pl.tracker.FullDict()

	seed := deriveSeed(p.tenantKey, pl.shardID, uint32(newEpoch), pl.viewID)
	pred := predict.NewHold()
	breaker := newBreaker(p.cfg.Churn)
	tracker := sketch.NewTracker(pred, breaker)
	acc := sketch.NewAccumulator(sketch.Params{M: p.cfg.M, D: p.cfg.D, Seed: seed, Bits: p.cfg.Bits})

	for _, dd := range prevActive {
		tracker.Observe(dd.Name, float64(dd.InitValue), now)
	}

	pl.epoch = newEpoch
	pl.seq = 0
	pl.tracker = tracker
	pl.acc = acc
	pl.pred = pred
	pl.breaker = breaker
	pl.prevKeyframeValues = nil
	pl.keyframeCount = 0
	pl.breakerAlerted = false
	pl.degraded = false
}

func (p *metricsProcessor) buildKeyframe(pl *pipeline, f *wire.Frame, forceGolden bool, dictDeltas []wire.DictDelta) {
	golden := forceGolden || pl.keyframeCount%p.goldenEvery == 0
	pl.keyframeCount++

	ids := pl.tracker.ActiveIDs()
	values64 := pl.tracker.CurrentValues()
	values32 := make(map[uint64]float32, len(values64))
	for id, v := range values64 {
		values32[id] = float32(v)
	}

	scale := adaptiveKDeltaScale(values32, pl.prevKeyframeValues)
	payload, flags, err := wire.EncodeKeyframe(ids, values32, pl.prevKeyframeValues, golden, scale)
	if err != nil {
		p.logger.Error("csresidual: EncodeKeyframe", zap.Error(err))
		payload, flags = nil, 0
	}

	f.FrameType = wire.FrameTypeKeyframe
	f.Flags |= flags
	f.Payload = payload
	f.QuantScale = scale
	f.DictRoot = wire.ComputeDictRoot(ids)
	if golden {
		f.DictDeltas = pl.tracker.FullDict()
	} else {
		f.DictDeltas = dictDeltas
	}
	pl.prevKeyframeValues = values32
	// ADR-003: a keyframe is the only steady-state point the open-loop
	// predictor baseline is allowed to move. Refresh it to the true current
	// value here, or residuals accumulate unbounded from birth forever
	// instead of resetting each keyframe, and substrate (c) slow-drift
	// detection (Matcher.EvalKeyframe) never sees a nonzero delta.
	pl.pred.LoadKeyframe(values64)
}

func (p *metricsProcessor) buildFallback(pl *pipeline, f *wire.Frame, dictDeltas []wire.DictDelta) {
	topK := pl.tracker.TopKResiduals(p.cfg.Storm.FallbackTopK)
	full := pl.tracker.FullDict() // (id, lastValue, name) for every currently active series

	lastValue := make(map[uint64]float32, len(full))
	var totalSum float64
	for _, dd := range full {
		lastValue[dd.ID] = dd.InitValue
		totalSum += float64(dd.InitValue)
	}

	ids := make([]uint64, 0, len(topK))
	for id := range topK {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	values := make(map[uint64]float32, len(ids))
	for _, id := range ids {
		values[id] = lastValue[id]
	}

	payload, err := wire.EncodeFallback(ids, values, totalSum, uint64(len(full)))
	if err != nil {
		p.logger.Error("csresidual: EncodeFallback", zap.Error(err))
		payload = nil
	}

	f.FrameType = wire.FrameTypeFallback
	f.Payload = payload
	f.DictDeltas = dictDeltas
	f.DictRoot = wire.ComputeDictRoot(pl.tracker.ActiveIDs())
}

func (p *metricsProcessor) buildResidual(f *wire.Frame, y []float64, scale float32, dictDeltas []wire.DictDelta, ids []uint64) {
	payload, err := wire.Quantize(y, f.Bits, scale)
	if err != nil {
		p.logger.Error("csresidual: Quantize", zap.Error(err))
		payload = nil
	}
	f.FrameType = wire.FrameTypeResidual
	f.Payload = payload
	f.QuantScale = scale
	f.DictDeltas = dictDeltas
	f.DictRoot = wire.ComputeDictRoot(ids)
}

// adaptiveScale picks the smallest RESIDUAL quantization scale (per-frame
// QuantScale, docs/SPEC.md) that keeps the window's largest-magnitude
// sketch value from clamping against Quantize's int8/int16 range: scale =
// max|y| / representable-limit, floored at minQuantScale so an all-zero
// window still yields a valid (finite, positive) scale.
func adaptiveScale(y []float64, bits int) float32 {
	var maxAbs float64
	for _, v := range y {
		if a := math.Abs(v); a > maxAbs {
			maxAbs = a
		}
	}
	limit := 127.0
	if bits == 16 {
		limit = 32767.0
	}
	return scaleFor(maxAbs, limit)
}

// adaptiveKDeltaScale picks a KEYFRAME KDELTA quantization scale (ADR-011)
// from the largest per-ID value change versus the prior keyframe (0 for a
// birth, matching EncodeKeyframe's own delta-from-zero treatment). Unlike
// RESIDUAL, KDELTA deltas are zigzag-varint packed rather than clamped to
// a fixed bit width, so there is no hard representable-range ceiling; a
// 16-bit-equivalent precision target is a reasonable default.
func adaptiveKDeltaScale(cur, prev map[uint64]float32) float32 {
	var maxAbs float64
	for id, v := range cur {
		d := math.Abs(float64(v - prev[id]))
		if d > maxAbs {
			maxAbs = d
		}
	}
	return scaleFor(maxAbs, 32767.0)
}

func scaleFor(maxAbs, steps float64) float32 {
	if maxAbs <= 0 {
		return minQuantScale
	}
	s := float32(maxAbs / steps)
	if s <= 0 {
		return minQuantScale
	}
	return s
}
