# ADR-008: Incident fingerprint corpus — the V1-to-V2 bridge

## Status

Accepted

## Context

V2 wants to narrate incidents (HMM-style state decoding) and eventually
retrieve similar past incidents (Wasserstein prefix-retrieval), but
building the matching/retrieval code before there is a corpus to match
against is premature — and building a corpus that isn't ready when V2
needs it is a much slower mistake to recover from. The right move is to
define the schema now, populate it from day one (both offline replay and
online flywheel), and defer the matching code until the corpus and the
product justify it.

Two constraints shape the schema. First, invariant 8: every channel must
be dimensionless (calibrated p-values, drift scores, EVT flags), never a
raw-unit metric, or fingerprints from a low-traffic service and a
high-traffic service could never be compared — the whole value of a
cross-service, cross-customer corpus depends on this. Second, invariant 7:
whatever eventually reads this corpus to narrate an incident (Viterbi
decoding, Wasserstein retrieval) must be structurally unable to feed back
into alpha, bands, suppression, or routing — a mis-decoded "Recovering"
state must not be capable of silencing a page, no matter how good the
decoder gets.

## Decision

- **Corpus content, per confirmed incident:**
  - Raw per-stratum series of calibrated p-values, SW drift scores, and
    EVT flags.
  - Four derived channel sketches: severity marginal; onset-lag
    distribution across strata; a `(phase, severity, drift)` point cloud;
    the affected-strata set.
  - Metadata, and an `incident_id` joining the violation log (ADR-005).
- **Dimensionless by construction (invariant 8).** Every channel is a
  calibrated p-value, a normalized drift score, or an EVT flag — never a
  raw-unit metric. This is what lets a fingerprint from one service
  transfer to another, and one customer's corpus entry be structurally
  comparable to another's.
- **Two writers, one corpus.** The replay harness (ADR-006's Tier 0)
  bootstraps the corpus offline from prospect history; the flywheel
  (ADR-005) continues writing it online, post-launch. Both writers target
  the same schema.
- **V2 sequencing — recorded now, not built now:**
  1. Viterbi narration first: cold-start-free, ~25 printable parameters,
     with emissions defined only on the dimensionless channels
     (p-value/drift/EVT) — never on raw metrics.
  2. Wasserstein prefix-retrieval second, gated on the corpus reaching
     >= ~30 incidents AND passing a leave-one-incident-out precision@3
     kill-gate against a dumb baseline (Jaccard on affected services +
     peak severity). If retrieval doesn't beat the dumb baseline, it does
     not ship.
- **Decoder narrates, conformal decides (invariant 7).** Both the Viterbi
  narrator and the (future) Wasserstein retrieval are read-only consumers
  of the violation log and the fingerprint corpus. Neither has a write
  path to alpha, bands, suppression, or routing.

## Consequences

- Building the schema before the matching code means the corpus is
  already populated (via replay-harness bootstrap) by the time Viterbi
  narration is worth building, instead of narration launching against an
  empty corpus.
- The kill-gate on Wasserstein retrieval means that feature can be
  deferred indefinitely without blocking anything else — it is optional
  by design, gated on both corpus size and a real performance bar against
  a dumb, cheap baseline.
- Because both writers (replay + flywheel) target one schema, there is
  never a "the offline corpus and the online corpus don't quite match"
  reconciliation problem to solve later.
- Invariant 7 means a badly-decoded incident state is a narration quality
  problem, never a paging-safety problem — the worst a bad decoder can do
  is describe an incident unhelpfully, not silence one.

## Falsification hooks

No G1-G8 gate (`docs/GATES.md`) currently covers this ADR's claims — the
statistical gate scheme is presently scoped to the conformal core, drift,
EVT, sketch algebra, flywheel, and triage modules, not yet the corpus. This
ADR's falsification hooks are ordinary (non-`gate`) tests until that
changes:

- Every persisted channel is dimensionless; a raw-unit metric write is
  REFUSEd (invariant 8) — `tests/corpus/`.
- Fingerprint distance is invariant to per-service/customer raw-unit
  rescaling — `tests/corpus/`.
- Replay-bootstrapped and flywheel-online corpus entries for the same
  `incident_id` are schema-identical and joinable against the violation
  log — `tests/corpus/`, `tests/replay/`, `tests/flywheel/`.
- The decoder/retrieval layer has no reachable write path to alpha,
  bands, suppression, or routing (invariant 7) — `tests/corpus/`.

The retrieval kill-gate this ADR specifies — leave-one-incident-out
precision@3 vs. the Jaccard + peak-severity dumb baseline, evaluated once
the corpus reaches >= 30 incidents — is a strong candidate for a future
named gate (`G9`) once the corpus exists to run it against; it is not
formalized in `docs/GATES.md` yet because there is no corpus to gate.
