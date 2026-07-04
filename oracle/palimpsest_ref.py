"""Palimpsest conformance oracle (ADR-001, ADR-008 through ADR-013).

Python is truth: every function here is the byte-exact reference that the
Go packages (pkg/sketch, pkg/wire) must reproduce. See docs/SPEC.md for the
prose spec this module implements; each section below is annotated with the
SPEC.md heading it covers.

Only numpy, scipy, and xxhash are used (oracle/requirements.txt); CRC32C and
HKDF are implemented directly against stdlib primitives (hashlib/hmac) to
avoid pulling in extra packages for two well-defined, easily-verified
algorithms.
"""

from __future__ import annotations

import hashlib
import hmac
import math
import struct
from dataclasses import dataclass, field

import numpy as np
import xxhash
from scipy import sparse

MASK64 = (1 << 64) - 1

# Same odd 64-bit golden-ratio constant used throughout the Go implementation
# (pkg/sketch/hash.go: phiMix) to decorrelate the second hash from the first.
PHI_MIX = 0x9E3779B97F4A7C15

# Stub tenant key used only for golden-vector generation (never a real
# secret): ADR-012 requires seeds to derive from a secret tenant_key, so the
# oracle needs *a* key, not the public-convention alternative it forbids.
TEST_TENANT_KEY = b"test-vector-key"

MAGIC = b"PLMP"
VERSION = 1

FRAME_TYPE_KEYFRAME = 1
FRAME_TYPE_RESIDUAL = 2
FRAME_TYPE_FALLBACK = 3

FLAG_GZIP = 1 << 0
FLAG_KDELTA = 1 << 1

DICT_FLAG_TOMBSTONE = 1 << 0

PREDICTOR_HOLD = 0

# FISTA iteration cap (docs/SPEC.md via palimpsest_copilot_prompts_v3_full.md
# PROMPT 5 guardrails: "FISTA Iters cap is 350 per SPEC").
FISTA_MAX_ITERS = 350


# --------------------------------------------------------------------------
# Hashing & Phi (SPEC.md "Hashing & Phi")
# --------------------------------------------------------------------------


def xxh64(data: bytes, seed: int = 0) -> int:
    """xxh64(data, seed) as an unsigned 64-bit integer."""
    return xxhash.xxh64(data, seed=seed & MASK64).intdigest()


def series_id(name: bytes) -> int:
    """seriesID = xxh64(name_bytes, seed=0)."""
    return xxh64(name, 0)


def buckets(name: bytes, seed: int, m: int, d: int) -> tuple[list[int], list[int]]:
    """Hash-implicit Phi columns for name under seed.

    h1 = xxh64(name, seed)
    h2 = xxh64(name, seed ^ PHI_MIX) | 1
    bucket[i] = (h1 + u64(i)*h2) mod m
    sign[i]   = +1 if bit (i+1) of h1 is set, else -1
    """
    h1 = xxh64(name, seed)
    h2 = xxh64(name, seed ^ PHI_MIX) | 1

    idx: list[int] = []
    sign: list[int] = []
    for i in range(d):
        val = (h1 + ((i * h2) & MASK64)) & MASK64
        idx.append(val % m)
        sign.append(1 if (h1 >> (i + 1)) & 1 == 1 else -1)
    return idx, sign


# --------------------------------------------------------------------------
# HKDF-SHA256 seed derivation (SPEC.md "Hashing & Phi"; ADR-012)
# --------------------------------------------------------------------------


def hkdf_extract(salt: bytes, ikm: bytes) -> bytes:
    """RFC 5869 extract: PRK = HMAC-Hash(salt, IKM)."""
    if not salt:
        salt = b"\x00" * hashlib.sha256().digest_size
    return hmac.new(salt, ikm, hashlib.sha256).digest()


def hkdf_expand(prk: bytes, info: bytes, length: int) -> bytes:
    """RFC 5869 expand: OKM = T(1) || T(2) || ... truncated to length."""
    okm = b""
    t = b""
    counter = 1
    while len(okm) < length:
        t = hmac.new(prk, t + info + bytes([counter]), hashlib.sha256).digest()
        okm += t
        counter += 1
    return okm[:length]


def derive_ephemeral_seed(tenant_key: bytes, shard_id: int, epoch_idx: int, view_id: int) -> int:
    """seed_ephemeral = HKDF-SHA256(tenant_key, shard_id || epoch_index || view_id), truncated to u64."""
    info = shard_id.to_bytes(8, "big") + epoch_idx.to_bytes(4, "big") + view_id.to_bytes(2, "big")
    prk = hkdf_extract(b"", tenant_key)
    okm = hkdf_expand(prk, info, 8)
    return int.from_bytes(okm, "big")


# --------------------------------------------------------------------------
# DictRoot (SPEC.md "Dict Root & Lifecycle"; ADR-008)
# --------------------------------------------------------------------------


def compute_dict_root(ids: list[int]) -> int:
    """xxh64(sorted active IDs serialized LE u64, seed=0)."""
    sorted_ids = sorted(ids)
    buf = b"".join(i.to_bytes(8, "little") for i in sorted_ids)
    return xxh64(buf, 0)


# --------------------------------------------------------------------------
# float32 helpers: Go carries Energy/QuantScale/keyframe values/DictDelta
# init_value as float32 on the wire; matching Go's arithmetic exactly means
# rounding to float32 precision at the same points Go would (a field's
# declared type), not computing everything in float64 throughout.
# --------------------------------------------------------------------------


def f32(x: float) -> float:
    """Round-trip x through float32, mirroring a Go float32 field/value."""
    return float(np.float32(x))


def f32_sub(a: float, b: float) -> float:
    """float32(a) - float32(b), as a float32 result (then widened to python float)."""
    return float(np.float32(np.float32(a) - np.float32(b)))


def go_round(x: float) -> int:
    """math.Round semantics: nearest integer, ties away from zero."""
    if x >= 0:
        return math.floor(x + 0.5)
    return math.ceil(x - 0.5)


# --------------------------------------------------------------------------
# Quantization (SPEC.md "Frame Layout"; pkg/wire/quant.go)
# --------------------------------------------------------------------------


def _clamp_round(v: float, scale_f64: float, lo: int, hi: int) -> int:
    q = go_round(v / scale_f64)
    if q < lo:
        return lo
    if q > hi:
        return hi
    return q


def quantize(values: list[float], bits: int, scale: float) -> bytes:
    """Quantize residuals into a RESIDUAL payload (int8/int16 LE, per bits)."""
    if not (scale > 0) or math.isnan(scale) or math.isinf(scale):
        raise ValueError(f"invalid quant_scale {scale!r}")
    scale_f64 = f32(scale)
    if bits == 8:
        out = bytearray(len(values))
        for i, v in enumerate(values):
            q = _clamp_round(v, scale_f64, -128, 127)
            out[i] = q & 0xFF
        return bytes(out)
    if bits == 16:
        out = bytearray(len(values) * 2)
        for i, v in enumerate(values):
            q = _clamp_round(v, scale_f64, -32768, 32767)
            struct.pack_into("<h", out, i * 2, q)
        return bytes(out)
    raise ValueError(f"unsupported quant bits {bits}")


def dequantize(payload: bytes, m: int, bits: int, scale: float) -> list[float]:
    """Inverse of quantize: out[i] = quantized[i] * scale."""
    scale_f64 = f32(scale)
    if bits == 8:
        if len(payload) != m:
            raise ValueError(f"residual payload length {len(payload)} != m {m}")
        return [struct.unpack_from("<b", payload, i)[0] * scale_f64 for i in range(m)]
    if bits == 16:
        if len(payload) != m * 2:
            raise ValueError(f"residual payload length {len(payload)} != 2*m {2 * m}")
        return [struct.unpack_from("<h", payload, i * 2)[0] * scale_f64 for i in range(m)]
    raise ValueError(f"unsupported quant bits {bits}")


# --------------------------------------------------------------------------
# Varints: standard unsigned LEB128 (matches Go's encoding/binary
# PutUvarint/Uvarint) plus zigzag signed mapping.
# --------------------------------------------------------------------------


def put_uvarint(x: int) -> bytes:
    if x < 0:
        raise ValueError("put_uvarint requires a non-negative integer")
    out = bytearray()
    while x >= 0x80:
        out.append((x & 0x7F) | 0x80)
        x >>= 7
    out.append(x & 0x7F)
    return bytes(out)


def uvarint(b: bytes, pos: int = 0) -> tuple[int, int]:
    """Returns (value, bytes_consumed)."""
    x = 0
    shift = 0
    i = pos
    while True:
        byte = b[i]
        if byte < 0x80:
            x |= byte << shift
            return x, i + 1 - pos
        x |= (byte & 0x7F) << shift
        shift += 7
        i += 1


def zigzag_encode(v: int) -> int:
    return (v << 1) ^ (v >> 63)


def zigzag_decode(zz: int) -> int:
    return (zz >> 1) ^ -(zz & 1)


# --------------------------------------------------------------------------
# KEYFRAME codec: Full + KDELTA (SPEC.md "Frame Layout"; ADR-011)
# --------------------------------------------------------------------------


def encode_changed_bitmap(changed: list[bool]) -> bytes:
    """Base-128 LE bitmap: put_uvarint of the integer whose bit i is changed[i]."""
    mask = 0
    for i, c in enumerate(changed):
        if c:
            mask |= 1 << i
    return put_uvarint(mask)


def decode_changed_bitmap(b: bytes, pos: int, n: int) -> tuple[list[bool], int]:
    mask, consumed = uvarint(b, pos)
    changed = [(mask >> i) & 1 == 1 for i in range(n)]
    return changed, consumed


def encode_keyframe_full(ids: list[int], values: dict[int, float]) -> bytes:
    """Full KEYFRAME payload: count u32 + repeated{id u64, value f32}."""
    buf = bytearray(struct.pack("<I", len(ids)))
    for i in ids:
        buf += struct.pack("<Qf", i, f32(values[i]))
    return bytes(buf)


def encode_keyframe_kdelta(
    ids: list[int], values: dict[int, float], prev: dict[int, float], scale: float
) -> bytes:
    """KDELTA KEYFRAME payload: changed_bitmap (varint) + varint-zigzag-deltas."""
    if not (scale > 0) or math.isnan(scale) or math.isinf(scale):
        raise ValueError(f"invalid quant_scale {scale!r} for KDELTA")
    scale_f64 = f32(scale)

    changed = []
    for i in ids:
        v = f32(values[i])
        pv = prev.get(i)
        changed.append(pv is None or f32(pv) != v)

    buf = bytearray(encode_changed_bitmap(changed))
    for flag, i in zip(changed, ids):
        if not flag:
            continue
        pv = prev.get(i, 0.0)
        delta_f64 = f32_sub(values[i], pv)
        delta = go_round(delta_f64 / scale_f64)
        buf += put_uvarint(zigzag_encode(delta))
    return bytes(buf)


def decode_keyframe_full(payload: bytes) -> dict[int, float]:
    count = struct.unpack_from("<I", payload, 0)[0]
    out: dict[int, float] = {}
    pos = 4
    for _ in range(count):
        i, v = struct.unpack_from("<Qf", payload, pos)
        out[i] = v
        pos += 12
    if pos != len(payload):
        raise ValueError("keyframe has trailing bytes")
    return out


def decode_keyframe_kdelta(
    payload: bytes, ids: list[int], prev: dict[int, float], scale: float
) -> dict[int, float]:
    scale_f64 = f32(scale)
    changed, pos = decode_changed_bitmap(payload, 0, len(ids))
    out: dict[int, float] = {}
    for flag, i in zip(changed, ids):
        if not flag:
            if i not in prev:
                raise ValueError(f"KDELTA id {i} unchanged but absent from prior keyframe")
            out[i] = prev[i]
            continue
        zz, n = uvarint(payload, pos)
        pos += n
        delta = zigzag_decode(zz)
        out[i] = f32(prev.get(i, 0.0) + delta * scale_f64)
    if pos != len(payload):
        raise ValueError("KDELTA keyframe has trailing bytes")
    return out


# --------------------------------------------------------------------------
# Snapshot blob (SPEC.md "Frame Layout"; ADR-012)
# --------------------------------------------------------------------------


def encode_snapshot(entries: list[tuple[int, int, float]]) -> bytes:
    """count u32 + repeated{id u64, ts_ms u64, value f32}, uncompressed."""
    buf = bytearray(struct.pack("<I", len(entries)))
    for sid, ts_ms, value in entries:
        buf += struct.pack("<QQf", sid, ts_ms, f32(value))
    return bytes(buf)


# --------------------------------------------------------------------------
# CRC32C (Castagnoli) -- pure-python table-driven implementation, verified
# against the standard check value ("123456789" -> 0xE3069283).
# --------------------------------------------------------------------------


def _make_crc32c_table() -> list[int]:
    poly = 0x82F63B78  # reversed (LSB-first) CRC-32C polynomial
    table = []
    for byte in range(256):
        crc = byte
        for _ in range(8):
            if crc & 1:
                crc = (crc >> 1) ^ poly
            else:
                crc >>= 1
        table.append(crc & 0xFFFFFFFF)
    return table


_CRC32C_TABLE = _make_crc32c_table()


def crc32c(data: bytes) -> int:
    crc = 0xFFFFFFFF
    for b in data:
        crc = (crc >> 8) ^ _CRC32C_TABLE[(crc ^ b) & 0xFF]
    return (crc ^ 0xFFFFFFFF) & 0xFFFFFFFF


assert crc32c(b"123456789") == 0xE3069283, "CRC32C table is wrong"


# --------------------------------------------------------------------------
# Frame marshal (SPEC.md "Frame Layout"; pkg/wire/codec.go)
# --------------------------------------------------------------------------


@dataclass
class DictDelta:
    id: int
    flags: int
    init_value: float
    name: bytes


@dataclass
class Frame:
    version: int = VERSION
    frame_type: int = FRAME_TYPE_RESIDUAL
    flags: int = 0
    bits: int = 8
    emitter_id: int = 0
    shard_id: int = 0
    epoch: int = 0
    seq: int = 0
    view_id: int = 0
    m: int = 0
    d: int = 0
    predictor: int = PREDICTOR_HOLD
    reserved: int = 0
    energy: float = 0.0
    quant_scale: float = 0.0
    dict_root: int = 0
    dict_deltas: list[DictDelta] = field(default_factory=list)
    snapshot_blob: bytes = b""
    payload: bytes = b""


def marshal_frame(f: Frame) -> bytes:
    buf = bytearray()
    buf += MAGIC
    buf += struct.pack("<BBBB", f.version, f.frame_type, f.flags, f.bits)
    buf += struct.pack("<QQQI", f.emitter_id, f.shard_id, f.epoch, f.seq)
    buf += struct.pack("<HIBBB", f.view_id, f.m, f.d, f.predictor, f.reserved)
    buf += struct.pack("<ffQ", f32(f.energy), f32(f.quant_scale), f.dict_root)

    buf += struct.pack("<I", len(f.dict_deltas))
    for dd in f.dict_deltas:
        buf += struct.pack("<QBf", dd.id, dd.flags, f32(dd.init_value))
        buf += struct.pack("<H", len(dd.name))
        buf += dd.name

    buf += struct.pack("<I", len(f.snapshot_blob))
    buf += f.snapshot_blob

    buf += struct.pack("<I", len(f.payload))
    buf += f.payload

    buf += struct.pack("<I", crc32c(bytes(buf)))
    return bytes(buf)


# --------------------------------------------------------------------------
# FISTA sparse recovery + debias (PROMPT 5's "feasibility harness": basis
# pursuit denoising over the implicit Phi, solved via FISTA, then a ridge
# least-squares refit on the recovered support to undo L1 shrinkage bias).
# --------------------------------------------------------------------------


def build_phi(names: list[bytes], seed: int, m: int, d: int) -> sparse.csc_matrix:
    """The (m x N) implicit measurement matrix, materialized as sparse CSC
    purely for the oracle's own recovery solver -- the Go encode path never
    does this (ADR-002); Go's decode-side recover package will use its own
    CSR (pkg/recover/csr.go, PROMPT 5) rather than this scipy matrix.
    """
    inv_sqrt_d = 1.0 / math.sqrt(d)
    n = len(names)
    rows = np.empty(n * d, dtype=np.int64)
    cols = np.empty(n * d, dtype=np.int64)
    vals = np.empty(n * d, dtype=np.float64)
    pos = 0
    for j, name in enumerate(names):
        idx, sign = buckets(name, seed, m, d)
        for i in range(d):
            rows[pos] = idx[i]
            cols[pos] = j
            vals[pos] = sign[i] * inv_sqrt_d
            pos += 1
    return sparse.csc_matrix((vals, (rows, cols)), shape=(m, n))


def _soft_threshold(v: np.ndarray, thresh: float) -> np.ndarray:
    return np.sign(v) * np.maximum(np.abs(v) - thresh, 0.0)


def _estimate_lipschitz(phi: sparse.csc_matrix, power_iters: int, rng: np.random.Generator) -> float:
    """Power iteration on Phi^T Phi to estimate its largest eigenvalue
    (the FISTA step size is 1/L)."""
    phi_t = phi.T.tocsr()
    n = phi.shape[1]
    v = rng.standard_normal(n)
    norm = np.linalg.norm(v)
    if norm == 0:
        return 1.0
    v /= norm
    for _ in range(power_iters):
        v = phi_t @ (phi @ v)
        norm = np.linalg.norm(v)
        if norm == 0:
            return 1.0
        v /= norm
    av = phi_t @ (phi @ v)
    L = float(v @ av)
    return L if L > 0 else 1.0


def fista(
    phi: sparse.csc_matrix,
    y: np.ndarray,
    lam: float,
    iters: int = FISTA_MAX_ITERS,
    power_iters: int = 50,
    seed: int = 0,
) -> np.ndarray:
    """FISTA for min_x 0.5||y - Phi x||^2 + lam||x||_1."""
    if iters > FISTA_MAX_ITERS:
        raise ValueError(f"iters {iters} exceeds FISTA_MAX_ITERS {FISTA_MAX_ITERS}")
    phi_t = phi.T.tocsr()
    rng = np.random.default_rng(seed)
    L = _estimate_lipschitz(phi, power_iters, rng)

    n = phi.shape[1]
    x = np.zeros(n)
    z = np.zeros(n)
    t = 1.0
    for _ in range(iters):
        grad = phi_t @ (phi @ z - y)
        x_new = _soft_threshold(z - grad / L, lam / L)
        t_new = (1 + math.sqrt(1 + 4 * t * t)) / 2
        z = x_new + ((t - 1) / t_new) * (x_new - x)
        x = x_new
        t = t_new
    return x


def debias(phi: sparse.csc_matrix, y: np.ndarray, support: list[int], ridge: float = 1e-6) -> np.ndarray:
    """Dense normal equations + ridge on the recovered support columns."""
    if not support:
        return np.zeros(0)
    phi_s = phi[:, support].toarray()
    a = phi_s.T @ phi_s + ridge * np.eye(len(support))
    b = phi_s.T @ y
    return np.linalg.solve(a, b)


def recover(
    phi: sparse.csc_matrix,
    y: np.ndarray,
    lam: float,
    threshold: float,
    iters: int = FISTA_MAX_ITERS,
    power_iters: int = 50,
    seed: int = 0,
) -> tuple[list[int], np.ndarray, np.ndarray]:
    """Runs FISTA then debias; returns (support_indices, x_fista, x_debiased)."""
    x = fista(phi, y, lam, iters=iters, power_iters=power_iters, seed=seed)
    support = sorted(int(i) for i in np.nonzero(np.abs(x) > threshold)[0])
    x_deb = debias(phi, y, support)
    return support, x, x_deb
