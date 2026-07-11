# ADR-004: Drift separation — SW on inputs only, reversible recalibration

## Status

Accepted

## Context

There are two very different reasons a band might stop looking right:
the world the service operates in changed (traffic mix, request
composition, a deploy), or the service itself is behaving badly (an
incident). Conflating these is the single most dangerous failure mode
available to this system: if recalibration is triggered by drift in the
*nonconformity score itself*, then a slow-burn incident — one that
degrades gradually rather than spiking — looks identical to "the world
changed," and the system will widen its band to accommodate the
degradation instead of flagging it. That is the boiling-frog failure
(invariant 1), and it is exactly the silent-precision-rot problem ADR-001
rejected static thresholds for, reintroduced through the back door of
adaptive recalibration.

The fix is a hard separation of what Sliced-Wasserstein drift is allowed
to watch: input/context features (traffic mix, request composition, deploy
markers) only, never scores. Score-distribution drift is not a
maintenance signal — it is the thing we alert on.

## Decision

- **Drift scope.** SW drift operates exclusively on input/context feature
  distributions. It is structurally forbidden from taking nonconformity
  scores as input (invariant 1) — enforced at the type/contract level, not
  just by convention.
- **Interpretation.** Score-distribution drift is SIGNAL (route to
  alerting). Input-distribution drift is MAINTENANCE (route to
  recalibration). These are different code paths with different
  consumers.
- **Recalibration mechanics.** Recalibration is an exponential-decay
  weighted merge over the time-bucketed sketch ring buffer (the same
  buffer ADR-003's strata live in) — never a delete, always a reweight
  (invariant 5). On a detected changepoint: zero out pre-changepoint
  bucket weights, temporarily boost DtACI's gamma (faster adaptive
  miscoverage response), then anneal both back to steady state.
- **Auditability.** Because recalibration is pure reweighting, the full
  history remains reconstructible — a "what did the band look like before
  this changepoint" query is always answerable from the ring buffer.

## Consequences

- The system can now tell "traffic mix changed" apart from "the service is
  degrading" — this is the entire reason SW drift exists as a component
  rather than folding drift detection into the conformal core's own
  miscoverage signal.
- Recalibration is cheap (a weight operation, not a refit from scratch),
  reversible (anneal-back, no deletion), and auditable (full ring-buffer
  history retained) — this is what invariant 5 requires and what makes a
  post-incident "why did the band move" review possible.
- **Retention is a distinct policy from recalibration, and the ring is
  sized to keep them from colliding** (amendment). Reweighting never deletes
  data; the only thing that removes a bucket is the ring aging past its
  fixed `capacity`. To keep that retention boundary from silently eating the
  auditability promise, `BucketRing` enforces
  `capacity >= calibration_horizon + audit_horizon` at construction (a loud
  error, not a runtime surprise): the ring must always be large enough to
  hold every bucket recalibration might reweight PLUS every bucket a
  "what did the band look like before this changepoint" audit might query.
  Spilling evicted buckets to cold storage for longer-horizon audits is a v2
  note, not built now.
- The cost of this separation is that a genuinely ambiguous case — where
  the operational cause of a real change happens to also be reflected in
  score drift — will not auto-recalibrate; it will alert instead. That is
  the intended, conservative failure direction: default to paging, never
  default to silently widening.
- Any future decoder/retrieval layer over the violation log and
  fingerprint corpus (ADR-008) is read-only with respect to this
  mechanism — it may narrate what drift/recalibration did, but it may
  never trigger or suppress it (invariant 7).
- **Decay weighting formally lapses the finite-sample coverage guarantee —
  by design, not by bug** (amendment). The exponential-decay weighted merge
  is the operating mode of the ring, but under `decay < 1` the weights are
  not likelihood ratios, so weighted-exchangeability theory does not apply
  and split conformal's finite-sample `1 - alpha` guarantee formally lapses.
  This is the architecture working as intended: the textbook guarantee holds
  only at `decay = 1` (uniform weights), and maintaining *empirical* coverage
  under decay-weighted, non-exchangeable calibration is precisely what the
  DtACI/ACI adaptive-miscoverage layer (ADR-002's "I" term) exists to do. Two
  consequences follow. (a) Every band's calibration provenance MUST carry the
  `decay` in force, so a post-incident audit can distinguish a `decay = 1`
  textbook-guaranteed band from a `decay < 1` adaptively-weighted one. (b) The
  coverage cost of `decay < 1` is tracked (a non-gating test), separate from
  the `decay = 1` coverage gate (G1), which remains the certified guarantee.

## Falsification hooks

- **G3** (`docs/GATES.md`) — the boiling-frog control: SW must never
  trigger recalibration on pure score drift under stationary inputs, and a
  band violation must fire before drift magnitude reaches a declared
  bound. The SW-triggers case is an invariant breach, not a tunable miss.
- **G4** — on a genuine input-distribution changepoint, recalibration
  (zero-out-then-anneal plus temporary DtACI gamma boost) completes within
  a declared window budget with false pages during recovery under a
  declared cap.
- **G6** — the ring-buffer weight operations this ADR specifies
  (zero-out, anneal-back) must be reversible within tolerance, with no
  calibration data deleted (invariant 5).
- The drift detector's input-type contract (structurally REFUSEs
  score-shaped data, enforcing invariant 1 at the type level) is an
  ordinary contract test, not yet a named gate — cover it in
  `tests/drift/`.
