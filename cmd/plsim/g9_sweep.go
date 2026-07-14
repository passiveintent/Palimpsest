/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"github.com/passiveintent/Palimpsest/pkg/recover"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// runG9Sweep re-draws the docs/DEADZONE.md map with the G9 fixes wired in
// (F1 normalized gate + F2 magnitude-independent lambda; F3 does not apply
// to a single-window injection). The existing --deadzone sweep is left
// untouched per the gate protocol; this is a parallel implementation of
// the same trial shape with the post-fix decode path. sigma_hat per trial
// comes from the trial's own quiet warm-up windows (median of per-window
// MAD estimates, per-series units, floored at g9SigmaFloor for the
// degenerate sigma=0 column).
func runG9Sweep(cfg g9Config) error {
	if !cfg.F1 || !cfg.F2 {
		return fmt.Errorf("--g9-scenario sweep expects -g9-fix-f1 and -g9-fix-f2 (it maps the post-fix decode path)")
	}
	noises := []float64{0, 0.05, 0.1, 0.2, 0.4, 0.8, 1.6, 3.2}
	mags := []float64{0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 12.8, 25.6, 51.2, 102.4}
	trials := cfg.Trials
	if trials > 25 {
		trials = 15
	}

	params := sketch.Params{M: cfg.M, D: cfg.D, Bits: cfg.Bits,
		Seed: sketch.DeriveEphemeralSeed(cfg.TenantKey, cfg.ShardID, 0, g9ViewID+1)}
	acc := sketch.NewAccumulator(params)
	dict := recover.NewDictionary()

	bgIDs := make([]uint64, cfg.Series)
	for i := range bgIDs {
		name := []byte(fmt.Sprintf("plsim_g9sweep_bg_%05d|shard=g9|agg=sum", i))
		acc.Update(name, 0)
		bgIDs[i] = sketch.SeriesID(name)
		dict.ApplyDelta(wire.DictDelta{ID: bgIDs[i], Name: name})
	}
	targetName := []byte("plsim_g9sweep_payment_errors|service=payments|agg=sum")
	acc.Update(targetName, 0)
	targetID := sketch.SeriesID(targetName)
	dict.ApplyDelta(wire.DictDelta{ID: targetID, Name: targetName})
	acc.Flush()

	n := cfg.Series + 1
	sqrtN := math.Sqrt(float64(n))
	lnTerm := math.Sqrt(2 * math.Log(float64(n)))

	cells := make([][]*deadzoneCell, len(noises))
	for ni, noise := range noises {
		cells[ni] = make([]*deadzoneCell, len(mags))
		for mi, mag := range mags {
			cell := &deadzoneCell{Noise: noise, Magnitude: mag}
			cellIdx := ni*len(mags) + mi
			for trial := 0; trial < trials; trial++ {
				rng := rand.New(rand.NewSource(cfg.Seed + int64(cellIdx)*1_000_003 + int64(trial)*7_919))
				storm := sketch.NewStormDetector(g9StormWindow, g9StormMult)

				quiet := make([]float64, 0, g9StormWindow)
				sigmaQ := recover.NewQuietSigma(g9SigmaWindow)
				for w := 0; w < g9StormWindow; w++ {
					if noise > 0 {
						for _, id := range bgIDs {
							acc.UpdateByID(id, rng.NormFloat64()*noise)
						}
						acc.UpdateByID(targetID, rng.NormFloat64()*noise)
					}
					y, energy := acc.Flush()
					storm.Trigger(energy)
					quiet = append(quiet, energy)
					sigmaQ.Observe(recover.MADSigma(y) * math.Sqrt(float64(cfg.M)/float64(n)))
				}
				sh, _ := sigmaQ.Estimate()
				sh = math.Max(sh, g9SigmaFloor)

				targetResidual := mag
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
				rankTopK := worse < g9TopK

				if med := medianOf(quiet); med > 0 {
					cell.HeadroomSum += energy / (med * g9StormMult)
					cell.HeadroomN++
				}

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
					Iters: g9FISTAIters, Lambda: g9LambdaMult,
					PowerIters: g9FISTAPower, Threshold: g9FISTAThreshold,
					LambdaAbs: cfg.Kappa * sh * lnTerm,
				})
				if err != nil {
					return err
				}
				gate := cfg.CGate * sqrtN * sh

				supportHit := false
				for i, id := range res.SupportIDs {
					if id == targetID {
						supportHit = true
						cell.ValErrSum += math.Abs(res.Values[i] - targetResidual)
					} else {
						cell.SpuriousSum++
					}
				}
				gated := res.Residual > gate
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
				if (stormTriggered && rankTopK) || (!stormTriggered && recovered) {
					cell.Detected++
				}
			}
			cells[ni][mi] = cell
		}
		log.Printf("plsim g9 sweep: noise=%g done (%d/%d)", noise, ni+1, len(noises))
	}

	csvPath := filepath.Join(cfg.Out, fmt.Sprintf("sweep-%s-seed%d.csv", cfg.Label, cfg.Seed))
	if err := os.WriteFile(csvPath, []byte(deadzoneCSV(cells)), 0o644); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Post-fix dead-zone sweep (G9: F1 c_gate=%g, F2 kappa=%g, seed %d, %d trials/cell)\n\n",
		cfg.CGate, cfg.Kappa, cfg.Seed, trials)
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
	mdPath := filepath.Join(cfg.Out, fmt.Sprintf("sweep-%s-seed%d.md", cfg.Label, cfg.Seed))
	if err := os.WriteFile(mdPath, []byte(b.String()), 0o644); err != nil {
		return err
	}
	for _, line := range deadzoneBoundaryLines(cells) {
		log.Printf("plsim g9 sweep: %s", line)
	}
	log.Printf("plsim g9 sweep: wrote %s, %s", csvPath, mdPath)
	return nil
}
