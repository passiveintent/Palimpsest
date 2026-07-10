# ADR-003: Stratification budget — Mondrian buckets and hierarchical shrinkage

## Status

Accepted

## Context

Conformal calibration needs (approximate) exchangeability within a
stratum. The forecaster (ADR-002) already learns seasonality — daily and
weekly cycles are exactly the kind of structure a Holt-Winters-class model
is good at. The question this ADR answers is what stratification (Mondrian
conformal buckets) should exist *on top of* that: not to re-learn
seasonality the model already captures, but to repair the exchangeability
violations the forecaster cannot absorb — chiefly business-hours vs.
off-hours vs. weekend regime shifts in traffic composition.

The naive choice — stratify by hour-of-week x service — produces roughly
8,400 buckets for a moderate service fleet. That is not a stratification
budget, it is a sparsity generator: most buckets would see too few samples
per calibration window to support exchangeable calibration, and the extra
granularity duplicates what the forecaster already learns from seasonality
features. The budget we actually want is the minimum stratification that
repairs exchangeability the model can't reach, not the maximum
stratification the label space allows.

## Decision

- **Buckets.** Mondrian strata = `service x coarse regime` (business-hours
  / off-hours / weekend), ~150 buckets total for a moderate fleet — not
  `hour-of-week x service` (~8,400 buckets). Seasonality that the
  forecaster can learn stays in the forecaster; strata exist only for the
  exchangeability breaks the forecaster cannot learn its way out of.
- **Sparse-bucket handling.** Buckets that are sparse (new service, low
  traffic, rare regime) get hierarchical shrinkage: a weighted sketch-merge
  of bucket-local + service-pooled + fleet-pooled calibration sets, with
  weights set by sample count, so a sparse bucket borrows strength from
  its parents rather than either running on too few samples or being
  silently dropped.
- **Provenance.** Every emitted band carries a calibration-provenance tag
  — `bucket-local | service-pooled | fleet-pooled` — recording which
  weighting regime actually produced it (invariant 6).

## Consequences

- Coverage is defended with roughly 1.8% of the naive bucket count, which
  is what makes hierarchical shrinkage computationally tractable in the
  first place — a 150-bucket ring buffer is a very different cost problem
  than an 8,400-bucket one (ADR-004's recalibration is a weight operation
  over this same buffer).
- Provenance tags mean a human auditing a page always knows whether the
  band that fired was calibrated on rich bucket-local data or leaned on
  service/fleet pooling — this is required, not optional, per invariant 6.
- If the coarse-regime split ever proves too coarse for a specific service
  (e.g., a service with a real intraday-but-not-business-hours cycle that
  the forecaster also fails to learn), that is a new ADR, not a silent
  bucket-count creep back toward hour-of-week granularity.
- This budget is deliberately falsifiable both ways: it must match
  8,400-bucket coverage (proving finer strata buy nothing here) and sparse
  buckets must actually need the shrinkage (proving bucket-local-alone is
  not already sufficient).

## Falsification hooks

- **G1** (`docs/GATES.md`) — per-stratum coverage is G1's explicit,
  non-optional half of the claim; a sparse or under-shrunk bucket that
  fails coverage fails G1 even if the marginal rate looks fine.
- **G6** — the hierarchical shrinkage merge (bucket-local +
  service-pooled + fleet-pooled) is an instance of sketch merge, so it
  inherits G6's associativity/commutativity-within-tolerance requirement.
- The ~150-vs-8,400-bucket budget-parity comparison, the
  seasonality-belongs-in-the-forecaster control, and provenance-tag
  integrity are ordinary falsifiable claims, not yet named gates — cover
  them as regular (non-`gate`) tests in `tests/calibrate/`.
