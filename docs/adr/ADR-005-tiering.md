# ADR-005: Tiering

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Billing meters/SLO evidence can't be lossy.

## Decision

exact / sketched / merged tiers. Billing meters and quantiles forced to
exact tier; counters/gauges/histogram-buckets are sketchable.
