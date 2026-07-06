# ADR-013: Temporal Semantics

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Clock drift + out-of-order arrival = misaligned summation = garbage.

## Decision

`emitter_id` + window-indexed `seq` (not wall-clock); watermark/repair
engine exploits sketch additivity for late/out-of-order frames;
coverage-annotated, revisioned results. Caught a latent multi-agent
correctness bug before any code shipped.

## Addendum: herd jitter on epoch rotation

**Problem.** Epoch index is a pure wall-clock bucket
(`floor(t / epoch_rotate)`, `otel/processor/csresidual/seed.go`'s
`epochIndex`): every emitter sharing the same `epoch_rotate` crosses each
epoch boundary at the *exact same instant*. At rotation, an emitter
rebuilds its Accumulator/Tracker from scratch and must open the new epoch
with a golden (Full) keyframe — the most expensive frame this system
emits (ADR-011: byte-parity with raw remote-write before compression).
A fleet-wide simultaneous rotation is a synchronized CPU/bandwidth spike:
a thundering herd.

**Decision.** Delay each emitter's rotation by a deterministic,
`emitter_id`-derived offset: the rotation instant for emitter `e` is
`epoch_boundary + (xxh64(emitter_id) % jitter_window)` (`jitter_window`
config, default 60s). `pkg/sketch.EpochJitter`/`EpochIndex` implement this
(xxhash is already the sole allowed `pkg/*` dependency, ADR-007) —
`otel/processor/csresidual`'s `epochIndex` now delegates to it, keyed by
that processor's own `emitterID` and `cfg.EpochJitterWindow`.

**No wire change.** Both sides can recompute an emitter's offset from
`emitter_id` alone (already on every frame, ADR-013's own `emitter_id`
field) — a decoder never needs the offset itself, since it only ever
reads whatever `epoch` a frame already carries (unmodified from before
this addendum) and continues to track each emitter's epoch independently
(`EmitterState.Epoch`, `internal/core/engine.go`). Jitter is purely an
encoder-side scheduling decision; the decode path was already correct for
a fleet split across two concurrent (epoch, view) coordinate systems
(every `EmitterState` tracks its own `Epoch`, and late/early frames near
any boundary are handled by the existing epoch-keyed state) — jitter just
makes that split the common case at every rotation instead of an edge
case.

**Verification.** `pkg/sketch.TestEpochJitterSpread`: 1000 simulated
emitters' jitter offsets bucketed into 1-second buckets across a 60s
window — max per-second bucket stays under 5% of the fleet (measured ~2.7%
on a representative run). `internal/core.TestE2E_JitteredEpochRotationZeroErrors`:
200 emitters staggered across a jitter window through a real Engine —
zero gated/dropped/degraded outcomes attributable to the transition.
