# ADR-004: Storm Fallback

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

k blows up during incidents -> recovery corrupts.

## Decision

Pre-quantization energy ‖y‖²/m monitored; spikes trigger FALLBACK frames
(heavy-hitters + aggregates) before recovery corrupts — detectable well
before the sparsity cliff.

## Known boundary (measured 2026-07)

The breaker guards the *dense/global* failure mode only. A single small
anomaly needs ε ≳ σ·√((multiplier−1)·n) to trip it — two orders of
magnitude above FISTA's recovery floor at defaults — and everything
between the recovery floor and that bar that recovery itself loses (noise
smear, the max-residual gate) is a Layer-2 dead zone: no deviation event,
no fallback. The boundary is mapped empirically by `plsim --deadzone` and
documented, with the measured table and trade-offs, in
[docs/DEADZONE.md](../DEADZONE.md).
