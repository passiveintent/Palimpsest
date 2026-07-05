#!/usr/bin/env python3
# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
#
# This source code is licensed under the AGPL-3.0-only license found in the
# LICENSE file in the root directory of this source tree.

"""Generates testdata/golden/* from palimpsest_ref.py (docs/SPEC.md, ADR-008
through ADR-013). Python is truth: Go's pkg/sketch and pkg/wire golden tests
must reproduce every vector this script writes, byte-for-byte.

Usage:
    python gen_golden.py --out ../testdata/golden
"""

from __future__ import annotations

import argparse
import json
import math
import sys
from pathlib import Path

import numpy as np

import palimpsest_ref as ref

# --------------------------------------------------------------------------
# Shared fixtures
# --------------------------------------------------------------------------

# Canonical (m, d) used across the hash/recovery vectors, matching the
# example config in the build prompts (m=2000, d=6, bits=8).
M_DEFAULT = 2000
D_DEFAULT = 6

METRICS = [
    "cpu_usage", "mem_usage", "disk_io", "net_bytes", "http_requests",
    "gc_pause", "queue_depth", "cache_hit_ratio", "thread_count", "error_rate",
]
REGIONS = ["us-east", "us-west", "eu-central", "ap-south", "sa-east"]
AGGS = ["sum", "count", "max"]


def logical_name(metric: str, region: str, agg: str) -> bytes:
    return f"{metric}|region={region},cluster=prod|agg={agg}".encode()


def fifty_names() -> list[bytes]:
    names = []
    for metric in METRICS:
        for region in REGIONS:
            agg = AGGS[len(names) % len(AGGS)]
            names.append(logical_name(metric, region, agg))
    assert len(names) == 50
    return names


def hex_bytes(b: bytes) -> str:
    return b.hex()


# --------------------------------------------------------------------------
# hash_vectors.json
# --------------------------------------------------------------------------


def gen_hash_vectors() -> dict:
    names = fifty_names()
    seed_configs = [
        {"shard_id": 1, "epoch_idx": 0, "view_id": 0},
        {"shard_id": 1, "epoch_idx": 1, "view_id": 0},
        {"shard_id": 2, "epoch_idx": 0, "view_id": 1},
    ]
    seeds = [
        ref.derive_ephemeral_seed(ref.TEST_TENANT_KEY, c["shard_id"], c["epoch_idx"], c["view_id"])
        for c in seed_configs
    ]

    cases = []
    for name in names:
        by_seed = []
        for seed in seeds:
            idx, sign = ref.buckets(name, seed, M_DEFAULT, D_DEFAULT)
            by_seed.append({"seed": seed, "idx": idx, "sign": sign})
        cases.append({
            "name": name.decode(),
            "series_id": ref.series_id(name),
            "by_seed": by_seed,
        })

    return {
        "tenant_key": ref.TEST_TENANT_KEY.decode(),
        "m": M_DEFAULT,
        "d": D_DEFAULT,
        "seed_configs": seed_configs,
        "seeds": seeds,
        "cases": cases,
    }


# --------------------------------------------------------------------------
# hkdf_vectors.json
# --------------------------------------------------------------------------


def gen_hkdf_vectors() -> dict:
    entries = [
        {"tenant_key": ref.TEST_TENANT_KEY.decode(), "shard_id": 1, "epoch_idx": 0, "view_id": 0},
        {"tenant_key": ref.TEST_TENANT_KEY.decode(), "shard_id": 1, "epoch_idx": 1, "view_id": 0},
        {"tenant_key": ref.TEST_TENANT_KEY.decode(), "shard_id": 2, "epoch_idx": 0, "view_id": 1},
        {"tenant_key": ref.TEST_TENANT_KEY.decode(), "shard_id": 0, "epoch_idx": 0, "view_id": 0},
        {"tenant_key": ref.TEST_TENANT_KEY.decode(), "shard_id": 0xFFFFFFFFFFFFFFFF, "epoch_idx": 0xFFFFFFFF, "view_id": 0xFFFF},
        {"tenant_key": "another-tenant-key", "shard_id": 1, "epoch_idx": 0, "view_id": 0},
    ]
    cases = []
    for e in entries:
        seed = ref.derive_ephemeral_seed(e["tenant_key"].encode(), e["shard_id"], e["epoch_idx"], e["view_id"])
        cases.append({**e, "expected_seed": seed})
    return {"cases": cases}


# --------------------------------------------------------------------------
# dictroot_vectors.json
# --------------------------------------------------------------------------


def gen_dictroot_vectors() -> dict:
    hv = fifty_names()
    all_ids = sorted(ref.series_id(n) for n in hv)

    rng = np.random.default_rng(7)
    random_ids = sorted(int(v) for v in rng.integers(0, 2**63, size=20, dtype=np.int64))

    reversed_hundred = list(range(1000, 900, -1))  # unsorted on purpose

    cases = [
        {"description": "empty", "ids": []},
        {"description": "single", "ids": [42]},
        {"description": "unsorted range of 100", "ids": reversed_hundred},
        {"description": "20 pseudo-random u64 ids", "ids": random_ids},
        {"description": "all 50 hash_vectors series ids", "ids": all_ids},
    ]
    for c in cases:
        c["root"] = ref.compute_dict_root(c["ids"])
    assert len(cases) == 5
    return {"cases": cases}


# --------------------------------------------------------------------------
# kdelta_sequence.json
# --------------------------------------------------------------------------


def gen_kdelta_sequence() -> dict:
    scale = 0.1
    names = {
        "A": b"cpu_usage|region=us-east,cluster=prod|agg=sum",
        "B": b"cpu_usage|region=us-west,cluster=prod|agg=sum",
        "C": b"mem_usage|region=us-east,cluster=prod|agg=sum",
        "D": b"mem_usage|region=us-west,cluster=prod|agg=sum",
        "E": b"disk_io|region=us-east,cluster=prod|agg=sum",
    }
    ids = {k: ref.series_id(v) for k, v in names.items()}

    def active_ids(keys):
        return sorted(ids[k] for k in keys)

    steps = []

    # Step 0: golden keyframe, full dict, opens the (emitter, epoch) stream
    # per ADR-008/ADR-013.
    values0 = {"A": 10.0, "B": 20.0, "C": 30.0, "D": 40.0}
    active0 = ["A", "B", "C", "D"]
    ids0 = active_ids(active0)
    valmap0 = {ids[k]: ref.f32(v) for k, v in values0.items()}
    payload0 = ref.encode_keyframe_full(ids0, valmap0)
    steps.append({
        "step": 0,
        "golden": True,
        "flags_kdelta": False,
        "births": [{"id": ids[k], "name": names[k].decode(), "init_value": values0[k]} for k in active0],
        "tombstones": [],
        "ids": ids0,
        "values": {str(ids[k]): ref.f32(values0[k]) for k in active0},
        "dict_root": ref.compute_dict_root(ids0),
        "payload_hex": hex_bytes(payload0),
    })

    # Step 1 (KDELTA): E is born; A drifts; B, C, D unchanged.
    prev_values = valmap0
    values1 = {"A": 11.0, "B": 20.0, "C": 30.0, "D": 40.0, "E": 50.0}
    active1 = ["A", "B", "C", "D", "E"]
    ids1 = active_ids(active1)
    valmap1 = {ids[k]: ref.f32(v) for k, v in values1.items()}
    payload1 = ref.encode_keyframe_kdelta(ids1, valmap1, prev_values, scale)
    steps.append({
        "step": 1,
        "golden": False,
        "flags_kdelta": True,
        "births": [{"id": ids["E"], "name": names["E"].decode(), "init_value": values1["E"]}],
        "tombstones": [],
        "ids": ids1,
        "values": {str(ids[k]): ref.f32(values1[k]) for k in active1},
        "dict_root": ref.compute_dict_root(ids1),
        "payload_hex": hex_bytes(payload1),
    })

    # Step 2 (KDELTA): C drifts; everyone else unchanged, no births/tombstones.
    prev_values = valmap1
    values2 = {"A": 11.0, "B": 20.0, "C": 31.0, "D": 40.0, "E": 50.0}
    active2 = ["A", "B", "C", "D", "E"]
    ids2 = active_ids(active2)
    valmap2 = {ids[k]: ref.f32(v) for k, v in values2.items()}
    payload2 = ref.encode_keyframe_kdelta(ids2, valmap2, prev_values, scale)
    steps.append({
        "step": 2,
        "golden": False,
        "flags_kdelta": True,
        "births": [],
        "tombstones": [],
        "ids": ids2,
        "values": {str(ids[k]): ref.f32(values2[k]) for k in active2},
        "dict_root": ref.compute_dict_root(ids2),
        "payload_hex": hex_bytes(payload2),
    })

    # Step 3 (KDELTA): D is tombstoned (leaves the active set); B drifts.
    prev_values = valmap2
    values3 = {"A": 11.0, "B": 21.0, "C": 31.0, "E": 50.0}
    active3 = ["A", "B", "C", "E"]
    ids3 = active_ids(active3)
    valmap3 = {ids[k]: ref.f32(v) for k, v in values3.items()}
    payload3 = ref.encode_keyframe_kdelta(ids3, valmap3, prev_values, scale)
    steps.append({
        "step": 3,
        "golden": False,
        "flags_kdelta": True,
        "births": [],
        "tombstones": [{"id": ids["D"], "name": names["D"].decode()}],
        "ids": ids3,
        "values": {str(ids[k]): ref.f32(values3[k]) for k in active3},
        "dict_root": ref.compute_dict_root(ids3),
        "payload_hex": hex_bytes(payload3),
    })

    # Self-check: replay each KDELTA step against the previous step's decoded
    # values and confirm it reproduces this generator's own "values" map,
    # exactly the property the Go golden test asserts.
    replay_prev = decode_step_full(steps[0])
    for step in steps[1:]:
        decoded = ref.decode_keyframe_kdelta(
            bytes.fromhex(step["payload_hex"]), step["ids"], replay_prev, scale
        )
        expected = {int(k): v for k, v in step["values"].items()}
        if decoded != expected:
            raise AssertionError(f"kdelta self-check failed at step {step['step']}: {decoded} != {expected}")
        replay_prev = decoded

    return {"scale": scale, "steps": steps}


def decode_step_full(step: dict) -> dict[int, float]:
    return ref.decode_keyframe_full(bytes.fromhex(step["payload_hex"]))


# --------------------------------------------------------------------------
# quant_vectors.json
# --------------------------------------------------------------------------


def gen_quant_vectors() -> dict:
    cases = []

    def add(description, values, bits, scale):
        packed = ref.quantize(values, bits, scale)
        roundtrip = ref.dequantize(packed, len(values), bits, scale)
        cases.append({
            "description": description,
            "values": values,
            "bits": bits,
            "scale": scale,
            "packed_hex": hex_bytes(packed),
            "roundtrip": roundtrip,
        })

    add("bits=8 simple values", [0.0, 1.2, -1.2, 6.0, -6.0, 0.001, -0.001, 3.14159], 8, 0.05)
    add("bits=8 clamps to int8 range", [1e9, -1e9, 200.0, -200.0], 8, 1.0)
    add(
        "bits=8 exact-half ties round away from zero",
        [0.5, -0.5, 1.5, -1.5, 2.5, -2.5, 100.5, -100.5],
        8,
        1.0,
    )
    add("bits=16 simple values", [0.0, 1.2, -1.2, 100.0, -100.0, 1600.0, -1600.0], 16, 0.01)
    add("bits=16 clamps to int16 range", [1e9, -1e9], 16, 1.0)
    add(
        "bits=16 exact-half ties round away from zero",
        [0.5, -0.5, 1.5, -1.5, 32767.5, -32767.5],
        16,
        1.0,
    )
    assert len(cases) == 6
    return {"cases": cases}


# --------------------------------------------------------------------------
# frame_residual.bin
# --------------------------------------------------------------------------

FRAME_BIRTH1_NAME = b"checkout_latency_ms|region=us-east,cluster=prod|agg=sum"
FRAME_BIRTH2_NAME = b"checkout_latency_ms|region=us-west,cluster=prod|agg=sum"
FRAME_TOMBSTONE_NAME = b"checkout_latency_ms|region=eu-central,cluster=prod|agg=sum"


def build_frame_residual() -> ref.Frame:
    birth1_id = ref.series_id(FRAME_BIRTH1_NAME)
    birth2_id = ref.series_id(FRAME_BIRTH2_NAME)
    tombstone_id = ref.series_id(FRAME_TOMBSTONE_NAME)

    dict_deltas = [
        ref.DictDelta(id=birth1_id, flags=0, init_value=12.5, name=FRAME_BIRTH1_NAME),
        ref.DictDelta(id=birth2_id, flags=0, init_value=9.75, name=FRAME_BIRTH2_NAME),
        ref.DictDelta(id=tombstone_id, flags=ref.DICT_FLAG_TOMBSTONE, init_value=0.0, name=b""),
    ]
    active_ids = sorted([birth1_id, birth2_id])
    dict_root = ref.compute_dict_root(active_ids)

    snapshot = ref.encode_snapshot([
        (birth1_id, 1750000000000, 13.1),
        (birth1_id, 1750000005000, 12.9),
    ])

    m = 64
    values = [(i - 32) * 0.25 for i in range(m)]
    quant_scale = 0.25
    payload = ref.quantize(values, 8, quant_scale)

    return ref.Frame(
        version=ref.VERSION,
        frame_type=ref.FRAME_TYPE_RESIDUAL,
        flags=0,
        bits=8,
        emitter_id=0x1122334455667788,
        shard_id=7,
        epoch=3,
        seq=42,
        view_id=1,
        m=m,
        d=4,
        predictor=ref.PREDICTOR_HOLD,
        key_version=0,
        codec=0,
        energy=1.25,
        quant_scale=quant_scale,
        dict_root=dict_root,
        dict_deltas=dict_deltas,
        snapshot_blob=snapshot,
        payload=payload,
    )


# --------------------------------------------------------------------------
# recovery_case1.json / recovery_watermark.json
# --------------------------------------------------------------------------


def make_names(n: int, prefix: str = "metric") -> list[bytes]:
    return [f"{prefix}_{i:06d}|shard={i % 16:02d},cell={i % 97:03d}|agg=sum".encode() for i in range(n)]


def gen_recovery_case1() -> dict:
    n, m, d, k = 50000, 2000, 6, 50
    emitter_id = 1
    seed = ref.derive_ephemeral_seed(ref.TEST_TENANT_KEY, 1, 0, 0)

    names = make_names(n)
    phi = ref.build_phi(names, seed, m, d)

    rng = np.random.default_rng(20260704)
    support_idx = sorted(int(i) for i in rng.choice(n, size=k, replace=False))
    residual_values = (rng.uniform(2.0, 10.0, size=k) * rng.choice([-1.0, 1.0], size=k)).tolist()

    x_true = np.zeros(n)
    for idx, r in zip(support_idx, residual_values):
        x_true[idx] = r
    y = phi @ x_true

    lam, threshold, iters, power_iters = 0.05, 0.3, 350, 50
    support, x_fista, x_deb = ref.recover(phi, y, lam, threshold, iters=iters, power_iters=power_iters, seed=1)

    support_ids = sorted(ref.series_id(names[i]) for i in support)
    debiased_values = {str(ref.series_id(names[i])): float(v) for i, v in zip(support, x_deb)}

    true_support_ids = set(ref.series_id(names[i]) for i in support_idx)
    recovered_ids = set(ref.series_id(names[i]) for i in support)
    recall = len(true_support_ids & recovered_ids) / k

    common = sorted(true_support_ids & recovered_ids)
    id_to_true = {ref.series_id(names[i]): r for i, r in zip(support_idx, residual_values)}
    id_to_deb = {ref.series_id(names[i]): v for i, v in zip(support, x_deb)}
    true_vals = np.array([id_to_true[i] for i in common])
    deb_vals = np.array([id_to_deb[i] for i in common])
    rmse = float(np.sqrt(np.mean((true_vals - deb_vals) ** 2)))
    rel_rmse = rmse / float(np.sqrt(np.mean(true_vals ** 2)))

    if recall < 0.95:
        raise AssertionError(f"recovery_case1 self-check failed: recall {recall} < 0.95")
    if rel_rmse > 0.05:
        raise AssertionError(f"recovery_case1 self-check failed: relative RMSE {rel_rmse} > 0.05")

    return {
        "n": n,
        "m": m,
        "d": d,
        "k": k,
        "emitter_id": emitter_id,
        "tenant_key": ref.TEST_TENANT_KEY.decode(),
        "shard_id": 1,
        "epoch_idx": 0,
        "view_id": 0,
        "seed": seed,
        "lambda": lam,
        "threshold": threshold,
        "iters": iters,
        "power_iters": power_iters,
        "names": [nm.decode() for nm in names],
        "support": [
            {"index": idx, "id": ref.series_id(names[idx]), "name": names[idx].decode(), "residual": r}
            for idx, r in zip(support_idx, residual_values)
        ],
        "y": y.tolist(),
        "expected_support_ids": support_ids,
        "debiased_values": debiased_values,
        "recall": recall,
        "rmse_relative": rel_rmse,
    }


def gen_recovery_watermark() -> dict:
    n, m, d = 3000, 350, 6
    seed = ref.derive_ephemeral_seed(ref.TEST_TENANT_KEY, 9, 0, 0)
    names = make_names(n, prefix="watermark_metric")
    phi = ref.build_phi(names, seed, m, d)

    rng = np.random.default_rng(13)
    all_idx = rng.choice(n, size=18, replace=False)
    a_idx = sorted(int(i) for i in all_idx[:10])
    b_idx = sorted(int(i) for i in all_idx[10:])

    a_residuals = (rng.uniform(2.0, 10.0, size=len(a_idx)) * rng.choice([-1.0, 1.0], size=len(a_idx))).tolist()
    b_residuals = (rng.uniform(2.0, 10.0, size=len(b_idx)) * rng.choice([-1.0, 1.0], size=len(b_idx))).tolist()

    x_a = np.zeros(n)
    for idx, r in zip(a_idx, a_residuals):
        x_a[idx] = r
    x_b = np.zeros(n)
    for idx, r in zip(b_idx, b_residuals):
        x_b[idx] = r

    y_initial = phi @ x_a  # only emitter A (window seq=100) has arrived
    y_revised = y_initial + phi @ x_b  # emitter B's late frame merges additively (ADR-013)

    lam, threshold, iters, power_iters = 0.05, 0.3, 350, 50

    support_i, _, x_deb_i = ref.recover(phi, y_initial, lam, threshold, iters=iters, power_iters=power_iters, seed=2)
    support_r, _, x_deb_r = ref.recover(phi, y_revised, lam, threshold, iters=iters, power_iters=power_iters, seed=2)

    ids_a = set(ref.series_id(names[i]) for i in a_idx)
    ids_b = set(ref.series_id(names[i]) for i in b_idx)
    recovered_i = set(ref.series_id(names[i]) for i in support_i)
    recovered_r = set(ref.series_id(names[i]) for i in support_r)

    recall_initial = len(ids_a & recovered_i) / len(ids_a)
    recall_revised = len((ids_a | ids_b) & recovered_r) / len(ids_a | ids_b)
    if recall_initial < 0.95 or recall_revised < 0.95:
        raise AssertionError(
            f"recovery_watermark self-check failed: recall_initial={recall_initial} recall_revised={recall_revised}"
        )
    if recovered_i & ids_b:
        raise AssertionError("recovery_watermark self-check failed: initial solve saw emitter B's series before it arrived")

    return {
        "n": n,
        "m": m,
        "d": d,
        "seed": seed,
        "shard_id": 9,
        "epoch_idx": 0,
        "view_id": 0,
        "window_seq": 100,
        "allowed_lateness_flushes": 3,
        "repair_horizon_minutes": 10,
        "lambda": lam,
        "threshold": threshold,
        "iters": iters,
        "power_iters": power_iters,
        "names": [nm.decode() for nm in names],
        "initial": {
            "emitter_id": 1,
            "revision": 0,
            "coverage_present": 1,
            "coverage_total": 2,
            "support": [
                {"index": idx, "id": ref.series_id(names[idx]), "name": names[idx].decode(), "residual": r}
                for idx, r in zip(a_idx, a_residuals)
            ],
            "y": y_initial.tolist(),
            "expected_support_ids": sorted(ids_a),
            "debiased_values": {
                str(ref.series_id(names[i])): float(v) for i, v in zip(support_i, x_deb_i)
            },
        },
        "late_frame": {
            "emitter_id": 2,
            "support": [
                {"index": idx, "id": ref.series_id(names[idx]), "name": names[idx].decode(), "residual": r}
                for idx, r in zip(b_idx, b_residuals)
            ],
        },
        "revised": {
            "revision": 1,
            "coverage_present": 2,
            "coverage_total": 2,
            "y": y_revised.tolist(),
            "expected_support_ids": sorted(ids_a | ids_b),
            "debiased_values": {
                str(ref.series_id(names[i])): float(v) for i, v in zip(support_r, x_deb_r)
            },
        },
    }


# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------


def _canon(obj: object, ndigits: int = 9) -> object:
    """Recursively round every float to *ndigits* decimal places.

    FISTA runs 350 iterations of matrix algebra whose low-order bits differ
    across BLAS backends (Windows/AVX2 vs Linux/OpenBLAS vs macOS/Accelerate).
    Rounding to 1e-9 is far above the numerical noise (~1e-12) while still
    well below the Go tolerance tests' 1e-4 threshold, so it produces stable
    JSON on every machine without changing any answer that matters.
    """
    if isinstance(obj, float):
        return round(obj, ndigits)
    if isinstance(obj, dict):
        return {k: _canon(v, ndigits) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_canon(v, ndigits) for v in obj]
    return obj


def write_json(path: Path, obj: dict) -> None:
    with path.open("w", encoding="utf-8", newline="\n") as f:
        json.dump(obj, f, indent=2, sort_keys=True)
        f.write("\n")


def write_recovery_json(path: Path, obj: dict) -> None:
    """Like write_json but canonicalises floats first (BLAS-determinism fix).

    Recovery vectors (recovery_case1.json, recovery_watermark.json) are the
    only files whose floats can differ across BLAS backends; _canon rounds
    them to 1e-9 before serialisation to produce stable JSON.
    """
    write_json(path, _canon(obj))


# --------------------------------------------------------------------------
# key_rotation_vectors.json   (wire v2, ADR-012 §Addendum)
# --------------------------------------------------------------------------

# Two distinct tenant keys used for the rotation test: v0 is the "before"
# key, v1 is the "after" key (active post-rotation).
KEY_V0 = b"rotation-test-key-v0"
KEY_V1 = b"rotation-test-key-v1"


def gen_key_rotation_vectors() -> dict:
    """Two residual frames for the same (shard, epoch, view, window), encoded
    under key_version 0 and key_version 1 respectively.

    The acceptance property (Go E2E test): the decoder holds both keys in its
    KeyRing; frames under either version are routed to their own ViewState,
    each independently recoverable; no keyring_miss is incremented for either.
    """
    shard_id = 42
    epoch_idx = 7
    view_id = 0
    seq = 3
    m = 16
    d = 4
    bits = 8

    # Build two tiny sketches — one per key version.  We inject the same
    # series into both so both views carry the same signal and recovery works.
    name = b"rotation_metric|shard=42|agg=sum"
    signal_value = 5.0
    inv_sqrt_d = 1.0 / math.sqrt(d)

    frames = []
    for kv, tenant_key in [(0, KEY_V0), (1, KEY_V1)]:
        seed = ref.derive_ephemeral_seed(tenant_key, shard_id, epoch_idx, view_id)
        idx, sign = ref.buckets(name, seed, m, d)
        y = [0.0] * m
        for i, bucket in enumerate(idx):
            y[bucket] += sign[i] * signal_value * inv_sqrt_d
        scale = max(abs(v) for v in y) / 127.0 if any(y) else 1e-6
        payload = ref.quantize(y, bits, scale)

        frame = ref.Frame(
            version=ref.VERSION,
            frame_type=ref.FRAME_TYPE_RESIDUAL,
            flags=0,
            bits=bits,
            emitter_id=0xABCD0000 | kv,
            shard_id=shard_id,
            epoch=epoch_idx,
            seq=seq,
            view_id=view_id,
            m=m,
            d=d,
            predictor=ref.PREDICTOR_HOLD,
            key_version=kv,
            codec=0,
            energy=float(np.dot(y, y) ** 0.5),
            quant_scale=scale,
            dict_root=ref.compute_dict_root([ref.series_id(name)]),
            dict_deltas=[],
            snapshot_blob=b"",
            payload=payload,
        )
        frames.append({
            "key_version": kv,
            "tenant_key": tenant_key.decode(),
            "seed": seed,
            "frame_hex": hex_bytes(ref.marshal_frame(frame)),
            "y": y,
            "quant_scale": scale,
        })

    return {
        "description": "two frames same window encoded under key_version 0 and 1; decoder KeyRing holds both",
        "shard_id": shard_id,
        "epoch_idx": epoch_idx,
        "view_id": view_id,
        "seq": seq,
        "m": m,
        "d": d,
        "bits": bits,
        "series_name": name.decode(),
        "series_id": ref.series_id(name),
        "frames": frames,
    }


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, help="output directory (e.g. ../testdata/golden)")
    args = parser.parse_args(argv)

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    steps = [
        ("hash_vectors.json", gen_hash_vectors),
        ("hkdf_vectors.json", gen_hkdf_vectors),
        ("dictroot_vectors.json", gen_dictroot_vectors),
        ("kdelta_sequence.json", gen_kdelta_sequence),
        ("quant_vectors.json", gen_quant_vectors),
        ("key_rotation_vectors.json", gen_key_rotation_vectors),
    ]
    for filename, fn in steps:
        print(f"generating {filename} ...", file=sys.stderr)
        write_json(out / filename, fn())

    print("generating frame_residual.bin ...", file=sys.stderr)
    frame_bytes = ref.marshal_frame(build_frame_residual())
    (out / "frame_residual.bin").write_bytes(frame_bytes)

    print("generating recovery_case1.json (FISTA solve over N=50000) ...", file=sys.stderr)
    write_recovery_json(out / "recovery_case1.json", gen_recovery_case1())

    print("generating recovery_watermark.json (FISTA solve x2) ...", file=sys.stderr)
    write_recovery_json(out / "recovery_watermark.json", gen_recovery_watermark())

    print("done.", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
