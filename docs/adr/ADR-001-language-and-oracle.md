# ADR-001: Language & Oracle

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Python + Go must match deterministically.

## Decision

Go monorepo for the OTel Collector wedge; Python feasibility harness
promoted to conformance oracle generating golden vectors Go must match
byte-exact.
