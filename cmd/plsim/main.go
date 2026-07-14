/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Command plsim is Palimpsest's synthetic workload generator: it simulates
// a fleet of logical metric groups and their instances (ADR-008),
// generates wire frames through the same Tracker+Accumulator+Hold pipeline
// a real agent uses, and writes them to --frames-out for palimpsestd to
// consume — alongside a truth.jsonl ground-truth log (internal/audit) so
// palimpsestd's emitted anomalies can be scored for precision, recall,
// value MAPE, lifecycle false positives, detection latency, and revision
// accuracy. See docs/SPEC.md and ADR-008/009/013, which this tool is
// specifically built to exercise end-to-end.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/passiveintent/Palimpsest/internal/audit"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("plsim: %v", err)
	}
}

type flags struct {
	logicalSeries     int
	instancesPerGroup int
	anomalies         int
	interval          time.Duration
	churnRate         float64
	driftRate         float64
	hotInstance       int
	skew              time.Duration
	latencyJitter     time.Duration
	partition         time.Duration
	duplicateRate     float64
	framesOut         string
	truthOut          string
	views             string

	windows   int
	emitters  int
	shardID   uint64
	seed      int64
	tenantKey string

	m             int
	d             int
	bits          int
	keyframeEvery int
	goldenEvery   int
	seriesTTL     time.Duration

	snapshotThreshold float64
	ringWindow        time.Duration
	codec             string

	baseline float64
	// noise defaults to 0 deliberately: compressed-sensing recovery
	// (pkg/recover) is magnitude-independent for an isolated sparse
	// signal, so even small per-window noise redrawn independently every
	// window (uncorrelated with the open-loop predictor's baseline, which
	// only refreshes at keyframes) gets picked up across many series at
	// once, breaking FISTA's sparsity assumption and collapsing precision
	// — confirmed empirically running this exact scenario end-to-end
	// (precision dropped from ~0.96 to ~0.08, and lifecycle_false_positives
	// went from 0 to nonzero, purely from --noise 1.0's default before this
	// was changed). A nonzero value is left available for exploration, not
	// recommended for the default demo story.
	noise            float64
	anomalyMagnitude float64

	// mergedEmitters (0 disables) simulates the ADR-015 merged-tier
	// scenario: a correlated group + scattered singletons fused across
	// this many weak observers at per-agent SNR ~1.0, independent of the
	// disjoint-group --emitters sharding demo above. See cmd/plsim/merged.go.
	mergedEmitters  int
	mergedGroupSize int
	mergedScattered int

	// dz (--deadzone) switches plsim into the ADR-002/ADR-004 dead-zone
	// sweep — mapping where a single small anomaly is both too small to
	// trip the storm fallback and too small to survive FISTA recovery —
	// instead of generating frames. See cmd/plsim/deadzone.go and
	// docs/DEADZONE.md.
	dz deadzoneFlags

	// month (--month) switches plsim into the 30-day byte-ledger
	// benchmark (M2): amortized bytes-on-wire vs raw remote-write across
	// a simulated month with three scripted incidents. See
	// cmd/plsim/month.go.
	month monthFlags

	// g9 (--g9) switches plsim into the Gate G9 pre-registered
	// confirmatory harness for the compressed-sensing layer verdict. See
	// cmd/plsim/g9.go and docs/ADR-016-cs-verdict.md.
	g9 g9Flags
}

func parseFlags() flags {
	var f flags
	flag.IntVar(&f.logicalSeries, "logical-series", 300, "number of logical metric groups to simulate (each folds into 3 tracked series: agg=sum/count/max, per ADR-008)")
	flag.IntVar(&f.instancesPerGroup, "instances-per-logical", 20, "simulated instances (pods) per logical group")
	flag.IntVar(&f.anomalies, "anomalies", 20, "number of injected sustained value-jump anomalies")
	flag.DurationVar(&f.interval, "interval", 10*time.Second, "flush interval (real wall-clock time between windows)")
	flag.Float64Var(&f.churnRate, "churn-rate", 0, "logical-group births+deaths per minute (ADR-008 lifecycle churn; each replacement counts as 2)")
	flag.Float64Var(&f.driftRate, "drift-rate", 0, "slow trend per series, in per-mille (‰) of baseline per flush, applied to a small dedicated subset of groups (ADR-009.c)")
	flag.IntVar(&f.hotInstance, "hot-instance", 0, "number of groups in which one instance jumps to 5x baseline partway through the run (ADR-008 two-level identity: agg=max should move sharply, agg=sum should not)")
	flag.DurationVar(&f.skew, "skew", 0, "fixed additional clock-skew delay applied to every emitter except the first (ADR-013)")
	flag.DurationVar(&f.latencyJitter, "latency-jitter", 0, "random additional per-frame delivery delay in [0, latency-jitter]")
	flag.DurationVar(&f.partition, "partition", 0, "network outage duration applied to the first emitter partway through the run; 0 disables")
	flag.Float64Var(&f.duplicateRate, "duplicate-rate", 0, "fraction (0..1) of frames independently resent as an exact byte-for-byte duplicate")
	flag.StringVar(&f.framesOut, "frames-out", "./frames", "directory to write wire-encoded *.plmp frame files (fswatch picks these up)")
	flag.StringVar(&f.truthOut, "truth-out", "./truth.jsonl", "path to write ground-truth JSONL (internal/audit reads this via --truth)")
	flag.StringVar(&f.views, "views", "", "comma-separated view names (ADR-010); empty means a single implicit \"default\" view (view_id 0)")

	flag.IntVar(&f.windows, "windows", 60, "number of windows to run before exiting")
	flag.IntVar(&f.emitters, "emitters", 1, "number of simulated emitter processes, each owning a disjoint subset of groups, sharing one shard")
	flag.Uint64Var(&f.shardID, "shard-id", 1, "shard ID tag for every emitted frame")
	flag.Int64Var(&f.seed, "seed", 1, "RNG seed (deterministic runs)")
	flag.StringVar(&f.tenantKey, "tenant-key", "plsim-demo-tenant-key-do-not-use-in-prod", "ADR-012 HKDF tenant key (demo/test only; a real deployment reads this from an env var, never a flag)")

	flag.IntVar(&f.m, "m", 2000, "sketch width")
	flag.IntVar(&f.d, "d", 6, "hashes per series")
	flag.IntVar(&f.bits, "bits", 8, "RESIDUAL quantization width: 8 or 16")
	flag.IntVar(&f.keyframeEvery, "keyframe-every", 6, "emit a KEYFRAME instead of RESIDUAL every Nth window")
	flag.IntVar(&f.goldenEvery, "golden-every", 10, "emit a full (non-KDELTA) golden keyframe every Nth keyframe")
	flag.DurationVar(&f.seriesTTL, "series-ttl", 90*time.Second, "a logical series silent this long is tombstoned")

	flag.Float64Var(&f.snapshotThreshold, "snapshot-threshold", 20.0, "absolute residual magnitude above which a speculative dashcam snapshot is attached to the frame (ADR-009/ADR-012); <=0 disables")
	flag.DurationVar(&f.ringWindow, "ring-window", 15*time.Minute, "per-series ring buffer retention backing snapshots")
	flag.StringVar(&f.codec, "codec", "none", "compression codec for KEYFRAME payloads and snapshot blobs (ADR-006 §Addendum): none, gzip, or zstd")

	flag.Float64Var(&f.baseline, "baseline", 100.0, "steady-state per-instance value")
	flag.Float64Var(&f.noise, "noise", 0, "uniform random per-instance read noise amplitude; nonzero values are not recommended (see README note on this flag)")
	flag.Float64Var(&f.anomalyMagnitude, "anomaly-magnitude", 60.0, "absolute value added to an injected anomaly's target aggregate")

	flag.IntVar(&f.mergedEmitters, "merged-emitters", 0, "ADR-015: simulate this many weak-observer emitters fusing a correlated-group anomaly at per-agent SNR ~1.0 into one shared view; 0 disables (independent of --emitters' disjoint-group sharding)")
	flag.IntVar(&f.mergedGroupSize, "merged-group-size", 500, "ADR-015 merged-tier scenario: correlated-group member count (AZ-outage shape, ignored if --merged-emitters is 0)")
	flag.IntVar(&f.mergedScattered, "merged-scattered", 20, "ADR-015 merged-tier scenario: scattered singleton anomaly count (ignored if --merged-emitters is 0)")

	registerDeadzoneFlags(&f.dz)
	registerMonthFlags(&f.month)
	registerG9Flags(&f.g9)

	flag.Parse()
	return f
}

func run() error {
	f := parseFlags()
	if f.g9.enabled {
		return runG9(f)
	}
	if f.month.enabled {
		return runMonth(f)
	}
	if f.dz.enabled {
		return runDeadzone(f)
	}
	if err := os.MkdirAll(f.framesOut, 0o755); err != nil {
		return fmt.Errorf("creating --frames-out %q: %w", f.framesOut, err)
	}
	if f.emitters < 1 {
		return fmt.Errorf("--emitters must be >= 1")
	}

	rng := rand.New(rand.NewSource(f.seed))
	tenantKey := []byte(f.tenantKey)

	codec, err := resolveCodec(f.codec)
	if err != nil {
		return err
	}

	world := NewWorld(WorldConfig{
		ShardID:           f.shardID,
		LogicalSeries:     f.logicalSeries,
		InstancesPerGroup: f.instancesPerGroup,
		HotGroups:         f.hotInstance,
		DriftGroups:       driftGroupCount(f.driftRate, f.logicalSeries),
		DriftDeltaPerWin:  f.driftRate,
		ChurnPerMinute:    f.churnRate,
		IntervalSeconds:   f.interval.Seconds(),
		TotalWindows:      f.windows,
		Baseline:          f.baseline,
		NoiseAmplitude:    f.noise,
		Rand:              rng,
	})
	scenario := NewScenario(world, f.anomalies, uint32(f.windows), f.anomalyMagnitude, rng)

	truth, err := audit.NewTruthWriter(f.truthOut)
	if err != nil {
		return err
	}
	defer truth.Close()

	runStart := time.Now()
	for _, ev := range scenario.TruthEvents(runStart, f.interval) {
		if err := truth.Write(ev); err != nil {
			return err
		}
	}
	for _, ev := range world.DriftTruthEvents(runStart, uint32(f.windows)) {
		if err := truth.Write(ev); err != nil {
			return err
		}
	}
	for _, ev := range world.HotInstanceTruthEvents(runStart, f.interval, uint32(f.windows)) {
		if err := truth.Write(ev); err != nil {
			return err
		}
	}

	var (
		merged        *mergedSim
		mergedChannel *channel
	)
	if f.mergedEmitters > 0 {
		merged = newMergedSim(mergedSimConfig{
			TenantKey: tenantKey, ShardID: f.shardID, M: f.m, D: f.d, Bits: f.bits,
			KeyframeEvery: f.keyframeEvery, GoldenEvery: f.goldenEvery, SeriesTTL: f.seriesTTL,
			GroupSize: f.mergedGroupSize, Scattered: f.mergedScattered, Emitters: f.mergedEmitters,
			InjectedWindow: uint32(f.windows * 2 / 5), Seed: f.seed,
		})
		mergedChannel = newChannel(f.framesOut, 0, 0, 0, rand.New(rand.NewSource(f.seed+1_000_000)))
		for _, ev := range merged.TruthEvents(runStart, f.interval) {
			if err := truth.Write(ev); err != nil {
				return err
			}
		}
		log.Printf("plsim: merged-tier scenario armed (emitters=%d group-size=%d scattered=%d injected-window=%d view-id=%d)",
			f.mergedEmitters, f.mergedGroupSize, f.mergedScattered, merged.cfg.InjectedWindow, mergedTierViewID)
	}

	viewNames := parseViews(f.views)

	pipelines := make([][]*pipeline, f.emitters)
	channels := make([]*channel, f.emitters)
	for e := 0; e < f.emitters; e++ {
		skew := time.Duration(0)
		if e > 0 {
			skew = f.skew
		}
		channels[e] = newChannel(f.framesOut, skew, f.latencyJitter, f.duplicateRate, rand.New(rand.NewSource(f.seed+1000+int64(e))))

		pipelines[e] = make([]*pipeline, len(viewNames))
		for v := range viewNames {
			pipelines[e][v] = newPipeline(pipelineConfig{
				TenantKey:         tenantKey,
				ShardID:           f.shardID,
				EmitterID:         emitterID(f.seed, e),
				ViewID:            uint16(v),
				Epoch:             0,
				M:                 f.m,
				D:                 f.d,
				Bits:              f.bits,
				KeyframeEvery:     f.keyframeEvery,
				GoldenEvery:       f.goldenEvery,
				SeriesTTL:         f.seriesTTL,
				SnapshotThreshold: f.snapshotThreshold,
				RingWindow:        f.ringWindow,
				Codec:             codec,
			})
		}
	}

	partitionStartWindow := -1
	partitionEndWindow := -1
	if f.partition > 0 && f.interval > 0 {
		partitionWindows := int(f.partition / f.interval)
		if partitionWindows < 1 {
			partitionWindows = 1
		}
		partitionStartWindow = f.windows * 2 / 5
		partitionEndWindow = partitionStartWindow + partitionWindows
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("plsim: started (logical-series=%d instances-per-logical=%d emitters=%d views=%d windows=%d interval=%s frames-out=%q truth-out=%q churn-rate=%v drift-rate=%v partition=%s duplicate-rate=%v)",
		f.logicalSeries, f.instancesPerGroup, f.emitters, len(viewNames), f.windows, f.interval, f.framesOut, f.truthOut, f.churnRate, f.driftRate, f.partition, f.duplicateRate)

	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	for window := uint32(0); int(window) < f.windows; window++ {
		select {
		case <-ctx.Done():
			log.Printf("plsim: shutting down early at window %d", window)
			return nil
		default:
		}

		wnow := time.Now()
		if int(window) == partitionStartWindow {
			log.Printf("plsim: emitter 0 entering partition for %s (window %d)", f.partition, window)
			channels[0].BeginPartition()
		}
		if int(window) == partitionEndWindow {
			log.Printf("plsim: emitter 0 partition ended, releasing buffered frames shuffled+duplicated (window %d)", window)
			channels[0].EndPartition()
		}

		lifecycleEvents := world.Step(window, wnow)
		for _, ev := range lifecycleEvents {
			if err := truth.Write(ev); err != nil {
				return err
			}
		}

		aggregates := world.Aggregates()
		scenario.Apply(window, aggregates)

		perEmitterValues := make([]map[string]float64, f.emitters)
		for i := range perEmitterValues {
			perEmitterValues[i] = make(map[string]float64, len(aggregates)/f.emitters*3+3)
		}
		for g, aggs := range aggregates {
			e := g.idx % f.emitters
			for agg, v := range aggs {
				perEmitterValues[e][g.logicalName(f.shardID, agg)] = v
			}
		}

		for e := 0; e < f.emitters; e++ {
			for v := range viewNames {
				frame := pipelines[e][v].flush(wnow, window, perEmitterValues[e])
				if err := channels[e].Send(frame); err != nil {
					log.Printf("plsim: emitter %d view %d: %v", e, v, err)
				}
			}
		}

		if merged != nil {
			if err := merged.Step(window, wnow, mergedChannel); err != nil {
				log.Printf("plsim: merged-tier scenario: %v", err)
			}
		}

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}

	log.Printf("plsim: completed %d windows", f.windows)
	return nil
}

// driftGroupCount picks how many groups drift when --drift-rate is set: a
// small, fixed handful is enough to demonstrate and audit substrate (c)
// without needing to break substrate (a)'s sparsity assumption (see
// internal/core/e2e_test.go's TestE2E_SlowDriftKeyframe doc comment for why
// that isn't necessary or reliably achievable).
func driftGroupCount(driftRate float64, logicalSeries int) int {
	if driftRate <= 0 {
		return 0
	}
	n := logicalSeries / 20
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10
	}
	return n
}

// emitterID derives a stable, distinct per-process emitter identity from
// the run seed and emitter index (ADR-013: emitter_id is a random u64 per
// agent process; deriving it from the seed keeps plsim's runs
// reproducible rather than actually random).
func emitterID(seed int64, idx int) uint64 {
	h := rand.New(rand.NewSource(seed*7919 + int64(idx)*104729))
	return h.Uint64()
}

// parseViews splits --views into an ordered, non-empty list of view names;
// an empty flag means a single implicit "default" view (view_id 0),
// mirroring otel/processor/csresidual's resolveViews.
func parseViews(spec string) []string {
	if strings.TrimSpace(spec) == "" {
		return []string{"default"}
	}
	parts := strings.Split(spec, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"default"}
	}
	return out
}
