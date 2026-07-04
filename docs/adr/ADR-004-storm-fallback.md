# ADR-004: Storm Fallback

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

k blows up during incidents -> recovery corrupts.

## Decision

Pre-quantization energy ‖y‖²/m monitored; spikes trigger FALLBACK frames
(heavy-hitters + aggregates) before recovery corrupts — detectable well
before the sparsity cliff.
