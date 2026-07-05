# ADR-005: Tiering

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Billing meters/SLO evidence can't be lossy.

## Decision

exact / sketched / merged tiers. Billing meters and quantiles forced to
exact tier; counters/gauges/histogram-buckets are sketchable.

## Consequences

* `pkg/tier` is the canonical, zero-dependency implementation of tier
  selection (regex list, first-match-wins, three values: `exact`, `sketched`,
  `merged`). The `otel/processor/csresidual` processor imports it via the
  `compileTierRules` / `matchTier` shim in `identity.go`; nothing else needs
  to re-implement the logic.
