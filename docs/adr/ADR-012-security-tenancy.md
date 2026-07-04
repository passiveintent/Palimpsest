# ADR-012: Security & Multi-Tenancy

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Pull endpoint is SSRF surface; cross-tenant sketch leak is real.

## Decision

Speculative push replaces the pull endpoint (zero listening sockets by
default). Tenancy is a hard boundary — sketches never sum across tenants.
Seeds derive via HKDF-SHA256(tenant_key, …), not public convention.
Explicit "CS is not encryption" policy.
