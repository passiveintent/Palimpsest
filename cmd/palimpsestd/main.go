/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Command palimpsestd is Palimpsest's hexagonal reconstructor: it consumes
// wire frames from one or more sources, reconstructs per-shard state
// (internal/core), and routes anomalies/recovered series to the configured
// sinks. See ADR-007 (hexagonal service) and ADR-012 (security/tenancy:
// zero listening sockets by default).
package main

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/wire"

	"github.com/passiveintent/Palimpsest/internal/adapters/fswatch"
	"github.com/passiveintent/Palimpsest/internal/adapters/httpsrc"
	"github.com/passiveintent/Palimpsest/internal/adapters/jsonl"
	"github.com/passiveintent/Palimpsest/internal/adapters/memstate"
	"github.com/passiveintent/Palimpsest/internal/adapters/remoteprom"
	"github.com/passiveintent/Palimpsest/internal/adapters/webhook"
	"github.com/passiveintent/Palimpsest/internal/audit"
	"github.com/passiveintent/Palimpsest/internal/core"
	"github.com/passiveintent/Palimpsest/internal/forensics"
	"github.com/passiveintent/Palimpsest/internal/metrics"
	"github.com/passiveintent/Palimpsest/internal/ports"
)

var processStart = time.Now()

func main() {
	if err := run(); err != nil {
		log.Fatalf("palimpsestd: %v", err)
	}
}

type flags struct {
	framesDir           string
	fswatchPollInterval time.Duration
	httpAddr            string
	allowPull           bool

	outDir         string
	webhookURL     string
	remoteWriteURL string
	stateFile      string

	tenantKeyEnv string

	maxResidual     float64
	allowedLateness int
	repairHorizon   time.Duration

	emittersExpected int
	fistaIters       int
	fistaLambda      float64
	fistaPowerIters  int
	fistaThreshold   float64

	driftThreshold              float64
	clockSkewToleranceMs        int64
	churnMaxBirthsPerViewPerMin int

	snapshotTTL time.Duration
	snapshotMax int

	truthPath     string
	auditInterval time.Duration
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.framesDir, "frames-dir", "", "directory to poll for wire-encoded *.plmp frame files (fswatch source)")
	flag.DurationVar(&f.fswatchPollInterval, "frames-poll-interval", time.Second, "poll interval for --frames-dir")
	flag.StringVar(&f.httpAddr, "http", ":8080", "listen address for the httpsrc POST /v1/frames endpoint and /debug/vars (only opened if --allow-pull)")
	flag.BoolVar(&f.allowPull, "allow-pull", false, "open a listening HTTP socket to receive speculatively-pushed frames (ADR-012: zero listening sockets by default; fswatch via --frames-dir is the safe default ingestion path)")

	flag.StringVar(&f.outDir, "out-dir", "", "directory for JSON-lines anomaly/series output (required)")
	flag.StringVar(&f.webhookURL, "webhook", "", "if set, also POST each anomaly event to this URL")
	flag.StringVar(&f.remoteWriteURL, "remote-write-url", "", "if set, write recovered/exact series to this Prometheus remote-write endpoint (ADR-010/ADR-011 Layer 1) instead of out-dir/series.jsonl")
	flag.StringVar(&f.stateFile, "state-file", "", "optional path for periodic operational-bookkeeping gob snapshots (memstate)")

	flag.StringVar(&f.tenantKeyEnv, "tenant-key-env", "PALIMPSEST_TENANT_KEY", "environment variable naming the ADR-012 HKDF tenant key (never passed on the command line)")

	flag.Float64Var(&f.maxResidual, "max-residual", 5.0, "gate threshold on recover.Result.Residual: solves above this are untrustworthy and mark their view DEGRADED")
	flag.IntVar(&f.allowedLateness, "allowed-lateness", 3, "windows a stream's high-water mark must advance past a window before its initial recovery (ADR-013)")
	flag.DurationVar(&f.repairHorizon, "repair-horizon", 10*time.Minute, "wall-clock window after a window's initial recovery during which a late frame triggers a repair re-solve (ADR-013)")

	flag.IntVar(&f.emittersExpected, "emitters-expected", 1, "expected distinct emitters per shard/window, annotated as Result.CoverageTotal")
	flag.IntVar(&f.fistaIters, "fista-iters", 350, "FISTA iteration count (1..350)")
	flag.Float64Var(&f.fistaLambda, "fista-lambda", 0.05, "FISTA L1 penalty weight")
	flag.IntVar(&f.fistaPowerIters, "fista-power-iters", 50, "power-iteration count for the FISTA Lipschitz estimate")
	flag.Float64Var(&f.fistaThreshold, "fista-threshold", 0.3, "post-FISTA support cutoff")

	flag.Float64Var(&f.driftThreshold, "drift-threshold", 10.0, "substrate (c): keyframe-to-keyframe absolute value change flagged as slow drift")
	flag.Int64Var(&f.clockSkewToleranceMs, "clock-skew-tolerance-ms", 50, "per-emitter arrival-time spread above which a clock-skew Alert fires; <=0 disables")
	flag.IntVar(&f.churnMaxBirthsPerViewPerMin, "churn-max-births-per-view-per-min", 0, "decode-side churn breaker: shard-wide dictionary birth rate limit, defense in depth beyond any single encoder's own breaker; 0 disables")

	flag.DurationVar(&f.snapshotTTL, "snapshot-ttl", time.Hour, "forensic dashcam snapshot retention")
	flag.IntVar(&f.snapshotMax, "snapshot-max", 10000, "forensic dashcam snapshot store capacity")

	flag.StringVar(&f.truthPath, "truth", "", "path to a plsim-generated truth.jsonl; if set, scores emitted anomalies against it (internal/audit) and writes out-dir/audit_report.json")
	flag.DurationVar(&f.auditInterval, "audit-interval", 10*time.Second, "how often to reload --truth (it may still be growing) and rewrite audit_report.json")

	flag.Parse()
	return f
}

func run() error {
	f := parseFlags()

	tenantKey := os.Getenv(f.tenantKeyEnv)
	if tenantKey == "" {
		return fmt.Errorf("environment variable %q (--tenant-key-env) is empty or unset (ADR-012: the tenant key is never passed on the command line)", f.tenantKeyEnv)
	}
	if f.outDir == "" {
		return fmt.Errorf("--out-dir is required")
	}
	if !f.allowPull && f.framesDir == "" {
		return fmt.Errorf("no frame source configured: set --frames-dir and/or --allow-pull")
	}
	if err := os.MkdirAll(f.outDir, 0o755); err != nil {
		return fmt.Errorf("creating --out-dir %q: %w", f.outDir, err)
	}

	m := metrics.New()
	m.Publish("palimpsest")

	fstore := forensics.NewSnapshotStore(f.snapshotTTL, f.snapshotMax)

	anomalyJSONL, err := jsonl.NewAnomalySink(filepath.Join(f.outDir, "anomalies.jsonl"))
	if err != nil {
		return err
	}
	defer anomalyJSONL.Close()

	var anomalySink ports.AnomalySink = anomalyJSONL
	if f.webhookURL != "" {
		anomalySink = fanoutAnomalySink{primary: anomalyJSONL, webhook: webhook.New(f.webhookURL)}
	}

	// --truth wires a Prompt-8 audit layer: every emitted anomaly is also
	// scored against a plsim-generated ground-truth log (precision,
	// recall, value MAPE, lifecycle false positives, detection latency,
	// revision accuracy), published as expvar and periodically written to
	// out-dir/audit_report.json.
	var auditor *audit.Auditor
	if f.truthPath != "" {
		auditor, err = audit.NewAuditor(f.truthPath)
		if err != nil {
			return fmt.Errorf("loading --truth: %w", err)
		}
		auditor.Publish("palimpsest_audit")
		anomalySink = audit.Sink{Next: anomalySink, Auditor: auditor}
	}

	var seriesSink ports.SeriesSink
	if f.remoteWriteURL != "" {
		seriesSink = remoteprom.New(f.remoteWriteURL)
	} else {
		seriesJSONL, err := jsonl.NewSeriesSink(filepath.Join(f.outDir, "series.jsonl"))
		if err != nil {
			return err
		}
		defer seriesJSONL.Close()
		seriesSink = seriesJSONL
	}

	// StateStore is wired end-to-end (LoadFromDisk at startup, periodic
	// Save below) but currently only persists operational bookkeeping, not
	// a full Engine snapshot: pkg/recover.Dictionary has no exported
	// serialization yet, so a faithful gob snapshot of ShardState isn't
	// possible without extending that package. A restarted palimpsestd
	// re-bootstraps every shard from the next golden keyframe, same as any
	// new decoder would (ADR-013).
	state := memstate.New(f.stateFile)
	if err := state.LoadFromDisk(); err != nil {
		log.Printf("palimpsestd: loading --state-file: %v (continuing fresh)", err)
	}

	cfg := core.Config{
		TenantKey:              []byte(tenantKey),
		MaxResidual:            f.maxResidual,
		AllowedLateness:        f.allowedLateness,
		RepairHorizon:          f.repairHorizon,
		EmittersExpected:       f.emittersExpected,
		FISTAIters:             f.fistaIters,
		FISTALambda:            f.fistaLambda,
		FISTAPowerIters:        f.fistaPowerIters,
		FISTAThreshold:         f.fistaThreshold,
		DriftThreshold:         f.driftThreshold,
		ClockSkewToleranceMs:   f.clockSkewToleranceMs,
		MaxBirthsPerViewPerMin: f.churnMaxBirthsPerViewPerMin,
	}
	engine := core.New(cfg, anomalySink, seriesSink, m, fstore)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var sources []ports.FrameSource
	if f.framesDir != "" {
		sources = append(sources, fswatch.New(f.framesDir, f.fswatchPollInterval))
	}
	if f.allowPull {
		sources = append(sources, httpsrc.New(f.httpAddr))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(sources))
	for _, src := range sources {
		wg.Add(1)
		go func(src ports.FrameSource) {
			defer wg.Done()
			if err := src.Run(ctx, func(fr *wire.Frame) { engine.HandleFrame(ctx, fr) }); err != nil {
				errCh <- err
			}
		}(src)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				saveHeartbeat(state)
				return
			case <-ticker.C:
				saveHeartbeat(state)
			}
		}
	}()

	if auditor != nil {
		reportPath := filepath.Join(f.outDir, "audit_report.json")
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(f.auditInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					writeAuditReport(auditor, f.truthPath, reportPath)
					return
				case <-ticker.C:
					writeAuditReport(auditor, f.truthPath, reportPath)
				}
			}
		}()
	}

	log.Printf("palimpsestd: started (frames-dir=%q allow-pull=%v http=%q out-dir=%q truth=%q)", f.framesDir, f.allowPull, f.httpAddr, f.outDir, f.truthPath)

	<-ctx.Done()
	log.Printf("palimpsestd: shutting down")
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// fanoutAnomalySink sends every anomaly to both a primary (jsonl) sink and
// a webhook, so --webhook supplements rather than replaces the on-disk
// record.
type fanoutAnomalySink struct {
	primary *jsonl.AnomalySink
	webhook *webhook.Sink
}

func (fo fanoutAnomalySink) EmitAnomaly(ctx context.Context, ev ports.AnomalyEvent) error {
	errPrimary := fo.primary.EmitAnomaly(ctx, ev)
	errWebhook := fo.webhook.EmitAnomaly(ctx, ev)
	if errPrimary != nil {
		return errPrimary
	}
	return errWebhook
}

type heartbeat struct {
	StartedAt  time.Time
	LastSaveAt time.Time
}

// writeAuditReport reloads truthPath (a concurrently-running plsim may
// still be appending to it) and writes a's current Report as JSON to
// reportPath (temp file + rename, so a reader never sees a partial
// write). Errors are logged, not fatal: a transient truth-file read
// hiccup shouldn't crash the reconstructor.
func writeAuditReport(a *audit.Auditor, truthPath, reportPath string) {
	if err := a.Reload(truthPath); err != nil {
		log.Printf("palimpsestd: reloading --truth %q: %v", truthPath, err)
	}
	b, err := json.MarshalIndent(a.Report(), "", "  ")
	if err != nil {
		log.Printf("palimpsestd: marshaling audit report: %v", err)
		return
	}
	tmp := reportPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("palimpsestd: writing audit report: %v", err)
		return
	}
	if err := os.Rename(tmp, reportPath); err != nil {
		log.Printf("palimpsestd: renaming audit report into place: %v", err)
	}
}

func saveHeartbeat(state *memstate.Store) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(heartbeat{StartedAt: processStart, LastSaveAt: time.Now()}); err != nil {
		return
	}
	_ = state.Save(context.Background(), "heartbeat", buf.Bytes())
}
