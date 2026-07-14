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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/recover"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// runG9Calib is Phase 2 of Gate G9 (docs/ADR-016-cs-verdict.md): calibrate
// c_gate and kappa on the calibration set ONLY (quiet traffic, seeds
// {100,101}; anomalies at non-confirmatory magnitudes are checked
// separately via --g9-scenario calib-anom). One quiet pass is captured
// through the real pipeline; each kappa in the grid is then solved against
// the captured windows, and every c_gate in the grid is evaluated against
// each kappa's (support, residual-ratio) trace. Output: a calibration
// curve CSV per seed. The selection rule is locked in the ADR: smallest
// kappa whose best c_gate meets <= 0.5 events/deployment-day, then the
// smallest such c_gate.
func runG9Calib(cfg g9Config) error {
	world := newG9World(&cfg)
	eng := newG9Engine(&cfg, world, false)
	spec := g9Spec{totalWindows: uint32(cfg.Days * 86400 / cfg.Interval.Seconds())}

	// Capture pass: reuse the engine's world/pipeline loop but store the
	// dequantized RESIDUAL payload and rolling sigma_hat of every window
	// the calibration would solve (every 2nd eligible window).
	type calWin struct {
		y     []float64
		sigma float64
	}
	var captured []calWin

	interval := cfg.Interval.Seconds()
	simStart := time.Unix(1_700_000_000, 0)
	n := len(world.names)
	solveEvery := cfg.SolveEvery
	if solveEvery <= 0 {
		solveEvery = 2
	}
	sigmaQ := recover.NewQuietSigma(g9SigmaWindow)
	for w := uint32(0); w < spec.totalWindows; w++ {
		now := simStart.Add(time.Duration(w) * cfg.Interval)
		world.valuesAt(float64(w)*interval, world.rng, eng.vals)
		for i, name := range world.names {
			eng.valueMap[name] = eng.vals[i]
		}
		frame := eng.pipe.flush(now, w, eng.valueMap)
		if frame.FrameType != wire.FrameTypeResidual {
			continue
		}
		y, err := wire.Dequantize(frame.Payload, frame.M, frame.Bits, frame.QuantScale)
		if err != nil {
			return err
		}
		sig := recover.MADSigma(y) * math.Sqrt(float64(cfg.M)/float64(n))
		if w >= g9Warmup && w%uint32(solveEvery) == 0 {
			sh, _ := sigmaQ.Estimate()
			captured = append(captured, calWin{y: y, sigma: math.Max(sh, g9SigmaFloor)})
		}
		// Quiet run: every window feeds the estimator (locked: the
		// calibration set is quiet by construction).
		sigmaQ.Observe(sig)
	}

	days := float64(spec.totalWindows) * interval / 86400
	sqrtN := math.Sqrt(float64(n))
	lnTerm := math.Sqrt(2 * math.Log(float64(n)))

	var b strings.Builder
	b.WriteString("kappa,c_gate,quiet_solves,support_nonempty,events,events_per_day,mean_support,mean_ratio\n")
	log.Printf("plsim g9 calib: seed=%d captured %d windows (%.2f days), kappa grid %v, c_gate grid %v",
		cfg.Seed, len(captured), days, cfg.KappaGrid, cfg.CGateGrid)

	for _, kappa := range cfg.KappaGrid {
		nonEmpty := 0
		var supSum float64
		var ratioSum float64
		ratios := make([]float64, 0, len(captured))
		supports := make([]int, 0, len(captured))
		for _, cw := range captured {
			opts := recover.Options{
				Iters: g9FISTAIters, Lambda: g9LambdaMult,
				PowerIters: g9FISTAPower, Threshold: g9FISTAThreshold,
				LambdaAbs: kappa * cw.sigma * lnTerm,
			}
			r, err := recover.Recover(cw.y, eng.dict, eng.params, opts)
			if err != nil {
				return err
			}
			ratio := r.Residual / (sqrtN * cw.sigma)
			ratios = append(ratios, ratio)
			supports = append(supports, len(r.SupportIDs))
			if len(r.SupportIDs) > 0 {
				nonEmpty++
			}
			supSum += float64(len(r.SupportIDs))
			ratioSum += ratio
		}
		for _, cGate := range cfg.CGateGrid {
			events := 0
			for i := range ratios {
				if supports[i] > 0 && ratios[i] <= cGate {
					events++
				}
			}
			perDay := float64(events) * float64(solveEvery) / days
			fmt.Fprintf(&b, "%s,%s,%d,%d,%d,%.4f,%.3f,%.4f\n",
				trimFloat(kappa), trimFloat(cGate), len(captured), nonEmpty, events, perDay,
				supSum/float64(max(len(captured), 1)), ratioSum/float64(max(len(captured), 1)))
		}
		log.Printf("plsim g9 calib: kappa=%g support-nonempty %d/%d mean-ratio %.4f",
			kappa, nonEmpty, len(captured), ratioSum/float64(max(len(captured), 1)))
	}

	path := filepath.Join(cfg.Out, fmt.Sprintf("calib-seed%d.csv", cfg.Seed))
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	log.Printf("plsim g9 calib: wrote %s", path)
	return nil
}
