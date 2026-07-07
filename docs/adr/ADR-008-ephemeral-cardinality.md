# ADR-008: Ephemeral Cardinality

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Kubernetes churn + per-pod sketching = baselines don't exist.

## Decision

Two-level identity — only persistent *logical* series
(deployment×metric×aggregate) are sketched; pod/instance labels live in an
agent-side ring buffer. Births bootstrap via `dict_delta.init_value` (first
sample never sketched); deaths are tombstoned; keyframes carry a
`dict_root` digest.

## Amendment: golden reconciliation and union-dictionary ownership (2026-07-07)

**This section is a new decision, not a restatement of the original
Decision above.** It was added after a code review found a real bug in
the original tombstone-handling implementation, and closes it with
mechanics the original 16-line Decision never specified either way. Any
prior draft or memory of a draft that implied "authoritative-replace"
tombstone semantics predates this repo's ADR text and is not authoritative
here — see this project's `.github/copilot-instructions.md` for the
general rule this incident established.

**Problem this closes.** `internal/core/engine.go`'s original tombstone
handling applied every incoming tombstone `dict_delta` unconditionally to
a shard's *shared* union dictionary (`ViewState.Dict`), with no notion of
which emitter(s) still depended on the series it named. `view.Dict` is
shared additively across every emitter contributing to a shard (ADR-013)
— but "shared" cut both ways: a *single* emitter's tombstone deleted a
series outright, even while a *different* emitter was still actively
sketching it. This fires on routine pod migration: an old emitter
replica's `SeriesTTL` tombstone for a logical series can race a new
replica's birth for that same series (same shard, same name) in either
arrival order. Whichever tombstone lands last deletes the series from the
shared dictionary out from under whichever emitter is still alive —
`BuildCSR` then has no column for it, and that emitter's genuine residual
energy smears onto whatever series happen to hash-collide with it
(ADR-002's implicit-Phi hashing has no way to detect or report this; it
silently corrupts attribution for the collision group).

A companion bug in every encoder implementation
(`otel/processor/csresidual`, `cmd/plsim`, and the `internal/core` test
harness that mirrors both) made this worse: a golden (Full-dict) keyframe
built `f.DictDeltas` from `Tracker.FullDict()` alone, which enumerates
only *currently active* series — it silently dropped any series that
expired via `Tracker.Expire()` in that same flush window, rather than
carrying an explicit tombstone for it. Omission is not the same as an
explicit tombstone: a decoder has no way to distinguish "encoder forgot
to mention it" from "still alive, just not itemized this frame" from
"just died." The two bugs compound: fixing the golden-keyframe omission
alone, without fixing ownership first, would only *increase* how often a
tombstone reaches the shared dictionary — amplifying the wrongful-eviction
rate. They shipped together.

**Decision.** `pkg/recover.Dictionary` stays a dumb registry — no
ownership concept is added to it. Ownership lives one layer up, at the
engine level: `internal/core.ViewState` gains

```go
Owners map[uint64]map[uint64]struct{} // seriesID -> {emitterID, ...}
```

- A birth `dict_delta` calls `ViewState.claimOwner(seriesID, emitterID)`
  — additive, idempotent, always safe.
- A tombstone `dict_delta` calls `ViewState.releaseOwner(seriesID,
  emitterID)`; the actual eviction (`view.Dict.ApplyDelta`) and the
  ADR-009 substrate-(d) absence event fire only when that call reports
  the owner set is now empty.
- A dead emitter that never sends an explicit tombstone at all (crash,
  permanent partition) is handled by roster liveness, not a special case
  of the above: once `EmitterState.LastArrival` exceeds the operator-set
  `Config.EmitterRosterTTL`, `reapStaleEmitters` walks that emitter's own
  per-view dictionary mirror (`emitterViewState.dict`, which already
  lists exactly what it believes it owns) and releases its claim on
  every series there, evicting wherever that empties the owner set — the
  same mechanism an explicit tombstone triggers, just reached via a
  wall-clock timeout instead of a wire frame. `Config.EmitterRosterTTL <=
  0` (the default) disables this entirely: an emitter's ownership then
  only ever releases via an explicit tombstone, matching pre-amendment
  behavior for any deployment that hasn't opted in.

**Decoder-side golden reconciliation.** The encoder fix (golden keyframes
carry `FullDict()` adds ++ same-window tombstones via `pkg/wire.BuildKeyframe`)
closes the primary path. As defense in depth against any other cause of
encoder/decoder dictionary drift, a golden (non-KDELTA) keyframe is also
treated as authoritative for the receiving emitter's per-view dictionary
mirror (`emitterViewState.dict`): `internal/core/engine.go`'s
`reconcileGoldenDict` runs before `VerifyKeyframe` on every golden frame.
It computes the set of non-tombstone IDs announced in the frame's
`DictDeltas` (the frame's declared active set) and compares it against
every ID currently marked active in `evs.dict` (the per-emitter mirror).
Any ID present in `evs.dict` but absent from the declared set is treated
as an implicit tombstone: `view.releaseOwner` is called for that ID from
this emitter, and the eviction/absence-event chain proceeds exactly as an
explicit tombstone `dict_delta` would — `view.Dict` is evicted and an
ADR-009 absence event fires only if `releaseOwner` reports the owner set
is now empty.

`view.Dict` (the shared union dictionary) is *never* reconciled wholesale
from a single emitter's golden frame: the mechanism only ever removes this
one emitter's claim, exactly as an explicit tombstone does. Multi-emitter
ownership (`Owners`) governs whether an eviction propagates to the shared
dict — an ID claimed by two emitters is not evicted when one emitter's
golden frame fails to list it; only that emitter's ownership is released.

`VerifyKeyframe` becomes a post-condition of `reconcileGoldenDict` for
golden frames: a root mismatch after reconciliation signals an encoder bug,
not decoder drift. For KDELTA frames `VerifyKeyframe` is unchanged — the
sole drift detector, as before (KDELTA frames are not authoritative for the
full active-ID set by definition, so `reconcileGoldenDict` does not run on
them).

**Design rule.** On the union dictionary, *err additive*: a zombie
entry — a series whose true owner died without either an explicit
tombstone or a roster-TTL expiry having caught up to it yet — costs one
benign CSR column (its residual settles near zero once nothing is
contributing to it, per ADR-002/ADR-003's open-loop coding). A *wrongful
eviction* costs correctness: it corrupts attribution for every series
that hash-collides with the wrongly-evicted one. Presence is cheap.
Removal requires proof of universal absence — every known owner having
explicitly or implicitly (roster TTL) released its claim — never a single
emitter's unilateral say-so.

**Non-goals.** This amendment changes no wire bytes (tombstone
`dict_delta`s are unchanged; the golden-keyframe fix only adds *more* of
the same, already-specified, delta type to the wire) and no encode-side
behavior beyond golden-keyframe completeness — an encoder's own `Tracker`
has no ownership concept and needs none; it already only ever tombstones
series *it itself* birthed. `Owners` is decode-side-only, in-memory,
unpersisted state, matching `ViewState`/`Dictionary`'s existing
non-persistence.
