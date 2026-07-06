# csresidual

An OpenTelemetry Collector **metrics** processor that sketches
high-cardinality metric residuals into small linear measurements
(compressed sensing) instead of shipping every series to a
per-series-billed backend. It implements ADR-005, ADR-007, ADR-008,
ADR-009, ADR-010, ADR-011, ADR-012, and ADR-013 from the Palimpsest repo
root (`../../docs/adr/`); `../../docs/SPEC.md` is the normative wire/math
spec this package encodes against. ADR-015 (merged tier) touches only the
`merged` tier's config validity here — its actual policy logic is entirely
decode-side (`internal/core`).

This module (`github.com/passiveintent/Palimpsest/otel`) is intentionally
separate from the root `github.com/passiveintent/Palimpsest` module
(ADR-007: "OTel deps churn fast; core must be embeddable"): the collector
SDK and its dependency tree live here; `pkg/sketch`, `pkg/wire`, and
`pkg/predict` (imported via a `replace` directive at the repo root, since
this module isn't published standalone) stay stdlib + xxhash only.

## What it does

1. **Tiers** every incoming metric (`tiers` config, ADR-005): summary
   (quantile) metrics are always forced `exact`; everything else matches
   the first `tiers` regex rule, defaulting to `sketched` if none match.
   `exact`-tier metrics pass through the pipeline completely untouched.
2. For `sketched`-tier metrics, builds each datapoint's **logical series
   identity** (`../../docs/SPEC.md` "Identity", ADR-008): pod/instance
   labels (`instance_key`) never enter the sketch — they ride an
   agent-side ring buffer instead. Histogram buckets each become their own
   logical series (`name|le=<bound>,...|agg=sum`).
3. **Folds** every instance's latest sample this window into
   sum/count/max per logical group (`aggregates` config) — these folded
   numbers, not raw per-instance samples, are what enter the dictionary
   lifecycle and sketch.
4. Runs each folded value through a `pkg/sketch.Tracker` +
   `pkg/predict.Hold` + `pkg/sketch.Accumulator` per **(shard, view)**
   pipeline (`shard_by` / declared `views`, ADR-010), completely
   independent state per pipeline.
5. On a `flush_interval` timer, emits a `pkg/wire.Frame` per pipeline:
   `KEYFRAME` on `keyframe_every` cadence (golden/full-dict every
   `keyframe_full_dict_every`, ADR-011), `FALLBACK` on a storm-energy
   trigger (ADR-004), else `RESIDUAL`. A churn breaker (ADR-009) degrades a
   crash-looping pipeline to aggregate-only buffering; a snapshot stager
   (ADR-009/ADR-012) attaches a gzip instance-ring-buffer blob to the frame
   when a logical series' local z-score crosses `snapshot.residual_threshold`.
6. Writes each frame out via `output.type` (`file`, the default, or
   `kafka`), unless a custom `frameSink` is wired in (as the test suite
   does, in-memory). `file` writes one file per frame under `output.dir`
   (`emitter<hex>-shard<hex>-epoch<hex>-view<hex>-seq<hex>.plmp`). `kafka`
   publishes to `output.kafka.topic` via `internal/adapters/kafka`, keyed by
   `tenant_id||shard_id` so one shard's frames land on one partition and are
   delivered in order (ADR-013 doesn't need cross-partition ordering: it
   keys recovery state on `(emitter_id, epoch, seq)` inside the frame, never
   on transport arrival order); a publish failure spools the frame to
   `output.kafka.spool_dir` (bounded by `spool_max_bytes`) instead of
   dropping it, and every subsequent publish attempt — plus a
   `drain_interval` background tick, so a quiet pipeline's backlog doesn't
   wait on new traffic — drains that backlog, oldest frame first, once the
   broker is reachable again.

## Building a collector with this processor (OCB)

This processor is a library, not a runnable binary: use the
[OpenTelemetry Collector Builder](https://github.com/open-telemetry/opentelemetry-collector/tree/main/cmd/builder)
(`ocb`) to generate and build a custom collector distribution that
imports it, alongside whatever receivers/exporters you need.

```bash
go install go.opentelemetry.io/collector/cmd/builder@v0.155.0
```

`builder-config.yaml` (adjust the receiver/exporter list to your fleet;
`gomod` here matches the versions this processor was built and tested
against — see `go.mod` in this directory):

```yaml
dist:
  name: palimpsest-otelcol
  description: OTel Collector distribution with the Palimpsest csresidual processor
  output_path: ./dist

exporters:
  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.155.0

receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.155.0

processors:
  - gomod: go.opentelemetry.io/collector/processor/batchprocessor v0.155.0
  - gomod: github.com/passiveintent/Palimpsest/otel v0.0.0
    # Local checkout, not yet published: point at your clone.
    # path: /abs/path/to/Palimpsest/otel
```

If you haven't published this module, add a `replace` line under `dist:`
(ocb supports a `replaces:` list) pointing at your local checkout:

```yaml
replaces:
  - github.com/passiveintent/Palimpsest/otel => /abs/path/to/Palimpsest/otel
  - github.com/passiveintent/Palimpsest => /abs/path/to/Palimpsest
```

Then generate and build:

```bash
builder --config builder-config.yaml
./dist/palimpsest-otelcol --config collector-config.yaml
```

Register the factory from `factory.go` (`csresidual.NewFactory()`) — ocb
wires this in for you from the `processors:` list above; you only need to
call it directly if you're assembling a collector by hand instead of via
ocb.

## Config reference

```yaml
processors:
  csresidual:
    tenant_id: "acme-corp"                 # required (ADR-012)
    tenant_key_env: PALIMPSEST_TENANT_KEY  # required; HKDF seed source, read from env, never in YAML (ADR-012)
    shard_by: [service.name]               # optional; label set that partitions series into independent sketch shards
    logical_key: [service.name, k8s.deployment.name]  # required (ADR-008); default view's identity labels
    instance_key: [k8s.pod.name]           # instance-level labels; ring-buffered, never sketched (ADR-008)
    aggregates: [sum, count, max]          # >=1 of sum/count/max; folded per logical group per window

    views:                                 # optional (ADR-010); omit for a single implicit view (id 0) using logical_key
      - name: "by_deployment"
        labels: [service.name, k8s.deployment.name]
      - name: "by_region_az"
        labels: [cloud.region, k8s.cluster.name]

    m: 2000                                # sketch width
    d: 6                                   # hashes per series
    bits: 8                                # RESIDUAL quantization width: 8 or 16

    flush_interval: 10s
    keyframe_every: 6                      # every Nth window is a KEYFRAME instead of RESIDUAL/FALLBACK
    keyframe_full_dict_every: 10           # golden (full, non-KDELTA) every Nth KEYFRAME (ADR-011)
    golden_keyframe_every: 10              # alias for the field above (docs/SPEC.md's own name for it);
                                            # set only one, or set both to the same value
    epoch_rotate: 1h                       # ADR-013: new independent coordinate system every rotate interval
    series_ttl: 90s                        # logical series silent this long -> tombstoned

    storm:                                 # ADR-004
      energy_multiplier: 25                # trigger FALLBACK when energy > multiplier * rolling median
      fallback_topk: 100                   # heavy-hitters carried in a FALLBACK frame

    snapshot:                              # ADR-009, ADR-012 speculative push
      enabled: true
      residual_threshold: 2.0              # local z-score
      max_series_per_flush: 100            # caps how many staged snapshots one flush merges into a blob
      blob_ttl: 1h                         # a staged-but-unpicked snapshot older than this is dropped

    ringbuffer:
      window: 15m                          # per-instance sample retention
      max_instances_per_logical: 1000      # per-group instance cap
      max_total_bytes: 100MB               # approximate global bound across all groups/instances

    churn:                                 # ADR-009 breaker
      max_births_per_logical_per_min: 100  # trips the pipeline's breaker; degrades to aggregate-only + alert

    pull:                                  # ADR-012, optional, default off (zero listening sockets)
      enabled: false
      addr: ":8889"
      mTLS: true
      bearer_token_env: PALIMPSEST_PULL_TOKEN
      # Required when mTLS is true. Not part of the field set this
      # processor's illustrative spec started from, but real mTLS needs a
      # server cert and a CA pool to verify client certs against:
      tls_cert_file: /etc/palimpsest/pull-server.crt
      tls_key_file: /etc/palimpsest/pull-server.key
      tls_client_ca_file: /etc/palimpsest/pull-client-ca.crt

    drilldown_hint: true                   # tag passthrough datapoints with a palimpsest.logical_series attribute

    tiers:                                 # ADR-005; first match wins, default sketched, summaries always exact
      - match: '^billing_.*'
        tier: exact
      - match: '^fleetwide_.*'
        tier: merged                       # sketched identically to `sketched` here (ADR-015: no agent-side
                                            # behavior change) -- see the merged-tier note below
      - match: '.*'
        tier: sketched

    shadow: false                          # true: sketched-tier datapoints also continue downstream unchanged
                                            # false: they're removed from the pipeline (now represented by frames only)
    output:
      type: file                           # "file" (default) or "kafka"
      dir: /var/lib/palimpsest/frames       # used when type: file
      kafka:                               # used when type: kafka
        brokers: ["kafka-0:9092", "kafka-1:9092"]
        topic: palimpsest-frames
        spool_dir: /var/lib/palimpsest/kafka-spool   # bounded on-disk backlog for broker outages
        spool_max_bytes: 1GB
        drain_interval: 10s                # background retry tick, independent of flush_interval
```

### Notes

- **`shadow`**: in `shadow: true`, sketched-tier datapoints are folded into
  frames *and* left in place for the rest of the pipeline (useful for a
  side-by-side rollout, comparing sketch-recovered values against the
  ground truth downstream). In `shadow: false` (the cost-saving mode),
  they're removed once sketched — only `exact`-tier data and emitted
  frames represent them from that point on.
- **`output.type: kafka`** depends on `github.com/twmb/franz-go` (chosen
  over `segmentio/kafka-go` for its idempotent-by-default producer and
  consumer-group manual-offset-commit support — see
  `internal/adapters/kafka`'s package doc at the repo root for the full
  justification) — pulled in transitively via this module's dependency on
  the root module's `internal/adapters/kafka` package. This is the "deps in
  otel module are acceptable" case ADR-007 draws a line around: the line is
  `pkg/*` staying stdlib + xxhash, not this module.
- **Views and shards are fully independent state.** Each (shard, view)
  pair gets its own `Tracker`/`Accumulator`/dictionary/ring buffers/churn
  breaker; nothing sums or leaks across them (ADR-010, ADR-012).
- **The pull endpoint is off by default and stays off** unless you set
  `pull.enabled: true`; when enabled it requires both mTLS (client cert
  verified against `tls_client_ca_file`) and a bearer token — there is no
  configuration that opens it without both. See
  `../../demo/networkpolicy-csresidual.yaml` for an example NetworkPolicy
  (deny-all-ingress unless you've opted into `pull.enabled`).
- **Tenant key and pull bearer token are read from the environment named
  by `tenant_key_env` / `pull.bearer_token_env`, never from YAML** —
  keeping secrets out of config files and collector logs (ADR-012).
- **`merged` tier (ADR-015) is agent-side-invisible.** A metric routed to
  `merged` is sketched exactly like `sketched` here — the trust-guardrail
  policy that makes cross-emitter fusion a safe product feature lives
  entirely on the decode side (`internal/core.Config.MergedViews`, keyed by
  `ViewID`). To dedicate a view to merged-tier traffic, give it its own
  `views` entry and configure a matching `MergedPolicy` at the same `ViewID`
  index on the decoder — the same implicit position-based correlation every
  other declared view already relies on. The one thing this ADR asks of a
  fleet-wide config: every emitter contributing to a merged-tier view must
  share the same `m`/`d`/`active_key_version` (a differing value builds an
  incompatible `Phi` or seed, making "summing" meaningless) — in the normal
  single-collector-config-per-fleet deployment shape, this is already
  guaranteed by construction, not something to configure separately.

## Package layout

| File | Responsibility |
| --- | --- |
| `factory.go` | `component.Type`, `processor.Factory`, default config |
| `config.go` | `Config` struct, validation, view resolution |
| `identity.go` | logical series naming, tier matching |
| `aggregate.go` | per-window instance fold (sum/count/max) |
| `ringbuffer.go` | per-logical instance buffer, bounds, pull endpoint |
| `seed.go` | tenant key loading, HKDF seed derivation, epoch indexing |
| `breaker.go` | churn circuit breaker integration, alerts |
| `snapshot.go` | residual z-score gating, staged snapshot blobs |
| `processor.go` | pipeline orchestration, `ConsumeMetrics`, flush loop |

## Testing

```bash
go test ./...
```

`processor_test.go` constructs `metricsProcessor` directly (bypassing the
factory) with an in-memory `frameSink` and calls `consumeMetrics`/
`flushAll` with explicit timestamps, so the churn/epoch/TTL/cadence tests
are deterministic rather than depending on real wall-clock sleeps.
