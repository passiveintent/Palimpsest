# ADR-009: Alerting Semantics

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Sketch is opaque; traditional polling doesn't work.

## Decision

Push-based — recovery *is* the alert evaluation. Five substrates
(deviation/recovered, named-series point-query, slow-drift/keyframe,
absence/metadata, exact). Flag-triggered "dashcam" snapshots beat the
ring-buffer window. Churn circuit breaker turns crash loops into detected
events.
