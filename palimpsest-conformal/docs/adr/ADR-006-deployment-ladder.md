# ADR-006: Deployment ladder — Tier 0 replay, Tier 1 shadow, Tier 2 agent

## Status

Accepted

## Context

Customers need a path from "evaluate this against our existing telemetry"
to "run the full Palimpsest agent," and that path has to be gradual: no
prospect installs an agent before trusting a backtest, and no backend
should have to trust two different sources of truth about whether a
violation happened depending on which tier produced the data. We also need
the compute split between backend and agent to be principled, not
whatever's convenient: thresholds are cheap to ship down, sketches are
cheap to ship up, and raw high-cardinality data should never have to cross
that boundary in either direction.

## Decision

- **Tier 0 — agent-free replay.** Runs entirely off TSDB APIs (Prometheus,
  Datadog, etc.), no agent installed. This is the evaluation/backtest tier
  — same scoring path as production per ADR-002's backtest/product parity
  claim.
- **Tier 1 — streaming shadow-mode.** An agent runs alongside production
  traffic but only shadows; this is where a prospect sees live behavior
  before committing to enforcement.
- **Tier 2 — full Palimpsest agent.** The fidelity upsell: the agent
  additionally corrects fleet-wide percentiles and provides request-level
  exceedances, which is the data EVT needs for tail estimation that
  window-summary-statistic scoring (ADR-002) cannot supply.
- **Compute split.** Thresholds flow down (backend to agent); sketches
  flow up (agent to backend). Raw metrics and raw threshold-inverses never
  cross this boundary.
- **Source of truth.** Violation evaluation happens in the backend in v1,
  regardless of tier — one versioned evaluation path, so a 3am audit of
  "why did/didn't this page" never has to reconcile two different
  evaluators.

## Consequences

- A prospect can be sold on backtest evidence (Tier 0) before ever
  installing anything, which lowers the adoption barrier the whole
  product depends on.
- Because evaluation is centralized in the backend, "why did this page"
  has exactly one answer regardless of which tier is deployed — this is
  the property that makes the ladder safe to offer at all three tiers
  simultaneously to the same customer.
- Tier 2's fidelity improvement (request-level EVT exceedances, corrected
  fleet-wide percentiles) has to be a measurable, not assumed, upsell —
  if Tier 1 already captures the tail well enough, the Tier 2 pitch is
  wrong and needs revisiting.
- Keeping raw data off the threshold-down/sketch-up boundary means a
  customer's raw request data never has to leave their environment in a
  form the backend could reconstruct — a privacy/trust property this
  compute split gets for free.

## Falsification hooks

- **G8** (`docs/GATES.md`) — the percentile-merge falsification fixture
  (naive per-host p95 averaging diverges sharply from true fleet p95) is
  the primary evidence for the Tier 2 fidelity claim; it is the same
  artifact used in the Tier-2 sales pitch, not a separate demo.
- A full Tier 0/1 vs. Tier 2 tail-precision A/B (paired-seed, equal
  budget), backend-evaluation parity across tiers, the threshold-down/
  sketch-up boundary contract, and Tier 0's agent-free replay parity are
  ordinary falsifiable claims, not yet named gates — cover them in
  `tests/replay/` (Tier 0), `tests/sketch/` (agent sketch emission), and
  `tests/connectors/` (TSDB API pulls).
