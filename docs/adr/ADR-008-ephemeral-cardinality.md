# ADR-008: Ephemeral Cardinality

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Kubernetes churn + per-pod sketching = baselines don't exist.

## Decision

Two-level identity — only persistent *logical* series
(deployment×metric×aggregate) are sketched; pod/instance labels live in an
agent-side ring buffer. Births bootstrap via `dict_delta.init_value` (first
sample never sketched); deaths are tombstoned; keyframes carry a
`dict_root` digest.
