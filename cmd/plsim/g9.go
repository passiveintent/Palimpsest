/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/recover"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// --g9 is the Gate G9 confirmatory harness (docs/ADR-016-cs-verdict.md):
// a pre-registered make-or-break evaluation of the compressed-sensing
// layer. It runs the real encode pipeline (Tracker + Hold + Accumulator +
// keyframe/RESIDUAL/FALLBACK, cmd/plsim/encoder.go) over a fixed fleet,
// solves every shipped RESIDUAL at palimpsestd-equivalent settings, and
// scores three detector families side by side:
//
//   - CS substrate (a): FISTA recovery + residual gate, pre-fix
//     (lambda = 0.05 * max|Phi^T y|, fixed gate 5.0) or post-fix
//     (F2: lambda = kappa * sigma_hat * sqrt(2 ln N);
//      F1: gate = c_gate * sqrt(n) * sigma_hat).
//   - substrate (c): keyframe-to-keyframe drift at the production
//     threshold, optionally with F3 baseline freeze.
//   - the ADR-004 storm/FALLBACK path.
//
// plus the K4 "dumb money" classical baselines: exact keyframes at 30 s
// and 15 s cadence (real wire.BuildKeyframe bytes, drift rule on that
// grid) and interestingness-routed 10 s exact on the top 5% of series.
//
// Every fix is behind a flag so pre/post A/B runs use identical seeds and
// identical world draws. Scenario generators for --deadzone/--month are
// untouched; this file is additive.
type g9Flags struct {
	enabled  bool
	out      string
	scenario string

	f1, f2, f3   bool
	cGate, kappa float64

	days        float64
	series      int
	noise       float64
	seasonalAmp float64
	trials      int
	solveEvery  int
	label       string

	kappaGrid string
	cGateGrid string
}

func registerG9Flags(g *g9Flags) {
	flag.BoolVar(&g.enabled, "g9", false, "run one Gate G9 confirmatory scenario (docs/ADR-016-cs-verdict.md) instead of the frame-generating simulation, then exit; uses --m/--d/--bits/--seed/--interval/--keyframe-every/--golden-every/--tenant-key/--shard-id")
	flag.StringVar(&g.out, "g9-out", "./g9", "directory to write the run's JSON result and summary")
	flag.StringVar(&g.scenario, "g9-scenario", "s5", "scenario: s1|s2|s3|s4|s5|s6|calib|calib-anom|sweep")
	flag.BoolVar(&g.f1, "g9-fix-f1", false, "F1: normalized residual gate c_gate*sqrt(n)*sigma_hat instead of the fixed 5.0")
	flag.BoolVar(&g.f2, "g9-fix-f2", false, "F2: magnitude-independent lambda kappa*sigma_hat*sqrt(2 ln N) instead of 0.05*max|Phi^T y|")
	flag.BoolVar(&g.f3, "g9-fix-f3", false, "F3: freeze keyframe re-basing (Hold baseline AND substrate (c) reference) for flagged series until 3 consecutive in-band windows")
	flag.Float64Var(&g.cGate, "g9-c-gate", 0, "F1 gate constant (calibrated in Phase 2; required with -g9-fix-f1)")
	flag.Float64Var(&g.kappa, "g9-kappa", 0, "F2 lambda constant (calibrated in Phase 2; required with -g9-fix-f2)")
	flag.Float64Var(&g.days, "g9-days", 2, "simulated days (s5/calib)")
	flag.IntVar(&g.series, "g9-series", 900, "background series count")
	flag.Float64Var(&g.noise, "g9-noise", 0.2, "per-series per-window Gaussian jitter sigma, absolute residual units")
	flag.Float64Var(&g.seasonalAmp, "g9-seasonal-amp", 0.25, "daily seasonal amplitude as a fraction of each background series' baseline")
	flag.IntVar(&g.trials, "g9-trials", 30, "anomaly bursts per seed (s3/s4/calib-anom) or trials per cell (sweep)")
	flag.IntVar(&g.solveEvery, "g9-solve-every", 0, "solve every Nth eligible window; 0 = auto (1 for incident scenarios, 2 for s5/calib)")
	flag.StringVar(&g.label, "g9-label", "run", "config label folded into output filenames (e.g. prefix/postfix)")
	flag.StringVar(&g.kappaGrid, "g9-kappa-grid", "0.4,0.5,0.6,0.7,0.8,1.0,1.2,1.4,1.7,2.0", "calib: kappa grid")
	flag.StringVar(&g.cGateGrid, "g9-c-gate-grid", "1.0,1.05,1.1,1.15,1.2,1.3,1.5", "calib: c_gate grid")
}

// Locked constants (docs/ADR-016-cs-verdict.md "Locked definitions").
const (
	g9StormWindow     = 30
	g9StormMult       = 25.0
	g9TopK            = 100
	g9FISTAIters      = 350
	g9FISTAPower      = 50
	g9FISTAThreshold  = 0.3
	g9LambdaMult      = 0.05 // pre-fix lambda multiplier (palimpsestd default)
	g9FixedGate       = 5.0  // pre-fix residual gate (palimpsestd default)
	g9DriftThresh     = 10.0 // substrate (c) production default
	g9SigmaWindow     = 30
	g9SigmaReady      = 5
	g9Warmup          = 36 // windows before detectors are scored
	g9FreezeStreak    = 3
	g9BandFloor       = 0.3
	g9BandSigmaMult   = 3.0
	g9ResRingLen      = 15
	g9SigmaFloor      = 1e-3
	g9GorillaPerSamp  = 1.37 // bytes/sample, Gorilla (VLDB'15) steady-state
	g9RoutedFrac      = 0.05
	g9ViewID          = uint16(9200)
	g9BurstScoreSlack = 6 // windows past burst end still attributed to it
)

type g9Config struct {
	M, D, Bits int
	Seed       int64
	ShardID    uint64
	TenantKey  []byte
	Interval   time.Duration

	KeyframeEvery, GoldenEvery int

	Scenario    string
	Out         string
	Label       string
	F1, F2, F3  bool
	CGate       float64
	Kappa       float64
	Days        float64
	Series      int
	Noise       float64
	SeasonalAmp float64
	Trials      int
	SolveEvery  int

	KappaGrid, CGateGrid []float64
}

func runG9(f flags) error {
	kg, err := parseDeadzoneGrid(f.g9.kappaGrid, false)
	if err != nil {
		return fmt.Errorf("--g9-kappa-grid: %w", err)
	}
	cg, err := parseDeadzoneGrid(f.g9.cGateGrid, false)
	if err != nil {
		return fmt.Errorf("--g9-c-gate-grid: %w", err)
	}
	cfg := g9Config{
		M: f.m, D: f.d, Bits: f.bits, Seed: f.seed, ShardID: f.shardID,
		TenantKey: []byte(f.tenantKey), Interval: f.interval,
		KeyframeEvery: f.keyframeEvery, GoldenEvery: f.goldenEvery,
		Scenario: f.g9.scenario, Out: f.g9.out, Label: f.g9.label,
		F1: f.g9.f1, F2: f.g9.f2, F3: f.g9.f3,
		CGate: f.g9.cGate, Kappa: f.g9.kappa,
		Days: f.g9.days, Series: f.g9.series, Noise: f.g9.noise,
		SeasonalAmp: f.g9.seasonalAmp, Trials: f.g9.trials, SolveEvery: f.g9.solveEvery,
		KappaGrid: kg, CGateGrid: cg,
	}
	if cfg.F1 && cfg.CGate <= 0 {
		return fmt.Errorf("--g9-fix-f1 requires --g9-c-gate > 0")
	}
	if cfg.F2 && cfg.Kappa <= 0 {
		return fmt.Errorf("--g9-fix-f2 requires --g9-kappa > 0")
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return err
	}
	switch cfg.Scenario {
	case "s1", "s2", "s3", "s4", "s5", "s6", "calib-anom":
		return runG9Scenario(cfg)
	case "calib":
		return runG9Calib(cfg)
	case "sweep":
		return runG9Sweep(cfg)
	default:
		return fmt.Errorf("--g9-scenario %q unknown", cfg.Scenario)
	}
}

// ---------------------------------------------------------------------------
// World.

type g9World struct {
	cfg       *g9Config
	names     []string
	ids       []uint64
	base      []float64
	phase     []float64
	targetIdx int // the reserved low-volume payment series
	rng       *rand.Rand
}

func newG9World(cfg *g9Config) *g9World {
	rng := rand.New(rand.NewSource(cfg.Seed*104729 + 17))
	total := cfg.Series + 1
	w := &g9World{
		cfg:   cfg,
		names: make([]string, total),
		ids:   make([]uint64, total),
		base:  make([]float64, total),
		phase: make([]float64, total),
		rng:   rng,
	}
	for i := 0; i < cfg.Series; i++ {
		w.names[i] = fmt.Sprintf("g9_bg_%05d|deployment=svc-%04d,namespace=prod|agg=sum", i, i%997)
		w.base[i] = 50 + rng.Float64()*100
		w.phase[i] = rng.Float64()
	}
	w.targetIdx = cfg.Series
	w.names[w.targetIdx] = "payment_errors_total|service=payments,namespace=prod|agg=sum"
	w.base[w.targetIdx] = 0.02 // ~20 req/s * 0.01% errors per 10 s window
	for i, n := range w.names {
		w.ids[i] = sketch.SeriesID([]byte(n))
	}
	return w
}

// valuesAt fills dst with every series' raw value at simulated time tSec,
// drawing jitter from rng — the caller owns the stream, so the 10 s
// pipeline and each classical cadence sampler evolve independent draws of
// the same statistical world. Injections are added by the caller.
func (w *g9World) valuesAt(tSec float64, rng *rand.Rand, dst []float64) {
	tDays := tSec / 86400
	for i := range dst {
		if i == w.targetIdx {
			dst[i] = w.base[i] + rng.NormFloat64()*w.cfg.Noise
			continue
		}
		dst[i] = w.base[i]*(1+w.cfg.SeasonalAmp*math.Sin(2*math.Pi*(tDays+w.phase[i]))) +
			rng.NormFloat64()*w.cfg.Noise
	}
}

// ---------------------------------------------------------------------------
// Scenario specs.

// g9Burst is one injected anomaly: +Magnitude on Targets during
// [StartWin, StartWin+DurWin). Sub-window classical samplers see it during
// the same wall-clock span [StartWin*10s, end*10s).
type g9Burst struct {
	Magnitude float64
	StartWin  uint32
	DurWin    uint32
	Targets   []int
}

func (b *g9Burst) activeWin(w uint32) bool { return w >= b.StartWin && w < b.StartWin+b.DurWin }
func (b *g9Burst) activeSec(tSec, interval float64) bool {
	return tSec >= float64(b.StartWin)*interval && tSec < float64(b.StartWin+b.DurWin)*interval
}
func (b *g9Burst) inScoreSpan(w uint32) bool {
	return w >= b.StartWin && w < b.StartWin+b.DurWin+g9BurstScoreSlack
}

type g9Spec struct {
	totalWindows uint32
	bursts       []g9Burst
	// magLabel distinguishes s3's per-magnitude sub-runs in the output.
	magLabel string
}

// massSet returns the "30% of the fleet" index set (idx%10 < 3),
// excluding the reserved payment target.
func g9MassSet(series int) []int {
	var out []int
	for i := 0; i < series; i++ {
		if i%10 < 3 {
			out = append(out, i)
		}
	}
	return out
}

// g9Specs builds the scenario's run specs (s3/calib-anom produce one spec
// per magnitude; everything else one spec).
func g9Specs(cfg *g9Config, world *g9World) ([]g9Spec, error) {
	windowsPerDay := uint32(24 * time.Hour / cfg.Interval)
	switch cfg.Scenario {
	case "s1":
		// Payment-service weak signal: error rate 0.01% -> 0.5% on a
		// ~200 req/window service = +1.0 absolute residual, sustained
		// 15 min; 3 bursts per seed.
		var bursts []g9Burst
		start := uint32(39)
		for i := 0; i < 3; i++ {
			bursts = append(bursts, g9Burst{Magnitude: 1.0, StartWin: start, DurWin: 90, Targets: []int{world.targetIdx}})
			start += 90 + 37 // cool-down; 37 also shifts keyframe phase
		}
		return []g9Spec{{totalWindows: start + 12, bursts: bursts}}, nil
	case "s2":
		return []g9Spec{{
			totalWindows: 39 + 180 + 45,
			bursts:       []g9Burst{{Magnitude: 100, StartWin: 39, DurWin: 180, Targets: g9MassSet(cfg.Series)}},
		}}, nil
	case "s3", "calib-anom":
		mags := []float64{1, 2, 5, 10, 20}
		if cfg.Scenario == "calib-anom" {
			mags = []float64{3, 7}
		}
		var specs []g9Spec
		for _, mag := range mags {
			var bursts []g9Burst
			start := uint32(39)
			for i := 0; i < cfg.Trials; i++ {
				bursts = append(bursts, g9Burst{Magnitude: mag, StartWin: start, DurWin: 6, Targets: []int{world.targetIdx}})
				start += 19 // prime vs keyframe cadence 6: onset phase cycles
			}
			specs = append(specs, g9Spec{totalWindows: start + 12, bursts: bursts, magLabel: trimFloat(mag)})
		}
		return specs, nil
	case "s4":
		// Sub-keyframe transient: 20-30 s spike fully inside one 60 s
		// keyframe cycle (never touching a keyframe window).
		const s4Target = 100
		var bursts []g9Burst
		for i := 0; i < cfg.Trials; i++ {
			cycle := uint32(42 + i*18) // multiple of keyframeEvery=6
			dur := uint32(2 + i%2)
			phase := uint32(1 + i%2)
			if phase+dur > 5 {
				phase = 5 - dur
			}
			bursts = append(bursts, g9Burst{Magnitude: 30, StartWin: cycle + phase, DurWin: dur, Targets: []int{s4Target}})
		}
		last := bursts[len(bursts)-1]
		return []g9Spec{{totalWindows: last.StartWin + last.DurWin + 18, bursts: bursts}}, nil
	case "s5":
		return []g9Spec{{totalWindows: uint32(cfg.Days * float64(windowsPerDay))}}, nil
	case "s6":
		return []g9Spec{{
			totalWindows: 39 + 270,
			bursts:       []g9Burst{{Magnitude: 15, StartWin: 39, DurWin: 270, Targets: g9MassSet(cfg.Series)}},
		}}, nil
	}
	return nil, fmt.Errorf("no spec for scenario %q", cfg.Scenario)
}

// ---------------------------------------------------------------------------
// Classical dumb-money samplers (K4).

type g9Sampler struct {
	name    string
	cadSec  float64
	rng     *rand.Rand
	world   *g9World
	f3      bool
	nextT   float64
	rounds  int
	prevRef []float64 // drift reference (freeze-aware under F3)
	haveRef bool
	frozen  map[int]int // idx -> in-band streak
	dRing   [][]float64 // per-series recent |sample deltas| for the band
	dPos    []int
	dN      []int
	prevKF  map[uint64]float32
	bytes   int64
	// flags: per burst scoring is done by the engine via flagAt callbacks;
	// the sampler just records (tSec, idx) flags.
	flags []g9SamplerFlag
	buf   []float64
}

type g9SamplerFlag struct {
	TSec float64
	Idx  int
}

func newG9Sampler(name string, cadSec float64, world *g9World, f3 bool, seed int64) *g9Sampler {
	total := len(world.names)
	s := &g9Sampler{
		name: name, cadSec: cadSec, world: world, f3: f3,
		rng:    rand.New(rand.NewSource(seed)),
		frozen: make(map[int]int),
		dRing:  make([][]float64, total),
		dPos:   make([]int, total),
		dN:     make([]int, total),
		buf:    make([]float64, total),
	}
	for i := range s.dRing {
		s.dRing[i] = make([]float64, g9ResRingLen)
	}
	return s
}

// advance processes every sample time <= tSec not yet taken. bursts give
// the injections; flags are recorded with their sample time.
func (s *g9Sampler) advance(tSec float64, interval float64, bursts []g9Burst) {
	for s.nextT <= tSec {
		s.sampleAt(s.nextT, interval, bursts)
		s.nextT += s.cadSec
	}
}

func (s *g9Sampler) sampleAt(t, interval float64, bursts []g9Burst) {
	s.world.valuesAt(t, s.rng, s.buf)
	for i := range bursts {
		b := &bursts[i]
		if b.activeSec(t, interval) {
			for _, idx := range b.Targets {
				s.buf[idx] += b.Magnitude
			}
		}
	}

	// Byte ledger: a real KDELTA/golden keyframe built from these samples.
	golden := s.rounds%10 == 0
	in := wire.KeyframeBuildInput{
		Golden: golden, Codec: wire.CodecNone,
		IDs: s.sortedIDs(), Values: s.valueMap(), PrevKeyframeValues: s.prevKF,
	}
	if golden {
		in.FullDict = s.fullDict()
	}
	res, err := wire.BuildKeyframe(in)
	if err == nil {
		f := &wire.Frame{
			Magic: wire.Magic, Version: wire.Version, FrameType: wire.FrameTypeKeyframe,
			Flags: res.Flags, EmitterID: 1, ShardID: 1, Seq: uint32(s.rounds),
			M: uint32(len(s.buf)), Payload: res.Payload, QuantScale: res.QuantScale,
			DictRoot: res.DictRoot, DictDeltas: res.DictDeltas,
		}
		s.bytes += int64(wire.EncodedSize(f))
		s.prevKF = res.Values32
	}
	s.rounds++

	// Drift rule (substrate (c) semantics on this grid), F3-aware.
	if !s.haveRef {
		s.prevRef = append([]float64(nil), s.buf...)
		s.haveRef = true
		return
	}
	for i, v := range s.buf {
		d := v - s.prevRef[i]
		ad := math.Abs(d)
		// delta ring feeds the per-series band estimate.
		s.dRing[i][s.dPos[i]] = ad
		s.dPos[i] = (s.dPos[i] + 1) % g9ResRingLen
		if s.dN[i] < g9ResRingLen {
			s.dN[i]++
		}
		flagged := ad > g9DriftThresh
		if flagged {
			s.flags = append(s.flags, g9SamplerFlag{TSec: t, Idx: i})
		}
		if !s.f3 {
			s.prevRef[i] = v
			continue
		}
		if flagged {
			s.frozen[i] = 0
			continue // reference frozen
		}
		if streak, isFrozen := s.frozen[i]; isFrozen {
			band := math.Max(g9BandSigmaMult*madOf(s.dRing[i][:s.dN[i]])/math.Sqrt2, g9BandFloor)
			if ad <= band {
				streak++
				if streak >= g9FreezeStreak {
					delete(s.frozen, i)
					s.prevRef[i] = v
					continue
				}
				s.frozen[i] = streak
			} else {
				s.frozen[i] = 0
			}
			continue // still frozen
		}
		s.prevRef[i] = v
	}
}

func (s *g9Sampler) sortedIDs() []uint64 {
	ids := append([]uint64(nil), s.world.ids...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *g9Sampler) valueMap() map[uint64]float64 {
	m := make(map[uint64]float64, len(s.buf))
	for i, v := range s.buf {
		m[s.world.ids[i]] = v
	}
	return m
}

func (s *g9Sampler) fullDict() []wire.DictDelta {
	ids := s.sortedIDs()
	byID := make(map[uint64]int, len(ids))
	for i, id := range s.world.ids {
		byID[id] = i
	}
	out := make([]wire.DictDelta, len(ids))
	for i, id := range ids {
		idx := byID[id]
		out[i] = wire.DictDelta{ID: id, InitValue: float32(s.buf[idx]), Name: []byte(s.world.names[idx])}
	}
	return out
}

// madOf is a small robust scale helper (median of values / 0.6745 is not
// wanted here: dRing holds |deltas|, whose median relates to the sample
// sigma by median(|N(0,sqrt2 sigma)|) = 0.6745*sqrt2*sigma).
func madOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return medianSortedG9(s) / 0.6745
}

func medianSortedG9(s []float64) float64 {
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// ---------------------------------------------------------------------------
// Engine.

type g9Event struct {
	Win     uint32
	IDs     []uint64
	Resid   float64
	Gate    float64
	Support int
}

type g9Engine struct {
	cfg   *g9Config
	world *g9World
	pipe  *pipeline

	dict   *recover.Dictionary
	params sketch.Params
	idxOf  map[uint64]int

	sigmaQ *recover.QuietSigma

	frozen map[uint64]int // id -> in-band streak (F3)

	resRing [][]float64
	resPos  []int
	resN    []int

	cPrev map[uint64]float64
	cHave bool

	events     []g9Event
	driftFlags map[uint32][]uint64 // keyframe window -> flagged ids
	stormIDs   map[uint32][]uint64 // fallback window -> top-K ids
	stormWins  []uint32

	bytes map[string]int64

	solves, quietSolves, quietEvents  int
	quietSupportSum                   int
	quietResidSum, quietGateSum       float64
	sigmaLast                         float64
	frozenMax                         int
	solveEvery                        int
	vals                              []float64
	valueMap                          map[string]float64
	routed                            map[int]bool
	samplers                          []*g9Sampler
}

func newG9Engine(cfg *g9Config, world *g9World, withSamplers bool) *g9Engine {
	total := len(world.names)
	e := &g9Engine{
		cfg: cfg, world: world,
		sigmaQ:     recover.NewQuietSigma(g9SigmaWindow),
		frozen:     make(map[uint64]int),
		resRing:    make([][]float64, total),
		resPos:     make([]int, total),
		resN:       make([]int, total),
		cPrev:      make(map[uint64]float64, total),
		driftFlags: make(map[uint32][]uint64),
		stormIDs:   make(map[uint32][]uint64),
		bytes:      make(map[string]int64),
		idxOf:      make(map[uint64]int, total),
		vals:       make([]float64, total),
		valueMap:   make(map[string]float64, total),
	}
	for i := range e.resRing {
		e.resRing[i] = make([]float64, g9ResRingLen)
		e.idxOf[world.ids[i]] = i
	}
	e.pipe = newPipeline(pipelineConfig{
		TenantKey: cfg.TenantKey, ShardID: cfg.ShardID,
		EmitterID: emitterID(cfg.Seed, 0), ViewID: g9ViewID, Epoch: 0,
		M: cfg.M, D: cfg.D, Bits: cfg.Bits,
		KeyframeEvery: cfg.KeyframeEvery, GoldenEvery: cfg.GoldenEvery,
		SeriesTTL: 24 * time.Hour, SnapshotThreshold: 0,
		Storm:            sketch.NewStormDetector(g9StormWindow, g9StormMult),
		FallbackTopK:     g9TopK,
		CaptureResiduals: true,
	})
	e.params = sketch.Params{
		M: cfg.M, D: cfg.D, Bits: cfg.Bits,
		Seed: sketch.DeriveEphemeralSeed(cfg.TenantKey, cfg.ShardID, 0, g9ViewID),
	}
	e.dict = recover.NewDictionary()
	for i, n := range world.names {
		e.dict.ApplyDelta(wire.DictDelta{ID: world.ids[i], Name: []byte(n)})
	}
	e.solveEvery = cfg.SolveEvery
	if e.solveEvery <= 0 {
		if cfg.Scenario == "s5" || cfg.Scenario == "calib" {
			e.solveEvery = 2
		} else {
			e.solveEvery = 1
		}
	}
	if withSamplers {
		e.samplers = []*g9Sampler{
			newG9Sampler("k30", 30, world, cfg.F3, cfg.Seed+1_000_001),
			newG9Sampler("k15", 15, world, cfg.F3, cfg.Seed+1_000_002),
		}
	}
	return e
}

// sigmaHat returns the current locked-definition noise estimate (per-series
// units) and whether it is ready for use.
func (e *g9Engine) sigmaHat() (float64, bool) {
	est, ok := e.sigmaQ.Estimate()
	if !ok || e.sigmaQ.Count() < g9SigmaReady {
		return 0, false
	}
	return math.Max(est, g9SigmaFloor), true
}

// run plays the spec through the pipeline and detectors.
func (e *g9Engine) run(spec g9Spec) error {
	cfg := e.cfg
	world := e.world
	interval := cfg.Interval.Seconds()
	simStart := time.Unix(1_700_000_000, 0)
	n := len(world.names)
	sqrtN := math.Sqrt(float64(n))
	lnTerm := math.Sqrt(2 * math.Log(float64(n)))

	for w := uint32(0); w < spec.totalWindows; w++ {
		tSec := float64(w) * interval
		now := simStart.Add(time.Duration(w) * cfg.Interval)

		world.valuesAt(tSec, world.rng, e.vals)
		for i := range spec.bursts {
			b := &spec.bursts[i]
			if b.activeWin(w) {
				for _, idx := range b.Targets {
					e.vals[idx] += b.Magnitude
				}
			}
		}
		for i, name := range world.names {
			e.valueMap[name] = e.vals[i]
		}

		if cfg.F3 {
			fz := make(map[uint64]struct{}, len(e.frozen))
			for id := range e.frozen {
				fz[id] = struct{}{}
			}
			e.pipe.SetFrozen(fz)
		}

		frame := e.pipe.flush(now, w, e.valueMap)
		sz := int64(wire.EncodedSize(frame))
		e.bytes["pal_total"] += sz
		var newFlags []uint64

		switch {
		case frame.FrameType == wire.FrameTypeKeyframe && frame.Flags&wire.FlagKDelta == 0:
			e.bytes["pal_golden"] += sz
		case frame.FrameType == wire.FrameTypeKeyframe:
			e.bytes["pal_kdelta"] += sz
		case frame.FrameType == wire.FrameTypeFallback:
			e.bytes["pal_fallback"] += sz
		default:
			e.bytes["pal_residual"] += sz
		}

		// Per-series residual rings (freeze bands, routing).
		res := e.pipe.lastResiduals
		for id, r := range res {
			i, ok := e.idxOf[id]
			if !ok {
				continue
			}
			e.resRing[i][e.resPos[i]] = r
			e.resPos[i] = (e.resPos[i] + 1) % g9ResRingLen
			if e.resN[i] < g9ResRingLen {
				e.resN[i]++
			}
		}

		stormWindow := false
		eventThisWindow := false

		switch frame.FrameType {
		case wire.FrameTypeKeyframe:
			// Substrate (c): keyframe-to-keyframe drift.
			if e.cHave {
				for i, id := range world.ids {
					prev, ok := e.cPrev[id]
					if !ok {
						continue
					}
					if math.Abs(e.vals[i]-prev) > g9DriftThresh {
						e.driftFlags[w] = append(e.driftFlags[w], id)
						newFlags = append(newFlags, id)
					}
				}
			}
			for i, id := range world.ids {
				if cfg.F3 {
					if _, isFrozen := e.frozen[id]; isFrozen {
						continue // reference frozen mid-incident
					}
					// A series flagged THIS keyframe freezes at its old
					// reference (the flag is what freezes it).
					if flaggedNow(newFlags, id) {
						continue
					}
				}
				e.cPrev[id] = e.vals[i]
			}
			e.cHave = true

		case wire.FrameTypeFallback:
			stormWindow = true
			e.stormWins = append(e.stormWins, w)
			if fb, err := wire.DecodeFallback(frame.Payload); err == nil {
				ids := make([]uint64, 0, len(fb.Values))
				for id := range fb.Values {
					ids = append(ids, id)
				}
				e.stormIDs[w] = ids
				newFlags = append(newFlags, ids...)
			}

		case wire.FrameTypeResidual:
			if w >= g9Warmup && w%uint32(e.solveEvery) == 0 {
				y, err := wire.Dequantize(frame.Payload, frame.M, frame.Bits, frame.QuantScale)
				if err != nil {
					return err
				}
				sigmaSeries := recover.MADSigma(y) * math.Sqrt(float64(cfg.M)/float64(n))
				sh, ready := e.sigmaHat()

				opts := recover.Options{
					Iters: g9FISTAIters, Lambda: g9LambdaMult,
					PowerIters: g9FISTAPower, Threshold: g9FISTAThreshold,
				}
				canSolve := true
				if cfg.F2 {
					if !ready {
						canSolve = false
					} else {
						opts.LambdaAbs = cfg.Kappa * sh * lnTerm
					}
				}
				gate := g9FixedGate
				if cfg.F1 {
					if !ready {
						canSolve = false
					} else {
						gate = cfg.CGate * sqrtN * sh
					}
				}
				if canSolve {
					r, err := recover.Recover(y, e.dict, e.params, opts)
					if err != nil {
						return err
					}
					e.solves++
					if len(r.SupportIDs) > 0 && r.Residual <= gate {
						eventThisWindow = true
						ev := g9Event{Win: w, IDs: append([]uint64(nil), r.SupportIDs...),
							Resid: r.Residual, Gate: gate, Support: len(r.SupportIDs)}
						e.events = append(e.events, ev)
						newFlags = append(newFlags, r.SupportIDs...)
					}
					if !inAnyBurstSpan(spec.bursts, w) {
						e.quietSolves++
						e.quietSupportSum += len(r.SupportIDs)
						e.quietResidSum += r.Residual
						e.quietGateSum += gate
						if eventThisWindow {
							e.quietEvents++
						}
					}
				}
				// Locked quiet criterion for the sigma buffer: no storm, no
				// emitted event, no series currently frozen.
				if !stormWindow && !eventThisWindow && len(e.frozen) == 0 {
					e.sigmaQ.Observe(sigmaSeries)
				}
				e.sigmaLast = sigmaSeries
			} else if w < g9Warmup {
				// Warm-up: feed the sigma buffer from quiet pre-scenario
				// windows so detectors are ready at the eligibility line.
				y, err := wire.Dequantize(frame.Payload, frame.M, frame.Bits, frame.QuantScale)
				if err != nil {
					return err
				}
				e.sigmaQ.Observe(recover.MADSigma(y) * math.Sqrt(float64(cfg.M)/float64(n)))
			}
		}

		// F3 freeze/unfreeze bookkeeping.
		if cfg.F3 {
			for _, id := range newFlags {
				e.frozen[id] = 0
			}
			for id, streak := range e.frozen {
				i, ok := e.idxOf[id]
				if !ok {
					delete(e.frozen, id)
					continue
				}
				r, ok := res[id]
				if !ok {
					continue
				}
				band := math.Max(g9BandSigmaMult*madSigmaRing(e.resRing[i][:e.resN[i]]), g9BandFloor)
				if math.Abs(r) <= band {
					streak++
					if streak >= g9FreezeStreak {
						delete(e.frozen, id)
						continue
					}
					e.frozen[id] = streak
				} else {
					e.frozen[id] = 0
				}
			}
			if len(e.frozen) > e.frozenMax {
				e.frozenMax = len(e.frozen)
			}
		}

		// Routing set locks at the eligibility line (top 5% by residual
		// variance over the warm-up rings).
		if w == g9Warmup && e.routed == nil {
			e.routed = e.pickRouted()
		}

		for _, s := range e.samplers {
			s.advance(tSec, interval, spec.bursts)
		}
	}
	return nil
}

func flaggedNow(flags []uint64, id uint64) bool {
	for _, f := range flags {
		if f == id {
			return true
		}
	}
	return false
}

func inAnyBurstSpan(bursts []g9Burst, w uint32) bool {
	for i := range bursts {
		if bursts[i].inScoreSpan(w) {
			return true
		}
	}
	return false
}

// madSigmaRing is MADSigma over a residual ring (plain values, not deltas).
func madSigmaRing(v []float64) float64 {
	return recover.MADSigma(v)
}

func (e *g9Engine) pickRouted() map[int]bool {
	type sv struct {
		idx int
		v   float64
	}
	all := make([]sv, 0, len(e.resRing))
	for i := range e.resRing {
		n := e.resN[i]
		if n == 0 {
			all = append(all, sv{i, 0})
			continue
		}
		var mean, ss float64
		for _, r := range e.resRing[i][:n] {
			mean += r
		}
		mean /= float64(n)
		for _, r := range e.resRing[i][:n] {
			ss += (r - mean) * (r - mean)
		}
		all = append(all, sv{i, ss / float64(n)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	k := int(math.Ceil(g9RoutedFrac * float64(len(all))))
	out := make(map[int]bool, k)
	for _, s := range all[:k] {
		out[s.idx] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// Scoring and reporting.

type g9Det struct {
	Detected bool    `json:"detected"`
	LagSec   float64 `json:"lag_sec"`
}

type g9BurstResult struct {
	Magnitude  float64          `json:"magnitude"`
	StartWin   uint32           `json:"start_win"`
	DurWin     uint32           `json:"dur_win"`
	Targets    int              `json:"targets"`
	Det        map[string]g9Det `json:"det"`
	CSSpurious int              `json:"cs_spurious"`
	CSpurious  int              `json:"c_spurious"`
}

type g9RunResult struct {
	Scenario   string  `json:"scenario"`
	MagLabel   string  `json:"mag_label,omitempty"`
	Label      string  `json:"label"`
	Seed       int64   `json:"seed"`
	F1         bool    `json:"f1"`
	F2         bool    `json:"f2"`
	F3         bool    `json:"f3"`
	CGate      float64 `json:"c_gate"`
	Kappa      float64 `json:"kappa"`
	N          int     `json:"n"`
	Windows    uint32  `json:"windows"`
	SolveEvery int     `json:"solve_every"`

	Solves            int     `json:"solves"`
	Events            int     `json:"events"`
	QuietSolves       int     `json:"quiet_solves"`
	QuietEvents       int     `json:"quiet_events"`
	QuietEventsPerDay float64 `json:"quiet_events_per_day"`
	QuietMeanSupport  float64 `json:"quiet_mean_support"`
	QuietMeanResid    float64 `json:"quiet_mean_resid"`
	QuietMeanGate     float64 `json:"quiet_mean_gate"`
	SigmaHatLast      float64 `json:"sigma_hat_last"`

	StormWindows int     `json:"storm_windows"`
	StormDuty    float64 `json:"storm_duty"`
	DriftFlags   int     `json:"drift_flags"`
	DriftFalse   int     `json:"drift_false"`
	CPersist     float64 `json:"c_persist"`
	FrozenMax    int     `json:"frozen_max"`
	FrozenAtEnd  int     `json:"frozen_at_end"`

	RoutedHasTarget bool `json:"routed_has_target"`

	Bursts []g9BurstResult `json:"bursts"`

	Bytes       map[string]int64   `json:"bytes"`
	BytesPerDay map[string]float64 `json:"bytes_per_day"`
}

// score builds the run result from the engine's tallies.
func (e *g9Engine) score(spec g9Spec, magLabel string) *g9RunResult {
	cfg := e.cfg
	interval := cfg.Interval.Seconds()
	days := float64(spec.totalWindows) * interval / 86400

	rr := &g9RunResult{
		Scenario: cfg.Scenario, MagLabel: magLabel, Label: cfg.Label, Seed: cfg.Seed,
		F1: cfg.F1, F2: cfg.F2, F3: cfg.F3, CGate: cfg.CGate, Kappa: cfg.Kappa,
		N: len(e.world.names), Windows: spec.totalWindows, SolveEvery: e.solveEvery,
		Solves: e.solves, Events: len(e.events),
		QuietSolves: e.quietSolves, QuietEvents: e.quietEvents,
		SigmaHatLast: e.sigmaLast,
		StormWindows: len(e.stormWins),
		FrozenMax:    e.frozenMax, FrozenAtEnd: len(e.frozen),
		Bytes: e.bytes, BytesPerDay: map[string]float64{},
	}
	if e.quietSolves > 0 {
		rr.QuietMeanSupport = float64(e.quietSupportSum) / float64(e.quietSolves)
		rr.QuietMeanResid = e.quietResidSum / float64(e.quietSolves)
		rr.QuietMeanGate = e.quietGateSum / float64(e.quietSolves)
	}
	// Quiet events/day, corrected for solve sampling (a production decoder
	// solves every residual window).
	quietDays := days
	for i := range spec.bursts {
		quietDays -= float64(spec.bursts[i].DurWin+g9BurstScoreSlack) * interval / 86400
	}
	if quietDays > 0 {
		rr.QuietEventsPerDay = float64(e.quietEvents) * float64(e.solveEvery) / quietDays
	}
	post := float64(spec.totalWindows - g9Warmup)
	if post > 0 {
		rr.StormDuty = float64(len(e.stormWins)) / post
	}

	// Substrate (c) flag totals + false flags (outside every burst span,
	// or on non-target series inside one).
	targetsAt := func(w uint32) map[uint64]bool {
		out := map[uint64]bool{}
		for i := range spec.bursts {
			b := &spec.bursts[i]
			if b.inScoreSpan(w) {
				for _, idx := range b.Targets {
					out[e.world.ids[idx]] = true
				}
			}
		}
		return out
	}
	for w, ids := range e.driftFlags {
		rr.DriftFlags += len(ids)
		tg := targetsAt(w)
		for _, id := range ids {
			if !tg[id] {
				rr.DriftFalse++
			}
		}
	}

	// Per-burst detector scoring.
	for bi := range spec.bursts {
		b := &spec.bursts[bi]
		tset := map[uint64]bool{}
		for _, idx := range b.Targets {
			tset[e.world.ids[idx]] = true
		}
		br := g9BurstResult{
			Magnitude: b.Magnitude, StartWin: b.StartWin, DurWin: b.DurWin,
			Targets: len(b.Targets), Det: map[string]g9Det{},
		}
		startSec := float64(b.StartWin) * interval
		endSec := float64(b.StartWin+b.DurWin+g9BurstScoreSlack) * interval

		// CS substrate (a).
		var det g9Det
		spur := map[uint64]bool{}
		for _, ev := range e.events {
			if !b.inScoreSpan(ev.Win) {
				continue
			}
			hit := false
			for _, id := range ev.IDs {
				if tset[id] {
					hit = true
				} else {
					spur[id] = true
				}
			}
			if hit && !det.Detected {
				det.Detected = true
				det.LagSec = float64(ev.Win-b.StartWin) * interval
			}
		}
		br.Det["cs"] = det
		br.CSSpurious = len(spur)

		// Substrate (c).
		det = g9Det{}
		cspur := map[uint64]bool{}
		keyframesInSpan, keyframesHit := 0, 0
		for w, ids := range e.driftFlags {
			if !b.inScoreSpan(w) {
				continue
			}
			hitThis := false
			for _, id := range ids {
				if tset[id] {
					hitThis = true
					if !det.Detected || float64(w-b.StartWin)*interval < det.LagSec {
						det.Detected = true
						det.LagSec = float64(w-b.StartWin) * interval
					}
				} else {
					cspur[id] = true
				}
			}
			if hitThis {
				keyframesHit++
			}
		}
		if cfg.KeyframeEvery > 0 {
			for w := b.StartWin; w < b.StartWin+b.DurWin; w++ {
				if w%uint32(cfg.KeyframeEvery) == 0 {
					keyframesInSpan++
				}
			}
		}
		if keyframesInSpan > 0 {
			// span-level persistence, aggregated below.
			rr.CPersist += float64(keyframesHit) / float64(keyframesInSpan) / float64(len(spec.bursts))
		}
		br.Det["c"] = det
		br.CSpurious = len(cspur)

		// Storm/FALLBACK.
		det = g9Det{}
		for _, w := range e.stormWins {
			if !b.inScoreSpan(w) {
				continue
			}
			for _, id := range e.stormIDs[w] {
				if tset[id] {
					det.Detected = true
					det.LagSec = float64(w-b.StartWin) * interval
					break
				}
			}
			if det.Detected {
				break
			}
		}
		br.Det["storm"] = det

		// Classical cadence samplers.
		for _, s := range e.samplers {
			det = g9Det{}
			for _, f := range s.flags {
				if f.TSec < startSec || f.TSec >= endSec {
					continue
				}
				if tset[e.world.ids[f.Idx]] {
					det.Detected = true
					det.LagSec = f.TSec - startSec
					break
				}
			}
			br.Det[s.name] = det
		}

		// Routed-exact: detected iff a target series is in the routed set
		// (then at the first injected sample, one 10 s window of lag —
		// deliberately generous to the classical config).
		det = g9Det{}
		for _, idx := range b.Targets {
			if e.routed != nil && e.routed[idx] {
				det.Detected = true
				det.LagSec = interval
				break
			}
		}
		br.Det["routed"] = det

		rr.Bursts = append(rr.Bursts, br)
	}

	if e.routed != nil {
		for _, idx := range spec.burstTargetUnion() {
			if e.routed[idx] {
				rr.RoutedHasTarget = true
			}
		}
	}

	// Byte ledger.
	nRaw := float64(len(e.world.names))
	e.bytes["gorilla_10s"] = int64(nRaw * float64(spec.totalWindows) * g9GorillaPerSamp)
	for _, s := range e.samplers {
		e.bytes["dumb_"+s.name] = s.bytes
	}
	routedN := math.Ceil(g9RoutedFrac * nRaw)
	e.bytes["dumb_routed"] = e.bytes["pal_golden"] + e.bytes["pal_kdelta"] +
		int64(routedN*float64(spec.totalWindows)*g9GorillaPerSamp)
	for k, v := range e.bytes {
		rr.BytesPerDay[k] = float64(v) / days
	}
	return rr
}

func (s *g9Spec) burstTargetUnion() []int {
	seen := map[int]bool{}
	var out []int
	for i := range s.bursts {
		for _, idx := range s.bursts[i].Targets {
			if !seen[idx] {
				seen[idx] = true
				out = append(out, idx)
			}
		}
	}
	return out
}

// runG9Scenario runs every spec of the configured scenario and writes one
// JSON result per spec.
func runG9Scenario(cfg g9Config) error {
	world := newG9World(&cfg)
	specs, err := g9Specs(&cfg, world)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		// A fresh world per spec keeps sub-runs (s3 magnitudes)
		// independent and identically seeded.
		world := newG9World(&cfg)
		eng := newG9Engine(&cfg, world, true)
		start := time.Now()
		if err := eng.run(spec); err != nil {
			return err
		}
		rr := eng.score(spec, spec.magLabel)
		name := fmt.Sprintf("%s%s-%s-seed%d.json", cfg.Scenario, dashIf(spec.magLabel), cfg.Label, cfg.Seed)
		path := filepath.Join(cfg.Out, name)
		blob, err := json.MarshalIndent(rr, "", " ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, blob, 0o644); err != nil {
			return err
		}
		det := map[string]int{}
		for _, b := range rr.Bursts {
			for k, d := range b.Det {
				if d.Detected {
					det[k]++
				}
			}
		}
		log.Printf("plsim g9: %s%s label=%s seed=%d windows=%d solves=%d events=%d quiet-events/day=%.2f det=%v storm-duty=%.2f frozen-max=%d (%.1fs) -> %s",
			cfg.Scenario, dashIf(spec.magLabel), cfg.Label, cfg.Seed, spec.totalWindows,
			rr.Solves, rr.Events, rr.QuietEventsPerDay, det, rr.StormDuty, rr.FrozenMax,
			time.Since(start).Seconds(), path)
	}
	return nil
}

func dashIf(s string) string {
	if s == "" {
		return ""
	}
	return "-m" + s
}
