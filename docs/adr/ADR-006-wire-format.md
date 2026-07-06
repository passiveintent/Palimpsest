# ADR-006: Wire Format

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Protobuf is non-deterministic, adds toolchain friction.

## Decision

Hand-specified binary frames + CRC32C, not protobuf — byte-deterministic,
zero-dep, fuzzable.

## Addendum: Compressor registry (codec byte, zstd)

**Rationale.** The `codec` byte (SPEC.md §"Frame Layout") was reserved
with `2=zstd` from the start, but pkg/wire cannot import a zstd library
without breaking ADR-007's zero-dep core. A registry decouples "the wire
format knows a codec id exists" from "this binary can actually compress/
decompress it".

**Design.**

- `pkg/wire` defines `type Codec uint8`, a `Compressor` interface
  (`Compress([]byte) ([]byte, error)`, `Decompress([]byte, maxBytes int)
  ([]byte, error)`), and `Register(Codec, Compressor)` over a process-wide
  map. `CodecNone` (identity) and `CodecGzip` (`compress/gzip`, stdlib)
  are registered by pkg/wire itself at package init; `CodecZstd` is not —
  no implementation of it lives in pkg/*.
- `zstdcodec` (a new top-level leaf package, not under `pkg/`) implements
  `Compressor` for `CodecZstd` via `github.com/klauspost/compress/zstd`.
  `cmd/palimpsestd`, `cmd/plsim`, and `otel/processor/csresidual` each call
  `zstdcodec.Register()` once at startup — unconditionally, regardless of
  their own configured codec, so any of them can *decode* a `CodecZstd`
  frame from a peer even if their own encoder is configured for gzip.
- A frame naming an unregistered codec is never silently dropped: decode
  returns the typed `ErrUnregisteredCodec`; callers (`internal/core`'s
  engine) gate the stream at low confidence and count it
  (`Metrics.IncUnregisteredCodec`) — the same policy ADR-012 §Addendum
  uses for an unknown `key_version`.
- The codec byte governs `SnapshotBlob` (as before) and, newly, a
  KEYFRAME's `Payload` too ("Keyframe + snapshot payloads honor the codec
  byte end-to-end") — RESIDUAL/FALLBACK payloads are left uncompressed
  (packed fixed-width arrays / small top-k lists; little to gain).

**Result** (docs/PERF.md "Codec comparison"): on a realistic 50k-series
golden keyframe, zstd beat gzip on both bytes (20% smaller) and CPU
(~7.3x faster compress, ~3.1x faster decompress) — adopted in
`demo/otelcol-config.yaml`; the package default stays `gzip` so no
existing deployment's behavior changes without an explicit opt-in.
