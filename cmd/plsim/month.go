/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"

	"github.com/passiveintent/Palimpsest/pkg/recover"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// The --month benchmark answers the commercial question the cost table in
// docs/COSTS.md can only model: over a simulated month of realistic
// Kubernetes-shaped telemetry — seasonal baselines, per-instance read
// noise, pod churn that never touches the sketch (ADR-008) and logical
// churn that does, plus three scripted incidents that exercise all the
// exact-data escape hatches — what actually goes over the wire?
//
// Two ledgers are kept, byte-exact on both sides:
//
//   - Palimpsest: every frame the real encode pipeline (Tracker + Hold +
//     Accumulator + keyframe/KDELTA/RESIDUAL/FALLBACK + dashcam
//     snapshots, cmd/plsim/encoder.go with ADR-004 storm fallback armed)
//     would ship, measured with wire.EncodedSize.
//   - Baseline: the same fleet as raw per-instance Prometheus
//     remote-write, one sample per series per flush window, protobuf
//     (prompb WriteRequest) encoded exactly and compressed with real
//     snappy (the remote-write 1.0 content encoding) and zstd (the
//     generous variant), measured on sampled windows and integrated.
//
// The three incidents are deterministic and placed at fixed fractions of
// the run (days 7/14/21 of a default 30-day run):
//
//  1. "spike"  — +50 on 5 series for 15 min (the FISTA recovery path,
//     and ADR-003's keyframe re-basing of a sustained shift);
//  2. "leak"   — one series ramps +20 over 6 h (deliberately inside the
//     docs/DEADZONE.md dead zone: per-window residual ~0.06, keyframe
//     drift ~0.6/keyframe, both far below every threshold);
//  3. "azdown" — +100 on 30% of the fleet for 30 min (shatters the
//     sparsity prior; arms ADR-004's storm fallback).
//
// Detection is reported per incident for substrate (a) (FISTA solves over
// the merged, dequantized frames at palimpsestd production settings) and
// substrate (c) (keyframe-to-keyframe drift vs palimpsestd's default
// threshold), alongside the storm/fallback behavior — including how long
// the rolling-median breaker actually keeps firing during a sustained
// storm before the storm becomes the new median.
type monthFlags struct {
	enabled bool
	out     string

	days                float64
	series              int
	instancesPerLogical int
	emitters            int

	noise            float64
	seasonalAmp      float64
	podChurnPerMin   float64
	logicalChurnPerM float64

	stormWindow     int
	stormMultiplier float64
	fallbackTopK    int

	baselineEvery int
	solveBudget   int
	maxResidual   float64
}

func registerMonthFlags(mf *monthFlags) {
	flag.BoolVar(&mf.enabled, "month", false, "run the 30-day byte-ledger benchmark (M2) instead of the frame-generating simulation, then exit; uses --m/--d/--bits/--seed/--shard-id/--tenant-key/--interval/--keyframe-every/--golden-every/--series-ttl/--snapshot-threshold/--ring-window/--codec and ignores the workload flags")
	flag.StringVar(&mf.out, "month-out", "./month", "directory to write month.csv, month.svg and month-summary.md")
	flag.Float64Var(&mf.days, "month-days", 30, "simulated days")
	flag.IntVar(&mf.series, "month-series", 10000, "raw per-instance series the baseline ships (folded per ADR-008 into series/instances-per-logical groups x 3 sketched aggregates)")
	flag.IntVar(&mf.instancesPerLogical, "month-instances-per-logical", 5, "pods per logical group")
	flag.IntVar(&mf.emitters, "month-emitters", 8, "agent processes; groups are partitioned across them and each ships its own frames and its own remote-write requests")
	flag.Float64Var(&mf.noise, "month-noise", 0.2, "per-instance Gaussian read-noise sigma, absolute units (per-instance baselines are 50-150, so 0.2 is ~0.2% jitter; below ~0.05 the fleet is quiet enough that the storm breaker hair-triggers on any lone spike — see docs/DEADZONE.md's σ=0 column)")
	flag.Float64Var(&mf.seasonalAmp, "month-seasonal-amp", 0.25, "daily seasonal amplitude as a fraction of each group's baseline")
	flag.Float64Var(&mf.podChurnPerMin, "month-pod-churn", 7, "pod replacements per minute fleet-wide (touches baseline labels and dashcam only — never the sketch, per ADR-008)")
	flag.Float64Var(&mf.logicalChurnPerM, "month-logical-churn", 2, "logical group replacements per minute fleet-wide (drives dict deltas and golden-keyframe re-announcement, ADR-017)")
	flag.IntVar(&mf.stormWindow, "month-storm-window", 30, "StormDetector rolling-median window (csresidual stormHistoryWindow)")
	flag.Float64Var(&mf.stormMultiplier, "month-storm-multiplier", 25, "StormDetector energy multiplier (csresidual factory default)")
	flag.IntVar(&mf.fallbackTopK, "month-fallback-topk", 100, "FALLBACK heavy-hitter count (csresidual factory default)")
	flag.IntVar(&mf.baselineEvery, "month-baseline-every", 360, "windows between exact baseline remote-write encodings (sizes are integrated between samples)")
	flag.IntVar(&mf.solveBudget, "month-solve-budget", 240, "maximum recovery solves per incident span")
	flag.Float64Var(&mf.maxResidual, "month-max-residual", 5.0, "palimpsestd's deviation-event residual gate (its --max-residual default)")
}

// monthIncident is one scripted anomaly. Kind is "spike" (flat +Magnitude
// on Groups groups), "ramp" (linear climb to +Magnitude by the end, one
// group), or "mass" (flat +Magnitude on GroupFrac of live groups).
type monthIncident struct {
	Name      string
	Kind      string
	StartWin  uint32
	DurWin    uint32
	Magnitude float64
	Groups    int
	GroupFrac float64
}

func (inc *monthIncident) active(w uint32) bool {
	return w >= inc.StartWin && w < inc.StartWin+inc.DurWin
}

// defaultMonthIncidents anchors the three incidents at days 7.5 / 14.33 /
// 21.58 of a 30-day run, scaled proportionally for other --month-days.
// Onsets are nudged to mid-keyframe-cycle: the fraction anchors land on
// multiples of the keyframe cadence, and an incident starting exactly ON
// a keyframe window is instantly re-based away by the Hold predictor
// (ADR-003) before it ever produces a residual — a degenerate alignment
// no real outage exhibits.
func defaultMonthIncidents(total uint32, keyframeEvery int) []monthIncident {
	at := func(frac float64) uint32 {
		s := uint32(float64(total) * frac)
		if keyframeEvery > 1 {
			s = s - s%uint32(keyframeEvery) + uint32(keyframeEvery)/2
		}
		return s
	}
	dur := func(frac float64) uint32 {
		d := uint32(float64(total) * frac)
		if d < 1 {
			d = 1
		}
		return d
	}
	return []monthIncident{
		{Name: "spike", Kind: "spike", StartWin: at(7.5 / 30), DurWin: dur(0.25 / 720), Magnitude: 50, Groups: 5},
		{Name: "leak", Kind: "ramp", StartWin: at((14 + 8.0/24) / 30), DurWin: dur(6.0 / 720), Magnitude: 20, Groups: 1},
		{Name: "azdown", Kind: "mass", StartWin: at((21 + 14.0/24) / 30), DurWin: dur(0.5 / 720), Magnitude: 100, GroupFrac: 0.3},
	}
}

// monthConfig is a fully resolved benchmark run.
type monthConfig struct {
	M, D, Bits int
	Seed       int64
	ShardID    uint64
	TenantKey  []byte
	Codec      wire.Codec

	Interval      time.Duration
	TotalWindows  uint32
	WindowsPerDay int

	Groups              int // logical groups; sketched series = 3x this
	InstancesPerLogical int
	Emitters            int

	KeyframeEvery, GoldenEvery int
	SeriesTTL                  time.Duration
	SnapshotThreshold          float64
	RingWindow                 time.Duration

	Noise              float64
	SeasonalAmp        float64
	PodChurnPerMin     float64
	LogicalChurnPerMin float64

	StormWindow     int
	StormMultiplier float64
	FallbackTopK    int

	BaselineEvery int
	SolveBudget   int
	MaxResidual   float64
	DriftThresh   float64 // palimpsestd substrate (c) default

	Incidents []monthIncident
}

// spikeTargetGroups/rampTargetGroup pin which global group indices the
// scripted incidents hit; those groups are excluded from logical churn so
// their ground truth stays unambiguous (mirroring World.ReserveForAnomaly).
const rampTargetOffset = 5 // spike takes idx 0..Groups-1, ramp takes the next

// runMonth is --month's entry point.
func runMonth(f flags) error {
	if f.dz.enabled {
		return fmt.Errorf("--month and --deadzone are mutually exclusive")
	}
	codec, err := resolveCodec(f.codec)
	if err != nil {
		return err
	}
	mf := f.month
	if mf.series < mf.instancesPerLogical || mf.instancesPerLogical < 1 || mf.emitters < 1 {
		return fmt.Errorf("--month-series/--month-instances-per-logical/--month-emitters out of range")
	}
	windowsPerDay := int(24 * time.Hour / f.interval)
	total := uint32(mf.days * float64(windowsPerDay))
	if total < 10 {
		return fmt.Errorf("--month-days %v at --interval %v is only %d windows; need >= 10", mf.days, f.interval, total)
	}

	cfg := monthConfig{
		M: f.m, D: f.d, Bits: f.bits, Seed: f.seed, ShardID: f.shardID,
		TenantKey: []byte(f.tenantKey), Codec: codec,
		Interval: f.interval, TotalWindows: total, WindowsPerDay: windowsPerDay,
		Groups:              mf.series / mf.instancesPerLogical,
		InstancesPerLogical: mf.instancesPerLogical,
		Emitters:            mf.emitters,
		KeyframeEvery:       f.keyframeEvery, GoldenEvery: f.goldenEvery,
		SeriesTTL:         f.seriesTTL,
		SnapshotThreshold: f.snapshotThreshold, RingWindow: f.ringWindow,
		Noise: mf.noise, SeasonalAmp: mf.seasonalAmp,
		PodChurnPerMin: mf.podChurnPerMin, LogicalChurnPerMin: mf.logicalChurnPerM,
		StormWindow: mf.stormWindow, StormMultiplier: mf.stormMultiplier, FallbackTopK: mf.fallbackTopK,
		BaselineEvery: mf.baselineEvery, SolveBudget: mf.solveBudget,
		MaxResidual: mf.maxResidual, DriftThresh: 10.0,
		Incidents: defaultMonthIncidents(total, f.keyframeEvery),
	}

	log.Printf("plsim: month benchmark started (days=%g windows=%d groups=%d x %d pods = %d raw series, %d sketched; emitters=%d m=%d codec=%s)",
		mf.days, total, cfg.Groups, cfg.InstancesPerLogical, cfg.Groups*cfg.InstancesPerLogical, cfg.Groups*3, cfg.Emitters, cfg.M, f.codec)

	res, err := runMonthBench(cfg)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(mf.out, 0o755); err != nil {
		return fmt.Errorf("creating --month-out %q: %w", mf.out, err)
	}
	csvPath := filepath.Join(mf.out, "month.csv")
	if err := os.WriteFile(csvPath, []byte(monthCSV(res)), 0o644); err != nil {
		return err
	}
	svgPath := filepath.Join(mf.out, "month.svg")
	if err := os.WriteFile(svgPath, []byte(renderMonthSVG(cfg, res)), 0o644); err != nil {
		return err
	}
	mdPath := filepath.Join(mf.out, "month-summary.md")
	if err := os.WriteFile(mdPath, []byte(monthSummaryMD(cfg, res)), 0o644); err != nil {
		return err
	}
	for _, line := range monthHeadlines(cfg, res) {
		log.Printf("plsim: %s", line)
	}
	log.Printf("plsim: month benchmark complete: %s, %s, %s", csvPath, svgPath, mdPath)
	return nil
}

// ---------------------------------------------------------------------------
// World: groups, pods, seasonality, churn.

type monthGroup struct {
	idx         int
	metric      string
	deployment  string
	namespace   string
	names       [3]string // agg=sum,count,max logical series names
	pods        []string
	baseline    float64
	phase       float64
	reserved    bool // spike/ramp incident target: excluded from mass overlay and churn
	churnExempt bool // kept alive for measurement (mass drift rep); still mass-affected
}

var monthMetrics = [...]string{
	"http_requests_total",
	"http_request_duration_seconds_sum",
	"container_memory_working_set_bytes",
	"container_cpu_usage_seconds_total",
	"process_open_fds",
	"go_goroutines",
}

var monthNamespaces = [...]string{"prod", "payments", "checkout", "search", "infra"}

func newMonthGroup(idx int, rng *rand.Rand, cfg *monthConfig) *monthGroup {
	g := &monthGroup{
		idx:        idx,
		metric:     monthMetrics[idx%len(monthMetrics)],
		deployment: fmt.Sprintf("svc-%04d", idx%997),
		namespace:  monthNamespaces[idx%len(monthNamespaces)],
		baseline:   50 + rng.Float64()*100, // per-instance steady value
		phase:      rng.Float64(),
	}
	for i, agg := range aggregates {
		g.names[i] = fmt.Sprintf("%s|deployment=%s,namespace=%s|agg=%s", g.metric, g.deployment, g.namespace, agg)
	}
	g.pods = make([]string, cfg.InstancesPerLogical)
	for i := range g.pods {
		g.pods[i] = monthPodName(g.deployment, rng)
	}
	return g
}

func monthPodName(deployment string, rng *rand.Rand) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 0, len(deployment)+16)
	b = append(b, deployment...)
	b = append(b, '-')
	for i := 0; i < 9; i++ {
		b = append(b, hex[rng.Intn(16)])
	}
	b = append(b, '-')
	for i := 0; i < 5; i++ {
		b = append(b, hex[rng.Intn(16)])
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Ledger.

// monthDay is one simulated day's byte accounting, summed across emitters.
type monthDay struct {
	BaseRaw, BaseSnappy, BaseZstd            int64
	PLTotal                                  int64
	PLGolden, PLKDelta, PLResidual, PLFallbk int64
	PLSnapshot                               int64 // subset of PLTotal
	FallbackWindows                          int64
}

// monthResult is everything the reporting layer needs.
type monthResult struct {
	Days       []monthDay
	RawSeries  int
	Sketched   int
	Incidents  []monthIncidentReport
	QuietSolve monthSolveStats
}

type monthSolveStats struct {
	Solves    int
	Gated     int
	MeanResid float64
	MeanSpur  float64
}

type monthIncidentReport struct {
	Inc monthIncident

	SolveWindows  int
	SupportLagWin int64 // windows from onset to first solver support hit; -1 never
	EmitLagWin    int64 // ... to first support hit that also clears the residual gate; -1 never
	SupportRate   float64
	GatedRate     float64
	DriftLagWin   int64   // substrate (c): first keyframe whose delta > DriftThresh; -1 never
	MaxDrift      float64 // largest keyframe-to-keyframe delta seen on a target
	StormWindows  int64   // fallback frames shipped during the span (all emitters)
	SpanWindows   int64   // span length x emitters, for rate context
	FallbackSeen  int64   // affected series that made a FALLBACK top-K, summed
	FallbackTotal int64   // affected series x fallback frames, the denominator
	RingCoverage  float64 // ring-window lookback / incident duration (<= 1)
	FirstStormWin int64   // -1 if the breaker never fired during the span
	LastStormWin  int64
}

// ---------------------------------------------------------------------------
// Per-emitter simulation.

// emitterOut is one emitter's contribution, merged after the parallel run.
type emitterOut struct {
	days     []monthDay
	captures map[uint32]*solveCapture
	drift    map[string]*driftTrack // per target series name
	inc      []emitterIncidentTally
}

type solveCapture struct {
	y     []float64 // dequantized RESIDUAL payload; nil when the window shipped KEYFRAME/FALLBACK
	names []string  // this emitter's live logical series at that window
}

type driftTrack struct {
	kind     string // which incident kind this series witnesses for
	prev     float64
	havePrev bool
	firstWin int64 // first keyframe window with |delta| > threshold; -1
	maxDelta float64
}

type emitterIncidentTally struct {
	stormWindows  int64
	firstStorm    int64
	lastStorm     int64
	fallbackSeen  int64
	fallbackTotal int64
}

// runMonthBench runs every emitter in parallel and merges.
func runMonthBench(cfg monthConfig) (*monthResult, error) {
	solveWins := monthSolveWindows(cfg)

	outs := make([]*emitterOut, cfg.Emitters)
	errs := make([]error, cfg.Emitters)
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for e := 0; e < cfg.Emitters; e++ {
		wg.Add(1)
		go func(e int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outs[e], errs[e] = runMonthEmitter(cfg, e, solveWins)
		}(e)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	res := &monthResult{
		RawSeries: cfg.Groups * cfg.InstancesPerLogical,
		Sketched:  cfg.Groups * 3,
		Days:      make([]monthDay, len(outs[0].days)),
	}
	for _, o := range outs {
		for d := range o.days {
			res.Days[d] = addMonthDay(res.Days[d], o.days[d])
		}
	}

	if err := monthSolve(cfg, outs, solveWins, res); err != nil {
		return nil, err
	}
	return res, nil
}

func addMonthDay(a, b monthDay) monthDay {
	a.BaseRaw += b.BaseRaw
	a.BaseSnappy += b.BaseSnappy
	a.BaseZstd += b.BaseZstd
	a.PLTotal += b.PLTotal
	a.PLGolden += b.PLGolden
	a.PLKDelta += b.PLKDelta
	a.PLResidual += b.PLResidual
	a.PLFallbk += b.PLFallbk
	a.PLSnapshot += b.PLSnapshot
	a.FallbackWindows += b.FallbackWindows
	return a
}

// monthSolveWindows picks which windows get a decode-side recovery solve:
// up to cfg.SolveBudget evenly spaced windows per incident span (plus a
// 5-minute pad on each side), and 24 quiet windows spread across the run.
func monthSolveWindows(cfg monthConfig) map[uint32]bool {
	wins := make(map[uint32]bool)
	pad := uint32(5 * time.Minute / cfg.Interval)
	for _, inc := range cfg.Incidents {
		start := inc.StartWin - min(inc.StartWin, pad)
		end := inc.StartWin + inc.DurWin + pad
		if end > cfg.TotalWindows {
			end = cfg.TotalWindows
		}
		span := end - start
		step := uint32(1)
		if int(span) > cfg.SolveBudget {
			step = span / uint32(cfg.SolveBudget)
		}
		for w := start; w < end; w += step {
			wins[w] = true
		}
	}
	for i := 0; i < 24; i++ {
		w := uint32((float64(i) + 0.5) / 24 * float64(cfg.TotalWindows))
		if !insideAnyIncident(cfg, w, pad) {
			wins[w] = true
		}
	}
	return wins
}

func insideAnyIncident(cfg monthConfig, w, pad uint32) bool {
	for _, inc := range cfg.Incidents {
		if w+pad >= inc.StartWin && w < inc.StartWin+inc.DurWin+pad {
			return true
		}
	}
	return false
}

// runMonthEmitter simulates one agent process end to end.
func runMonthEmitter(cfg monthConfig, e int, solveWins map[uint32]bool) (*emitterOut, error) {
	rng := rand.New(rand.NewSource(cfg.Seed*31 + int64(e)))

	// This emitter's partition: global group idx ≡ e (mod Emitters).
	var groups []*monthGroup
	for idx := e; idx < cfg.Groups; idx += cfg.Emitters {
		g := newMonthGroup(idx, rng, &cfg)
		g.reserved = idx < rampTargetOffset+1 // spike targets + ramp target
		groups = append(groups, g)
	}
	nextLocal := (cfg.Groups + cfg.Emitters - 1 - e) / cfg.Emitters // next fresh local ordinal

	pipe := newPipeline(pipelineConfig{
		TenantKey: cfg.TenantKey, ShardID: cfg.ShardID, EmitterID: emitterID(cfg.Seed, e),
		ViewID: 0, Epoch: 0, M: cfg.M, D: cfg.D, Bits: cfg.Bits,
		KeyframeEvery: cfg.KeyframeEvery, GoldenEvery: cfg.GoldenEvery, SeriesTTL: cfg.SeriesTTL,
		SnapshotThreshold: cfg.SnapshotThreshold, RingWindow: cfg.RingWindow, Codec: cfg.Codec,
		Storm:        sketch.NewStormDetector(cfg.StormWindow, cfg.StormMultiplier),
		FallbackTopK: cfg.FallbackTopK,
	})

	out := &emitterOut{
		days:     make([]monthDay, int(cfg.TotalWindows)/cfg.WindowsPerDay+1),
		captures: make(map[uint32]*solveCapture),
		drift:    make(map[string]*driftTrack),
		inc:      make([]emitterIncidentTally, len(cfg.Incidents)),
	}
	for i := range out.inc {
		out.inc[i] = emitterIncidentTally{firstStorm: -1, lastStorm: -1}
	}
	// Substrate (c) drift tracking: every spike/ramp target this emitter
	// owns, plus one representative mass-affected series, so each
	// incident's keyframe-drift detection is measured, not guessed.
	spikeN, massTenths := 0, 0
	for _, inc := range cfg.Incidents {
		switch inc.Kind {
		case "spike":
			spikeN = inc.Groups
		case "mass":
			massTenths = int(inc.GroupFrac*10 + 0.5)
		}
	}
	haveMassRep := false
	for _, g := range groups {
		switch {
		case g.idx < spikeN:
			out.drift[g.names[0]] = &driftTrack{kind: "spike", firstWin: -1}
		case g.idx == rampTargetOffset:
			out.drift[g.names[0]] = &driftTrack{kind: "ramp", firstWin: -1}
		case !haveMassRep && massTenths > 0 && g.idx%10 < massTenths && !g.reserved:
			out.drift[g.names[0]] = &driftTrack{kind: "mass", firstWin: -1}
			g.churnExempt = true // the rep must survive churn to witness day 21
			haveMassRep = true
		}
	}

	zenc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer zenc.Close()

	simStart := time.Unix(1_700_000_000, 0)
	values := make(map[string]float64, len(groups)*3)
	churnBudgetPods, churnBudgetLogical := 0.0, 0.0
	podChurnPerWin := cfg.PodChurnPerMin / float64(cfg.Emitters) / 60 * cfg.Interval.Seconds()
	logicalChurnPerWin := cfg.LogicalChurnPerMin / float64(cfg.Emitters) / 60 * cfg.Interval.Seconds()
	var pbBuf, cmpBuf []byte

	for w := uint32(0); w < cfg.TotalWindows; w++ {
		now := simStart.Add(time.Duration(w) * cfg.Interval)
		day := int(w) / cfg.WindowsPerDay
		tDays := float64(w) * cfg.Interval.Seconds() / 86400

		// Churn. Pod churn touches labels only; logical churn retires a
		// group (its series expire via TTL) and spawns a fresh one.
		churnBudgetPods += podChurnPerWin
		for churnBudgetPods >= 1 && len(groups) > 0 {
			churnBudgetPods--
			g := groups[rng.Intn(len(groups))]
			g.pods[rng.Intn(len(g.pods))] = monthPodName(g.deployment, rng)
		}
		churnBudgetLogical += logicalChurnPerWin
		for churnBudgetLogical >= 1 {
			churnBudgetLogical--
			pick := -1
			for tries := 0; tries < 8; tries++ {
				c := rng.Intn(len(groups))
				if !groups[c].reserved && !groups[c].churnExempt {
					pick = c
					break
				}
			}
			if pick < 0 {
				break
			}
			old := groups[pick]
			for _, n := range old.names {
				delete(values, n)
			}
			ng := newMonthGroup(nextLocal*cfg.Emitters+e, rng, &cfg)
			nextLocal++
			groups[pick] = ng
		}

		// Fold every live group's instances into sum/count/max, then
		// overlay incidents (additive, like Scenario.Apply).
		for _, g := range groups {
			seasonal := g.baseline * (1 + cfg.SeasonalAmp*math.Sin(2*math.Pi*(tDays+g.phase)))
			sum, max := 0.0, math.Inf(-1)
			for range g.pods {
				v := seasonal + rng.NormFloat64()*cfg.Noise
				sum += v
				if v > max {
					max = v
				}
			}
			values[g.names[0]] = sum
			values[g.names[1]] = float64(len(g.pods))
			values[g.names[2]] = max
		}
		for i := range cfg.Incidents {
			inc := &cfg.Incidents[i]
			if !inc.active(w) {
				continue
			}
			switch inc.Kind {
			case "spike":
				for _, g := range groups {
					if g.idx < inc.Groups {
						values[g.names[0]] += inc.Magnitude
					}
				}
			case "ramp":
				for _, g := range groups {
					if g.idx == rampTargetOffset {
						values[g.names[0]] += inc.Magnitude * float64(w-inc.StartWin+1) / float64(inc.DurWin)
					}
				}
			case "mass":
				// Reserved groups (the spike/ramp targets) are excluded
				// so each incident's ground truth stays unambiguous.
				for _, g := range groups {
					if g.idx%10 < int(inc.GroupFrac*10+0.5) && !g.reserved {
						values[g.names[0]] += inc.Magnitude
					}
				}
			}
		}

		frame := pipe.flush(now, w, values)
		sz := int64(wire.EncodedSize(frame))
		d := &out.days[day]
		d.PLTotal += sz
		d.PLSnapshot += int64(len(frame.SnapshotBlob))
		switch {
		case frame.FrameType == wire.FrameTypeKeyframe && frame.Flags&wire.FlagKDelta == 0:
			d.PLGolden += sz
		case frame.FrameType == wire.FrameTypeKeyframe:
			d.PLKDelta += sz
		case frame.FrameType == wire.FrameTypeFallback:
			d.PLFallbk += sz
			d.FallbackWindows++
		default:
			d.PLResidual += sz
		}

		// Incident-scoped storm/fallback bookkeeping.
		for i := range cfg.Incidents {
			inc := &cfg.Incidents[i]
			if !inc.active(w) {
				continue
			}
			t := &out.inc[i]
			if frame.FrameType == wire.FrameTypeFallback {
				t.stormWindows++
				if t.firstStorm < 0 {
					t.firstStorm = int64(w)
				}
				t.lastStorm = int64(w)
				if inc.Kind == "mass" {
					fb, err := wire.DecodeFallback(frame.Payload)
					if err == nil {
						for _, g := range groups {
							if g.idx%10 < int(inc.GroupFrac*10+0.5) && !g.reserved {
								t.fallbackTotal++
								if _, ok := fb.Values[sketch.SeriesID([]byte(g.names[0]))]; ok {
									t.fallbackSeen++
								}
							}
						}
					}
				}
			}
		}

		// Substrate (c): track keyframe-to-keyframe deltas on targets.
		if cfg.KeyframeEvery > 0 && w%uint32(cfg.KeyframeEvery) == 0 {
			for name, dt := range out.drift {
				v, ok := values[name]
				if !ok {
					continue
				}
				if dt.havePrev {
					delta := math.Abs(v - dt.prev)
					if delta > dt.maxDelta {
						dt.maxDelta = delta
					}
					if delta > cfg.DriftThresh && dt.firstWin < 0 {
						dt.firstWin = int64(w)
					}
				}
				dt.prev, dt.havePrev = v, true
			}
		}

		// Solve-window capture: the dequantized RESIDUAL this emitter
		// shipped (or nil if this window's frame wasn't a RESIDUAL), plus
		// its live logical-series names for the decode-side dictionary.
		if solveWins[w] {
			sc := &solveCapture{}
			if frame.FrameType == wire.FrameTypeResidual {
				y, err := wire.Dequantize(frame.Payload, frame.M, frame.Bits, frame.QuantScale)
				if err != nil {
					return nil, err
				}
				sc.y = y
			}
			sc.names = make([]string, 0, len(groups)*3)
			for _, g := range groups {
				sc.names = append(sc.names, g.names[0], g.names[1], g.names[2])
			}
			out.captures[w] = sc
		}

		// Baseline ledger: on sampled windows, encode this emitter's
		// remote-write request exactly and integrate until the next sample.
		if w%uint32(cfg.BaselineEvery) == 0 {
			pbBuf = monthWriteRequest(pbBuf[:0], groups, values, now.UnixMilli())
			span := int64(cfg.BaselineEvery)
			if rem := int64(cfg.TotalWindows - w); rem < span {
				span = rem
			}
			raw := int64(len(pbBuf))
			cmpBuf = s2.EncodeSnappy(cmpBuf[:0], pbBuf)
			snap := int64(len(cmpBuf))
			cmpBuf = zenc.EncodeAll(pbBuf, cmpBuf[:0])
			zst := int64(len(cmpBuf))
			// Attribute the integrated bytes to each covered day.
			for off := int64(0); off < span; {
				dd := (int64(w) + off) / int64(cfg.WindowsPerDay)
				dayEnd := (dd + 1) * int64(cfg.WindowsPerDay)
				n := dayEnd - (int64(w) + off)
				if n > span-off {
					n = span - off
				}
				out.days[dd].BaseRaw += raw * n
				out.days[dd].BaseSnappy += snap * n
				out.days[dd].BaseZstd += zst * n
				off += n
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Baseline remote-write encoding (prompb WriteRequest, exact bytes).

// monthWriteRequest appends a prompb.WriteRequest carrying one sample per
// per-instance raw series: WriteRequest{ repeated TimeSeries timeseries=1 }
// with TimeSeries{ repeated Label labels=1, repeated Sample samples=2 },
// Label{ string name=1, value=2 }, Sample{ double value=1, int64
// timestamp=2 } — the same message shapes internal/adapters/remoteprom
// decodes, encoded here without a proto dependency.
func monthWriteRequest(dst []byte, groups []*monthGroup, values map[string]float64, tsMs int64) []byte {
	var ts, lbl, smp []byte
	for _, g := range groups {
		perPod := values[g.names[0]] / float64(len(g.pods))
		for _, pod := range g.pods {
			ts = ts[:0]
			for _, kv := range [...][2]string{
				{"__name__", g.metric},
				{"container", g.deployment},
				{"deployment", g.deployment},
				{"namespace", g.namespace},
				{"pod", pod},
			} {
				lbl = lbl[:0]
				lbl = pbString(lbl, 1, kv[0])
				lbl = pbString(lbl, 2, kv[1])
				ts = pbBytes(ts, 1, lbl)
			}
			smp = smp[:0]
			smp = pbDouble(smp, 1, perPod)
			smp = pbVarintField(smp, 2, uint64(tsMs))
			ts = pbBytes(ts, 2, smp)
			dst = pbBytes(dst, 1, ts)
		}
	}
	return dst
}

func pbKey(dst []byte, field, wt int) []byte { return pbUvarint(dst, uint64(field<<3|wt)) }

func pbUvarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func pbBytes(dst []byte, field int, b []byte) []byte {
	dst = pbKey(dst, field, 2)
	dst = pbUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func pbString(dst []byte, field int, s string) []byte {
	dst = pbKey(dst, field, 2)
	dst = pbUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func pbDouble(dst []byte, field int, v float64) []byte {
	dst = pbKey(dst, field, 1)
	bits := math.Float64bits(v)
	for i := 0; i < 8; i++ {
		dst = append(dst, byte(bits>>(8*i)))
	}
	return dst
}

func pbVarintField(dst []byte, field int, v uint64) []byte {
	dst = pbKey(dst, field, 0)
	return pbUvarint(dst, v)
}

// ---------------------------------------------------------------------------
// Decode-side recovery solves and incident reports.

// monthSolve merges the per-emitter captures window by window (sketch
// additivity), runs palimpsestd-default FISTA recovery, and fills
// res.Incidents and res.QuietSolve.
func monthSolve(cfg monthConfig, outs []*emitterOut, solveWins map[uint32]bool, res *monthResult) error {
	params := sketch.Params{M: cfg.M, D: cfg.D,
		Seed: sketch.DeriveEphemeralSeed(cfg.TenantKey, cfg.ShardID, 0, 0), Bits: cfg.Bits}

	wins := make([]uint32, 0, len(solveWins))
	for w := range solveWins {
		wins = append(wins, w)
	}
	sort.Slice(wins, func(i, j int) bool { return wins[i] < wins[j] })

	// Target IDs per incident (global group indices are deterministic).
	spikeIDs := make(map[uint64]bool)
	rampIDs := make(map[uint64]bool)
	for i := range cfg.Incidents {
		inc := &cfg.Incidents[i]
		switch inc.Kind {
		case "spike":
			for idx := 0; idx < inc.Groups; idx++ {
				g := newMonthGroupNameOnly(idx)
				spikeIDs[sketch.SeriesID([]byte(g))] = true
			}
		case "ramp":
			rampIDs[sketch.SeriesID([]byte(newMonthGroupNameOnly(rampTargetOffset)))] = true
		}
	}

	reports := make([]monthIncidentReport, len(cfg.Incidents))
	for i := range cfg.Incidents {
		reports[i] = monthIncidentReport{
			Inc: cfg.Incidents[i], SupportLagWin: -1, EmitLagWin: -1, DriftLagWin: -1,
			FirstStormWin: -1, LastStormWin: -1,
			RingCoverage: math.Min(1, cfg.RingWindow.Seconds()/(float64(cfg.Incidents[i].DurWin)*cfg.Interval.Seconds())),
		}
		reports[i].SpanWindows = int64(cfg.Incidents[i].DurWin) * int64(cfg.Emitters)
	}

	var quiet monthSolveStats
	y := make([]float64, cfg.M)
	for _, w := range wins {
		for i := range y {
			y[i] = 0
		}
		got := 0
		var names []string
		for _, o := range outs {
			sc := o.captures[w]
			if sc == nil {
				continue
			}
			names = append(names, sc.names...)
			if sc.y != nil {
				got++
				for i, v := range sc.y {
					y[i] += v
				}
			}
		}
		if got == 0 {
			continue // keyframe- or fallback-only window: nothing to solve
		}

		dict := recover.NewDictionary()
		for _, n := range names {
			dict.ApplyDelta(wire.DictDelta{ID: sketch.SeriesID([]byte(n)), Name: []byte(n)})
		}
		r, err := recover.Recover(y, dict, params, recover.Options{
			Iters: 350, Lambda: 0.05, PowerIters: 50, Threshold: 0.3,
		})
		if err != nil {
			return err
		}
		gated := r.Residual > cfg.MaxResidual

		inIncident := false
		for i := range cfg.Incidents {
			inc := &cfg.Incidents[i]
			if !inc.active(w) {
				continue
			}
			inIncident = true
			rep := &reports[i]
			rep.SolveWindows++
			if gated {
				rep.GatedRate++
			}
			targets := spikeIDs
			if inc.Kind == "ramp" {
				targets = rampIDs
			} else if inc.Kind == "mass" {
				targets = nil // mass detection is the storm path, tallied below
			}
			hit := false
			for _, id := range r.SupportIDs {
				if targets[id] {
					hit = true
					break
				}
			}
			if hit {
				rep.SupportRate++
				if rep.SupportLagWin < 0 {
					rep.SupportLagWin = int64(w) - int64(inc.StartWin)
				}
				if !gated && rep.EmitLagWin < 0 {
					rep.EmitLagWin = int64(w) - int64(inc.StartWin)
				}
			}
		}
		if !inIncident {
			quiet.Solves++
			if gated {
				quiet.Gated++
			}
			quiet.MeanResid += r.Residual
			quiet.MeanSpur += float64(len(r.SupportIDs))
		}
	}
	if quiet.Solves > 0 {
		quiet.MeanResid /= float64(quiet.Solves)
		quiet.MeanSpur /= float64(quiet.Solves)
	}
	res.QuietSolve = quiet

	// Fold per-emitter tallies and drift tracks into the reports.
	for i := range reports {
		rep := &reports[i]
		if rep.SolveWindows > 0 {
			rep.SupportRate /= float64(rep.SolveWindows)
			rep.GatedRate /= float64(rep.SolveWindows)
		}
		for _, o := range outs {
			t := o.inc[i]
			rep.StormWindows += t.stormWindows
			rep.FallbackSeen += t.fallbackSeen
			rep.FallbackTotal += t.fallbackTotal
			if t.firstStorm >= 0 && (rep.FirstStormWin < 0 || t.firstStorm < rep.FirstStormWin) {
				rep.FirstStormWin = t.firstStorm
			}
			if t.lastStorm > rep.LastStormWin {
				rep.LastStormWin = t.lastStorm
			}
		}
		for _, o := range outs {
			for _, dt := range o.drift {
				if dt.kind != cfg.Incidents[i].Kind {
					continue
				}
				if dt.maxDelta > rep.MaxDrift {
					rep.MaxDrift = dt.maxDelta
				}
				if dt.firstWin >= 0 {
					lag := dt.firstWin - int64(cfg.Incidents[i].StartWin)
					if rep.DriftLagWin < 0 || lag < rep.DriftLagWin {
						rep.DriftLagWin = lag
					}
				}
			}
		}
	}
	res.Incidents = reports
	return nil
}

// newMonthGroupNameOnly rebuilds the deterministic agg=sum logical name
// for global group idx without a World (the parts derive from idx alone).
func newMonthGroupNameOnly(idx int) string {
	return fmt.Sprintf("%s|deployment=svc-%04d,namespace=%s|agg=sum",
		monthMetrics[idx%len(monthMetrics)], idx%997, monthNamespaces[idx%len(monthNamespaces)])
}

// ---------------------------------------------------------------------------
// Reporting.

func monthCSV(res *monthResult) string {
	var b strings.Builder
	b.WriteString("day,base_raw,base_snappy,base_zstd,pl_total,pl_golden,pl_kdelta,pl_residual,pl_fallback,pl_snapshot,fallback_windows\n")
	for d, day := range res.Days {
		if day.BaseRaw == 0 && day.PLTotal == 0 {
			continue
		}
		fmt.Fprintf(&b, "%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n", d,
			day.BaseRaw, day.BaseSnappy, day.BaseZstd, day.PLTotal,
			day.PLGolden, day.PLKDelta, day.PLResidual, day.PLFallbk, day.PLSnapshot, day.FallbackWindows)
	}
	return b.String()
}

func monthTotals(res *monthResult) (base monthDay) {
	for _, d := range res.Days {
		base = addMonthDay(base, d)
	}
	return base
}

func gb(v int64) float64 { return float64(v) / (1 << 30) }

func ratio(a, b int64) float64 {
	if b == 0 {
		return math.NaN()
	}
	return float64(a) / float64(b)
}

func lagStr(lagWin int64, interval time.Duration) string {
	if lagWin < 0 {
		return "never"
	}
	return (time.Duration(lagWin) * interval).String()
}

func monthHeadlines(cfg monthConfig, res *monthResult) []string {
	t := monthTotals(res)
	lines := []string{
		fmt.Sprintf("month ledger: baseline raw %.2f GiB, snappy %.2f GiB, zstd %.2f GiB; palimpsest %.3f GiB (golden %.3f / kdelta %.3f / residual %.3f / fallback %.4f / snapshots %.4f)",
			gb(t.BaseRaw), gb(t.BaseSnappy), gb(t.BaseZstd), gb(t.PLTotal),
			gb(t.PLGolden), gb(t.PLKDelta), gb(t.PLResidual), gb(t.PLFallbk), gb(t.PLSnapshot)),
		fmt.Sprintf("month ledger: amortized compression %.1fx vs snappy remote-write, %.1fx vs zstd remote-write",
			ratio(t.BaseSnappy, t.PLTotal), ratio(t.BaseZstd, t.PLTotal)),
	}
	for _, rep := range res.Incidents {
		lines = append(lines, fmt.Sprintf(
			"incident %s: substrate(a) support %s, emit %s (support-rate %.0f%%, gated %.0f%%); substrate(c) drift %s (max Δ %.2f); storm windows %d (breaker active %s..%s); fallback top-K coverage %s; ring lookback covers %.1f%% of the span",
			rep.Inc.Name,
			lagStr(rep.SupportLagWin, cfg.Interval), lagStr(rep.EmitLagWin, cfg.Interval),
			rep.SupportRate*100, rep.GatedRate*100,
			lagStr(rep.DriftLagWin, cfg.Interval), rep.MaxDrift,
			rep.StormWindows,
			lagStr(rep.FirstStormWin-int64(rep.Inc.StartWin), cfg.Interval),
			lagStr(rep.LastStormWin-int64(rep.Inc.StartWin), cfg.Interval),
			coverageStr(rep), rep.RingCoverage*100))
	}
	lines = append(lines, fmt.Sprintf("quiet windows: %d solves, %.0f%% gated at max-residual %g (mean solve residual %.2f, mean support size %.0f)",
		res.QuietSolve.Solves, 100*float64(res.QuietSolve.Gated)/float64(max(res.QuietSolve.Solves, 1)), cfg.MaxResidual,
		res.QuietSolve.MeanResid, res.QuietSolve.MeanSpur))
	return lines
}

func coverageStr(rep monthIncidentReport) string {
	if rep.FallbackTotal == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(rep.FallbackSeen)/float64(rep.FallbackTotal), rep.FallbackSeen, rep.FallbackTotal)
}

func monthSummaryMD(cfg monthConfig, res *monthResult) string {
	t := monthTotals(res)
	var b strings.Builder
	b.WriteString("# 30-day byte ledger (M2 benchmark)\n\n")
	fmt.Fprintf(&b, "Generated by `plsim --month` (seed %d): %d simulated days, %d raw per-instance series (%d logical groups × %d pods → %d sketched series per ADR-008), %d emitters, flush %s, keyframe every %d windows (golden every %d keyframes), codec per --codec, storm %g× over %d windows, FALLBACK top-K %d.\n\n",
		cfg.Seed, int(float64(cfg.TotalWindows)/float64(cfg.WindowsPerDay)), res.RawSeries, cfg.Groups, cfg.InstancesPerLogical, res.Sketched,
		cfg.Emitters, cfg.Interval, cfg.KeyframeEvery, cfg.GoldenEvery, cfg.StormMultiplier, cfg.StormWindow, cfg.FallbackTopK)

	b.WriteString("## Totals\n\n")
	b.WriteString("| line | bytes | GiB | vs Palimpsest |\n| --- | --- | --- | --- |\n")
	fmt.Fprintf(&b, "| baseline remote-write, uncompressed | %d | %.2f | %.1f× |\n", t.BaseRaw, gb(t.BaseRaw), ratio(t.BaseRaw, t.PLTotal))
	fmt.Fprintf(&b, "| baseline remote-write, snappy (RW 1.0) | %d | %.2f | **%.1f×** |\n", t.BaseSnappy, gb(t.BaseSnappy), ratio(t.BaseSnappy, t.PLTotal))
	fmt.Fprintf(&b, "| baseline remote-write, zstd | %d | %.2f | %.1f× |\n", t.BaseZstd, gb(t.BaseZstd), ratio(t.BaseZstd, t.PLTotal))
	fmt.Fprintf(&b, "| **Palimpsest, everything** | %d | %.3f | 1× |\n\n", t.PLTotal, gb(t.PLTotal))

	b.WriteString("| Palimpsest component | bytes | share |\n| --- | --- | --- |\n")
	for _, row := range [...]struct {
		name string
		v    int64
	}{
		{"golden keyframes", t.PLGolden}, {"KDELTA keyframes", t.PLKDelta},
		{"RESIDUAL sketches", t.PLResidual}, {"FALLBACK frames", t.PLFallbk},
		{"dashcam snapshot blobs (subset)", t.PLSnapshot},
	} {
		fmt.Fprintf(&b, "| %s | %d | %.1f%% |\n", row.name, row.v, 100*float64(row.v)/float64(max(int(t.PLTotal), 1)))
	}

	b.WriteString("\n## Incidents\n\n")
	b.WriteString("| incident | injected | substrate (a) support / emit | substrate (c) drift | storm behavior | notes |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, rep := range res.Incidents {
		inc := rep.Inc
		injected := fmt.Sprintf("window %d, %s", inc.StartWin, (time.Duration(inc.DurWin) * cfg.Interval).String())
		subA := fmt.Sprintf("%s / %s (support %.0f%%, gated %.0f%%)", lagStr(rep.SupportLagWin, cfg.Interval), lagStr(rep.EmitLagWin, cfg.Interval), rep.SupportRate*100, rep.GatedRate*100)
		subC := fmt.Sprintf("%s (max Δ %.2f vs threshold %g)", lagStr(rep.DriftLagWin, cfg.Interval), rep.MaxDrift, cfg.DriftThresh)
		stormB := "never fired"
		if rep.StormWindows > 0 {
			stormB = fmt.Sprintf("%d fallback windows, active %s→%s of the span; top-K coverage %s",
				rep.StormWindows, lagStr(rep.FirstStormWin-int64(inc.StartWin), cfg.Interval), lagStr(rep.LastStormWin-int64(inc.StartWin), cfg.Interval), coverageStr(rep))
		}
		note := ""
		switch inc.Kind {
		case "ramp":
			note = fmt.Sprintf("climb is dead-zone by design; a substrate hit near span end is the collapse when the ramp stops (the OOM kill), not the cause; ring-buffer lookback covers the final %.1f%% of the climb", rep.RingCoverage*100)
		case "mass":
			note = "the breaker fires only until the next keyframe re-bases the outage into every baseline (ADR-003), then RESIDUAL frames resume against a fleet-wide re-based state; the recovery transient at outage end fires it again"
		case "spike":
			note = "sustained shift: substrate (a) sees it only until the next keyframe re-bases (ADR-003); substrate (c) owns it from there"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", inc.Name, injected, subA, subC, stormB, note)
	}

	fmt.Fprintf(&b, "\n## Quiet-window recovery health\n\n%d solves outside incident spans: %.0f%% gated at the palimpsestd default max-residual %g; mean solve residual %.2f; mean support size %.0f.\n",
		res.QuietSolve.Solves, 100*float64(res.QuietSolve.Gated)/float64(max(res.QuietSolve.Solves, 1)), cfg.MaxResidual, res.QuietSolve.MeanResid, res.QuietSolve.MeanSpur)

	b.WriteString("\n## Honest accounting notes\n\n")
	b.WriteString("- The baseline is per-instance remote-write (labels repeated every window, as RW 1.0 actually behaves), snappy-compressed with a real compressor; the zstd row is a generous baseline no stock Prometheus ships today.\n")
	b.WriteString("- Palimpsest's ledger includes every escape hatch that fired: FALLBACK frames, dashcam snapshot blobs, dict deltas, golden re-announcements under churn.\n")
	b.WriteString("- Dashcam snapshots here are per logical series (plsim's pipeline); the otel processor buffers per instance, so real snapshot bytes scale with flagged-series instance counts — see docs/ENVELOPE.md §6-8.\n")
	b.WriteString("- Pod churn never touches the sketch (ADR-008): only the baseline pays label-set turnover. Logical churn is charged to Palimpsest via dict deltas and golden keyframes (ADR-017).\n")
	b.WriteString("- Detection columns are measured against palimpsestd defaults; see docs/DEADZONE.md for why the leak is invisible and what it would take to see it.\n")
	return b.String()
}
