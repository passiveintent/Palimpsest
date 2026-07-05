# ADR-005: Tiering

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Billing meters/SLO evidence can't be lossy.

## Decision

exact / sketched / merged tiers. Billing meters and quantiles forced to
exact tier; counters/gauges/histogram-buckets are sketchable.

## Consequences

* Tier routing logic is not yet implemented. The former `pkg/tier` stub
  package has been removed (it was doc.go only). Tier selection currently
  lives implicitly in the processor config
  (`otel/processor/csresidual/processor.go`). When implementing, create
  `pkg/tier` fresh and wire it through the hexagonal port
  (see ADR-007).
