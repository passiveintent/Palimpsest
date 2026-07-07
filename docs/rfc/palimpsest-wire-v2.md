# RFC: Palimpsest Wire Protocol v2

**Status:** Stable. v2 is the current, encoder-mandatory version; v1 is
read-only legacy (see "Versioning policy" below).

**Source of truth:** this document is extracted from `docs/SPEC.md` and the
ADRs it implements (ADR-002, ADR-006, ADR-007, ADR-008, ADR-009, ADR-011,
ADR-012, ADR-013) into a single, freestanding protocol reference. Where this
document and `docs/SPEC.md` ever disagree, this document governs for wire
compatibility purposes; file an issue rather than relying on the
discrepancy.

**Scope.** This RFC specifies the wire frame format, the hashing and seed
derivation a conforming implementation must reproduce byte-exactly, dict
lifecycle and resync rules, temporal semantics, and the codec/frame-type/
predictor registries. It does **not** specify the sparse-recovery algorithm
(FISTA, Group-OMP debiasing) that reconstructs a frame's content on the
decode side: recovery is an implementation detail behind the wire contract,
not part of the protocol (see "Conformance" below).

---

## 1. Terminology

- **Tenant**: an isolation boundary. Sketches, dictionaries, and seeds never
  cross tenant lines. A tenant is identified only by its `tenant_key`
  (out-of-band secret material); no tenant identifier rides the wire.
- **Shard**: a numeric partition (`shard_id`) of a tenant's series space,
  chosen by the deployer (e.g. one shard per cluster or per team).
- **Emitter**: one encoding agent process (`emitter_id`, a random `u64`
  chosen per process start). An emitter owns a disjoint subset of series
  within a shard.
- **View**: a numeric tag (`view_id`) letting one shard carry more than one
  independent sketch/recovery instance over the same series (e.g. a
  "merged tier" fused view alongside each emitter's own view). `view_id = 0`
  is the default, single-view case.
- **Epoch**: a numeric, monotonically increasing coordinate (`epoch`) an
  emitter advances on a schedule (`epoch_rotate`, deployer-configured). Each
  `(emitter, epoch)` pair is an independent coordinate system with its own
  dictionary and predictor state, bootstrapped from scratch by a golden
  keyframe (§5).
- **Logical series**: the unit of identity this protocol sketches:
  `metric_name + "|" + join(logical_key values, ",") + "|agg=" + one of
  {sum, count, max}`. Instance-level labels (which pod, which node) are
  never part of a logical series name and never ride the wire as sketch
  input — they are an implementation's own local concern (e.g. a ring
  buffer feeding dashcam snapshots, §"Security Considerations").
- **Series ID**: `seriesID = xxh64(name_bytes, seed=0)` — a deterministic,
  stable `u64` derived from a logical series' name alone. Every dict_delta,
  keyframe value, and recovered support index is keyed by series ID, never
  by name, on the wire.
- **Dictionary**: the set of currently active (born, not yet tombstoned)
  series IDs an emitter has ever reported into a given `(epoch, view)`.
- **dict_root**: a digest over a dictionary's active ID set, carried on
  every frame, letting a decoder verify its own dictionary state agrees
  with the encoder's (§5).
- **Keyframe / Residual / Fallback**: the three frame types (§2, §"IANA-
  style registries").
- **Golden keyframe**: a KEYFRAME frame whose payload is a Full (not
  KDELTA) encoding, fired on a configured cadence
  (`keyframe_full_dict_every` keyframes). A golden keyframe's `dict_delta`
  list is always the dictionary's complete current contents (a "full
  dict"), not an incremental delta.
- **Window**: one flush interval's worth of accumulated sketch state,
  identified by `seq` (§6), not by wall-clock time.
- **Confidence**: a decode-side annotation (`recovered | pointquery |
  keyframe | fallback`) on a reconstructed value, reflecting which recovery
  substrate produced it (ADR-009). Not itself a wire field — recovery
  results are a decoder-local output, not a protocol structure.

## 2. Versioning policy

| Version | Status | Notes |
| --- | --- | --- |
| 1 | Read-only legacy | A conforming decoder MUST still accept v1 frames. An encoder MUST NOT emit v1. v1 had no `key_version` byte and no `codec` byte: a decoder synthesizes `key_version = 0` and derives `codec` from the (now-deprecated) `flags` bit 0 (`FlagGzip`) — see the frame-layout table's "v1 note" column. |
| 2 | Current | Every encoder MUST write v2. This is the only version this document specifies new behavior for. |
| >2 | Rejected | A decoder encountering a version byte greater than 2 MUST return a distinct, typed error (`wire.ErrVersion`) rather than attempting to interpret the frame — this is a forward-compatibility signal ("I don't understand this yet"), not a malformed-frame error. |

v2 was introduced in one batched version bump (adding `key_version` and
`codec`, ADR-006 §Addendum, ADR-012 §Addendum) specifically so that no
second wire bump would be needed before this RFC's publication. This RFC
therefore documents v2 **as shipped**: it makes no protocol changes.
Defects discovered while producing this RFC, and the evidence-gated Merkle
dict-root proposal, are recorded as an **informative, non-normative** v3
proposal appendix (§10) — proposals, not part of this specification.

## 3. Frame Layout

A frame is a flat, length-prefixed binary structure: a fixed-size header,
a repeated `dict_delta` section, an optional `snapshot_blob`, a `payload`,
and a trailing checksum. All multi-byte integers and floats are
**little-endian**. The checksum (`crc32c`, Castagnoli polynomial) covers
every preceding byte of the frame.

### 3.1 Fixed header (66 bytes, offsets 0-65)

| Offset | Size | Field | Type | v1 note |
| --- | --- | --- | --- | --- |
| 0 | 4 | `magic` | `[4]byte` (`"PLMP"`) | unchanged |
| 4 | 1 | `version` | `u8` | `1` or `2` |
| 5 | 1 | `frame_type` | `u8` | unchanged (§"Frame type registry") |
| 6 | 1 | `flags` | `u8` | bit 0 (`gzip`) deprecated in v2, see `codec` |
| 7 | 1 | `bits` | `u8` | unchanged (RESIDUAL quantization width: 8 or 16) |
| 8 | 8 | `emitter_id` | `u64` | unchanged |
| 16 | 8 | `shard_id` | `u64` | unchanged |
| 24 | 8 | `epoch` | `u64` | unchanged |
| 32 | 4 | `seq` | `u32` | unchanged (window index, §6) |
| 36 | 2 | `view_id` | `u16` | unchanged |
| 38 | 4 | `m` | `u32` | unchanged (sketch width) |
| 42 | 1 | `d` | `u8` | unchanged (hashes per series) |
| 43 | 1 | `predictor` | `u8` | unchanged (§"Predictor registry") |
| 44 | 1 | `key_version` | `u8` | **v2-only.** A v1 frame carries no byte here; a decoder reading a v1 frame synthesizes `key_version = 0` and does not consume a byte at this position (v1's fixed header is 65 bytes, one reserved byte where v2 splits `key_version`/`codec`) |
| 45 | 1 | `codec` | `u8` | **v2-only** (§"Codec registry"). A v1 frame's codec is derived instead from `flags` bit 0: `1` (gzip) if set, else `0` (none) |
| 46 | 4 | `energy` | `f32` | unchanged |
| 50 | 4 | `quant_scale` | `f32` | unchanged |
| 54 | 8 | `dict_root` | `u64` | unchanged (§5) |
| 62 | 4 | `dict_delta_count` | `u32` | unchanged |

v1's fixed header is 65 bytes (one reserved byte in place of v2's
`key_version` + `codec` split); v2's is 66 bytes. Every other section
below is identical in both versions.

### 3.2 `dict_delta` section (repeated `dict_delta_count` times)

| Relative offset | Size | Field | Type |
| --- | --- | --- | --- |
| +0 | 8 | `id` | `u64` |
| +8 | 1 | `dflags` | `u8` (bit 0 = TOMBSTONE, bit 1 = reserved) |
| +9 | 4 | `init_value` | `f32` |
| +13 | 2 | `name_len` | `u16` |
| +15 | `name_len` | `name` | bytes |

A tombstone entry (`dflags` bit 0 set) MUST carry `init_value = 0` and
`name_len = 0`; a decoder MUST reject a tombstone that carries either
non-zero (`ErrInvalidFrame`). See §5 for lifecycle semantics.

### 3.3 Snapshot blob, payload, and trailer

| Field | Type | Notes |
| --- | --- | --- |
| `snapshot_blob_len` | `u32` | 0 if no snapshot is attached |
| `snapshot_blob` | bytes (`snapshot_blob_len`) | optional dashcam snapshot (ADR-009/ADR-012); governed by `codec` |
| `payload_len` | `u32` | |
| `payload` | bytes (`payload_len`) | structure depends on `frame_type` (§3.4); governed by `codec` only for KEYFRAME |
| `crc32c` | `u32` | Castagnoli CRC32 over every preceding byte of this frame |

`codec` governs `snapshot_blob` always, and `payload` only when
`frame_type` is KEYFRAME. RESIDUAL and FALLBACK payloads are never
compressed (they are already packed fixed-width arrays or short top-k
lists — little to gain, per the codec-comparison measurement in
`docs/PERF.md`).

### 3.4 Payload structure by frame type

- **KEYFRAME**:
  - Full (golden; `flags` bit 1 clear): `count u32` + repeated `{id u64,
    value f32}`.
  - KDELTA (`flags` bit 1 set): `changed_bitmap` (varint-length-prefixed
    bitmap) + varint-encoded deltas for each set bit, decoded against the
    prior keyframe's values.
- **RESIDUAL**: packed `int8` or `int16` (per `bits`) × `m`, one quantized
  measurement per sketch bucket.
- **FALLBACK**: `k u32` + repeated `{id u64, value f32}` (heavy-hitters
  top-k) + `total_sum f64` + `total_count u64` (ADR-004 storm fallback).

### 3.5 Frame struct field cross-reference

Every field of `pkg/wire.Frame` (the Go implementation's in-memory
representation) maps to a row above:

| `wire.Frame` field | Wire location |
| --- | --- |
| `Magic` | `magic` |
| `Version` | `version` |
| `FrameType` | `frame_type` |
| `Flags` | `flags` |
| `Bits` | `bits` |
| `EmitterID` | `emitter_id` |
| `ShardID` | `shard_id` |
| `Epoch` | `epoch` |
| `Seq` | `seq` |
| `ViewID` | `view_id` |
| `M` | `m` |
| `D` | `d` |
| `Predictor` | `predictor` |
| `KeyVersion` | `key_version` |
| `Codec` | `codec` |
| `Energy` | `energy` |
| `QuantScale` | `quant_scale` |
| `DictRoot` | `dict_root` |
| `DictDeltas` | `dict_delta_count` + repeated `dict_delta` entries |
| `SnapshotBlob` | `snapshot_blob_len` + `snapshot_blob` |
| `Payload` | `payload_len` + `payload` |
| `CRC32C` | `crc32c` |

`pkg/wire`'s `TestRFCFrameTableInSync` (`rfc_sync_test.go`) fails the build
if a `Frame` struct field is ever added without a corresponding entry in
this table — this table cannot silently drift from the implementation.

## 4. Hashing & Seed Derivation

```
seriesID       = xxh64(name_bytes, seed=0)
seed_ephemeral = HKDF-SHA256(tenant_key, shard_id || epoch_index || view_id)
h1             = xxh64(name_bytes, seed_ephemeral)
h2             = xxh64(name_bytes, seed_ephemeral XOR 0x9E3779B97F4A7C15) | 1
bucket[i]      = (h1 + u64(i) * h2) mod u64(m),  for i in 0..d-1
sign[i]        = +1 if ((h1 >> (i+1)) & 1) == 1 else -1
Phi[bucket[i]] += sign[i] / sqrt(d)   -- accumulated, never materialized (ADR-002)
```

`seriesID` is stable for a name's whole lifetime (independent of tenant,
epoch, or key rotation) and is what every `dict_delta.id` and recovered
support index refers to. `seed_ephemeral` is what actually defines a
series' measurement columns (`bucket`/`sign`), and changes whenever
`tenant_key`, `shard_id`, `epoch_index`, or `view_id` changes — this is the
mechanism, not `seriesID`, that makes epochs and tenants independent
coordinate systems.

**`key_version` semantics (v2; ADR-012 §Addendum).** `key_version` names
which generation of a tenant's HKDF input keyed a given frame's seed
derivation. `key_version = 0` is the implicit version for v1 frames.
Rotation works forward-compatibly, without a wire change:

1. A tenant adds a new key at a higher `version` to its `tenant_keys`
   config and marks it `active_key_version`. New frames are stamped with
   the new version on the next config reload; frames already in flight
   under the old version remain decodable as long as the old key stays in
   the decoder's `KeyRing`.
2. `KDELTA` keyframe chains span a single `(epoch, key_version)`
   coordinate system: a `key_version` change is a coordinate-system break
   equivalent to a new epoch (the first frame after a version bump MUST be
   a golden keyframe).
3. An unknown `key_version` MUST gate the frame at low confidence and
   increment a `keyring_miss` counter — it MUST NOT be silently dropped.
4. `KeyRing` is multi-tenant-scoped: keys for different tenants are never
   mixed, and the seed derivation formula itself is unchanged by rotation
   — only which `tenant_key` bytes feed it changes.

## 5. Lifecycle rules

- Only **logical series** are ever sketched or dictionary-tracked
  (ADR-008's two-level identity). Instance-level labels never enter a
  `dict_delta` or a recovered support set.
- A series' **first-ever sample never enters the sketch**: it bootstraps
  the dictionary via a birth `dict_delta` (`init_value` = that first
  sample), with zero residual contribution.
- A `dict_delta` is either a **birth** (`dflags` bit 0 clear: `id`,
  `init_value`, `name` all meaningful) or a **tombstone** (`dflags` bit 0
  set: `init_value` and `name` MUST be zero/empty). A birth for an ID
  already active is a legal re-announcement (idempotent upsert), not an
  error.
- **`dict_root`** is `xxh64(sorted active ids serialized as little-endian
  u64, seed=0)` — order-independent, content-sensitive, carried on every
  frame type (not only KEYFRAME). A decoder verifies it against its own
  locally-tracked active-ID set on every KEYFRAME frame; a mismatch MUST
  gate that view at low confidence (never silently accepted) and is only
  cleared by a subsequent **golden** keyframe's full-dict resync (a
  matching KDELTA does not prove agreement on the decoder's *entire*
  active set, only on the small delta just decoded).
- **Every emitter MUST open each `(emitter, epoch)` stream with a golden
  keyframe carrying its full dictionary contents** — this is the only
  resync mechanism v2 defines, and the only way a decoder can bootstrap or
  recover its dictionary without replaying full history. See §10 for a
  known implementation gap in this mechanism and a proposed successor.
- `series_ttl` (a decoder-side and encoder-side operational tuning value,
  not a wire field) governs how long a silent series stays active before
  being tombstoned; the exact duration is implementation/deployment policy,
  not part of this protocol.

## 6. Temporal semantics

- `seq` is a **window index**, not a wall-clock timestamp or a monotonic
  sequence counter: window `seq`'s samples cover
  `[epoch_start + seq*flush_interval, epoch_start + (seq+1)*flush_interval)`.
  Clock drift between emitters causes window-boundary sample smear
  (bounded by drift magnitude relative to flush interval), never
  misalignment of *which* window a sample belongs to.
- **Epoch rotation is jittered per emitter**, not a wire concept: emitter
  `e`'s rotation instant is `epoch_boundary + (xxh64(emitter_id) %
  jitter_window)`. Both encoder and decoder derive this offset from
  `emitter_id` alone (already on every frame) — there is no wire field for
  it, and a decoder never needs one, since it only ever reads whatever
  `epoch` a frame already carries and tracks each emitter's epoch
  independently. This exists purely to avoid a fleet-wide simultaneous
  golden-keyframe spike at every rotation boundary.
- **Watermark and repair** (decoder-local, not wire structure): a window
  is first considered ready after `allowed_lateness` (default 3 flushes)
  have elapsed past it. A frame arriving for an already-solved window
  within `repair_horizon` (default 10 minutes) triggers a re-solve using
  sketch additivity (linear measurements sum), producing a new result with
  an incremented **revision counter**; downstream sinks upsert on key
  rather than treating each revision as a new fact.
- **Coverage** (`emitters_present / emitters_expected` for a given window)
  is a decode-side annotation on a recovery result, not a wire field: it
  reflects how many distinct emitters have contributed to a window's
  merged sketch so far, and gates result confidence alongside the
  recovery-residual gate.

## 7. Security Considerations

Reused verbatim from ADR-012's threat model (`docs/SECURITY.md`):

- **No listening sockets by default.** Agents push frames (and optionally
  snapshot blobs) speculatively; there is no pull/query endpoint unless an
  operator explicitly opts in (`pull.enabled`, default `false`, requiring
  mTLS *and* a bearer token when enabled). Nothing to scan, nothing to
  SSRF, by default.
- **Tenancy is a hard boundary.** Sketches never sum across tenants. Seeds
  are derived per-tenant via `HKDF-SHA256(tenant_key, shard_id ||
  epoch_index || view_id)`, not a public or well-known convention — an
  attacker without `tenant_key` cannot forge bucket assignments or
  correlate another tenant's sketch.
- **Compressed sensing is not encryption.** The measurement matrix Phi and
  quantized residuals are **not** a confidentiality mechanism. Treat frames
  on the wire exactly as you would any other telemetry payload: TLS in
  transit, access control at rest. Recovering a frame's content given Phi
  (derivable from `tenant_key` alone) is by design, not a vulnerability —
  but the reverse (an attacker without `tenant_key` learning anything from
  ciphertext-shaped bytes alone) is not a property this protocol claims or
  provides.
- **Key rotation is an operational runbook, not an automated wire feature**
  (`docs/SECURITY.md`'s "Key rotation" section): rotate at an epoch
  boundary, expect a transient DEGRADED state (not a security failure)
  during transition, and retain the old key readable for at least one
  `epoch_rotate` interval past cutover so in-flight/late frames aren't
  orphaned.
- **No re-encryption of data at rest.** Rotating `tenant_key` changes
  future seed derivation; it does not (and cannot) alter already-written
  keyframe or snapshot bytes. This protocol provides no mechanism for that
  and does not claim to.

## 8. IANA-style registries

These registries are closed sets today; a new value in any of them is a
protocol change (a new RFC or an amendment to this one), not a
configuration option. All three are single-byte (`u8`) fields.

### 8.1 Frame type registry (`frame_type`, offset 5)

| Value | Name | Meaning |
| --- | --- | --- |
| 0 | *(reserved)* | Unassigned. A decoder encountering `frame_type = 0` treats it as an unrecognized frame type per normal error handling — no v2 encoder ever emits it. |
| 1 | KEYFRAME | Exact per-series values, Full or KDELTA-coded (§3.4) |
| 2 | RESIDUAL | Quantized sketch measurements |
| 3 | FALLBACK | Heavy-hitters top-k, storm fallback (ADR-004) |
| 4-255 | *(unassigned)* | Reserved for future allocation |

### 8.2 Codec registry (`codec`, offset 45; v2 only — see §3.1 for v1's derivation)

| Value | Name | Implementation |
| --- | --- | --- |
| 0 | none | Identity (no compression); registered unconditionally |
| 1 | gzip | `compress/gzip` (stdlib); registered unconditionally |
| 2 | zstd | `github.com/klauspost/compress/zstd`, via the `zstdcodec` leaf package — **not** registered by `pkg/wire` itself (ADR-007: `pkg/*` stays stdlib + xxhash only); a binary must call `zstdcodec.Register()` at startup to decode a frame naming this codec |
| 3-255 | *(unassigned)* | A frame naming an unregistered codec (whether unassigned, or assigned but not `Register`-ed in this process) MUST decode as a typed error (`ErrUnregisteredCodec`), gated at low confidence and counted — never silently dropped |

### 8.3 Predictor registry (`predictor`, offset 43)

| Value | Name | Meaning |
| --- | --- | --- |
| 0 | HOLD | Open-loop baseline held constant between keyframes/births (ADR-003); the only predictor v2 defines |
| 1-255 | *(unassigned)* | Reserved for a future closed-loop or adaptive predictor |

### 8.4 DictDelta flags registry (`dflags`, dict_delta offset +8)

| Bit | Name | Meaning |
| --- | --- | --- |
| 0 | TOMBSTONE | Entry evicts `id`; `init_value` and `name` MUST be zero/empty |
| 1-7 | *(reserved)* | MUST be zero on encode; a decoder MUST ignore unset reserved bits (not reject the frame) |

### 8.5 Frame flags registry (`flags`, offset 6)

| Bit | Name | Meaning |
| --- | --- | --- |
| 0 | GZIP (deprecated in v2) | v1-only: snapshot blob is gzip-compressed. A v2 encoder MUST NOT set this bit; use `codec` instead. A v2 decoder ignores this bit and reads `codec` (offset 45) |
| 1 | KDELTA | KEYFRAME payload is delta-coded against the prior keyframe (§3.4), rather than Full |
| 2-7 | *(reserved)* | MUST be zero on encode; a decoder MUST ignore unset reserved bits |

## 9. Conformance

Conformance to this specification is defined **only** over the byte-exact
vector classes below. An implementation claiming v2 conformance MUST
reproduce every vector in these classes byte-for-byte against the
reference oracle (`oracle/palimpsest_ref.py`, `oracle/gen_golden.py` —
"Python is truth", ADR-001):

| Vector class | Golden file(s) | Covers |
| --- | --- | --- |
| Wire framing | `testdata/golden/frame_residual.bin` | §3 Marshal/Unmarshal byte layout, CRC32C |
| Hashing | `testdata/golden/hash_vectors.json` | §4 `seriesID`, `h1`/`h2`, bucket/sign |
| `dict_root` | `testdata/golden/dictroot_vectors.json` | §5 digest computation |
| HKDF seed derivation | `testdata/golden/hkdf_vectors.json` | §4 `seed_ephemeral` |
| `key_version` / key rotation | `testdata/golden/key_rotation_vectors.json` | §4 rotation semantics |
| KDELTA keyframe coding | `testdata/golden/kdelta_sequence.json` | §3.4 KDELTA payload |
| Quantization | `testdata/golden/quant_vectors.json` | §3.4 RESIDUAL/KEYFRAME quantization |

Run `make golden` to regenerate these from the Python oracle; the Go golden
tests (`pkg/wire/golden_test.go`, `pkg/sketch/golden_test.go`, and peers)
are the executable form of this conformance requirement.

**Recovery vectors are INFORMATIVE, not conformance-required.**
`testdata/golden/recovery_case1.json`, `recovery_watermark.json`, and
`recovery_group_case.json` exercise FISTA and Group-OMP sparse recovery,
and are **tolerance-compared**, not byte-exact: different BLAS backends
accumulate floating-point rounding differently across 350 iterations over
large matrices, producing bit-identical *answers* (same recovered support,
values differing by well under 1 ULP) serialized with visibly different
low-order digits (ADR-001's consequences, "BLAS-determinism asterisk").
Recovery is a reconstruction *algorithm* an implementation chooses, not a
protocol requirement this RFC governs — a conforming decoder could in
principle use a different recovery method entirely and remain compliant,
provided the wire frames it reads and writes satisfy the byte-exact classes
above.

## 10. v3 proposal appendix (informative)

This section is **non-normative**. It records proposals and a discovered
defect surfaced while producing this RFC; per this prompt's own guardrails,
neither is implemented here — v2 is documented as shipped, with no protocol
changes.

### 10.1 Merkle dict root + bidirectional resync (proposed)

`docs/adr/ADR-017-merkle-decision.md` evaluated the backlog proposal to
replace §5's flat `dict_root` digest and single golden-keyframe resync
mechanism with a Merkle tree over the active-ID set and a bidirectional
resync channel (request/respond over just the differing subtrees, instead
of re-sending the whole dictionary). Measured evidence (`docs/PERF.md`
"Merkle dict_root / resync evidence gate"): full-dict-keyframe bytes are
**18-35%** of total wire bytes at realistic churn (10-300 events/min over a
1h-equivalent run) — well past the 10% evidence-gate threshold at every
rate tested, including the lowest. **Decision: the backlog item is not
closed; a Merkle root + resync channel is justified and proposed as a v3
wire change.** It is out of scope for v2 (this RFC documents v2 as shipped)
and is not designed here — this appendix records that the evidence
supports building it, not a design for it.

### 10.2 Discovered defect: golden keyframes silently drop same-window tombstones

While measuring §10.1's evidence, `internal/core.TestDictRootEvidenceGateMeasurement`
found a real bug, not a contrived one: under **lossless** delivery (no
dropped, duplicated, or reordered frames), `dict_root` mismatches occurred
anyway, at every churn rate tested, leaving the affected view permanently
degraded for the rest of each run.

Root cause: `cmd/plsim/encoder.go`, `otel/processor/csresidual/processor.go`,
and `internal/core`'s test-only encoder mirror all share this pattern on a
golden keyframe window:

```go
tombstones := tracker.Expire(now, seriesTTL)  // mutates tracker state now
...
if golden {
    f.DictDeltas = tracker.FullDict()         // adds only -- replaces dictDeltas wholesale
} else {
    f.DictDeltas = dictDeltas                 // births ++ tombstones
}
```

`Expire`'s tombstones take effect in the encoder's own tracker regardless,
but are never placed on the wire when that same flush happens to be a
golden keyframe: `FullDict()` unconditionally replaces `dictDeltas` rather
than extending it, and (being a full re-announcement of currently-active
IDs) contains adds only — it has no mechanism to tell a decoder to *remove*
an ID that expired in that same call. A decoder holding that ID keeps it
forever: the encoder has already expired it once and will not tombstone it
again. Unlike a dropped *birth* (repaired by the very next golden keyframe,
per `internal/core.TestDictRootHealTimeMeasurement`), a dropped *tombstone*
at a golden boundary is a **permanent**, not a bounded, divergence — and
over a realistic ~1h run, with tombstones firing roughly every 10-15
windows against a 60-window golden cadence, a same-window collision is
close to inevitable.

**Proposed v3 fix** (not implemented here): a golden keyframe's
`DictDeltas` should carry that same flush's tombstones *alongside*
`FullDict()`'s adds, not instead of them (`dict_delta` entries already
support mixed birth/tombstone lists in any frame type — this requires no
wire-format change, only an encoder-side fix in the three call sites
above). A Merkle resync channel (§10.1) would also detect and repair this
class of divergence automatically, independent of whether the underlying
bug is fixed.
