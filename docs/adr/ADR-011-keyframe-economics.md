# ADR-011: Keyframe Economics

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Naive keyframes are byte-parity with raw metrics.

## Decision

Delta-coded (KDELTA) keyframes with periodic golden frames; the real
savings come from placement arbitrage (commodity storage for keyframes,
per-series-billed backends only see 1–5% of series) — not from bytes
alone. Includes the honest anti-claim in COSTS.md.
