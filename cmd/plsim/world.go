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
	"math"
	"math/rand"
	"time"

	"github.com/passiveintent/Palimpsest/internal/audit"
	"github.com/passiveintent/Palimpsest/pkg/sketch"
)

// aggregates are the three ADR-008 fold kinds every group reports.
var aggregates = [3]string{"sum", "count", "max"}

// instance is one simulated pod/replica within a group: never sketched
// directly (ADR-008), only its contribution to the group's folded
// sum/count/max aggregates.
type instance struct {
	baseline float64 // current true value (moves via drift/hot-instance/noise)
}

// group is one logical metric group (metric_name x logical_key), which
// folds its instances into three independently-tracked logical series
// (agg=sum, agg=count, agg=max per ADR-008's naming scheme).
type group struct {
	idx     int
	name    string // "plsim_metric_%05d"
	bornAt  uint32
	retired bool

	instances []*instance

	hot         bool
	hotIdx      int // index into instances, valid when hot
	hotAtWindow uint32

	drifting   bool
	driftDelta float64 // added to every instance's baseline each window
}

// logicalName builds the full ADR-008 name for one of this group's three
// aggregates: metric_name|shard=<shardID>|agg=<agg>.
func (g *group) logicalName(shardID uint64, agg string) string {
	return fmt.Sprintf("%s|shard=%d|agg=%s", g.name, shardID, agg)
}

// WorldConfig configures the synthetic instance/group population.
type WorldConfig struct {
	ShardID           uint64
	LogicalSeries     int
	InstancesPerGroup int
	HotGroups         int
	DriftGroups       int
	DriftDeltaPerWin  float64 // per-mille of baseline, per flush (i.e. --drift-rate/1000 * baseline)
	ChurnPerMinute    float64 // births+deaths per minute (each retirement+spawn pair counts as 2)
	IntervalSeconds   float64
	TotalWindows      int // the run's planned length; hot instances activate at TotalWindows/3
	Baseline          float64
	NoiseAmplitude    float64
	Rand              *rand.Rand
}

// World simulates LogicalSeries groups of InstancesPerGroup instances each,
// with continuous per-window dynamics (noise, drift, a hot instance per
// drift/hot group) and scheduled group-level churn (ADR-008 lifecycle:
// whole groups retiring and new ones spawning, which is what actually
// drives Tracker births/tombstones — instance-level membership within a
// live group does not, by design, since instances never enter the sketch).
type World struct {
	cfg WorldConfig

	groups      []*group
	activePlain []int // indices into groups: plain (non-hot, non-drifting) groups eligible for churn
	nextIdx     int

	churnBudget float64 // accumulates fractional churn events; >=1 triggers a replacement
}

// NewWorld builds the initial population: cfg.LogicalSeries groups, the
// first cfg.HotGroups marked hot and the next cfg.DriftGroups marked
// drifting (both counts clamped to what's available), the remainder plain
// and eligible for scheduled churn.
func NewWorld(cfg WorldConfig) *World {
	w := &World{cfg: cfg}

	hot := cfg.HotGroups
	if hot > cfg.LogicalSeries {
		hot = cfg.LogicalSeries
	}
	drift := cfg.DriftGroups
	if drift > cfg.LogicalSeries-hot {
		drift = cfg.LogicalSeries - hot
	}

	// Hot instances kick in a third of the way into the run: early enough
	// to leave time for detection (and, in a short --windows run, to
	// happen at all — a fixed absolute offset would either never trigger
	// or produce a WindowEnd < WindowStart truth record once the run is
	// shorter than that offset).
	hotAtWindow := uint32(cfg.TotalWindows / 3)

	for i := 0; i < cfg.LogicalSeries; i++ {
		g := &group{
			idx:    i,
			name:   fmt.Sprintf("plsim_metric_%05d", i),
			bornAt: 0,
			hotIdx: -1,
		}
		g.instances = make([]*instance, cfg.InstancesPerGroup)
		for j := range g.instances {
			g.instances[j] = &instance{baseline: cfg.Baseline}
		}
		switch {
		case i < hot:
			g.hot = true
			g.hotIdx = cfg.Rand.Intn(cfg.InstancesPerGroup)
			g.hotAtWindow = hotAtWindow
		case i < hot+drift:
			g.drifting = true
			g.driftDelta = cfg.Baseline * (cfg.DriftDeltaPerWin / 1000.0)
		default:
			w.activePlain = append(w.activePlain, i)
		}
		w.groups = append(w.groups, g)
	}
	w.nextIdx = cfg.LogicalSeries
	return w
}

// ReserveForAnomaly picks up to n distinct plain (non-hot, non-drifting)
// groups and removes them from the churn-eligible pool, so a scheduled
// anomaly's target group is never also retired by churn mid-scenario
// (keeping each group's ground truth unambiguous). Returns fewer than n if
// the pool is exhausted.
func (w *World) ReserveForAnomaly(n int) []*group {
	if n > len(w.activePlain) {
		n = len(w.activePlain)
	}
	out := make([]*group, 0, n)
	for i := 0; i < n; i++ {
		pick := w.cfg.Rand.Intn(len(w.activePlain))
		gi := w.activePlain[pick]
		w.activePlain = append(w.activePlain[:pick], w.activePlain[pick+1:]...)
		out = append(out, w.groups[gi])
	}
	return out
}

// Step advances every instance's baseline by one window (noise, drift,
// hot-instance activation) and applies any scheduled churn, returning the
// lifecycle TruthEvents (births/tombstones) generated this window. Callers
// read current aggregate values afterward via Aggregates.
func (w *World) Step(window uint32, now time.Time) []audit.TruthEvent {
	for _, g := range w.groups {
		if g.retired {
			continue
		}
		if g.drifting {
			for _, inst := range g.instances {
				inst.baseline += g.driftDelta
			}
		}
		if g.hot && window == g.hotAtWindow {
			g.instances[g.hotIdx].baseline = w.cfg.Baseline * 5
		}
	}

	return w.stepChurn(window, now)
}

// stepChurn accumulates a fractional churn budget from cfg.ChurnPerMinute
// and retires+spawns whole groups once it crosses 1.0, logging a
// TruthTombstone for the retired group's three series and a TruthBirth for
// the newly spawned group's three series (series IDs are a pure hash of
// the name, so World can log both directly without waiting on the
// encoder's Tracker to actually observe them).
func (w *World) stepChurn(window uint32, now time.Time) []audit.TruthEvent {
	if w.cfg.ChurnPerMinute <= 0 || len(w.activePlain) == 0 {
		return nil
	}
	// Each replacement is one death + one birth, i.e. 2 lifecycle events;
	// ChurnPerMinute already counts both, so a replacement's rate is half.
	perWindow := (w.cfg.ChurnPerMinute / 2) / 60 * w.cfg.IntervalSeconds
	w.churnBudget += perWindow

	var events []audit.TruthEvent
	for w.churnBudget >= 1 && len(w.activePlain) > 0 {
		w.churnBudget--

		pick := w.cfg.Rand.Intn(len(w.activePlain))
		gi := w.activePlain[pick]
		w.activePlain = append(w.activePlain[:pick], w.activePlain[pick+1:]...)

		old := w.groups[gi]
		old.retired = true
		events = append(events, w.lifecycleEvents(old, audit.TruthTombstone, window, now)...)

		ng := &group{
			idx:    w.nextIdx,
			name:   fmt.Sprintf("plsim_metric_%05d", w.nextIdx),
			bornAt: window,
			hotIdx: -1,
		}
		ng.instances = make([]*instance, w.cfg.InstancesPerGroup)
		for j := range ng.instances {
			ng.instances[j] = &instance{baseline: w.cfg.Baseline}
		}
		w.nextIdx++
		w.groups = append(w.groups, ng)
		w.activePlain = append(w.activePlain, len(w.groups)-1)
		events = append(events, w.lifecycleEvents(ng, audit.TruthBirth, window, now)...)
	}
	return events
}

// lifecycleEvents builds one TruthEvent per aggregate (sum/count/max) for
// g's birth or retirement at window.
func (w *World) lifecycleEvents(g *group, typ audit.TruthEventType, window uint32, now time.Time) []audit.TruthEvent {
	events := make([]audit.TruthEvent, len(aggregates))
	for i, agg := range aggregates {
		name := g.logicalName(w.cfg.ShardID, agg)
		events[i] = audit.TruthEvent{
			Type:        typ,
			SeriesID:    sketch.SeriesID([]byte(name)),
			SeriesName:  name,
			ShardID:     w.cfg.ShardID,
			WindowStart: window,
			Timestamp:   now,
		}
	}
	return events
}

// DriftTruthEvents returns one TruthDrift record per drifting group's sum
// and max aggregates (count does not drift in this model — every
// instance's baseline moves together, so count, the number of live
// instances, is unaffected), covering the whole run: drift is continuous
// from window 0, so there is no separate "episode end" the way an
// injected anomaly has one. ExpectedValue/Magnitude describe the
// cumulative drift by the run's final window, for documentation; what
// internal/audit actually keys on is whether a keyframe-substrate
// detection's window falls within [0, totalWindows-1] at all.
func (w *World) DriftTruthEvents(now time.Time, totalWindows uint32) []audit.TruthEvent {
	var events []audit.TruthEvent
	lastWindow := uint32(0)
	if totalWindows > 0 {
		lastWindow = totalWindows - 1
	}
	for _, g := range w.groups {
		if !g.drifting {
			continue
		}
		total := g.driftDelta * float64(totalWindows)
		for _, agg := range []string{"sum", "max"} {
			name := g.logicalName(w.cfg.ShardID, agg)
			// sum scales with instance count; max is a single instance's
			// value regardless of how many others exist (ADR-008).
			magnitude := total
			baseline := w.cfg.Baseline
			if agg == "sum" {
				magnitude = total * float64(w.cfg.InstancesPerGroup)
				baseline = w.cfg.Baseline * float64(w.cfg.InstancesPerGroup)
			}
			events = append(events, audit.TruthEvent{
				Type:          audit.TruthDrift,
				SeriesID:      sketch.SeriesID([]byte(name)),
				SeriesName:    name,
				ShardID:       w.cfg.ShardID,
				WindowStart:   0,
				WindowEnd:     lastWindow,
				ExpectedValue: baseline + magnitude,
				Magnitude:     magnitude,
				Timestamp:     now,
			})
		}
	}
	return events
}

// HotInstanceTruthEvents returns one TruthAnomaly record per hot group's
// agg=max series (a single instance jumping to 5x baseline moves the
// group's max sharply while barely moving its sum — ADR-008's point about
// per-aggregate sketching — so only max gets a truth record here),
// covering from hotAtWindow to the run's last window.
// runStart is the real wall-clock time window 0 fires; interval converts a
// window index into a predicted real occurrence time, since a hot
// instance's window (hotAtWindow) is not window 0 and detection latency
// must be measured from when it actually happens, not from process
// startup.
func (w *World) HotInstanceTruthEvents(runStart time.Time, interval time.Duration, totalWindows uint32) []audit.TruthEvent {
	var events []audit.TruthEvent
	lastWindow := uint32(0)
	if totalWindows > 0 {
		lastWindow = totalWindows - 1
	}
	for _, g := range w.groups {
		if !g.hot {
			continue
		}
		name := g.logicalName(w.cfg.ShardID, "max")
		events = append(events, audit.TruthEvent{
			Type:          audit.TruthAnomaly,
			SeriesID:      sketch.SeriesID([]byte(name)),
			SeriesName:    name,
			ShardID:       w.cfg.ShardID,
			WindowStart:   g.hotAtWindow,
			WindowEnd:     lastWindow,
			ExpectedValue: w.cfg.Baseline * 5,
			Magnitude:     w.cfg.Baseline * 4,
			Timestamp:     runStart.Add(time.Duration(g.hotAtWindow) * interval),
		})
	}
	return events
}

// Aggregates folds every live group's current instance values into
// sum/count/max, applying read-time noise (so the underlying drift trend
// stays exact/noise-free for World.Step's bookkeeping, matching how a real
// fleet's instantaneous readings jitter around a true trend). Retired
// groups are omitted entirely (their aggregates stop being observed, which
// is what eventually tombstones them at the Tracker level after
// series_ttl — the group-level TruthTombstone above records the ground
// truth immediately, ahead of that TTL-driven decode-side effect).
func (w *World) Aggregates() map[*group]map[string]float64 {
	out := make(map[*group]map[string]float64, len(w.groups))
	for _, g := range w.groups {
		if g.retired {
			continue
		}
		sum, max := 0.0, math.Inf(-1)
		count := 0.0
		for _, inst := range g.instances {
			v := inst.baseline + (w.cfg.Rand.Float64()*2-1)*w.cfg.NoiseAmplitude
			sum += v
			count++
			if v > max {
				max = v
			}
		}
		out[g] = map[string]float64{"sum": sum, "count": count, "max": max}
	}
	return out
}
