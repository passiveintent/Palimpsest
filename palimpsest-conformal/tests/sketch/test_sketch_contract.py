# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Fast (non-gate) contract tests for the sketch layer.

These lock in the behaviours the G6 property tests assume but do not assert
head-on: the empty-sketch degrade-visibly path, the merge-accuracy guard,
the exact weighting semantics, operation purity, and provenance content.
"""

from __future__ import annotations

import numpy as np
import pytest
from ddsketch import DDSketch
from ddsketch.store import DenseStore

from pconformal.sketch import BucketRing, CalibrationProvenance, DDSketchQuantile


def _filled(values: list[float], relative_accuracy: float = 0.01) -> DDSketchQuantile:
    sketch = DDSketchQuantile(relative_accuracy)
    for value in values:
        sketch.add(value)
    return sketch


def test_ddsketch_internal_layout_scaled_depends_on() -> None:
    # `DDSketchQuantile.scaled` reaches into DDSketch internals to multiply
    # counts (there is no public scaling API). The dep is pinned <4, but this
    # is the contract that makes a breaking internal change fail LOUDLY here
    # rather than silently corrupt weighted merges. If this test breaks after
    # a ddsketch bump, re-audit `_scale_counts`, do not just update the test.
    sketch = _filled([1.0, -1.0, 0.0, 5.0])
    inner = sketch._sketch
    assert isinstance(inner, DDSketch)
    assert isinstance(inner._store, DenseStore)
    assert isinstance(inner._negative_store, DenseStore)
    assert isinstance(inner._store.bins, list)
    for attr in ("count",):
        assert isinstance(getattr(inner._store, attr), (int, float))
    for attr in ("_zero_count", "_count", "_sum"):
        assert isinstance(getattr(inner, attr), (int, float))
    # behavioural contract: scaling multiplies the total count exactly.
    assert sketch.scaled(3.0)._sketch.count == pytest.approx(inner.count * 3.0)


def test_ring_capacity_must_cover_calibration_and_audit_horizons() -> None:
    # Retention contract (ADR-004): eviction must never drop calibration/audit
    # data. Under-sizing the ring fails loudly at construction, not silently.
    with pytest.raises(ValueError, match="calibration_horizon"):
        BucketRing(capacity=5, calibration_horizon=4, audit_horizon=2)
    ring = BucketRing(capacity=6, calibration_horizon=4, audit_horizon=2)  # exactly covers
    assert ring.capacity == 6
    assert len(BucketRing(capacity=3)) == 0  # horizons default to 0: contract inert


def test_empty_sketch_quantile_degrades_visibly() -> None:
    with pytest.raises(ValueError, match="empty sketch"):
        DDSketchQuantile().quantile(0.5)


def test_merge_rejects_mismatched_relative_accuracy() -> None:
    with pytest.raises(ValueError, match="relative_accuracy"):
        _filled([1.0], 0.01).merge(_filled([2.0], 0.02))


def test_merge_and_scaled_are_pure() -> None:
    a, b = _filled([1.0, 2.0, 3.0]), _filled([4.0, 5.0])
    _ = a.merge(b)
    _ = a.scaled(3.0)
    assert (a.n(), b.n()) == (3, 2)


def test_scaled_by_uniform_weight_preserves_quantiles() -> None:
    sketch = _filled([float(v) for v in range(-50, 50)])
    scaled = sketch.scaled(7.5)
    for q in (0.05, 0.5, 0.95):
        assert scaled.quantile(q) == sketch.quantile(q)


def test_weight_zero_bucket_is_dropped_from_the_merge() -> None:
    ring = BucketRing(capacity=2, decay=1.0)
    ring.open_bucket()
    ring.extend([1.0, 2.0, 3.0])
    ring.open_bucket()
    ring.extend([100.0, 200.0, 300.0])

    only_first = ring.merged_quantile(0.5, np.array([1.0, 0.0]))
    assert only_first == _filled([1.0, 2.0, 3.0]).quantile(0.5)


def test_merged_quantile_with_no_contributing_data_raises() -> None:
    ring = BucketRing(capacity=2)
    ring.open_bucket()
    ring.extend([1.0, 2.0])
    with pytest.raises(ValueError, match="no calibration data"):
        ring.merged_quantile(0.5, np.array([0.0]))


def test_base_weights_decay_newest_heaviest_and_capacity_evicts() -> None:
    ring = BucketRing(capacity=3, decay=0.5)
    ids = [ring.open_bucket() for _ in range(5)]  # 5 opened, capacity 3

    assert ring.bucket_ids() == tuple(ids[2:])  # oldest two evicted, newest retained
    weights = ring.base_weights()
    assert weights.tolist() == [0.25, 0.5, 1.0]  # decay**2, decay**1, decay**0


def test_boost_and_reweight_do_not_mutate_input_weights() -> None:
    ring = BucketRing(capacity=3, decay=0.9)
    ids = []
    for _ in range(3):
        ids.append(ring.open_bucket())
        ring.add(1.0)

    weights = ring.base_weights()
    snapshot = weights.copy()

    boosted = ring.boost([ids[0]], 4.0, weights)
    masked = ring.reweight(np.array([True, False, True]), weights)

    assert np.array_equal(weights, snapshot)  # inputs untouched
    assert boosted[0] == snapshot[0] * 4.0
    assert masked[1] == 0.0


def test_provenance_reports_buckets_weights_and_effective_n() -> None:
    ring = BucketRing(capacity=2, decay=1.0)
    a = ring.open_bucket()
    ring.extend([1.0, 2.0])
    b = ring.open_bucket()
    ring.extend([3.0, 4.0, 5.0])

    prov = ring.provenance(np.array([2.0, 1.0]))
    assert prov.bucket_ids == (a, b)
    assert prov.weights == (2.0, 1.0)
    assert prov.effective_n == 2.0 * 2 + 1.0 * 3
    assert prov.tier is CalibrationProvenance.BUCKET_LOCAL
