# ADR-006: Wire Format

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Protobuf is non-deterministic, adds toolchain friction.

## Decision

Hand-specified binary frames + CRC32C, not protobuf — byte-deterministic,
zero-dep, fuzzable.
