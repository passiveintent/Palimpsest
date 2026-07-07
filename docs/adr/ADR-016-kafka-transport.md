# ADR-016: Kafka Transport

**Status:** Accepted (implemented)

## Problem

The only transport between an agent and `palimpsestd` was the fswatch
convention (ADR-007): one `*.plmp` file per frame on a shared filesystem
(or `httpsrc`'s speculative-push HTTP endpoint, ADR-012). That's the right
default — zero extra infrastructure, trivially inspectable — but it doesn't
fit every deployment: no shared filesystem between agent and reconstructor,
or an existing message-bus operational story a team would rather reuse.
Palimpsest needs a durable, at-least-once transport option without
loosening any of ADR-013's temporal-alignment guarantees or ADR-007's
dependency quarantine.

## Decision

### Library: franz-go over segmentio/kafka-go

`internal/adapters/kafka` (root module) uses
`github.com/twmb/franz-go`: a pure-Go client with an **idempotent producer
by default** (the broker dedupes a producer's own retries — one layer of
"exactly-once-adjacent" behavior for free) and a consumer-group
implementation that supports manual offset commit, which `Source` needs to
get an honest at-least-once contract (commit only after every record in a
fetch has been handed to `handle`). `segmentio/kafka-go` doesn't give
idempotent production by default and its consumer-group support has a
rockier history. Neither library — nor Kafka itself — gives end-to-end
exactly-once against palimpsestd's own state; that guarantee already
exists one layer up (see below), so transport-level idempotence is a nice
property, not a load-bearing one.

### Partitioning: `tenant_id||shard_id`

Every record is keyed by `PartitionKey(tenantID, shardID)`
(`tenant_id||shard_id`), so every frame belonging to one shard — across
every window and emitter — lands on one partition and is therefore
delivered to a `Source` in send order *within that partition*.
Cross-partition (cross-shard) ordering is deliberately not preserved, and
doesn't need to be: ADR-013 keys all recovery state on `(emitter_id,
epoch, seq)` carried inside the frame itself, never on transport arrival
order. `tenant_id` has no wire representation (ADR-012: tenancy is a
cryptographic boundary via HKDF seed derivation, not a frame field), so
it's supplied out-of-band — the tenant a given `Sink`/topic is scoped to —
rather than read off the frame.

### Delivery semantics: at-least-once, ledger absorbs it

`Source` commits offsets only after `handle` has run for every record in a
poll batch, so a crash between delivery and commit redelivers the whole
batch on restart. This is intentionally at-least-once, not exactly-once:
ADR-013's `WindowState.Contributors` ledger already rejects a second
contribution from an emitter that already contributed to a window
(originally built for network-retry/duplicate-frame handling, independent
of transport), so Kafka redelivery is just another source of the same
duplicate-frame case the engine already handles
(`TestE2E_DuplicateResidualFrameDeduped`). Reimplementing dedup at the
transport layer would be redundant with, and no more correct than, the
ledger that already exists.

### Agent-side spool-then-forward

`SpoolingSink` wraps a `Sink` (or any `Producer`) with a bounded on-disk
backlog: a publish failure spools the frame instead of losing it, and
every subsequent publish attempt (plus, in `otel/processor/csresidual`'s
`kafkaFrameSink`, a `drain_interval` background tick so a quiet pipeline's
backlog doesn't wait on new traffic) drains the backlog, oldest frame
first, once the broker is reachable again. Once maxBytes is reached,
further frames are dropped rather than growing disk unboundedly — a
bounded, observable data gap over an unbounded one.

## Consequences

* `internal/ports`: new `FrameSink` interface, the encode-side mirror of
  the existing `FrameSource` (`Send(ctx, f) error`, `Close() error`).
* `internal/adapters/kafka` (root module): `Source` (consumer group),
  `Sink` (producer), `SpoolingSink`, `PartitionKey`. Depends on
  `github.com/twmb/franz-go`; `pkg/*` is untouched and stays stdlib +
  xxhash (ADR-007) — the dependency lives entirely in the service module.
* `cmd/palimpsestd`: `--kafka-brokers`/`--kafka-topic`/`--kafka-group`
  flags wire a `kafka.Source` in alongside (not instead of) `fswatch`/
  `httpsrc`; multiple `FrameSource`s may run concurrently.
* `otel/processor/csresidual`: `output.type: kafka` (alongside the existing
  `file` default) selects a `kafkaFrameSink` built on `SpoolingSink`. This
  otel-module file imports the root module's `internal/adapters/kafka` —
  allowed despite `otel/` being a separate Go module, because Go's
  internal-import rule is scoped by import-path text tree
  (`github.com/passiveintent/Palimpsest/...`), not module boundaries — so
  there is exactly one Kafka client implementation, not two. This is the
  "deps in otel module are acceptable" case: ADR-007's quarantine line is
  `pkg/*`, not `otel/`.
* `go.mod` (root): `go 1.22` -> `go 1.25.0` (franz-go's mock-broker test
  package, `kfake`, requires it); `otel/go.mod` was already on `go 1.25.0`.
  `demo/palimpsestd.Dockerfile` and `demo/plsim.Dockerfile` bumped their
  build stage to match (`demo/otelcol.Dockerfile` already was).
* Tests: unit tests (both modules) run against `kfake`, an in-memory mock
  Kafka cluster — no docker required, part of the normal `go test ./...`
  run in CI. A separate `internal/adapters/kafka` suite, gated behind the
  `integration` build tag, runs the same scenarios (per-partition ordering,
  redelivery dedup, broker-outage spool/drain/repair) against a real,
  single-node KRaft broker (`demo/docker-compose.yml`'s `kafka` profile);
  it is not part of CI (ACCEPTANCE: "integration green locally") — see
  `demo/README.md`'s Kafka section.

## Guardrails

* Kinesis, Cloud Pub/Sub, and Event Hubs are natural follow-ups copying
  this same `ports.FrameSource`/`ports.FrameSink` shape — none of them are
  scaffolded here.
* `internal/adapters/kafka` stays under ~500 lines total; if the port
  abstraction starts leaking Kafka-specific concepts back into
  `internal/ports` or `internal/core`, that's a signal to stop and
  refactor rather than grow the adapter further.
