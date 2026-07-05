# Palimpsest

Palimpsest is a Go library and toolchain for compressed-sensing telemetry.
Instrumented agents fold metric samples into residuals against a predicted
baseline, scatter those residuals into a small number of linear measurements
(a "sketch"), and ship the sketch instead of every raw series. A backend
recovers which exact series misbehaved via sparse recovery, reconstructing
anomalies from a fraction of the bytes a full remote-write stream would cost.

Observability incumbents force a choice between paying full per-series price
or losing fidelity to downsampling. Palimpsest keeps three layers instead: an
exact keyframe layer forever (PromQL-queryable), a sketched layer for anomaly
recovery, and a short instance-level ring buffer for drilldown — while only
routing the small slice of genuinely interesting series to expensive
per-series-billed stores. See [docs/COSTS.md](docs/COSTS.md) for the full
cost decomposition (including an honest anti-claim about when this doesn't
save money), and [docs/adr/](docs/adr/) for the architecture decision records
the design is built from.

**Status: v0.** All 13 ADRs are implemented end-to-end: the wire codec
(`pkg/wire`), the encode-side sketch/lifecycle/prediction stack
(`pkg/sketch`, `pkg/predict`), sparse recovery (`pkg/recover`), the OTel
Collector processor (`otel/processor/csresidual`), the reconstructor
(`internal/core`, `cmd/palimpsestd`), a synthetic workload generator with a
trust/audit layer (`cmd/plsim`, `internal/audit`), and a runnable offline
demo (`demo/`, see [demo/README.md](demo/README.md)). See
[docs/PERF.md](docs/PERF.md) for real benchmark numbers and
[docs/SECURITY.md](docs/SECURITY.md) for the threat model. This is a young
project, not a hardened one — see [Limitations](#limitations) below before
depending on it for anything that matters.

## Architecture

```text
 Agent (per shard/emitter/view)                    Reconstructor (palimpsestd)
 ───────────────────────────────                   ───────────────────────────
 raw samples (name, value)
        │
        ▼
 ┌──────────────┐  birth/tombstone   ┌───────────┐
 │   Tracker    │───dict_delta──────▶│           │
 │ (ADR-008     │                    │           │
 │  lifecycle)  │  residual          │  wire.Frame
 └──────┬───────┘  (value-baseline)  │  (ADR-006) │
        │                 │          │           │
        ▼                 ▼          │  KEYFRAME  │        ┌──────────────┐
 ┌──────────────┐  ┌─────────────┐   │  (exact,   │───────▶│  Dictionary  │
 │ Hold predictor│  │ Accumulator │   │  KDELTA,   │        │  + Watermark │
 │  (ADR-003     │  │ (ADR-002    │   │  ADR-011)  │        │  (ADR-013)   │
 │  open-loop)   │  │  hash-      │   │            │        └──────┬───────┘
 └──────────────┘  │  implicit Φ)│   │  RESIDUAL  │               │
                    └─────────────┘   │  (quantized)│              ▼
                                       │            │        ┌──────────────┐
                          snapshot ───▶│  FALLBACK  │        │ pkg/recover  │
                          blob (ADR-   │  (storm,   │        │ FISTA+debias │
                          009/012)     │  ADR-004)  │        │ (ADR-002)    │
                                       └─────┬──────┘        └──────┬───────┘
                                             │                      │
                                             ▼                      ▼
                                      ┌────────────┐         ┌─────────────┐
                                      │ fswatch /   │        │  Matcher     │
                                      │ httpsrc     │───────▶│ 5 substrates │
                                      │ (ADR-012:   │        │  (ADR-009)   │
                                      │  push, no   │        └──────┬───────┘
                                      │  listening  │               │
                                      │  socket by  │               ▼
                                      │  default)   │        anomalies.jsonl /
                                      └────────────┘         webhook / remote-write
```

Three layers ride the same wire format, at different cadences and cost
profiles (see [docs/COSTS.md](docs/COSTS.md)):

- **Layer 1 (exact, keyframe cadence)** — every logical series' true value,
  KDELTA-compressed against the prior keyframe (ADR-011), persisted forever
  to cheap storage (self-hosted Prometheus, S3/Parquet). Any label, any
  late-bound group-by, PromQL-queryable, always.
- **Layer 2 (sketched, flush cadence)** — a handful of linear measurements
  per (shard, view, window), recovered via sparse FISTA solve only when
  something looks wrong. Kilobytes per flush regardless of fleet size.
- **Layer 3 (instance ring buffer + dashcam snapshots)** — bounded,
  agent-side, 5-15 minutes by default; a flagged series' recent instance
  -level history rides along in the frame that flagged it (ADR-009,
  ADR-012), for the one thing sketching structurally can't answer on its
  own: *which pod*.

## Tiering by interestingness, not by age

Traditional downsampling tiers by *age*: recent data is fine-grained, old
data gets rolled up, and you lose resolution on a schedule regardless of
whether anything interesting happened. Palimpsest tiers by
**interestingness** instead: every series gets the exact Layer-1 keyframe
treatment forever, every series gets sketched at flush cadence for
recovery, and only the tiny fraction that's actually flagged — a real
deviation, a slow drift crossing a threshold, a series that vanished —
gets promoted to expensive per-series-billed storage or instance-level
forensic detail. A metric that never does anything unusual costs the same
either way: cheap. A metric that spikes once, three months from now,
still has full keyframe history *and* triggers full drilldown at the
moment it misbehaves — not "well, it was in the 6-hour-rollup tier by
then." See [docs/adr/ADR-005-tiering.md](docs/adr/ADR-005-tiering.md) and
[docs/adr/ADR-011-keyframe-economics.md](docs/adr/ADR-011-keyframe-economics.md).

## Two-level identity: pod IDs aren't identities

Kubernetes churns constantly — pods are born and die on a schedule having
nothing to do with whether anything is wrong. If you sketch per-pod, every
deploy, every autoscale event, every routine eviction looks
indistinguishable from cardinality explosion, and your baselines never
stabilize long enough to mean anything. ADR-008's answer is a two-level
identity: only the **logical series** (`metric_name × deployment/service
labels × aggregate`) is sketched and gets a stable dictionary entry with a
lifecycle (birth, observation, tombstone); **instance labels** (which pod,
which node) never enter the sketch at all — they ride an agent-side ring
buffer, and surface only if that logical series is flagged, as a dashcam
snapshot attached to the anomaly. A pod ID was never the thing being
measured; the deployment's aggregate behavior was. `cmd/plsim`'s
`--churn-rate` flag exists specifically to prove this holds under load: see
`internal/core/e2e_test.go`'s `TestE2E_LifecycleChurn` and
`demo/README.md`'s audit report walkthrough for
`lifecycle_false_positives == 0` in practice.

## Limitations

Said plainly, because the alternative is someone finding out the hard way:

- **Quantiles and percentiles can't be sketched.** Linear measurements
  don't compose through a quantile function. Summary/histogram-quantile
  metrics are forced to the exact tier (ADR-005) — you pay full price for
  them, by design, not by oversight.
- **The storm cliff is real.** Sparse recovery degrades once too many
  series deviate at once relative to the sketch width — informally, above
  roughly m/5 density. `pkg/sketch.StormDetector` catches this via
  pre-quantization energy and falls back to heavy-hitters (ADR-004), but
  fallback mode is lower-fidelity by construction: a genuine
  fleet-wide incident is exactly when you get the least detail from this
  layer. (It's also, not coincidentally, exactly when you least need
  per-series detail to know something is on fire.)
- **A short anomaly that straddles a keyframe boundary can look like it
  "reverses."** Keyframes periodically re-base the open-loop predictor to
  the current exact value (ADR-003); if a brief spike is still elevated
  when that happens, the new baseline bakes in the spike, and the return
  to normal 1-2 windows later shows up as an equal-and-opposite deviation.
  This is correct, explainable behavior (see
  `internal/core/e2e_test.go`'s `TestE2E_SlowDriftKeyframe` doc comment and
  `demo/README.md`'s audit report interpretation), not corruption — but it
  does mean precision on very short, frequent anomaly bursts is lower than
  on sustained ones.
- **Mergeability (fleet-wide weak-signal fusion) needs real scale.**
  Detecting a signal too small to see in any one shard by summing across
  thousands of agents is sound in principle but needs enough agents and
  good-enough baselines to clear a real SNR bar — this is backlog
  (ADR-010's merged tier), not implemented.
- **Instance-level forensics is bounded, by design.** The ring buffer is
  5-15 minutes; a dashcam snapshot is captured once, at flag time. Beyond
  that window, the exact-value Layer 1 and the sketch-recovered Layer 2
  are the record — there is no "go back and look at every pod from last
  Tuesday." See ADR-008/ADR-009 and `docs/SPEC.md`.

## Cost model

See [docs/COSTS.md](docs/COSTS.md) for the full decomposition and the
honest anti-claim (if you're already self-hosting Prometheus at 60s, this
doesn't save you money — the wedge is per-series-billed SaaS). Summary:

| Line | v1 (raw 15s remote-write) | Palimpsest | Savings |
| --- | --- | --- | --- |
| Wire bytes | 86-170 GB/day (10M series) | 15-20 GB/day | ~4-6x |
| Per-series billing | $100k+/mo at typical rates | 1-5% of series sent to expensive store | 30-100x |
| Storage (any tier) | 86-170 GB/day | Keyframe (6 GB/day) + sketch (0.1 GB/day) + anomaly detail (3-10 GB/day) | ~6x |
| Placement | one tier, one price | keyframes on commodity storage; only flagged series touch per-series billing | the actual moat (ADR-011) |
| Blended | n/a | n/a | 10-50x |

## Getting started

There's no published release yet; build from source (`go build ./...`,
`go test ./...`, `make bench`). The fastest way to see the whole system
working end to end is the offline demo: [demo/README.md](demo/README.md)
(`cd demo && docker compose up --build`). `docs/PERF.md` has real benchmark
numbers from this repo's own test suite.

## License

Palimpsest is licensed under the GNU Affero General Public License,
version 3 only (`AGPL-3.0-only`). See [LICENSE](LICENSE).
