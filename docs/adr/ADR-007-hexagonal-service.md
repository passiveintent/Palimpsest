# ADR-007: Hexagonal Service, Zero-Dep Core

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

OTel deps churn fast; core must be embeddable.

## Decision

pkg/* limited to stdlib + xxhash; palimpsestd is ports-and-adapters; OTel
deps quarantined in a separate `otel/` module.
