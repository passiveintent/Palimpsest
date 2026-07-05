# Palimpsest wire & math spec v1 (ADR-008 through ADR-013)

## Identity (ADR-008)

Logical series name: metric_name + "|" + join(logical_key values, ",") +
"|agg=" + one of {sum, count, max}. Only logical series are sketched.
Instance labels are NOT part of the sketch name; they ride the ring buffer.

## Hashing & Phi (ADR-002, ADR-012)

    seriesID = xxh64(name_bytes, seed=0)
    seed_ephemeral = HKDF-SHA256(tenant_key, shard_id || epoch_index || view_id)
    h1 = xxh64(name_bytes, seed_ephemeral)
    h2 = xxh64(name_bytes, seed_ephemeral ^ 0x9E3779B97F4A7C15) | 1
    bucket[i] = (h1 + u64(i)*h2) mod u64(m)
    sign[i] = +1 if ((h1 >> (i+1)) & 1) == 1 else -1
    Phi entry value = sign[i] / sqrt(d).

## Dict Root & Lifecycle (ADR-008, ADR-013)

DictRoot: xxh64(sorted active IDs serialized LE u64, seed=0).

    dict_delta: (id u64, dflags u8, init_value f32, name_len u16, name bytes)
      dflags: bit0=TOMBSTONE, bit1=reserved. TOMBSTONE: init_value=0, name empty.

Every emitter MUST open each (emitter, epoch) stream with golden keyframe +
full dict. Epochs are independent coordinate systems.

## Frame Layout (little-endian; CRC32C over all prior bytes)

    magic "PLMP"(4) | version u8=2 | frame_type u8 | flags u8 | bits u8
    emitter_id u64 | shard_id u64 | epoch u64 | seq u32
      (seq = window index within epoch; [epoch_start+seq*flush, +flush))
    view_id u16 | m u32 | d u8 | predictor u8 | key_version u8 | codec u8
    energy f32 | quant_scale f32 | dict_root u64
    dict_delta_count u32
      repeated{ id u64, dflags u8, init_value f32, name_len u16, name bytes }
    snapshot_blob_len u32 | snapshot_blob bytes (optional, ADR-012)
      (blob format: count u32 + repeated{id u64, ts_ms u64, value f32})
    payload_len u32 | payload bytes | crc32c u32

    frame_type: 1=KEYFRAME 2=RESIDUAL 3=FALLBACK
    flags: bit0=gzip (DEPRECATED in v2; use codec field instead), bit1=KDELTA
      (if set, KEYFRAME payload is delta-coded)
    key_version: identifies the HKDF tenant_key generation used for seed
      derivation (ADR-012 §Addendum). First byte after predictor in v2.
    codec: 0=none, 1=gzip, 2=zstd (reserved; implementation deferred).
      Replaces flags.bit0 for snapshot blob compression in v2 frames.

    Version compatibility:
      Encoder MUST write v2 only.
      Decoder MUST accept v1 and v2.
        v1 frames: key_version=0 (implied), codec = flags.bit0 ? 1 : 0.
      Version >2: decoder MUST return ErrVersion (typed sentinel).

    KEYFRAME: Full=[count u32 + repeated{id u64, value f32}]
              KDELTA=[changed_bitmap (varint) + varint-deltas for set bits]
              Golden keyframes (every keyframe_full_dict_every) use Full.
    RESIDUAL: packed int8/int16 x m (per bits field).
    FALLBACK: k u32 + repeated{id u64, value f32} + total_sum f64 + total_count u64.

## Recovery (ADR-009, ADR-013)

Five substrates per ADR-009:

  a. Deviation (via recovered support): latency ~ flush + decode.
  b. Named-series fast (count-sketch point query): O(d), error ~ ||r||/sqrt(m).
  c. Slow-drift (keyframe scan): latency = keyframe cadence.
  d. Absence (metadata/tombstone): latency = series_ttl.
  e. Exact tier (ADR-005): unchanged from status quo.

Results carry confidence (recovered | pointquery | keyframe | fallback),
recovery-residual gate, and coverage = emitters_present / emitters_expected.

## Watermarks & Repair (ADR-013)

Initial recovery at allowed_lateness (default 3 flushes). Late-arriving
frames within repair_horizon (default 10 min) increment window-sum sketches
and re-solve. Results carry revision counter; downstream sinks upsert on key.

## Temporal (ADR-013)

seq is WINDOW INDEX, not a sequence counter. Clock drift causes window-boundary
sample smear (~drift/flush), not misalignment. Per-emitter clock skew is
measured and alerted (tolerance: tens of ms, NTP-grade fleets are safe).
