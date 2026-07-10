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
	"math/rand"
	"time"

	"github.com/passiveintent/Palimpsest/internal/audit"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// mergedTierViewID is a fixed, out-of-band ViewID for the ADR-015
// merged-tier weak-observer simulation, chosen well past any index a
// --views-declared view could take, so it never collides with the normal
// simulation's own (emitter, view) pipelines.
const mergedTierViewID = uint16(9000)

// mergedSimConfig configures newMergedSim.
type mergedSimConfig struct {
	TenantKey     []byte
	ShardID       uint64
	M, D, Bits    int
	KeyframeEvery int
	GoldenEvery   int
	SeriesTTL     time.Duration

	GroupSize      int // correlated-group member count (ADR-014 AZ-outage shape)
	Scattered      int // scattered singleton anomaly count
	Emitters       int // A: simulated weak observers fused at per-agent SNR ~1.0
	InjectedWindow uint32
	Seed           int64
}

// mergedSim drives the ADR-015 correlated-group-across-many-weak-emitters
// scenario: a genesis emitter births and steadily reports GroupSize+
// Scattered (plus a modest quiet background) series every window; at
// InjectedWindow, Emitters weak observers each contribute one window's
// worth of per-agent-SNR~1.0 signal for the shared group+scattered
// anomaly, fused via the same additive merge a real fleet-scale incident
// relies on (ADR-013) -- see docs/adr/ADR-015-merged-tier.md.
//
// The aggregate-equivalence trick (sketch additivity: sum-of-agents ==
// agents-of-sum) lets this draw the equivalent COMBINED per-agent noise
// directly -- one sketch.Accumulator per simulated agent, not a full
// Tracker/Predictor pipeline each: a weak observer's only job is to
// contribute one window's signal for an already-known series set, not to
// track open-loop prediction state across many windows the way a real
// agent (or this file's own genesis pipeline) does.
type mergedSim struct {
	cfg       mergedSimConfig
	genesis   *pipeline
	names     []string
	group     []string
	scattered []string
	shifts    map[string]float64
	rng       *rand.Rand
	seed      uint64
}

func newMergedSim(cfg mergedSimConfig) *mergedSim {
	rng := rand.New(rand.NewSource(cfg.Seed))

	group := make([]string, cfg.GroupSize)
	for i := range group {
		group[i] = fmt.Sprintf("plsim_merged_az_%04d|region=merged-az-east-1,cluster=prod|agg=sum", i)
	}
	scattered := make([]string, cfg.Scattered)
	for i := range scattered {
		scattered[i] = fmt.Sprintf("plsim_merged_scattered_%03d|uid=%d,cluster=prod|agg=sum", i, i)
	}
	names := make([]string, 0, len(group)+len(scattered)+cfg.GroupSize/10+1)
	names = append(names, group...)
	names = append(names, scattered...)
	quiet := cfg.GroupSize/10 + 1 // a modest quiet background, not a full N=50000 fleet
	for i := 0; i < quiet; i++ {
		names = append(names, fmt.Sprintf("plsim_merged_quiet_%05d|shard=merged|agg=sum", i))
	}

	shifts := make(map[string]float64, len(group)+len(scattered))
	for _, n := range group {
		shifts[n] = 0.5 + rng.Float64()*1.5 // [0.5, 2.0) -- ADR-014's AZ-group amplitude range
	}
	for _, n := range scattered {
		shifts[n] = 2.0 + rng.Float64()*6.0 // [2.0, 8.0) -- ADR-014's scattered amplitude range
	}

	genesis := newPipeline(pipelineConfig{
		TenantKey: cfg.TenantKey, ShardID: cfg.ShardID, EmitterID: mergedGenesisEmitterID(cfg.Seed),
		ViewID: mergedTierViewID, Epoch: 0, M: cfg.M, D: cfg.D, Bits: cfg.Bits,
		KeyframeEvery: cfg.KeyframeEvery, GoldenEvery: cfg.GoldenEvery, SeriesTTL: cfg.SeriesTTL,
	})
	seed := sketch.DeriveEphemeralSeed(cfg.TenantKey, cfg.ShardID, 0, mergedTierViewID)

	return &mergedSim{cfg: cfg, genesis: genesis, names: names, group: group, scattered: scattered, shifts: shifts, rng: rng, seed: seed}
}

// mergedGenesisEmitterID derives a stable emitter ID for the merged-tier
// scenario's genesis emitter, distinct from every normal-simulation
// emitter (main.go's emitterID) and from every weak observer below.
func mergedGenesisEmitterID(seed int64) uint64 {
	h := rand.New(rand.NewSource(seed*7919 + 424242))
	return h.Uint64()
}

// mergedWeakEmitterID derives a stable, distinct emitter ID for weak
// observer `agent` (0-indexed).
func mergedWeakEmitterID(seed int64, agent int) uint64 {
	h := rand.New(rand.NewSource(seed*7919 + 909090 + int64(agent)))
	return h.Uint64()
}

// Step emits the genesis emitter's steady baseline frame for this window
// (keeping the dictionary populated and the quiet background quiet), then,
// at cfg.InjectedWindow only, generates cfg.Emitters weak-observer frames
// fusing the shared group+scattered anomaly.
func (s *mergedSim) Step(window uint32, now time.Time, ch *channel) error {
	values := make(map[string]float64, len(s.names))
	for _, n := range s.names {
		values[n] = 100.0
	}
	if err := ch.Send(s.genesis.flush(now, window, values)); err != nil {
		return fmt.Errorf("plsim: merged-tier genesis emitter: %w", err)
	}

	if s.cfg.Emitters <= 0 || window != s.cfg.InjectedWindow {
		return nil
	}

	for agent := 0; agent < s.cfg.Emitters; agent++ {
		acc := sketch.NewAccumulator(sketch.Params{M: s.cfg.M, D: s.cfg.D, Seed: s.seed, Bits: s.cfg.Bits})
		for name, trueShift := range s.shifts {
			sigmaAgent := trueShift / float64(s.cfg.Emitters) // per-agent SNR ~1.0, see ADR-015
			v := trueShift/float64(s.cfg.Emitters) + s.rng.NormFloat64()*sigmaAgent
			acc.Update([]byte(name), v)
		}
		y, _ := acc.Flush()
		scale := wire.AdaptiveScale(y, s.cfg.Bits)
		payload, err := wire.Quantize(y, uint8(s.cfg.Bits), scale)
		if err != nil {
			return fmt.Errorf("plsim: merged-tier weak observer %d: %w", agent, err)
		}
		f := &wire.Frame{
			Magic: wire.Magic, Version: wire.Version, FrameType: wire.FrameTypeResidual,
			EmitterID: mergedWeakEmitterID(s.cfg.Seed, agent), ShardID: s.cfg.ShardID, Epoch: 0, Seq: window,
			ViewID: mergedTierViewID, M: uint32(s.cfg.M), D: uint8(s.cfg.D), Bits: uint8(s.cfg.Bits),
			QuantScale: scale, Payload: payload,
		}
		if err := ch.Send(f); err != nil {
			return fmt.Errorf("plsim: merged-tier weak observer %d: %w", agent, err)
		}
	}
	return nil
}

// TruthEvents returns one ground-truth record per group/scattered series,
// written once at startup like every other scenario's truth (main.go).
func (s *mergedSim) TruthEvents(runStart time.Time, interval time.Duration) []audit.TruthEvent {
	if s.cfg.Emitters <= 0 {
		return nil
	}
	at := runStart.Add(time.Duration(s.cfg.InjectedWindow) * interval)
	events := make([]audit.TruthEvent, 0, len(s.group)+len(s.scattered))
	for _, n := range append(append([]string(nil), s.group...), s.scattered...) {
		events = append(events, audit.TruthEvent{
			Type:          audit.TruthAnomaly,
			SeriesID:      sketch.SeriesID([]byte(n)),
			SeriesName:    n,
			ShardID:       s.cfg.ShardID,
			WindowStart:   s.cfg.InjectedWindow,
			WindowEnd:     s.cfg.InjectedWindow,
			ExpectedValue: 100.0 + s.shifts[n],
			Magnitude:     s.shifts[n],
			Timestamp:     at,
		})
	}
	return events
}
