# Palimpsest demo

A fully offline, self-contained run of the whole v0 story: a synthetic
fleet (`plsim`) with churn, slow drift, and a hot instance, sketched and
reconstructed (`palimpsestd`), scored against ground truth (`internal/audit`),
and visualized (Prometheus + Grafana). No cloud credentials, no external
services — every image is either built from this repo or an official
upstream image (`prometheus`, `grafana`), and every service only talks to
the others on the compose network.

## Quickstart

```bash
cd demo
docker compose up --build
```

That's it. `plsim` starts generating a 300-group fleet with churn-rate 30
(births+deaths/min), drift-rate 10, and 3 hot instances; `palimpsestd`
consumes it and writes `demo/out/audit_report.json` every 10 seconds.
Watch it converge:

```bash
watch -n2 cat demo/out/audit_report.json     # or, on Windows: type demo\out\audit_report.json (repeat)
```

The default run is 60 windows x 10s = 10 minutes (`plsim`'s own flag
defaults — see `docker-compose.yml`'s `command:`). `palimpsestd` and the
observability stack keep running after `plsim` exits; stop everything with
`docker compose down`.

## Expected output

- `demo/frames/` — wire-encoded `*.plmp` frames (`plsim` writes them,
  `palimpsestd`'s fswatch source consumes and archives them under
  `frames/.processed/`).
- `demo/truth/truth.jsonl` — ground truth: every injected anomaly, drift
  episode, hot-instance activation, and lifecycle birth/tombstone `plsim`
  generated, with the exact window range and expected value.
- `demo/out/anomalies.jsonl` — every `AnomalyEvent` `palimpsestd` emitted
  (substrate, confidence, series, value, coverage, revision).
- `demo/out/series.jsonl` — Layer-1 exact/recovered series (only written
  here if `--remote-write-url` isn't set; in this compose file it *is* set,
  so Layer 1 goes to Prometheus instead and this file won't appear).
- `demo/out/audit_report.json` — the scored report (see below), rewritten
  every 10 seconds for as long as `palimpsestd` runs.

A representative `audit_report.json` from a real run of this exact compose
file (see `docs/PERF.md` for the methodology behind these classes of
numbers):

```json
{
  "truth_anomalies": 38,
  "detections": 1513,
  "true_positives": 1452,
  "false_positives": 61,
  "precision": 0.96,
  "recall": 1.0,
  "recall_by_type": {
    "anomaly": { "truth": 18, "recalled": 18, "recall": 1.0 },
    "drift":   { "truth": 20, "recalled": 20, "recall": 1.0 }
  },
  "value_mape": 0.002,
  "lifecycle_false_positives": 0,
  "detection_latency_by_substrate": {
    "deviation": { "count": 1313, "mean_seconds": 20.5, "p95_seconds": 38.2 },
    "keyframe":  { "count": 139,  "mean_seconds": 19.9, "p95_seconds": 39.2 }
  },
  "revision_accuracy_samples": 0
}
```

## Audit report interpretation

- **`recall` / `recall_by_type`**: fraction of ground-truth anomalies
  (`"anomaly"`: injected value jumps + the hot instance; `"drift"`: slow
  trends) that were caught by *any* substrate at least once. The default
  churn-active run should show `recall >= 0.95` — this is the acceptance
  bar.
- **`lifecycle_false_positives`**: a `"deviation"`-substrate false alarm
  whose series had a birth/tombstone within +/-1 window — i.e. churn itself
  masquerading as an anomaly. ADR-008's whole point is that this is
  structurally impossible (a birth's first sample never enters the
  residual path), and this number should be **exactly 0**. If it isn't,
  that's a real regression, not noise.
- **`precision`**: of everything flagged, how much was real. Expect well
  under 100% here specifically because `--hot-instance` and `--drift-rate`
  create genuinely continuous conditions (a hot instance stays hot; a
  keyframe interaction can make the *end* of a short injected anomaly look
  like an equal-and-opposite deviation once its elevated value gets baked
  into a keyframe as the new baseline — see `internal/core/e2e_test.go`'s
  `TestE2E_SlowDriftKeyframe` doc comment and this repo's
  [README Limitations](../README.md#limitations) section for why this is expected
  system behavior, not a bug).
- **`value_mape`**: mean absolute percentage error, computed only against
  `"anomaly"`-type truth (drift is deliberately excluded — its true value
  changes continuously through its whole truth window, so there is no
  single fixed target a mid-run detection can be fairly scored against;
  see `internal/audit/audit.go`'s `expectedValueFor`/canonicalization doc
  comments).
- **`revision_accuracy_samples: 0`** in the default run is expected: no
  repair happens without a partition/duplicate scenario (see below) or a
  second emitter. It is not a bug in the default run; it means the
  denominator is honestly empty rather than a misleading 0/1.

## The ADR-013 watermark scenario (partition + duplicate + repair)

The default run doesn't exercise watermark repair (dedup and late-frame
merge, yes; a genuine repair needs a *second* emitter's contribution to
arrive late for a window a *different* emitter already got recovered —
see `internal/core/engine.go`'s dedup gate and
`TestE2E_DuplicateResidualFrameDeduped`/`TestE2E_WatermarkRepair`). To see
it:

```bash
PLSIM_EMITTERS=2 PLSIM_PARTITION=2m PLSIM_DUPLICATE_RATE=0.2 docker compose up --build
```

This partitions one of two emitters for 2 minutes partway through the run,
then releases its buffered frames shuffled (reordered) with a 20% chance
each is also resent as an exact duplicate. Watch `demo/out/anomalies.jsonl`
for `"revision":1` entries appearing after the partition ends, and
`audit_report.json`'s `revision_accuracy`/`revision_accuracy_samples`
becoming non-zero.

## The slow-drift scenario

Drift is on by default (`--drift-rate 10`, a handful of dedicated groups).
To make it the star of the show on its own:

```bash
PLSIM_CHURN_RATE=0 PLSIM_HOT_INSTANCE=0 PLSIM_DRIFT_RATE=10 docker compose up --build
```

`recall_by_type.drift` should reach 1.0, and
`detection_latency_by_substrate.keyframe` should show a mean latency on
the order of `keyframe_every x flush_interval` (the default `plsim`
encoder cadence) — the ADR-009.c acceptance bar ("latency <= keyframe
cadence").

## Grafana (optional)

Open **[localhost:3000](http://localhost:3000)** (anonymous viewer access,
no login required). The **Palimpsest** folder has one pre-provisioned
dashboard, "Layer 1 Keyframe + Anomaly Detail"
(`demo/grafana-dashboard.json`): pick any `$logical_name` the fleet has
produced, and watch its exact value — sparse points at keyframe cadence
during quiet periods, densifying to flush cadence whenever that series is
under active recovery. Prometheus itself is at
**[localhost:9090](http://localhost:9090)** if you'd rather run raw PromQL
(`{__name__=~".+", palimpsest_logical_name="..."}`).

## Optional: the real OTel Collector ingestion path

Everything above drives `palimpsestd` directly from `plsim`'s synthetic
frames — deliberately, since that's the only path that can carry a
ground-truth log for the audit report to score against. A **separate,
optional** compose profile additionally builds a real OTel Collector
distribution (via `ocb`) running this repo's own
`otel/processor/csresidual` processor against real host metrics
(`hostmetrics` receiver), in shadow mode, to demonstrate the genuine
ingestion path end-to-end:

```bash
docker compose --profile otel up --build otel-collector
```

Verified working as of this writing (`docker compose --profile otel up
--build otel-collector` builds, starts, ingests real `hostmetrics` data,
and writes real `*.plmp` frames to the shared volume via the csresidual
processor in shadow mode). **It's still best-effort going forward**: `ocb`
and the `hostmetricsreceiver` component version
(`demo/otelcol-builder.yaml`) must match the collector core version
`otel/go.mod` is pinned to (also note `demo/otelcol.Dockerfile` builds this
stage on a newer Go than the root module's images, since `ocb`/`otel/go.mod`
require it) — if upstream versions have drifted since this was written,
the build may fail with a dependency resolution error. The default
`docker compose up` above does not depend on this profile at all — it is a
bonus demonstration, not part of the acceptance story. If it fails for
you, `docker compose build otel-collector` will show exactly which module
resolution step broke; that's the place to bump
a version pin.

## Cleaning up

```bash
docker compose down            # stop and remove containers
rm -rf frames out truth        # discard generated run artifacts (gitignored already)
```
