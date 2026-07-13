# Changelog

All notable changes to this project are documented in this file. Format
loosely follows [Keep a Changelog](https://keepachangelog.com/); this
project has no tagged releases yet (see README's "Getting started"), so
entries accumulate under **Unreleased** until the first one ships.

## [Unreleased]

### Added

- **Dead-zone sweep (`plsim --deadzone`)**: an empirical hunt for the gap
  between ADR-004's storm fallback and ADR-002's FISTA recovery — a single
  small anomaly diluted in per-series background noise, run through the
  real Accumulator → quantize/dequantize → Recover path plus the real
  StormDetector at production defaults, mapped over a magnitude × noise
  grid. Emits `deadzone.csv`, a `deadzone.svg` heatmap and a boundary
  summary; `docs/DEADZONE.md` documents the measured boundary (absolute
  recovery floor ~0.4 residual units, noise-scaled floor, max-residual
  gate closure from σ≈0.3 — including the λ-coupling band-pass where a
  *larger* anomaly suppresses its own deviation event — and the ~147σ
  storm bar). README Limitations and ADR-004 now point at it, and
  `cmd/plsim/deadzone_test.go` pins the zone's existence.

- **Wire protocol RFC**: [docs/rfc/palimpsest-wire-v2.md](docs/rfc/palimpsest-wire-v2.md)
  freezes v2 as the specified, encoder-mandatory wire protocol (v1 remains
  read-only legacy), extracted from `docs/SPEC.md` into a standalone
  reference with byte tables, IANA-style codec/frame-type/predictor
  registries, and a conformance section scoped to the byte-exact golden
  vector classes (recovery vectors are informative, not
  conformance-required). A doc-sync test
  (`pkg/wire.TestRFCFrameTableInSync`) fails the build if a `wire.Frame`
  field is ever added without a matching RFC byte-table entry.
- **ADR-017 Merkle dict_root evidence gate**: measured full-dict-keyframe
  bytes at 18-35% of total wire bytes at realistic churn (10-300 events/min)
  — over the evidence gate's 10% threshold at every rate tested. Decision:
  the Merkle dict root + bidirectional resync backlog item is **not**
  closed; it is proposed as an informative v3 change in the RFC's appendix.
  See `docs/adr/ADR-017-merkle-decision.md` and `docs/PERF.md`'s "Merkle
  dict_root / resync evidence gate" section.
- Per-shard wire-byte and dict_root-mismatch/heal-time metrics
  (`internal/metrics.Metrics`: `AddWireBytes`, `AddFullDictKeyframeBytes`,
  `IncDictRootMismatches`, `ObserveDictRootHealSeconds`) backing the
  ADR-017 measurement, wired into `internal/core.Engine`.
- `wire.EncodedSize`: exact `Marshal`-equivalent frame byte length without
  paying allocation/CRC cost per frame.

### Discovered (not fixed in this change)

- A golden keyframe silently drops any tombstone whose TTL expires in that
  same flush window (`cmd/plsim/encoder.go`, `otel/processor/csresidual/processor.go`,
  and `internal/core`'s test encoder all replace that window's dict_delta
  list with `tracker.FullDict()`'s adds-only re-announcement instead of
  extending it), causing a **permanent** `dict_root` divergence rather than
  one bounded by the next golden keyframe. Documented in ADR-017 and the
  RFC's v3 proposal appendix; fix proposed but out of scope for this
  change (no protocol changes).
