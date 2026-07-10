# ADR-001: Product thesis — alerting as calibrated statistical inference

## Status

Accepted

## Context

Alerting systems either fabricate a model of the world (reconstruction
methods) or refuse to adapt at all (static thresholds), and both failure
modes are worst exactly when it matters most: during an incident, and at
3am when a human has to trust the output without a chance to sanity-check
it. We need a core that (a) makes an explicit, falsifiable coverage
promise, (b) has a dedicated mechanism for the extreme tail rather than
treating it as more of the same distribution, and (c) recalibrates on a
cause it can name rather than on the alert signal itself.

Alternatives considered and rejected:

- **Matrix-completion reconstruction.** Framing missing/anomalous points as
  a low-rank recovery problem fails precisely during incidents, because an
  incident is a rank perturbation — the exact regime the training data
  under-samples. The method's failure mode is silent fabrication: it
  produces a plausible-looking number instead of a coverage statement, and
  a fabricated number at 3am destroys operator trust in the whole system,
  not just that one point.
- **Static thresholds.** No adaptive mechanism means no feedback loop
  between "the world changed" and "the threshold changed." Precision rots
  silently as traffic mix, seasonality, or scale drift, and nobody is
  notified that the threshold is now wrong until it either floods on-call
  with false positives or (worse) goes quiet during a real incident.

This decision is the root of every invariant in
`.github/copilot-instructions.md`: the PID framing (conformal quantile = P,
adaptive miscoverage = I, SW drift = D) only makes sense if the system's
job is stated as calibrated inference, not reconstruction or fixed rules.

## Decision

The product is built on three cooperating layers, each with a distinct and
falsifiable job:

1. **Conformal core** (conformalized quantile regression, see ADR-002) owns
   the day-to-day band and makes an explicit coverage promise:
   `P(y in band) >= 1 - alpha`, adaptively maintained (DtACI).
2. **EVT tail** (GPD, see later ADRs) owns the extreme tail exclusively —
   the region where conformal quantile regression's asymptotics break down
   — and its violations are unsuppressible (invariant 3).
3. **Sliced-Wasserstein drift** feeds forward into recalibration by
   watching input/context distributions only, never nonconformity scores
   (invariant 1) — this is what lets the system tell "the world changed"
   apart from "the incident is happening," instead of conflating the two
   as reconstruction and static thresholds both do.

We explicitly do not reconstruct missing or anomalous data, and we
explicitly do not ship a fixed-threshold mode as anything other than a
`REFUSE`-adjacent degraded path.

## Consequences

- Every band the system emits carries a coverage claim that can be
  falsified by a held-out backtest — this is the engineering discipline's
  "every statistical claim ships with a test that would falsify it"
  applied at the product level.
- We give up the apparent simplicity of "just show me the reconstructed
  signal" — operators see a band and a coverage promise, not a fabricated
  point estimate, which is a UX cost we accept deliberately.
- Because EVT ownership of the extreme tail is exclusive and unsuppressible
  (invariant 3), the conformal core is never asked to stretch its coverage
  promise to cover events its own asymptotics don't hold for.
- SW drift's scope (input/context only, never scores) is inherited by
  every downstream module; violating it anywhere reintroduces the
  boiling-frog failure this ADR exists to prevent.

## Falsification hooks

- **G1** (`docs/GATES.md`) — the core coverage promise, marginal and
  per-stratum.
- **G5** — EVT tail parameter recovery, with REFUSE-to-max-band fallback
  when the fit is ill-posed; this ADR's exclusive-EVT-ownership claim only
  means something if G5 holds.
- The rejected-alternative control experiments this ADR narrates —
  matrix-completion reconstruction fabricating on incident windows, static
  thresholds' precision rotting silently over a regime-shift horizon — are
  not yet formalized as named gates in `docs/GATES.md`, which currently
  scopes G1-G8 to claims about the chosen design rather than the rejected
  ones. Until written, treat those two claims as narratively justified,
  not gate-verified; ordinary (non-`gate`) tests for them belong in
  `tests/score/` and `tests/evt/`.

Module-specific mechanics are falsified in ADR-002 through ADR-004's own
hooks.
