# ADR-005: Flywheel v1 — append-only violation log, human-gated alpha proposals

## Status

Accepted

## Context

Human judgment is valuable feedback, but it is also the single easiest way
to quietly corrupt a calibrated system: if a human's label of "this alert
was wrong" can touch a sketch, a calibration set, or live alpha directly,
the system stops making a falsifiable coverage promise and starts making a
promise conditional on whoever last clicked "not helpful" (invariant 2).
We need a flywheel — labels should make the system better over time — that
cannot become a backdoor around the conformal core's guarantees.

At the same time, v1 has to ship something usable now: a violation log,
some way to act on it (routing), and some way for the system to slowly
improve (alpha proposals), without building the full suppression/routing
engine (multi-human concordance, TTL rules, BigPanda-scale scope) that a
mature product eventually needs but v1 does not.

## Decision

- **Violation log.** Append-only. Band violations are immutable events;
  nothing in v1 (or any future version) may update or delete a logged
  violation.
- **Labels train nothing online.** Human labels are logged immutably
  alongside the violation they annotate. They never touch sketches,
  calibration sets, or live alpha directly (invariant 2).
- **Alpha adjustment.** A weekly batch job may PROPOSE per-service alpha
  adjustments from accumulated labels. Proposals are human-approved before
  application, rate-limited, and floored at `alpha_max` — the system can
  never propose its way to an alpha that under-covers past the floor.
- **EVT is exempt from all of this.** EVT-zone violations page
  unconditionally; no rule, label, or alpha change can suppress them
  (invariant 3), including anything this flywheel produces.
- **Routing (v1 scope).** A severity-tagged webhook to PagerDuty/Opsgenie.
  No online suppression engine, no multi-human concordance, no TTL rules —
  those are deferred to v2 and are explicitly out of scope here.

## Consequences

- The system's coverage promise (ADR-001) remains true regardless of label
  volume or label quality, because labels cannot reach the mechanisms that
  would let them distort it — this is the load-bearing property of
  invariant 2, and it is what makes "human feedback improves the system"
  safe to build at all.
- The weekly-batch-plus-human-approval cadence means alpha changes are
  slow and legible (a human reviews a proposal, not an opaque online
  update), at the cost of not adapting within the week — that latency is
  accepted deliberately in exchange for auditability.
- Deferring the suppression/routing engine to v2 means v1 on-call still
  gets paged for things a smarter routing layer would eventually dedupe or
  suppress — an accepted v1 noise cost, not an oversight.
- Any future decoder/retrieval layer that reads this violation log
  (ADR-008) remains read-only with respect to it; narrating a violation is
  never the same as being able to act on it.

## Falsification hooks

- **G7** (`docs/GATES.md`) — the counterfactual audit: every proposed
  alpha change is replayed against confirmed-incident windows and must
  not suppress any incident the current alpha caught. This is the
  machine-checkable gate sitting between "propose" and "human-approved
  apply."
- Violation-log immutability, label isolation from sketches/calibration/
  alpha (invariant 2), the `alpha_max` floor, EVT's unconditional page
  under every v1 suppression hook (invariant 3), and routing
  severity-tag correctness are ordinary falsifiable claims, not yet named
  gates — cover them in `tests/flywheel/` (log, labels, proposals) and
  `tests/connectors/` (webhook routing).
