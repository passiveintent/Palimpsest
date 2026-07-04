# ADR-003: Open-Loop Residual Coding

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Encoder/decoder must agree on baseline.

## Decision

Video-codec structure — keyframes (I-frames, exact values) + residual
frames (P-frames, sketched) between them. HOLD predictor v0.
