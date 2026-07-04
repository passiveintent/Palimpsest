# ADR-013: Temporal Semantics

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Clock drift + out-of-order arrival = misaligned summation = garbage.

## Decision

`emitter_id` + window-indexed `seq` (not wall-clock); watermark/repair
engine exploits sketch additivity for late/out-of-order frames;
coverage-annotated, revisioned results. Caught a latent multi-agent
correctness bug before any code shipped.
