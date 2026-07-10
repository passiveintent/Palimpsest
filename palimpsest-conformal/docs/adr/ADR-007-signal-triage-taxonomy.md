# ADR-007: Signal triage taxonomy — native-mergeable vs. rollup artifact

## Status

Accepted

## Context

Not all "percentile" metrics a customer hands us mean the same thing
mathematically. A Prometheus histogram or a Datadog distribution metric
(DDSketch) can be merged and re-quantiled correctly across time windows or
series — the aggregation is native to the representation. A statsd-style
summary that pre-computes a gauge percentile client-side cannot be merged
this way at all: averaging or re-aggregating already-computed percentiles
is a well-known statistical error, and treating it as if it were a native
histogram silently corrupts every downstream coverage claim.

If we blend both signal types into one headline backtest number, a strong
result on the native-mergeable subset can be diluted — or a weak result
inflated — by artifacts we know are structurally unfit for the method. We
need a triage step that classifies signal type before scoring, and an
honest story for the degraded case rather than a silently-bad fit.

## Decision

- **NATIVELY_MERGEABLE.** Prometheus histograms (summed buckets, then
  quantile) and Datadog distribution metrics / DDSketch. These get full
  conformal treatment.
- **ROLLUP_ARTIFACT.** Gauge percentiles emitted from statsd-style
  client-side summaries. These are mathematically unfit for re-aggregation
  and are triaged separately.
- **Headline backtest.** Runs on the clean (NATIVELY_MERGEABLE) set only.
  ROLLUP_ARTIFACT results are never blended into the headline number.
- **The data-quality map itself is a customer deliverable** — telling a
  customer which of their signals are native-mergeable vs. rollup-artifact
  is valuable independent of whether we ever calibrate the artifact
  signals at all.
- **Degraded signal handling.** A signal classified ROLLUP_ARTIFACT gets an
  explicit REFUSE from the conformal core, not a silently-fit band. A
  clearly-labeled null result on a rollup artifact is itself part of the
  Tier 2 agent sale (invariant: "null results are results").

## Consequences

- The headline backtest number is defensible under scrutiny — it is never
  quietly propped up (or dragged down) by signals the method was never fit
  for.
- We can sell the data-quality map as a standalone deliverable even before
  a customer adopts full conformal alerting, which gives Tier 0 (ADR-006)
  something concrete to hand over on day one.
- ROLLUP_ARTIFACT signals get worse day-one coverage than native ones —
  that gap is the honest pitch for Tier 2 (agent-side request-level data
  bypasses the rollup problem entirely), not a defect to paper over.
- This taxonomy has to generalize to new backends without becoming an
  ever-growing special-case list; new signal types get classified against
  the same NATIVELY_MERGEABLE / ROLLUP_ARTIFACT test, not a new bespoke
  rule.

## Falsification hooks

- **G8** (`docs/GATES.md`) — the percentile-merge falsification gate is
  this ADR's central evidence: it is precisely the demonstration that
  naive per-host percentile averaging (ROLLUP_ARTIFACT) diverges sharply
  from the true fleet quantile that NATIVELY_MERGEABLE aggregation
  recovers.
- NATIVELY_MERGEABLE aggregation correctness against synthetic ground
  truth, ROLLUP_ARTIFACT classifier accuracy, the conformal core's REFUSE
  on degraded signals, and headline-backtest clean-set-only aggregation
  are ordinary falsifiable claims, not yet named gates — cover them in
  `tests/triage/`.
