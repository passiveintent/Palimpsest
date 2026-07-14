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

**Status: v0.** All 17 ADRs are accounted for end-to-end: the wire codec
(`pkg/wire`), the encode-side sketch/lifecycle/prediction stack
(`pkg/sketch`, `pkg/predict`), sparse recovery (`pkg/recover`, FISTA +
Group-OMP debiasing — ADR-014 records the Group-OMP evaluation and its
negative result), the OTel Collector processor (`otel/processor/csresidual`),
the reconstructor (`internal/core`, `cmd/palimpsestd`), Kafka transport
(`internal/adapters/kafka`, ADR-016), the merged-tier trust policy
(`pkg/tier`, `internal/core`, ADR-015), a synthetic workload generator with a
trust/audit layer (`cmd/plsim`, `internal/audit`), and a runnable offline
demo (`demo/`, see [demo/README.md](demo/README.md)). ADR-017 records the
Merkle dict-root evidence-gate measurement — Condition 1 is met, so the
Merkle + bidirectional-resync backlog item stays open pending pilot-week
shadow metrics. See [docs/PERF.md](docs/PERF.md) for real benchmark numbers
and [docs/SECURITY.md](docs/SECURITY.md) for the threat model. This is a
young project, not a hardened one — see [Limitations](#limitations) below
before depending on it for anything that matters.

## ADR status matrix

| ADR | Title | Status | Key tests | Demo coverage | Production caveats |
| --- | --- | --- | --- | --- | --- |
| [001](docs/adr/ADR-001-language-and-oracle.md) | Language & oracle | ✅ Complete | `pkg/*/golden_test.go` — byte-exact against Python oracle | `make golden` regenerates | Recovery vectors are tolerance-compared, not byte-exact (BLAS rounding; ADR-001 asterisk) |
| [002](docs/adr/ADR-002-hash-implicit-phi.md) | Hash-implicit Φ | ✅ Complete | `pkg/sketch/hash_test.go`, `accumulator_test.go`, golden hashing + HKDF vectors | Every sketch in demo | Deterministic by construction; none |
| [003](docs/adr/ADR-003-open-loop-residuals.md) | Open-loop (Hold) predictor | ✅ Complete | `pkg/predict/predict_test.go`, `TestE2E_SlowDriftKeyframe` | Default predictor in demo | Spike at keyframe boundary appears to "reverse" — correct, explainable (see Limitations) |
| [004](docs/adr/ADR-004-storm-fallback.md) | Storm fallback | ✅ Complete | `pkg/sketch/storm_test.go`, `TestFallbackOnStormTrigger`, `TestMatcher_EvalFallback` | `--hot-instance` triggers FALLBACK frames; `--deadzone` maps the small-anomaly boundary | FALLBACK is lower-fidelity; m/5 density cliff is real; small-anomaly dead zone measured in [docs/DEADZONE.md](docs/DEADZONE.md) |
| [005](docs/adr/ADR-005-tiering.md) | Tiering (exact / sketched / merged) | ✅ Complete | `pkg/tier/tier_test.go`, `TestMatchTier_AllTiers`, `TestExactTierPassesUntouched` | Tier rules in `otelcol-config.yaml` | Quantiles/percentiles must use exact tier — no savings available for them |
| [006](docs/adr/ADR-006-wire-format.md) | Wire format + codec registry | ✅ Complete | `pkg/wire/codec_test.go`, `golden_test.go`, `rfc_sync_test.go`, `zstdcodec_test.go` | All demo frames; `TestRFCFrameTableInSync` CI-guards the RFC table | v1 is decode-only; `zstdcodec.Register()` required at startup for zstd frames |
| [007](docs/adr/ADR-007-hexagonal-service.md) | Hexagonal service | ✅ Complete | Adapter tests: fswatch, httpsrc, jsonl, kafka, remoteprom, webhook | `palimpsestd` is demo reconstructor | None |
| [008](docs/adr/ADR-008-ephemeral-cardinality.md) | Ephemeral cardinality + ownership amendment | ✅ Complete | `ownership_test.go` (4 tests), `TestE2E_LifecycleChurn` | `--churn-rate`; audit shows `lifecycle_false_positives == 0` | `EmitterRosterTTL` (dead-emitter reap) is opt-in, off by default |
| [009](docs/adr/ADR-009-alerting.md) | Alerting (5 substrates) | ✅ Complete | `matcher_test.go`, `TestE2E_SteadyStateAnomalies` | `anomalies.jsonl`; audit precision/recall in demo output | Keyframe-reversal caveat applies to substrate-(c) slow-drift (see ADR-003 note) |
| [010](docs/adr/ADR-010-dimensional-slicing.md) | Dimensional slicing | ✅ Complete | `TestMultiViewIndependence` (otel), e2e view isolation | Single view default; multi-view via otelcol config | Sketch-cube (cross-dimensional fused view) is backlog |
| [011](docs/adr/ADR-011-keyframe-economics.md) | Keyframe economics (KDELTA) | ✅ Complete | `pkg/wire/keyframe_test.go`, `TestKeyframeCadenceAndGoldenEvery`, golden KDELTA vectors | Grafana Layer-1 panel shows exact keyframe values | Golden keyframes are 18-35% of wire bytes at realistic churn — ADR-017 |
| [012](docs/adr/ADR-012-security-tenancy.md) | Security & tenancy | ✅ Complete | `TestE2E_KeyRotation`, `TestSink_DifferentTenantsDifferentKeys`, `TestE2E_UnregisteredCodecGatesKeyframe` | `PALIMPSEST_TENANT_KEY` env var; no exposed ports by default | Key rotation needs explicit epoch boundary; no at-rest re-encryption |
| [013](docs/adr/ADR-013-temporal-alignment.md) | Temporal alignment | ✅ Complete | `watermark_test.go`, `TestE2E_WatermarkRepair`, `TestE2E_JitteredEpochRotationZeroErrors` | `--partition`/`--duplicate-rate` flags in plsim | Late frames beyond `repair_horizon` are discarded; `seq` is a window index, not wall-clock |
| [014](docs/adr/ADR-014-group-sparse-recovery.md) | Group-sparse recovery | ✅ Complete | `pkg/recover/group_test.go` (8 tests incl. golden group case) | Escalation is transparent — fires automatically when FISTA residual is high | FISTA+debias is the default path (the ADR-014 evaluation result); Group-OMP activates via `Config.EscalateThreshold` when needed |
| [015](docs/adr/ADR-015-merged-tier.md) | Merged-tier trust policy | ✅ Complete | `merged_e2e_test.go` (3 tests), `TestMergedPolicyEvaluate` | `--emitters > 1` exercises `proven`/`unproven` guardrail | Requires multiple contributing emitters per shard; thin fleets see no benefit from the merged path |
| [016](docs/adr/ADR-016-kafka-transport.md) | Kafka transport | ✅ Complete | `kafka/` tests (source, sink, spool, integration, engine redelivery), `TestKafkaFrameSink_*` | `docker compose --profile otel` + `--kafka-brokers` flag | Spool disk budget must be configured; at-least-once delivery (engine deduplicates) |
| [017](docs/adr/ADR-017-merkle-decision.md) | Merkle dict-root evidence gate | 🔬 Decision only | `evidence_gate_test.go` (2 tests; skip under `-short`) | Shadow gauges (`palimpsest_dict_block_*`) visible in Grafana | Merkle + bidirectional-resync backlog open; pilot-week shadow metrics settle the real-name compressibility question |

**Legend:** ✅ Complete and stable · 🔄 Implementation in progress · 🔬 Evaluated / decision-recorded (not a production feature gap)

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
                                      │ httpsrc /   │───────▶│ 5 substrates │
                                      │ kafka       │        │  (ADR-009)   │
                                      │ (ADR-012,   │        └──────┬───────┘
                                      │  ADR-016:   │               │
                                      │  push, no   │               ▼
                                      │  listening  │        anomalies.jsonl /
                                      │  socket by  │        webhook / remote-write
                                      │  default)   │
                                      └────────────┘
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
measured; the deployment's aggregate behavior was.

The ADR-008 amendment ("golden reconciliation and union-dictionary
ownership") adds the decoder-side ownership model that makes multi-emitter
shards safe: a tombstone from one emitter can only evict a series from the
shared dictionary once *every* emitter that claimed it has released it —
preventing a routine pod-migration tombstone from wrongly deleting a series
a newly-started replica still owns. `cmd/plsim`'s `--churn-rate` flag
exists specifically to prove this holds under load: see
`internal/core/e2e_test.go`'s `TestE2E_LifecycleChurn` and
`demo/README.md`'s audit report walkthrough for
`lifecycle_false_positives == 0` in practice.

## Limitations

Said plainly, because the alternative is someone finding out the hard way.
The quantified version — every loss mode with its boundary numbers and the
test or measurement backing each one — is the operating envelope:
[docs/ENVELOPE.md](docs/ENVELOPE.md).

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
- **There is a measured dead zone between the storm breaker and sparse
  recovery.** A single small anomaly (a low-volume payment service going
  0.01%→0.5% errors) is far too small to spike global sketch energy, and
  once per-series background jitter is large enough, it is also too small
  to survive FISTA recovery — so Layer 2 emits nothing for it. The
  boundary is mapped empirically by `plsim --deadzone` and documented in
  [docs/DEADZONE.md](docs/DEADZONE.md): at production defaults the
  absolute floor is ~0.4 residual units (ratio-shaped series never clear
  it), the floor rises with noise, and per-series jitter σ ≳ 0.3 residual
  units closes palimpsestd's max-residual gate entirely. Alerting below
  that boundary needs the exact tier (ADR-005), by design.
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
- **Mergeability (fleet-wide weak-signal fusion) requires enough emitters.**
  ADR-015's merged-tier trust policy is implemented (`pkg/tier`,
  `internal/core`): a multi-emitter window's recovery is only promoted once
  the `proven` guardrail clears — enough contributing emitters and a
  sufficiently low residual. The mechanism works, but detecting a signal too
  small to see in any one shard still needs enough agents and
  good-enough baselines to clear a real SNR bar. Thin fleets (one or two
  emitters per shard) see no benefit from the merged path.
- **Instance-level forensics is bounded, by design — and the bound is a
  lookback, not root-cause coverage.** The ring buffer is 5-15 minutes; a
  dashcam snapshot is captured once, at flag time. A slow-burn cause that
  started before the window (a memory leak at T-45 minutes that only
  trips a threshold at T-0) is structurally absent from the snapshot; the
  only earlier record is Layer-1 keyframes, at logical — not instance —
  granularity. Memory is hard-capped (`max_instances_per_logical`,
  `max_total_bytes`), and when a cap binds, *new* instances silently stop
  being buffered (their aggregates are unaffected), so drilldown coverage
  narrows exactly during cardinality explosions. There is no "go back and
  look at every pod from last Tuesday." See ADR-008/ADR-009,
  `docs/SPEC.md`, and the quantified trade-offs in
  [docs/ENVELOPE.md](docs/ENVELOPE.md).

## Protocol

The wire frame format — frame layout, hashing/seed derivation, dict
lifecycle, temporal semantics, security considerations, and the codec/
frame-type/predictor registries — is specified independently of this
implementation in [docs/rfc/palimpsest-wire-v2.md](docs/rfc/palimpsest-wire-v2.md).
v2 is current and encoder-mandatory; v1 is read-only legacy. Conformance
covers the byte-exact vector classes only (wire framing, hashing,
`dict_root`, HKDF, KDELTA — see [testdata/golden/](testdata/golden/) and
[oracle/](oracle/)); sparse-recovery vectors are informative, not
conformance-required, since recovery is an implementation choice behind the
wire contract, not part of the protocol itself.

## Cost model

See [docs/COSTS.md](docs/COSTS.md) for the full decomposition and the
honest anti-claim (if you're already self-hosting Prometheus at 60s, this
doesn't save you money — the wedge is per-series-billed SaaS). For a
*measured* number instead of a modeled one, `plsim --month` replays a
simulated month — 10,000 Kubernetes-shaped series, churn, seasonality,
and three scripted incidents that fire every exact-data escape hatch —
and keeps a byte-exact ledger against raw remote-write:
[docs/LEDGER.md](docs/LEDGER.md). Summary of the model:

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

Palimpsest is source-available under the Business Source License 1.1
(`BUSL-1.1`). Non-production use is allowed under the license. Production use,
including internal production use, paid products, hosted or managed services,
resale, sublicensing, and revenue-generating use, requires a separate
commercial license from the copyright holder — see
[COMMERCIAL-LICENSE.md](COMMERCIAL-LICENSE.md) for what that covers and how
to get one.

Each BUSL-licensed version converts to the GNU Affero General Public License,
version 3 only (`AGPL-3.0-only`), on its Change Date. The Change Date rolls
forward on a schedule while the project is under active development (see
[LICENSE](LICENSE) for the current date, and
[.github/workflows/license-rolling-window.yml](.github/workflows/license-rolling-window.yml)
for the mechanism) rather than being fixed at a single date in this README.

"Palimpsest" is a trademark claim independent of the code license — see
[TRADEMARKS.md](TRADEMARKS.md).
