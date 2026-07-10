# Palimpsest Architecture Decision Records (ADR) Index

**Version:** v2 (Compressed Sensing Foundation)  
**Last Updated:** 2026-07-09  
**Focus:** Compressed sensing-based time-series sketching for observability

---

## Overview

Palimpsest's ADRs are organized into five layers, describing the design of a compressed-sensing sketching system for high-cardinality metrics. ADRs 001–017 form the **stable foundation**; extended features and mathematical enhancements will use the **ADR-X** prefix (see [Numbering Strategy](#numbering-strategy) below).

---

## Layer 1: Mathematical Foundation & Sketching

**Core compressed-sensing algorithm and measurement-matrix design.**

| ADR | Title | Status | Key Decision |
|-----|-------|--------|--------------|
| [ADR-002](ADR-002-hash-implicit-phi.md) | Hash-Implicit Φ | Accepted (pre-alpha) | Measurement matrix never materialized; columns derived via xxh64 hashing. Solves cardinality churn (m ~ k·log(N/k)). |
| [ADR-003](ADR-003-open-loop-residuals.md) | Open-Loop Residual Coding | Accepted (pre-alpha) | Video-codec structure: keyframes (I-frames, exact) + residuals (P-frames, sketched). HOLD predictor v0. |
| [ADR-004](ADR-004-storm-fallback.md) | Storm Fallback | Accepted (pre-alpha) | Pre-quantization energy spike detection triggers FALLBACK frames (heavy-hitters + aggregates) before recovery corrupts. |
| [ADR-014](ADR-014-group-sparse-recovery.md) | Group-Sparse Recovery (Group-OMP) | Accepted (implementation in progress) | Detection-then-refit algorithm for correlated failures: plain FISTA → Group-OMP escalation on dense regime. Size-aware chi-squared threshold detects coherent groups. |

---

## Layer 2: State & Cardinality Management

**Handling dynamic service topology, ephemeral identities, and dictionary evolution.**

| ADR | Title | Status | Key Decision |
|-----|-------|--------|--------------|
| [ADR-008](ADR-008-ephemeral-cardinality.md) | Ephemeral Cardinality | Accepted (pre-alpha) | Two-level identity: persistent logical series + ephemeral pod/instance labels in ring buffer. Dict births via `init_value`, deaths via tombstones. Amendment: multi-emitter ownership semantics + golden reconciliation (2026-07-07). |
| [ADR-005](ADR-005-tiering.md) | Tiering | Accepted (pre-alpha) | Three tiers: exact (billing/SLOs), sketched (all else), merged (cross-emitter fusion). Tier selection via regex rules. |
| [ADR-017](ADR-017-merkle-decision.md) | Merkle Dict Root & Bidirectional Resync — Evidence-Gate Decision | Accepted (evidence-gated) | Full-dict-keyframe bytes exceed 10% of wire at realistic churn → Merkle root + resync justified. Defect documented (golden-frame tombstone omission, since fixed in ADR-008 amendment). |

---

## Layer 3: Wire Format & Transport

**Frame serialization, codec, and inter-component communication.**

| ADR | Title | Status | Key Decision |
|-----|-------|--------|--------------|
| [ADR-006](ADR-006-wire-format.md) | Wire Format | Accepted (pre-alpha) | Hand-specified binary frames + CRC32C (byte-deterministic, zero-dep, fuzzable). Compressor registry decouples format from codec implementation. |
| [ADR-016](ADR-016-kafka-transport.md) | Kafka Transport | Accepted (implemented) | franz-go (idempotent producer, manual offset commit) for durable, at-least-once delivery without loosening temporal-alignment (ADR-013) or dependency isolation (ADR-007). |
| [ADR-012](ADR-012-security-tenancy.md) | Security & Multi-Tenancy | Accepted (pre-alpha) | Speculative push replaces pull endpoint. Tenancy is hard boundary. Seeds derive via HKDF-SHA256(tenant_key), not public convention. Amendment: `key_version` for tenant-key rotation post-compromise (wire v2). |

---

## Layer 4: Temporal Semantics & Consistency

**Clock-independent stream ordering, multi-agent alignment, and state merging.**

| ADR | Title | Status | Key Decision |
|-----|-------|--------|--------------|
| [ADR-013](ADR-013-temporal-alignment.md) | Temporal Semantics | Accepted (pre-alpha) | `emitter_id` + window-indexed `seq` (not wall-clock). Watermark/repair engine exploits sketch additivity. Amendment: herd jitter on epoch rotation (deterministic offset per emitter). |
| [ADR-015](ADR-015-merged-tier.md) | Merged Tier — Trust Policy for Cross-Emitter Fusion | Accepted (implemented) | Merger plumbing exists (ADR-013's additivity). Policy addition: coverage annotation + escalation rules. Merged tier relies on Group-OMP (ADR-014) for correlated-fleet-incident resolution. |

---

## Layer 5: Operational & System Architecture

**Service structure, query semantics, and monitoring contracts.**

| ADR | Title | Status | Key Decision |
|-----|-------|--------|--------------|
| [ADR-001](ADR-001-language-and-oracle.md) | Language & Oracle | Accepted (pre-alpha) | Go monorepo (OTel Collector wedge) + Python feasibility harness (conformance oracle). Byte-exact match on all deterministic paths; BLAS tolerance for recovery vectors. Rounding to 9 decimal places stabilizes golden vectors across platforms. |
| [ADR-007](ADR-007-hexagonal-service.md) | Hexagonal Service, Zero-Dep Core | Accepted (pre-alpha) | `pkg/*` limited to stdlib + xxhash; `palimpsestd` is ports-and-adapters; OTel deps in separate `otel/` module. Enables embedding + rapid OTel iteration. |
| [ADR-010](ADR-010-dimensional-slicing.md) | Dimensional Slicing | Accepted (pre-alpha) | Keyframes persist as exact, forever-queryable PromQL layer (Layer 1). Sketch-cube views for finer grains. Group-lasso (ADR-014) is hard prerequisite for correlated-failure recovery at fine grain. |
| [ADR-009](ADR-009-alerting.md) | Alerting Semantics | Accepted (pre-alpha) | Push-based: recovery **is** alert evaluation. Five substrates: deviation/recovered, point-query, slow-drift, absence/metadata, exact. Dashcam snapshots + churn circuit-breaker. |
| [ADR-011](ADR-011-keyframe-economics.md) | Keyframe Economics | Accepted (pre-alpha) | Delta-coded (KDELTA) keyframes + periodic golden frames. Real savings from placement arbitrage (1–5% of series on per-series-billed backends), not byte-level compression alone. |

---

## Architecture Diagram

```
Layer 5: Operational                [ADR-001] [ADR-007] [ADR-010] [ADR-009] [ADR-011]
                                    Language  Hexagon   Views     Alerting  Economics
                                       ↓         ↓        ↓         ↓        ↓

Layer 4: Temporal & Consistency     ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
                                    [ADR-013]         [ADR-015]
                                    Temporal          Merged Tier
                                    Alignment         (with Group-OMP)

Layer 3: Wire & Transport           ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
                                    [ADR-006]  [ADR-016]  [ADR-012]
                                    Wire Fmt   Kafka      Security
                                       ↓         ↓         ↓

Layer 2: State & Cardinality        ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
                                    [ADR-008]         [ADR-005]  [ADR-017]
                                    Ephemeral Dict    Tiering    Merkle
                                       ↓                ↓         Resync
                                    (ownership,    (metric rules) (evidence
                                    golden-recon)                  gate)

Layer 1: Math & Sketching           ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
                                    [ADR-002]  [ADR-003]   [ADR-004]  [ADR-014]
                                    Hash-Φ     Residuals   Storm      Group-OMP
```

---

## Numbering Strategy

**Problem:** ADRs 001–017 are stable and foundational. New advanced math and CS features will naturally also start at `ADR-001`, creating numbering conflicts and risking accidental overwrites.

**Solution:** Use the **ADR-X** prefix for extended features:

```
ADR-001 through ADR-017     Core Palimpsest (compressed sensing foundation)
ADR-X-001 through ADR-X-NNN Advanced math/CS extensions (non-breaking)
```

### Examples

- **ADR-X-001:** Advanced measurement-matrix design (e.g., learning-based optimization, adaptive sparsity)
- **ADR-X-002:** Enhanced recovery algorithms (e.g., deep-learning-assisted signal reconstruction)
- **ADR-X-003:** Nonlinear sketching or quantum-ready arithmetic
- ...and so on.

### Rationale

1. **No conflicts:** The `X` prefix disambiguates entirely. You cannot accidentally overwrite ADR-003 when writing ADR-X-003.
2. **Clear layering:** Core (001–017) + Extensions (X-001+) are visually distinct.
3. **Git-safe:** New files `ADR-X-001-title.md` do not collide with existing `ADR-001-language-and-oracle.md`.
4. **Dependency clarity:** ADR-X decisions can reference core ADRs (e.g., "ADR-X-001 builds on ADR-002 but replaces the hashing strategy").

---

## Key Relationships & Dependencies

### Dependency Chain (bottom-up)

1. **ADR-002 (Hash-Φ)** ← foundational; assumed by all downstream layers
2. **ADR-003 (Residuals)** ← assumes ADR-002
3. **ADR-004 (Storm)** ← assumes ADR-002, ADR-003; triggers FALLBACK before k explosion
4. **ADR-014 (Group-OMP)** ← replaces/upgrades ADR-004 with detection-then-refit; uses ADR-010 label hierarchy
5. **ADR-008 (Ephemeral)** ← independent; works with any Layer-1 algorithm
6. **ADR-005 (Tiering)** ← independent; metric routing orthogonal to algorithm
7. **ADR-017 (Merkle)** ← evidence-gates complexity; works in ADR-008 context
8. **ADR-006 (Wire)** ← serializes output of any Layer-1 algorithm
9. **ADR-012 & ADR-016** ← transport; layer-agnostic
10. **ADR-013 (Temporal)** ← sequence semantics; layer-agnostic
11. **ADR-015 (Merged)** ← depends on ADR-013 additivity + ADR-014 Group-OMP
12. **ADR-010 (Dimensional Slicing)** ← requires ADR-014 group-lasso as prerequisite
13. **ADR-011 (Economics)** ← justifies design choices in ADR-003, ADR-004, ADR-008
14. **ADR-009 (Alerting)** ← built on top of ADR-010's views
15. **ADR-001 & ADR-007** ← implementation strategy and code organization

### Backward Compatibility Notes

- **ADR-008 Amendment (2026-07-07):** Fixed tombstone handling and golden-frame reconciliation without wire-format changes. Code-level breaking change (ownership semantics added to `ViewState`); wire-compatible.
- **ADR-012 Amendment (wire v2):** Added `key_version` for key rotation. Wire-compatible with v1; decoders must support both.
- **ADR-013 Amendment:** Herd jitter offset for epoch rotation (deterministic, per-emitter).
- **ADR-017 Defect Note:** Golden-frame tombstone omission fixed in ADR-008 amendment (not a wire change; encoder fix + decoder defense-in-depth).

---

## When to Reference Each ADR

| Question | ADR(s) |
|----------|--------|
| *Why not just use Protobuf?* | ADR-006, ADR-001 (determinism) |
| *How do measurements not explode with cardinality?* | ADR-002, ADR-008 |
| *Why sketched instead of exact?* | ADR-003, ADR-004, ADR-011 |
| *How do we handle Kubernetes churn (pods, replicas)?* | ADR-008 (identity), ADR-013 (ordering), ADR-015 (fusion) |
| *What happens when recovery can't find a sparse solution?* | ADR-004 (fallback), ADR-014 (group-OMP) |
| *How do we ensure different agents agree on results?* | ADR-001 (oracle), ADR-013 (temporal), ADR-008 (dict reconciliation) |
| *Why push and not pull?* | ADR-012 (security/SSRF), ADR-016 (Kafka option) |
| *Can I route some metrics exactly and others as sketches?* | ADR-005 (tiering) |
| *How do I query sketched data?* | ADR-010 (dimensional slicing), ADR-009 (alerting semantics) |

---

## Document Status Key

- **Accepted (pre-alpha; implementation pending):** Decision is final; code is planned but not yet shipped.
- **Accepted (implementation in progress):** Code partially or fully integrated.
- **Accepted (implemented):** Shipped; tested in production or integration contexts.
- **Accepted (evidence-gated; backlog item OPEN):** Decision gate passed; backlog item remains open pending future work cycles.

---

## Copilot & Memory Links

For Claude's copilot memory and integration:

- **Core Foundation:** Start with [[ADR-002-ADR-003-ADR-004]] (Algorithm foundation)
- **State & Cardinality:** [[ADR-008-ADR-017]] (Dictionary, ephemeral identity, resync)
- **System Design:** [[ADR-007]] (Hexagonal architecture, dependency isolation)
- **Temporal Correctness:** [[ADR-013]] (Ordering, multi-emitter alignment)
- **Operations:** [[ADR-010]], [[ADR-011]], [[ADR-009]] (Views, economics, alerting)
- **Amendments & Defect Fixes:** [[ADR-008 Amendment]], [[ADR-017 Defect]]

---

## Next Steps for Extended Features (ADR-X)

When adding advanced math/CS features:

1. Create new files with **ADR-X-NNN** numbering (e.g., `ADR-X-001-advanced-basis.md`).
2. In the new ADR, explicitly state which core ADRs it builds on or modifies (e.g., "ADR-X-001 replaces ADR-002's hash-based Φ with a learned basis").
3. Update this index under a new section: "## Extended Features (ADR-X)".
4. Link back to core ADRs in the "Depends On" section to maintain traceability.
5. Keep wire-format and backward-compatibility notes clear (use amendments if extending an existing ADR scope).
