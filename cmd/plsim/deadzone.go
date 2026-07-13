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
	"sort"
	"strconv"
	"strings"

	"github.com/passiveintent/Palimpsest/pkg/recover"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// The --deadzone sweep empirically hunts the gap between Palimpsest's two
// Layer-2 safety nets (see docs/DEADZONE.md for the resulting map):
//
//   - ADR-004's storm fallback trips on *global* sketch energy: a whole AZ
//     dying spikes ‖y‖²/m far past the rolling median and the encoder
//     degrades to exact heavy-hitters. A single low-volume service's error
//     counter creeping from 0.01% to 0.5% moves that global number by
//     essentially nothing.
//   - ADR-002's FISTA recovery finds an anomaly only if its residual
//     survives L1 shrinkage, the post-solve support threshold, RESIDUAL
//     quantization, and dilution by every other series' background jitter
//     — and palimpsestd additionally suppresses deviation events entirely
//     when the solve residual exceeds its --max-residual gate.
//
// Between those two lies a dead zone: anomalies too small to trip the
// fallback breaker AND too small to survive lossy compressed-sensing
// recovery. This mode injects a single synthetic anomaly of magnitude ε
// into a fleet of `--deadzone-background` series each jittering with
// per-window Gaussian residual noise σ, runs the real production path
// (sketch.Accumulator → wire.AdaptiveScale/Quantize/Dequantize →
// recover.Recover) plus the real sketch.StormDetector at production
// defaults, and maps recovery probability over the (σ, ε) grid.
//
// Residuals are injected directly (the Tracker/Hold layer is bypassed):
// for this question the predictor is an identity — what reaches the sketch
// IS the residual, and the sweep parameterizes it explicitly. Keyframe
// re-basing effects (ADR-003) and substrate (c)/(d) detections are out of
// scope here; docs/DEADZONE.md discusses what they do and don't catch.
type deadzoneFlags struct {
	enabled bool
	out     string

	background int
	magnitudes string
	noises     string
	trials     int

	stormWindow     int
	stormMultiplier float64
	fallbackTopK    int

	fistaIters      int
	fistaLambda     float64
	fistaPowerIters int
	fistaThreshold  float64
	maxResidual     float64
}

// registerDeadzoneFlags declares the --deadzone* flag set. Every default
// that models a production knob is copied from where that knob really
// lives: storm window/multiplier/topk from otel/processor/csresidual
// (stormHistoryWindow, factory.go's defaults), FISTA and the residual gate
// from cmd/palimpsestd's flag defaults.
func registerDeadzoneFlags(dz *deadzoneFlags) {
	flag.BoolVar(&dz.enabled, "deadzone", false, "run the ADR-002/ADR-004 dead-zone sweep (single small anomaly diluted in per-series noise, mapped over a magnitude x noise grid) instead of the frame-generating simulation, then exit; uses --m/--d/--bits/--seed/--shard-id/--tenant-key and ignores the workload flags")
	flag.StringVar(&dz.out, "deadzone-out", "./deadzone", "directory to write deadzone.csv, deadzone.svg and deadzone-summary.md")
	flag.IntVar(&dz.background, "deadzone-background", 900, "background series diluting the target (default matches plsim's default fleet: 300 logical groups x 3 aggregates)")
	flag.StringVar(&dz.magnitudes, "deadzone-magnitudes", "0.1,0.2,0.4,0.8,1.6,3.2,6.4,12.8,25.6,51.2,102.4", "comma-separated injected anomaly magnitudes, in absolute residual units")
	flag.StringVar(&dz.noises, "deadzone-noises", "0,0.05,0.1,0.2,0.4,0.8,1.6,3.2", "comma-separated per-series background noise levels sigma (Gaussian, redrawn every window), in absolute residual units")
	flag.IntVar(&dz.trials, "deadzone-trials", 25, "independent trials per (noise, magnitude) cell")
	flag.IntVar(&dz.stormWindow, "deadzone-storm-window", 30, "StormDetector rolling-median window, in flush windows (otel/processor/csresidual's stormHistoryWindow)")
	flag.Float64Var(&dz.stormMultiplier, "deadzone-storm-multiplier", 25, "StormDetector energy multiplier (csresidual factory default storm.energy_multiplier)")
	flag.IntVar(&dz.fallbackTopK, "deadzone-fallback-topk", 100, "FALLBACK-frame heavy-hitter count (csresidual factory default storm.fallback_topk)")
	flag.IntVar(&dz.fistaIters, "deadzone-fista-iters", 350, "FISTA iteration count (palimpsestd --fista-iters default)")
	flag.Float64Var(&dz.fistaLambda, "deadzone-fista-lambda", 0.05, "FISTA L1 penalty weight (palimpsestd --fista-lambda default)")
	flag.IntVar(&dz.fistaPowerIters, "deadzone-fista-power-iters", 50, "FISTA Lipschitz power-iteration count (palimpsestd --fista-power-iters default)")
	flag.Float64Var(&dz.fistaThreshold, "deadzone-fista-threshold", 0.3, "post-FISTA support cutoff (palimpsestd --fista-threshold default)")
	flag.Float64Var(&dz.maxResidual, "deadzone-max-residual", 5.0, "solve-residual gate above which palimpsestd suppresses deviation events (palimpsestd --max-residual default)")
}

// deadzoneViewID keeps the sweep's derived sketch seed out-of-band of any
// normal simulation view, mirroring merged.go's mergedTierViewID.
const deadzoneViewID = uint16(9100)

// deadzoneConfig is one sweep's full resolved configuration (flags merged
// with the shared --m/--d/--bits/--seed sketch parameters).
type deadzoneConfig struct {
	M, D, Bits int
	Seed       int64
	SketchSeed uint64

	Background int
	Magnitudes []float64 // ascending
	Noises     []float64 // ascending
	Trials     int

	StormWindow     int
	StormMultiplier float64
	FallbackTopK    int

	FISTAIters      int
	FISTALambda     float64
	FISTAPowerIters int
	FISTAThreshold  float64
	MaxResidual     float64
}

// deadzoneCell accumulates one (noise, magnitude) grid cell's trial
// outcomes. Counters are per-trial unless noted.
type deadzoneCell struct {
	Noise     float64
	Magnitude float64

	Trials      int
	Storms      int     // StormDetector tripped on the anomaly window
	RankTopK    int     // target's |residual| ranked within FallbackTopK of all series
	SupportHits int     // FISTA support contained the target (solver-level success)
	Gated       int     // solve residual exceeded MaxResidual (palimpsestd emits nothing)
	Recovered   int     // SupportHit AND not Gated: a deviation event actually emits
	Detected    int     // operational outcome: storm ? RankTopK : Recovered
	SpuriousSum int     // non-target support entries, summed over trials
	ValErrSum   float64 // |debiased - injected residual|, summed over SupportHits
	ResidualSum float64
	HeadroomSum float64 // anomaly-window energy / storm trigger bar, summed
	HeadroomN   int     // trials with a nonzero quiet median (headroom defined)
}

func (c *deadzoneCell) rate(n int) float64 {
	if c.Trials == 0 {
		return 0
	}
	return float64(n) / float64(c.Trials)
}

func (c *deadzoneCell) StormRate() float64     { return c.rate(c.Storms) }
func (c *deadzoneCell) RankTopKRate() float64  { return c.rate(c.RankTopK) }
func (c *deadzoneCell) SupportRate() float64   { return c.rate(c.SupportHits) }
func (c *deadzoneCell) GatedRate() float64     { return c.rate(c.Gated) }
func (c *deadzoneCell) RecoveryRate() float64  { return c.rate(c.Recovered) }
func (c *deadzoneCell) DetectionRate() float64 { return c.rate(c.Detected) }

// MeanValErr is the mean absolute error of the debiased value versus the
// injected residual, over the trials where the target made the support.
// NaN when it never did.
func (c *deadzoneCell) MeanValErr() float64 {
	if c.SupportHits == 0 {
		return math.NaN()
	}
	return c.ValErrSum / float64(c.SupportHits)
}

// MeanHeadroom is the mean of energy/(median*multiplier) on the anomaly
// window — how far (as a fraction) the window got toward the storm trigger
// bar. NaN when the quiet median was zero in every trial (σ=0).
func (c *deadzoneCell) MeanHeadroom() float64 {
	if c.HeadroomN == 0 {
		return math.NaN()
	}
	return c.HeadroomSum / float64(c.HeadroomN)
}

// runDeadzone is --deadzone's entry point: parse and validate the grid,
// run the sweep, and write deadzone.csv / deadzone.svg /
// deadzone-summary.md into --deadzone-out.
func runDeadzone(f flags) error {
	mags, err := parseDeadzoneGrid(f.dz.magnitudes, false)
	if err != nil {
		return fmt.Errorf("--deadzone-magnitudes: %w", err)
	}
	noises, err := parseDeadzoneGrid(f.dz.noises, true)
	if err != nil {
		return fmt.Errorf("--deadzone-noises: %w", err)
	}

	cfg := deadzoneConfig{
		M: f.m, D: f.d, Bits: f.bits, Seed: f.seed,
		SketchSeed: sketch.DeriveEphemeralSeed([]byte(f.tenantKey), f.shardID, 0, deadzoneViewID),
		Background: f.dz.background,
		Magnitudes: mags,
		Noises:     noises,
		Trials:     f.dz.trials,

		StormWindow:     f.dz.stormWindow,
		StormMultiplier: f.dz.stormMultiplier,
		FallbackTopK:    f.dz.fallbackTopK,

		FISTAIters:      f.dz.fistaIters,
		FISTALambda:     f.dz.fistaLambda,
		FISTAPowerIters: f.dz.fistaPowerIters,
		FISTAThreshold:  f.dz.fistaThreshold,
		MaxResidual:     f.dz.maxResidual,
	}
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("deadzone: %w", err)
	}

	log.Printf("plsim: dead-zone sweep started (m=%d d=%d bits=%d background=%d cells=%dx%d trials=%d storm=%gx/%dw topk=%d fista iters=%d lambda=%g threshold=%g gate=%g)",
		cfg.M, cfg.D, cfg.Bits, cfg.Background, len(cfg.Noises), len(cfg.Magnitudes), cfg.Trials,
		cfg.StormMultiplier, cfg.StormWindow, cfg.FallbackTopK,
		cfg.FISTAIters, cfg.FISTALambda, cfg.FISTAThreshold, cfg.MaxResidual)

	cells, err := runDeadzoneSweep(cfg)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(f.dz.out, 0o755); err != nil {
		return fmt.Errorf("creating --deadzone-out %q: %w", f.dz.out, err)
	}
	csvPath := filepath.Join(f.dz.out, "deadzone.csv")
	if err := os.WriteFile(csvPath, []byte(deadzoneCSV(cells)), 0o644); err != nil {
		return err
	}
	svgPath := filepath.Join(f.dz.out, "deadzone.svg")
	if err := os.WriteFile(svgPath, []byte(renderDeadzoneSVG(cfg, cells)), 0o644); err != nil {
		return err
	}
	summaryPath := filepath.Join(f.dz.out, "deadzone-summary.md")
	if err := os.WriteFile(summaryPath, []byte(deadzoneSummaryMD(cfg, cells)), 0o644); err != nil {
		return err
	}

	for _, line := range deadzoneBoundaryLines(cells) {
		log.Printf("plsim: %s", line)
	}
	log.Printf("plsim: dead-zone sweep complete: %s, %s, %s", csvPath, svgPath, summaryPath)
	return nil
}

func (cfg deadzoneConfig) validate() error {
	switch {
	case cfg.M <= 0 || cfg.D <= 0:
		return fmt.Errorf("--m and --d must be positive")
	case cfg.Bits != 8 && cfg.Bits != 16:
		return fmt.Errorf("--bits must be 8 or 16")
	case cfg.Background <= 0:
		return fmt.Errorf("--deadzone-background must be positive")
	case cfg.Trials <= 0:
		return fmt.Errorf("--deadzone-trials must be positive")
	case cfg.StormWindow <= 0:
		return fmt.Errorf("--deadzone-storm-window must be positive")
	case cfg.StormMultiplier <= 1:
		return fmt.Errorf("--deadzone-storm-multiplier must be > 1")
	case cfg.FallbackTopK <= 0:
		return fmt.Errorf("--deadzone-fallback-topk must be positive")
	case cfg.FISTAIters <= 0 || cfg.FISTAIters > 350:
		return fmt.Errorf("--deadzone-fista-iters out of range (1..350)")
	case cfg.FISTAPowerIters <= 0:
		return fmt.Errorf("--deadzone-fista-power-iters must be positive")
	case cfg.FISTAThreshold <= 0:
		return fmt.Errorf("--deadzone-fista-threshold must be positive")
	}
	return nil
}

// parseDeadzoneGrid parses a comma-separated float list into a sorted,
// deduplicated ascending grid. Magnitudes must be strictly positive;
// noises (allowZero) may include 0 for the perfectly-quiet column.
func parseDeadzoneGrid(spec string, allowZero bool) ([]float64, error) {
	var out []float64
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing %q: %w", part, err)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || (!allowZero && v == 0) {
			return nil, fmt.Errorf("value %q out of range", part)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty grid")
	}
	sort.Float64s(out)
	dedup := out[:1]
	for _, v := range out[1:] {
		if v != dedup[len(dedup)-1] {
			dedup = append(dedup, v)
		}
	}
	return dedup, nil
}

// runDeadzoneSweep runs the full grid and returns cells indexed
// [noise][magnitude], both ascending, matching cfg.Noises/cfg.Magnitudes.
func runDeadzoneSweep(cfg deadzoneConfig) ([][]*deadzoneCell, error) {
	params := sketch.Params{M: cfg.M, D: cfg.D, Seed: cfg.SketchSeed, Bits: cfg.Bits}
	acc := sketch.NewAccumulator(params)
	dict := recover.NewDictionary()

	// One shared universe for the whole sweep: cfg.Background diluting
	// series plus the single target. The Accumulator's bucket cache and
	// the Dictionary are deterministic in (name, seed), so both are built
	// once; Flush resets y between windows and Recover never mutates dict.
	bgIDs := make([]uint64, cfg.Background)
	for i := range bgIDs {
		name := []byte(fmt.Sprintf("plsim_deadzone_bg_%05d|shard=deadzone|agg=sum", i))
		acc.Update(name, 0) // prime the bucket cache; contributes nothing
		bgIDs[i] = sketch.SeriesID(name)
		dict.ApplyDelta(wire.DictDelta{ID: bgIDs[i], Name: name})
	}
	targetName := []byte("plsim_deadzone_payment_errors|service=payments|agg=sum")
	acc.Update(targetName, 0)
	targetID := sketch.SeriesID(targetName)
	dict.ApplyDelta(wire.DictDelta{ID: targetID, Name: targetName})
	acc.Flush() // drop the priming zeros

	cells := make([][]*deadzoneCell, len(cfg.Noises))
	for ni, noise := range cfg.Noises {
		cells[ni] = make([]*deadzoneCell, len(cfg.Magnitudes))
		for mi, mag := range cfg.Magnitudes {
			cell := &deadzoneCell{Noise: noise, Magnitude: mag}
			cellIdx := ni*len(cfg.Magnitudes) + mi
			for trial := 0; trial < cfg.Trials; trial++ {
				if err := runDeadzoneTrial(cfg, params, acc, dict, bgIDs, targetID, cellIdx, trial, cell); err != nil {
					return nil, fmt.Errorf("deadzone cell (noise=%g, magnitude=%g) trial %d: %w", noise, mag, trial, err)
				}
			}
			cells[ni][mi] = cell
		}
		log.Printf("plsim: deadzone noise=%g done (%d/%d columns)", noise, ni+1, len(cfg.Noises))
	}
	return cells, nil
}

// runDeadzoneTrial plays one trial into cell: cfg.StormWindow quiet
// warm-up windows to establish the StormDetector's rolling median, then
// one anomaly window carrying the target's +magnitude residual, pushed
// through the lossy wire round trip and recovered.
func runDeadzoneTrial(cfg deadzoneConfig, params sketch.Params, acc *sketch.Accumulator, dict *recover.Dictionary, bgIDs []uint64, targetID uint64, cellIdx, trial int, cell *deadzoneCell) error {
	// Distinct, deterministic stream per (seed, cell, trial): the cell
	// stride (1e6+3) exceeds any trial offset so streams never collide.
	rng := rand.New(rand.NewSource(cfg.Seed + int64(cellIdx)*1_000_003 + int64(trial)*7_919))
	storm := sketch.NewStormDetector(cfg.StormWindow, cfg.StormMultiplier)

	noise := cell.Noise
	quiet := make([]float64, 0, cfg.StormWindow)
	for w := 0; w < cfg.StormWindow; w++ {
		if noise > 0 {
			for _, id := range bgIDs {
				acc.UpdateByID(id, rng.NormFloat64()*noise)
			}
			acc.UpdateByID(targetID, rng.NormFloat64()*noise)
		}
		_, energy := acc.Flush()
		storm.Trigger(energy)
		quiet = append(quiet, energy)
	}

	// Anomaly window. The target's residual is ε plus its own jitter; the
	// heavy-hitter rank mirrors Tracker.TopKResiduals' |residual| ordering
	// (what a FALLBACK frame would carry if the storm breaker tripped).
	targetResidual := cell.Magnitude
	if noise > 0 {
		targetResidual += rng.NormFloat64() * noise
	}
	absTarget := math.Abs(targetResidual)
	worse := 0
	for _, id := range bgIDs {
		r := 0.0
		if noise > 0 {
			r = rng.NormFloat64() * noise
			if math.Abs(r) > absTarget {
				worse++
			}
		}
		acc.UpdateByID(id, r)
	}
	acc.UpdateByID(targetID, targetResidual)
	y, energy := acc.Flush()
	stormTriggered := storm.Trigger(energy)
	rankTopK := worse < cfg.FallbackTopK

	if med := medianOf(quiet); med > 0 {
		cell.HeadroomSum += energy / (med * cfg.StormMultiplier)
		cell.HeadroomN++
	}

	// The lossy wire round trip a RESIDUAL frame really takes (ADR-006).
	scale := wire.AdaptiveScale(y, cfg.Bits)
	payload, err := wire.Quantize(y, uint8(cfg.Bits), scale)
	if err != nil {
		return err
	}
	yq, err := wire.Dequantize(payload, uint32(cfg.M), uint8(cfg.Bits), scale)
	if err != nil {
		return err
	}

	res, err := recover.Recover(yq, dict, params, recover.Options{
		Iters:      cfg.FISTAIters,
		Lambda:     cfg.FISTALambda,
		PowerIters: cfg.FISTAPowerIters,
		Threshold:  cfg.FISTAThreshold,
	})
	if err != nil {
		return err
	}

	supportHit := false
	for i, id := range res.SupportIDs {
		if id == targetID {
			supportHit = true
			cell.ValErrSum += math.Abs(res.Values[i] - targetResidual)
		} else {
			cell.SpuriousSum++
		}
	}
	gated := res.Residual > cfg.MaxResidual
	recovered := supportHit && !gated

	cell.Trials++
	cell.ResidualSum += res.Residual
	if stormTriggered {
		cell.Storms++
	}
	if rankTopK {
		cell.RankTopK++
	}
	if supportHit {
		cell.SupportHits++
	}
	if gated {
		cell.Gated++
	}
	if recovered {
		cell.Recovered++
	}
	// Operational outcome: on a storm window the encoder ships a FALLBACK
	// frame instead of a RESIDUAL (otel/processor/csresidual flush), so
	// detection means ranking in the heavy hitters; otherwise it means the
	// emitted deviation-event path (support hit and not residual-gated).
	if (stormTriggered && rankTopK) || (!stormTriggered && recovered) {
		cell.Detected++
	}
	return nil
}

func medianOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// deadzoneCSV renders every cell as one CSV row, noises outer, magnitudes
// inner, both ascending.
func deadzoneCSV(cells [][]*deadzoneCell) string {
	var b strings.Builder
	b.WriteString("noise,magnitude,trials,storm_rate,fallback_topk_rate,support_rate,gated_rate,recovery_rate,detection_rate,mean_value_error,mean_solve_residual,mean_spurious_support,storm_headroom\n")
	for _, col := range cells {
		for _, c := range col {
			fmt.Fprintf(&b, "%s,%s,%d,%.4f,%.4f,%.4f,%.4f,%.4f,%.4f,%s,%.4f,%.2f,%s\n",
				trimFloat(c.Noise), trimFloat(c.Magnitude), c.Trials,
				c.StormRate(), c.RankTopKRate(), c.SupportRate(), c.GatedRate(),
				c.RecoveryRate(), c.DetectionRate(),
				csvNaN(c.MeanValErr()),
				c.ResidualSum/float64(c.Trials),
				float64(c.SpuriousSum)/float64(c.Trials),
				csvNaN(c.MeanHeadroom()))
		}
	}
	return b.String()
}

func csvNaN(v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// deadzoneFloor returns the smallest magnitude whose rate clears minRate
// at that magnitude AND at every larger one in the same noise column
// (a monotone envelope from the top), or NaN if even the largest doesn't.
func deadzoneFloor(col []*deadzoneCell, rate func(*deadzoneCell) float64, minRate float64) float64 {
	floor := math.NaN()
	for i := len(col) - 1; i >= 0; i-- {
		if rate(col[i]) < minRate {
			break
		}
		floor = col[i].Magnitude
	}
	return floor
}

// deadzoneCeiling returns the largest magnitude with rate < maxRate — the
// top edge of the region that fails the given outcome — or NaN if none do.
func deadzoneCeiling(col []*deadzoneCell, rate func(*deadzoneCell) float64, maxRate float64) float64 {
	for i := len(col) - 1; i >= 0; i-- {
		if rate(col[i]) < maxRate {
			return col[i].Magnitude
		}
	}
	return math.NaN()
}

func floatOrDash(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	return trimFloat(v)
}

// deadzoneBoundaryLines summarizes each noise column's measured
// boundaries, one line per column, for the run log.
func deadzoneBoundaryLines(cells [][]*deadzoneCell) []string {
	lines := make([]string, 0, len(cells))
	for _, col := range cells {
		recFloor := deadzoneFloor(col, (*deadzoneCell).RecoveryRate, 0.9)
		detFloor := deadzoneFloor(col, (*deadzoneCell).DetectionRate, 0.9)
		stormFloor := deadzoneFloor(col, (*deadzoneCell).StormRate, 0.5)
		deadTop := deadzoneCeiling(col, (*deadzoneCell).DetectionRate, 0.5)
		lines = append(lines, fmt.Sprintf(
			"deadzone noise=%s: recovery floor (>=90%%) %s, detection floor (>=90%%) %s, storm fires from %s, undetected up to %s",
			trimFloat(col[0].Noise), floatOrDash(recFloor), floatOrDash(detFloor),
			floatOrDash(stormFloor), floatOrDash(deadTop)))
	}
	return lines
}

// deadzoneSummaryMD renders the machine-written summary that accompanies
// deadzone.csv/deadzone.svg (docs/DEADZONE.md is the curated companion).
func deadzoneSummaryMD(cfg deadzoneConfig, cells [][]*deadzoneCell) string {
	var b strings.Builder
	b.WriteString("# Dead-zone sweep summary\n\n")
	fmt.Fprintf(&b, "Generated by `plsim --deadzone` (seed %d). Full grid in `deadzone.csv`, heatmap in `deadzone.svg`; methodology and interpretation in `docs/DEADZONE.md`.\n\n", cfg.Seed)
	fmt.Fprintf(&b, "- sketch: m=%d, d=%d, bits=%d; %d background series + 1 target\n", cfg.M, cfg.D, cfg.Bits, cfg.Background)
	fmt.Fprintf(&b, "- storm: %g× rolling median over %d windows; FALLBACK top-K %d\n", cfg.StormMultiplier, cfg.StormWindow, cfg.FallbackTopK)
	fmt.Fprintf(&b, "- recovery: FISTA iters=%d λ=%g power-iters=%d threshold=%g; deviation events gated above solve residual %g\n", cfg.FISTAIters, cfg.FISTALambda, cfg.FISTAPowerIters, cfg.FISTAThreshold, cfg.MaxResidual)
	fmt.Fprintf(&b, "- %d trials per cell\n\n", cfg.Trials)
	b.WriteString("Magnitudes and noise levels are absolute residual units (value minus the Hold baseline, per window).\n\n")
	b.WriteString("| noise σ | recovery floor (≥90%) | detection floor (≥90%) | storm fires from (≥50%) | undetected up to (<50%) |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, col := range cells {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			trimFloat(col[0].Noise),
			floatOrDash(deadzoneFloor(col, (*deadzoneCell).RecoveryRate, 0.9)),
			floatOrDash(deadzoneFloor(col, (*deadzoneCell).DetectionRate, 0.9)),
			floatOrDash(deadzoneFloor(col, (*deadzoneCell).StormRate, 0.5)),
			floatOrDash(deadzoneCeiling(col, (*deadzoneCell).DetectionRate, 0.5)))
	}
	b.WriteString("\n- **recovery floor**: smallest magnitude whose deviation event reliably emits (support hit, solve residual under the gate) at this and every larger tested magnitude.\n")
	b.WriteString("- **detection floor**: same, but counting a FALLBACK heavy-hitter hit as detection when the storm breaker trips.\n")
	b.WriteString("- **storm fires from**: smallest magnitude tripping the StormDetector in ≥50% of trials (— means never, i.e. the fallback breaker offers no help in this column).\n")
	b.WriteString("- **undetected up to**: largest magnitude still missed by both paths in >50% of trials — the dead zone's measured top edge.\n")
	return b.String()
}
