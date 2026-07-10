/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// --- test helpers -----------------------------------------------------

// testClock lets tests drive consumeMetrics/flushAll from an explicit,
// synthetic timeline instead of real wall-clock time, which is essential
// since epoch rotation, keyframe cadence, and series_ttl all key off "now".
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock(t0 time.Time) *testClock { return &testClock{t: t0} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// memoryFrameSink captures flushed frames in memory instead of writing
// them to disk. Every stored frame has already round-tripped through
// wire.Marshal/wire.Unmarshal, so a captured frame having been produced at
// all is itself proof it is valid, parseable wire format.
type memoryFrameSink struct {
	mu     sync.Mutex
	frames []*wire.Frame
}

func (s *memoryFrameSink) writeFrame(f *wire.Frame) error {
	b, err := wire.Marshal(f)
	if err != nil {
		return err
	}
	decoded, err := wire.Unmarshal(b)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.frames = append(s.frames, decoded)
	s.mu.Unlock()
	return nil
}

func (s *memoryFrameSink) Frames() []*wire.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*wire.Frame, len(s.frames))
	copy(out, s.frames)
	return out
}

// baseTestConfig returns a Config tuned for fast, deterministic behavioral
// tests: small m/d, a huge keyframe_every (so only the pipeline's very
// first window is a KEYFRAME unless a test overrides it), snapshotting and
// the pull endpoint off unless a test opts in.
func baseTestConfig(t *testing.T) *Config {
	t.Helper()
	envVar := fmt.Sprintf("CSRESIDUAL_TEST_TENANT_KEY_%s", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	t.Setenv(envVar, "test-tenant-key-material")
	return &Config{
		TenantID:              "test-tenant",
		TenantKeyEnv:          envVar,
		LogicalKey:            []string{"service.name"},
		InstanceKey:           []string{"k8s.pod.name"},
		Aggregates:            []string{"sum", "count", "max"},
		M:                     256,
		D:                     4,
		Bits:                  8,
		FlushInterval:         time.Second, // unused directly; tests call flushAll explicitly
		KeyframeEvery:         100000,
		KeyframeFullDictEvery: 10,
		EpochRotate:           time.Hour,
		SeriesTTL:             2 * time.Second,
		Storm:                 StormConfig{EnergyMultiplier: 25, FallbackTopK: 10},
		Snapshot:              SnapshotConfig{Enabled: false, ResidualThreshold: 2.0, MaxSeriesPerFlush: 10, BlobTTL: time.Hour},
		RingBuffer:            RingBufferConfig{Window: 15 * time.Minute, MaxInstancesPerLogical: 1000, MaxTotalBytes: "10MB"},
		Churn:                 ChurnConfig{MaxBirthsPerLogicalPerMin: 1000},
		Pull:                  PullConfig{Enabled: false},
		DrilldownHint:         true,
		Tiers: []TierConfig{
			{Match: `^billing_.*`, Tier: tierExact},
			{Match: `.*`, Tier: tierSketched},
		},
		Shadow: false,
		Output: OutputConfig{Dir: t.TempDir()},
	}
}

// newTestProcessor validates cfg, builds a metricsProcessor wired to an
// in-memory sink, and points its clock at t0 (adjustable via the returned
// *testClock).
func newTestProcessor(t *testing.T, cfg *Config, t0 time.Time) (*metricsProcessor, *memoryFrameSink, *testClock) {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	sink := &memoryFrameSink{}
	mp, err := newMetricsProcessor(cfg, zap.NewNop(), sink)
	if err != nil {
		t.Fatalf("newMetricsProcessor: %v", err)
	}
	clock := newTestClock(t0)
	mp.now = clock.Now
	return mp, sink, clock
}

// point is one datapoint to inject: attrs are datapoint-level attributes
// (typically instance labels); value is the raw sample value.
type point struct {
	attrs map[string]string
	value float64
}

// gaugeMetrics builds a single-resource, single-metric pmetric.Metrics
// with one Gauge datapoint per pts entry.
func gaugeMetrics(metricName string, resAttrs map[string]string, pts []point) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	for k, v := range resAttrs {
		rm.Resource().Attributes().PutStr(k, v)
	}
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName(metricName)
	m.SetEmptyGauge()
	ts := pcommon.NewTimestampFromTime(time.Now())
	for _, pt := range pts {
		dp := m.Gauge().DataPoints().AppendEmpty()
		for k, v := range pt.attrs {
			dp.Attributes().PutStr(k, v)
		}
		dp.SetDoubleValue(pt.value)
		dp.SetTimestamp(ts)
	}
	return md
}

func podNames(prefix string, from, to int) []string {
	out := make([]string, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf("%s-%d", prefix, i))
	}
	return out
}

func mustConsume(t *testing.T, mp *metricsProcessor, md pmetric.Metrics) pmetric.Metrics {
	t.Helper()
	out, err := mp.consumeMetrics(context.Background(), md)
	if err != nil {
		t.Fatalf("consumeMetrics: %v", err)
	}
	return out
}

// --- (a) exact-tier metrics pass untouched -----------------------------

func TestExactTierPassesUntouched(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.DrilldownHint = false // isolate the "untouched" claim from the separate drilldown-hint feature
	mp, _, _ := newTestProcessor(t, cfg, time.Unix(1_700_000_000, 0))

	md := gaugeMetrics("billing_invoice_total", map[string]string{"service.name": "billing"},
		[]point{{attrs: map[string]string{"k8s.pod.name": "pod-0"}, value: 42.5}})
	original := pmetric.NewMetrics()
	md.CopyTo(original)

	out := mustConsume(t, mp, md)

	if out.ResourceMetrics().Len() != original.ResourceMetrics().Len() {
		t.Fatalf("resource metrics count changed: got %d, want %d", out.ResourceMetrics().Len(), original.ResourceMetrics().Len())
	}
	gotDP := out.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints().At(0)
	wantDP := original.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints().At(0)
	if gotDP.DoubleValue() != wantDP.DoubleValue() {
		t.Fatalf("exact-tier datapoint value changed: got %v, want %v", gotDP.DoubleValue(), wantDP.DoubleValue())
	}
	if !gotDP.Attributes().Equal(wantDP.Attributes()) {
		t.Fatalf("exact-tier datapoint attributes changed: got %v, want %v", gotDP.Attributes().AsRaw(), wantDP.Attributes().AsRaw())
	}

	mp.mu.Lock()
	n := len(mp.pipelines)
	mp.mu.Unlock()
	if n != 0 {
		t.Fatalf("exact-tier-only batch unexpectedly created %d pipeline(s)", n)
	}
}

// --- (b) sketched-tier frames parse, energy > 0, m=2000, d=6 -----------

func TestSketchedFramesParseWithEnergyAboveZero(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.M, cfg.D = 2000, 6
	cfg.Aggregates = []string{"sum"}
	mp, sink, clock := newTestProcessor(t, cfg, time.Unix(1_700_000_000, 0))

	mustConsume(t, mp, gaugeMetrics("cpu_usage", nil, []point{
		{attrs: map[string]string{"service.name": "svc", "k8s.pod.name": "pod-0"}, value: 1.0},
	}))
	mp.flushAll(clock.Now()) // seq0: birth

	clock.Set(clock.Now().Add(time.Second))
	mustConsume(t, mp, gaugeMetrics("cpu_usage", nil, []point{
		{attrs: map[string]string{"service.name": "svc", "k8s.pod.name": "pod-0"}, value: 3.0},
	}))
	mp.flushAll(clock.Now()) // seq1: real residual

	frames := sink.Frames()
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	f := frames[1]
	if f.FrameType != wire.FrameTypeResidual {
		t.Fatalf("frames[1].FrameType = %d, want RESIDUAL (%d)", f.FrameType, wire.FrameTypeResidual)
	}
	if f.M != 2000 {
		t.Fatalf("frames[1].M = %d, want 2000", f.M)
	}
	if f.D != 6 {
		t.Fatalf("frames[1].D = %d, want 6", f.D)
	}
	if f.Energy <= 0 {
		t.Fatalf("frames[1].Energy = %v, want > 0", f.Energy)
	}
}

// --- (c) keyframe cadence + dict_root + golden every Nth ---------------

func TestKeyframeCadenceAndGoldenEvery(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.KeyframeEvery = 2
	cfg.KeyframeFullDictEvery = 2
	cfg.GoldenKeyframeEvery = 2
	cfg.Aggregates = []string{"sum"}
	mp, sink, clock := newTestProcessor(t, cfg, time.Unix(1_700_000_000, 0))

	for i := 0; i < 5; i++ {
		mustConsume(t, mp, gaugeMetrics("cpu_usage", nil, []point{
			{attrs: map[string]string{"service.name": "svc", "k8s.pod.name": "pod-0"}, value: float64(i) + 1.0},
		}))
		mp.flushAll(clock.Now())
		clock.Set(clock.Now().Add(time.Second))
	}

	frames := sink.Frames()
	if len(frames) != 5 {
		t.Fatalf("got %d frames, want 5", len(frames))
	}
	wantRoot := wire.ComputeDictRoot([]uint64{sketch.SeriesID([]byte("cpu_usage|service.name=svc|agg=sum"))})
	// seq 0, 2, 4 are KEYFRAMEs (keyframe_every=2); of those, keyframe
	// index 0 and 2 are golden (golden_every=2), index 1 (seq 2) is not.
	wantGolden := map[uint32]bool{0: true, 2: false, 4: true}
	for seq, golden := range wantGolden {
		f := frames[seq]
		if f.FrameType != wire.FrameTypeKeyframe {
			t.Fatalf("frames[%d].FrameType = %d, want KEYFRAME", seq, f.FrameType)
		}
		if gotGolden := f.Flags&wire.FlagKDelta == 0; gotGolden != golden {
			t.Fatalf("frames[%d] golden = %v, want %v (flags=%#x)", seq, gotGolden, golden, f.Flags)
		}
		if f.DictRoot != wantRoot {
			t.Fatalf("frames[%d].DictRoot = %#x, want %#x", seq, f.DictRoot, wantRoot)
		}
	}
	for _, seq := range []uint32{1, 3} {
		if frames[seq].FrameType != wire.FrameTypeResidual {
			t.Fatalf("frames[%d].FrameType = %d, want RESIDUAL", seq, frames[seq].FrameType)
		}
	}
}

// --- (d) churn: births produce dict deltas with no residual spike; ------
// --- tombstones appear after series_ttl; zero lifecycle false positives

func TestChurnInstanceLifecycle(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.SeriesTTL = 3 * time.Second
	t0 := time.Unix(1_700_000_000, 0)
	mp, sink, clock := newTestProcessor(t, cfg, t0)

	feed := func(service string, pods []string, value float64) {
		pts := make([]point, len(pods))
		for i, pod := range pods {
			pts[i] = point{attrs: map[string]string{"service.name": service, "k8s.pod.name": pod}, value: value}
		}
		mustConsume(t, mp, gaugeMetrics("requests_total", nil, pts))
	}

	// Window 0: "checkout" is born with 100 instances; "idle" is born
	// with a single instance that will later go silent.
	feed("checkout", podNames("pod", 0, 100), 1.0)
	feed("idle", []string{"idle-pod-0"}, 1.0)
	mp.flushAll(clock.Now())

	// Window 1 (+1s): steady state, same 100 checkout pods, unchanged
	// values; "idle" stays silent.
	clock.Set(t0.Add(time.Second))
	feed("checkout", podNames("pod", 0, 100), 1.0)
	mp.flushAll(clock.Now())

	// Window 2 (+2s): kill 30 checkout pods, birth 30 new ones (pure
	// instance churn); "idle" still silent (2s < series_ttl of 3s).
	clock.Set(t0.Add(2 * time.Second))
	churned := append(podNames("pod", 30, 100), podNames("pod", 100, 130)...)
	feed("checkout", churned, 1.0)
	mp.flushAll(clock.Now())

	// Window 3 (+6s): well past series_ttl since idle's last observation
	// (window 0); checkout keeps reporting normally throughout.
	clock.Set(t0.Add(6 * time.Second))
	feed("checkout", churned, 1.0)
	mp.flushAll(clock.Now())

	frames := sink.Frames()
	if len(frames) != 4 {
		t.Fatalf("got %d frames, want 4", len(frames))
	}

	// Window 0 is this pipeline's first ever window: every series
	// observed is a birth, so it must carry exactly zero sketch energy
	// (ADR-008: a series' first sample never enters the residual path).
	if frames[0].Energy != 0 {
		t.Fatalf("frames[0].Energy = %v, want exactly 0 (birth-only window)", frames[0].Energy)
	}

	// Windows 1 and 2: no dict activity at all (pod churn never touches
	// the dictionary) and exactly zero energy, since checkout's folded
	// sum/count/max are unchanged by which specific 100 pods contribute.
	for _, seq := range []uint32{1, 2} {
		f := frames[seq]
		if len(f.DictDeltas) != 0 {
			t.Fatalf("frames[%d].DictDeltas = %+v, want empty: pod churn must not touch the dictionary (ADR-008)", seq, f.DictDeltas)
		}
		if f.Energy != 0 {
			t.Fatalf("frames[%d].Energy = %v, want exactly 0: checkout's folded aggregates are unchanged by pod churn", seq, f.Energy)
		}
	}

	// Window 3 must tombstone exactly idle's 3 logical series (sum/count/
	// max) after series_ttl, and nothing else.
	f3 := frames[3]
	idleIDs := map[uint64]bool{
		sketch.SeriesID([]byte("requests_total|service.name=idle|agg=sum")):   true,
		sketch.SeriesID([]byte("requests_total|service.name=idle|agg=count")): true,
		sketch.SeriesID([]byte("requests_total|service.name=idle|agg=max")):   true,
	}
	if len(f3.DictDeltas) != len(idleIDs) {
		t.Fatalf("frames[3].DictDeltas has %d entries, want %d (idle's sum/count/max tombstones)", len(f3.DictDeltas), len(idleIDs))
	}
	for _, dd := range f3.DictDeltas {
		if !dd.IsTombstone() {
			t.Fatalf("frames[3] dict_delta %+v is not a tombstone", dd)
		}
		if !idleIDs[dd.ID] {
			t.Fatalf("frames[3] tombstoned unexpected id %#x", dd.ID)
		}
	}

	// Lifecycle_false_positives == 0: collect every birth id seen across
	// non-golden frames (a golden keyframe's FullDict lists every active
	// id unconditionally, so it is not itself "a birth event") and assert
	// none repeats.
	seenBirths := map[uint64]bool{}
	falsePositives := 0
	for _, f := range frames {
		golden := f.FrameType == wire.FrameTypeKeyframe && f.Flags&wire.FlagKDelta == 0
		if golden {
			continue
		}
		for _, dd := range f.DictDeltas {
			if dd.IsTombstone() {
				continue
			}
			if seenBirths[dd.ID] {
				falsePositives++
			}
			seenBirths[dd.ID] = true
		}
	}
	if falsePositives != 0 {
		t.Fatalf("lifecycle_false_positives = %d, want 0", falsePositives)
	}
}

// --- (e) hot instance: agg=max carries a large residual, agg=sum -------
// --- moves <5%, and a snapshot blob captures the hot instance ----------

func TestHotInstanceResidualAndSnapshot(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.Aggregates = []string{"sum", "max"}
	cfg.Snapshot = SnapshotConfig{Enabled: true, ResidualThreshold: 1.5, MaxSeriesPerFlush: 10, BlobTTL: time.Hour}
	t0 := time.Unix(1_700_000_000, 0)
	mp, sink, clock := newTestProcessor(t, cfg, t0)

	residuals := map[string]float64{}
	mp.onObserve = func(_ pipelineKey, _, agg string, _ uint64, _, residual float64, isNew bool) {
		if !isNew {
			residuals[agg] = residual
		}
	}

	pods := podNames("pod", 0, 100)
	feedValues := func(hotIdx int, hotValue float64) {
		pts := make([]point, len(pods))
		for i, pod := range pods {
			v := 1.0
			if i == hotIdx {
				v = hotValue
			}
			pts[i] = point{attrs: map[string]string{"service.name": "svc", "k8s.pod.name": pod}, value: v}
		}
		mustConsume(t, mp, gaugeMetrics("cpu_usage", nil, pts))
	}

	feedValues(-1, 0) // window 0: birth, all 100 pods at 1.0
	mp.flushAll(clock.Now())

	clock.Set(t0.Add(time.Second))
	feedValues(-1, 0) // window 1: steady state, establishes a flat (zero-residual) baseline
	mp.flushAll(clock.Now())

	const hotIdx = 42
	clock.Set(t0.Add(2 * time.Second))
	feedValues(hotIdx, 5.0) // window 2: pod-42 jumps from 1.0 to 5.0
	mp.flushAll(clock.Now())

	sumResidual, maxResidual := residuals["sum"], residuals["max"]
	if got, limit := math.Abs(sumResidual), 0.05*100; got >= limit {
		t.Fatalf("agg=sum |residual| = %v, want < %v (5%% of baseline sum 100)", got, limit)
	}
	if math.Abs(maxResidual) < 1 {
		t.Fatalf("agg=max residual = %v, want a large residual (baseline 1.0, hot instance now 5.0)", maxResidual)
	}

	frames := sink.Frames()
	last := frames[len(frames)-1]
	if len(last.SnapshotBlob) == 0 {
		t.Fatal("expected a snapshot blob on the hot-instance window's frame, got none")
	}
	entries, err := wire.DecodeSnapshot(last.SnapshotBlob, last.Codec)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	hotInstanceID := sketch.SeriesID([]byte(fmt.Sprintf("k8s.pod.name=%s", pods[hotIdx])))
	found := false
	for _, e := range entries {
		if e.ID == hotInstanceID && e.Value == 5.0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("snapshot blob does not contain the hot instance (%s, value 5.0); entries: %+v", pods[hotIdx], entries)
	}
}

// --- (f) churn breaker: birth-rate trip degrades to aggregate-only, -----
// --- and fires an alert -------------------------------------------------

func TestChurnBreakerTripDegradesAndAlerts(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.Aggregates = []string{"sum"}
	cfg.Churn = ChurnConfig{MaxBirthsPerLogicalPerMin: 3}
	t0 := time.Unix(1_700_000_000, 0)
	mp, _, clock := newTestProcessor(t, cfg, t0)

	feedNewServices := func(names ...string) {
		pts := make([]point, len(names))
		for i, name := range names {
			pts[i] = point{attrs: map[string]string{"service.name": name, "k8s.pod.name": "pod-0"}, value: 1.0}
		}
		mustConsume(t, mp, gaugeMetrics("requests_total", nil, pts))
	}

	// One flush registers 4 distinct new logical series against a
	// max-3-births/minute breaker: the crash-loop simulation.
	feedNewServices("svc-a", "svc-b", "svc-c", "svc-d")
	mp.flushAll(clock.Now())

	mp.mu.Lock()
	pl := mp.pipelines[pipelineKey{shard: 0, view: 0}]
	mp.mu.Unlock()
	if pl == nil {
		t.Fatal("expected a pipeline to have been created")
	}
	if !pl.tracker.BreakerTripped() {
		t.Fatal("expected the Tracker's breaker to have tripped")
	}
	if !pl.degraded {
		t.Fatal("expected the pipeline to be marked degraded after the breaker trips")
	}
	if got := len(pl.tracker.ActiveIDs()); got != cfg.Churn.MaxBirthsPerLogicalPerMin {
		t.Fatalf("active dictionary ids = %d, want exactly %d (one refused birth)", got, cfg.Churn.MaxBirthsPerLogicalPerMin)
	}
	alerts := mp.Alerts()
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want exactly 1", len(alerts))
	}
	if alerts[0].Shard != 0 || alerts[0].View != 0 {
		t.Fatalf("alert = %+v, want shard=0 view=0", alerts[0])
	}

	// While degraded, ring-buffer instance recording must stop entirely
	// ("aggregate-only buffering"), even for a brand new group.
	clock.Set(t0.Add(time.Second))
	feedNewServices("svc-e")
	group := groupKey("requests_total", []kv{{key: "service.name", val: "svc-e"}})
	if n := pl.rings.instanceCount(group); n != 0 {
		t.Fatalf("rings.instanceCount(%q) = %d while degraded, want 0 (no instances recorded)", group, n)
	}

	// A second flush must not fire a second alert for the same trip.
	mp.flushAll(clock.Now())
	if got := len(mp.Alerts()); got != 1 {
		t.Fatalf("got %d alerts after a second flush, want still 1 (no duplicate alert for an already-tripped breaker)", got)
	}
}

// --- (g) FALLBACK frames are emitted when residuals force a storm ------
// --- energy spike --------------------------------------------------------

func TestFallbackOnStormTrigger(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.Aggregates = []string{"sum"}
	cfg.Storm = StormConfig{EnergyMultiplier: 2, FallbackTopK: 5}
	services := []string{"svc-a", "svc-b", "svc-c", "svc-d", "svc-e"}
	t0 := time.Unix(1_700_000_000, 0)
	mp, sink, clock := newTestProcessor(t, cfg, t0)

	feed := func(values map[string]float64) {
		pts := make([]point, 0, len(services))
		for _, svc := range services {
			pts = append(pts, point{attrs: map[string]string{"service.name": svc, "k8s.pod.name": "pod-0"}, value: values[svc]})
		}
		mustConsume(t, mp, gaugeMetrics("latency_ms", nil, pts))
	}
	flat := func(v float64) map[string]float64 {
		out := make(map[string]float64, len(services))
		for _, svc := range services {
			out[svc] = v
		}
		return out
	}

	feed(flat(10.0)) // window 0: birth
	mp.flushAll(clock.Now())

	clock.Set(t0.Add(time.Second))
	drift := flat(10.0)
	drift["svc-a"] = 10.1 // small, consistent drift: establishes a small storm-energy baseline
	feed(drift)
	mp.flushAll(clock.Now())

	clock.Set(t0.Add(2 * time.Second))
	spike := flat(10.0)
	spike["svc-a"] = 10.1
	spike["svc-c"] = 5000.0 // huge residual: energy should dwarf the established baseline
	feed(spike)
	mp.flushAll(clock.Now())

	frames := sink.Frames()
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	f := frames[2]
	if f.FrameType != wire.FrameTypeFallback {
		t.Fatalf("frames[2].FrameType = %d, want FALLBACK (%d); energy=%v", f.FrameType, wire.FrameTypeFallback, f.Energy)
	}
	result, err := wire.DecodeFallback(f.Payload)
	if err != nil {
		t.Fatalf("DecodeFallback: %v", err)
	}
	if result.TotalCount == 0 {
		t.Fatal("FALLBACK payload TotalCount = 0, want > 0")
	}
	hotID := sketch.SeriesID([]byte("latency_ms|service.name=svc-c|agg=sum"))
	if _, ok := result.Values[hotID]; !ok {
		t.Fatalf("FALLBACK payload does not include the spiking series (svc-c); values: %+v", result.Values)
	}
}

// --- (h) identical (tenant_key, shard, epoch, view) -> identical seed --

func TestSeedDeterminism(t *testing.T) {
	tenantKey := []byte("tenant-key-material")
	s1 := deriveSeed(tenantKey, 7, 3, 1)
	s2 := deriveSeed(tenantKey, 7, 3, 1)
	if s1 != s2 {
		t.Fatalf("deriveSeed not deterministic: %d != %d", s1, s2)
	}

	variants := map[string]uint64{
		"different shard":      deriveSeed(tenantKey, 8, 3, 1),
		"different epoch":      deriveSeed(tenantKey, 7, 4, 1),
		"different view":       deriveSeed(tenantKey, 7, 3, 2),
		"different tenant key": deriveSeed([]byte("other-tenant-key"), 7, 3, 1),
	}
	for name, v := range variants {
		if v == s1 {
			t.Fatalf("%s: seed unexpectedly matched the base seed (%d)", name, s1)
		}
	}
}

// --- (i) multi-view: 2 declared views tag frames view_id 0/1 and keep --
// --- fully independent state --------------------------------------------

func TestMultiViewIndependence(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.Aggregates = []string{"sum"}
	cfg.Views = []ViewConfig{
		{Name: "by_service", Labels: []string{"service.name"}},
		{Name: "by_region", Labels: []string{"cloud.region"}},
	}
	mp, sink, clock := newTestProcessor(t, cfg, time.Unix(1_700_000_000, 0))

	mustConsume(t, mp, gaugeMetrics("cpu_usage", nil, []point{
		{attrs: map[string]string{"service.name": "svc-a", "cloud.region": "us-east", "k8s.pod.name": "pod-0"}, value: 1.0},
	}))
	mp.flushAll(clock.Now())

	frames := sink.Frames()
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (one per declared view)", len(frames))
	}
	byView := map[uint16]*wire.Frame{}
	for _, f := range frames {
		byView[f.ViewID] = f
	}
	f0, ok0 := byView[0]
	f1, ok1 := byView[1]
	if !ok0 || !ok1 {
		t.Fatalf("expected frames tagged view_id 0 and 1, got %+v", byView)
	}
	if f0.DictRoot == f1.DictRoot {
		t.Fatalf("view 0 and view 1 produced the same dict_root (%#x); expected independent identities", f0.DictRoot)
	}

	mp.mu.Lock()
	pl0 := mp.pipelines[pipelineKey{shard: 0, view: 0}]
	pl1 := mp.pipelines[pipelineKey{shard: 0, view: 1}]
	mp.mu.Unlock()
	if pl0 == nil || pl1 == nil || pl0 == pl1 {
		t.Fatalf("expected two distinct pipeline objects, got pl0=%p pl1=%p", pl0, pl1)
	}
	if pl0.acc.Params().Seed == pl1.acc.Params().Seed {
		t.Fatalf("view 0 and view 1 derived the same Accumulator seed (%d); seeds must be view-scoped (ADR-012)", pl0.acc.Params().Seed)
	}
}

// --- supplementary coverage: shadow mode and lifecycle -----------------

func TestShadowModeKeepsSketchedDatapointsDownstream(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.Shadow = true
	mp, _, _ := newTestProcessor(t, cfg, time.Unix(1_700_000_000, 0))

	md := gaugeMetrics("cpu_usage", nil, []point{
		{attrs: map[string]string{"service.name": "svc", "k8s.pod.name": "pod-0"}, value: 1.0},
	})
	out := mustConsume(t, mp, md)
	if out.DataPointCount() != 1 {
		t.Fatalf("shadow=true: got %d datapoints downstream, want 1 (sketched data must still pass through)", out.DataPointCount())
	}
}

func TestNonShadowModeRemovesSketchedDatapoints(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.Shadow = false
	mp, _, _ := newTestProcessor(t, cfg, time.Unix(1_700_000_000, 0))

	md := gaugeMetrics("cpu_usage", nil, []point{
		{attrs: map[string]string{"service.name": "svc", "k8s.pod.name": "pod-0"}, value: 1.0},
	})
	out := mustConsume(t, mp, md)
	if out.DataPointCount() != 0 {
		t.Fatalf("shadow=false: got %d datapoints downstream, want 0 (sketched data now represented by frames only)", out.DataPointCount())
	}
}

func TestDrilldownHintTagsPassthroughDatapoints(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.DrilldownHint = true
	mp, _, _ := newTestProcessor(t, cfg, time.Unix(1_700_000_000, 0))

	md := gaugeMetrics("billing_invoice_total", map[string]string{"service.name": "billing"},
		[]point{{attrs: map[string]string{"k8s.pod.name": "pod-0"}, value: 42.5}})
	out := mustConsume(t, mp, md)

	dp := out.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints().At(0)
	got, ok := dp.Attributes().Get("palimpsest.logical_series")
	if !ok {
		t.Fatal("expected a palimpsest.logical_series attribute on the exact-tier datapoint")
	}
	want := "billing_invoice_total|service.name=billing"
	if got.AsString() != want {
		t.Fatalf("palimpsest.logical_series = %q, want %q", got.AsString(), want)
	}
}

func TestStartShutdownLifecycle(t *testing.T) {
	cfg := baseTestConfig(t)
	cfg.FlushInterval = 20 * time.Millisecond
	mp, err := newMetricsProcessor(cfg, zap.NewNop(), &memoryFrameSink{})
	if err != nil {
		t.Fatalf("newMetricsProcessor: %v", err)
	}

	if err := mp.start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("start: %v", err)
	}
	mustConsume(t, mp, gaugeMetrics("cpu_usage", nil, []point{
		{attrs: map[string]string{"service.name": "svc", "k8s.pod.name": "pod-0"}, value: 1.0},
	}))
	time.Sleep(60 * time.Millisecond) // let the real ticker fire at least once

	if err := mp.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// shutdown's wg.Wait() already proves the flush-loop goroutine (and
	// pull-server goroutine, if any) exited cleanly; a second shutdown
	// call must also be safe (component.Component's documented contract).
	if err := mp.shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

// --- guardrail: ring buffer bounded memory (global + per-series cap) --

func TestRingBufferBounds(t *testing.T) {
	t.Run("per-group instance cap", func(t *testing.T) {
		b := newInstanceBuffers(time.Minute, 2, 0)
		b.push("group", "inst-0", 1, 1000, 1.0)
		b.push("group", "inst-1", 2, 1000, 1.0)
		b.push("group", "inst-2", 3, 1000, 1.0) // exceeds the per-group cap of 2
		if got := b.instanceCount("group"); got != 2 {
			t.Fatalf("instanceCount = %d, want 2 (max_instances_per_logical enforced)", got)
		}
	})

	t.Run("approximate global byte cap", func(t *testing.T) {
		b := newInstanceBuffers(time.Minute, 1000, snapshotEntryWireBytes) // room for exactly 1 sample
		b.push("group", "inst-0", 1, 1000, 1.0)
		b.push("group", "inst-1", 2, 1000, 1.0) // a new instance once the byte budget is spent
		if got := b.instanceCount("group"); got != 1 {
			t.Fatalf("instanceCount = %d, want 1 (max_total_bytes enforced)", got)
		}
	})

	t.Run("drain garbage-collects expired instances", func(t *testing.T) {
		b := newInstanceBuffers(time.Minute, 1000, 0)
		b.push("group", "inst-0", 1, 1000, 1.0) // a timestamp this old is always expired relative to real time.Now()
		if got := b.instanceCount("group"); got != 1 {
			t.Fatalf("instanceCount = %d, want 1 before drain", got)
		}
		b.drain(0)
		if got := b.instanceCount("group"); got != 0 {
			t.Fatalf("instanceCount = %d, want 0 after drain: expired instances must not hang in memory", got)
		}
	})
}
