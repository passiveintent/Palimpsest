# ADR-002: Hash-Implicit Φ

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Matrix cardinality explosion.

## Decision

Measurement matrix never materialized; columns derived via xxh64
double-hashing of series name + epoch seed. Solves cardinality churn — m
grows ~k·log(N/k).
