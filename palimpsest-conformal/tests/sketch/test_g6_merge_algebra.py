# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G6: merge algebra. See docs/GATES.md#g6--merge-algebra.

Three `hypothesis` property tests, one per clause of the gate:

1. Sketch merge is associative and commutative within the sketch's
   rank-error tolerance (differently-ordered merges of the same data agree).
2. Ring-buffer weight ops are reversible: `boost` inverts exactly on the
   weight vector, and zeroing then annealing back restores the query.
3. Masking a bucket equals giving it weight zero, and the bucket's data
   survives the operation (invariant 5 — recalibration never deletes).
"""

from __future__ import annotations

import numpy as np
import pytest
from hypothesis import given, settings
from hypothesis import strategies as st

from pconformal.sketch import BucketRing, CalibrationProvenance, DDSketchQuantile

_REL_ACC = 0.01
_VALUES = st.floats(min_value=-1e6, max_value=1e6, allow_nan=False, allow_infinity=False)
_VALUE_LISTS = st.lists(_VALUES, min_size=1, max_size=200)
_QUANTILES = (0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99)


def _sketch(values: list[float]) -> DDSketchQuantile:
    sketch = DDSketchQuantile(_REL_ACC)
    for value in values:
        sketch.add(value)
    return sketch


def _agreement_tol(*estimates: float) -> float:
    # Two estimates of the same true quantile each sit within `relative
    # accuracy` of it, so they agree within 2 * rel_acc of each other; the
    # floor absorbs floating error near zero.
    return 2 * _REL_ACC * max(abs(v) for v in estimates) + 1e-9


@pytest.mark.gate
@given(a=_VALUE_LISTS, b=_VALUE_LISTS, c=_VALUE_LISTS)
@settings(max_examples=200, deadline=None)
def test_g6_sketch_merge_is_associative_and_commutative_within_tolerance(
    a: list[float], b: list[float], c: list[float]
) -> None:
    sa, sb, sc = _sketch(a), _sketch(b), _sketch(c)

    left = sa.merge(sb).merge(sc)  # (A . B) . C
    right = sa.merge(sb.merge(sc))  # A . (B . C)   -- associativity
    swapped = sb.merge(sa).merge(sc)  # (B . A) . C  -- commutativity

    # merge is pure: no operand was mutated by being merged.
    assert (sa.n(), sb.n(), sc.n()) == (len(a), len(b), len(c))

    for q in _QUANTILES:
        v_left, v_right, v_swapped = left.quantile(q), right.quantile(q), swapped.quantile(q)
        tol = _agreement_tol(v_left, v_right, v_swapped)
        assert abs(v_left - v_right) <= tol, f"associativity broke at q={q}"
        assert abs(v_left - v_swapped) <= tol, f"commutativity broke at q={q}"


@st.composite
def _rings(draw: st.DrawFn) -> BucketRing:
    n_buckets = draw(st.integers(min_value=1, max_value=6))
    decay = draw(st.floats(min_value=0.5, max_value=1.0))
    ring = BucketRing(capacity=n_buckets, decay=decay, relative_accuracy=_REL_ACC)
    for _ in range(n_buckets):
        ring.open_bucket()
        ring.extend(draw(st.lists(_VALUES, min_size=1, max_size=50)))
    return ring


@pytest.mark.gate
@given(
    ring=_rings(),
    factor=st.floats(min_value=0.1, max_value=10.0),
    q=st.sampled_from(_QUANTILES),
    seed=st.integers(min_value=0, max_value=2**31 - 1),
)
@settings(max_examples=150, deadline=None)
def test_g6_ring_buffer_weight_ops_are_reversible(
    ring: BucketRing, factor: float, q: float, seed: int
) -> None:
    ids = list(ring.bucket_ids())
    rng = np.random.default_rng(seed)
    subset = [bid for bid in ids if rng.random() < 0.5] or ids[:1]
    keep = np.array([rng.random() < 0.5 for _ in ids], dtype=bool)
    keep[-1] = True  # keep the newest bucket so the query stays defined

    base = ring.base_weights()
    snapshot = base.copy()  # the pre-image the recalibration sequence restores to
    n_before = {bid: ring.sketch_at(bid).n() for bid in ids}
    original = ring.merged_quantile(q, base)

    # Weight ops are PURE: each returns a NEW vector and mutates neither its
    # input nor the ring. So a recalibration sequence (ADR-004: zero out
    # pre-changepoint buckets, then boost) leaves the pre-image bit-identical,
    # and its undo is a SNAPSHOT RESTORE, never an algebraic inverse (which is
    # only float-close, and DDSketch's step-function quantile would flip a
    # bucket under 1-ulp weight drift). Reversibility within tolerance would
    # not be the invariant.
    masked = ring.reweight(keep, base)
    boosted = ring.boost(subset, factor, masked)
    assert masked is not base and boosted is not masked  # new vectors, not aliases
    assert np.array_equal(base, snapshot)  # the ops never touched the input

    # Anneal back == restore the snapshot == recompute steady-state weights,
    # all three bit-identical. The query is therefore restored EXACTLY.
    restored = ring.base_weights()
    assert np.array_equal(restored, snapshot)
    assert ring.merged_quantile(q, restored) == original

    # no calibration data was deleted at any point (invariant 5).
    assert {bid: ring.sketch_at(bid).n() for bid in ids} == n_before


@pytest.mark.gate
@given(
    ring=_rings(),
    q=st.sampled_from(_QUANTILES),
    seed=st.integers(min_value=0, max_value=2**31 - 1),
)
@settings(max_examples=150, deadline=None)
def test_g6_masking_a_bucket_equals_weight_zero_and_data_survives(
    ring: BucketRing, q: float, seed: int
) -> None:
    ids = list(ring.bucket_ids())
    rng = np.random.default_rng(seed)
    base = ring.base_weights()
    n_before = {bid: ring.sketch_at(bid).n() for bid in ids}

    # mask out a random subset, always keeping the newest bucket.
    keep = np.array([True] * len(ids))
    for i in range(len(ids) - 1):
        keep[i] = rng.random() < 0.5

    masked = ring.reweight(keep, base)
    manual_zero = base.copy()
    manual_zero[~keep] = 0.0

    # masking is exactly "set those buckets to weight zero".
    assert np.array_equal(masked, manual_zero)
    assert ring.merged_quantile(q, masked) == ring.merged_quantile(q, manual_zero)

    # data survives (invariant 5): every bucket still holds its observations.
    assert {bid: ring.sketch_at(bid).n() for bid in ids} == n_before

    # provenance reports only the surviving buckets, tagged bucket-local.
    prov = ring.provenance(masked)
    kept = {bid for bid, k in zip(ids, keep, strict=True) if k}
    assert set(prov.bucket_ids) == kept
    assert prov.tier is CalibrationProvenance.BUCKET_LOCAL
