# Palimpsest — standing instructions for Copilot
- Read docs/SPEC.md, docs/COSTS.md, docs/SECURITY.md and docs/adr/*.md
  before writing any code. They win over your instincts.
- The ADR text actually committed in docs/adr/*.md is authoritative — full
  stop. A prompt draft, a prior conversation's summary, or your own memory
  of what an ADR "must have said" is not a source of truth, even when it
  sounds plausible or was true of an earlier draft. If a prompt describes
  behavior as "the existing ADR-NNN semantics" or "clarifying existing
  intent," verify that claim against the actual file before writing code
  or docs against it; if the file doesn't say it, the prompt is proposing
  something new and should be implemented (and written up) as an
  explicitly-labeled amendment, not folded into the original Decision text
  as if it were always there. Spec drift during transcription — a draft's
  intent diverging from what actually landed in the repo — is a documented
  failure mode here (ADR-008's Ownership & Eviction amendment exists
  because of it), not a hypothetical.
- Core modules (pkg/*) allow exactly ONE external dep:
  github.com/cespare/xxhash/v2. Everything else is stdlib. The otel/ module
  is the only place collector deps live.
- NEVER materialize the measurement matrix Phi as a dense or sparse structure
  on the encode path. Encoding is hashing + accumulation (ADR-002).
- Identity is two-level (ADR-008): only persistent LOGICAL series enter the
  sketch; instance labels never do. A series' FIRST sample never enters the
  residual path — it bootstraps via dict_delta init_value.
- Deterministic everything: hashing, quantization, dict_root, frame bytes,
  seed derivation. Golden tests compare against Python-oracle vectors.
- Temporal: seq is a window index, not a counter. Emitter identity is
  per-frame (ADR-013). All state is per (emitter, epoch).
- Multi-tenant: tenancy is a hard boundary; sketches never sum across tenants.
  Seeds derive from HKDF(tenant_key, ...) not public convention (ADR-012).
- Style: table-driven tests; exported symbols documented; errors wrapped with
  %w; no global mutable state; no goroutine leaks (ctx-owned); files under
  ~400 lines; benchmarks for hot paths.
- When unsure, stub with TODO(adr-ref) rather than inventing behavior.
