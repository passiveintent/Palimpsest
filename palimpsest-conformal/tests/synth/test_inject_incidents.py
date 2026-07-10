# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

from __future__ import annotations

import numpy as np
from hypothesis import given, settings
from hypothesis import strategies as st

from pconformal.synth import inject_incidents, seasonal_latency


@given(
    k=st.integers(min_value=1, max_value=5),
    incident_len=st.integers(min_value=1, max_value=8),
    magnitude=st.floats(min_value=1.1, max_value=10.0, allow_nan=False),
    seed=st.integers(min_value=0, max_value=2**32 - 1),
)
@settings(max_examples=25, deadline=None)
def test_inject_incidents_invariants(
    k: int, incident_len: int, magnitude: float, seed: int
) -> None:
    base, _base_gt = seasonal_latency(0, weeks=4)
    n = base.p50.shape[0]
    if k * incident_len * 2 > n:
        # keep enough room for k non-overlapping blocks with headroom
        return

    frame, gt = inject_incidents(base, seed, k, incident_len=incident_len, magnitude=magnitude)

    assert gt.mask.sum() == k * incident_len
    assert gt.starts.shape == (k,)
    assert np.all(frame.p50[gt.mask] == base.p50[gt.mask] * magnitude)
    assert np.all(frame.p50[~gt.mask] == base.p50[~gt.mask])

    # Non-overlapping: no two incidents share a window.
    intervals = sorted((s, s + incident_len) for s in gt.starts)
    for (_, end), (next_start, _) in zip(intervals, intervals[1:], strict=False):
        assert end <= next_start


def test_does_not_mutate_input_series() -> None:
    base, _ = seasonal_latency(0, weeks=2)
    original = base.p50.copy()
    inject_incidents(base, seed=0, k=2)
    assert np.array_equal(base.p50, original)


def test_rejects_invalid_parameters() -> None:
    base, _ = seasonal_latency(0, weeks=1)
    n = base.p50.shape[0]

    try:
        inject_incidents(base, seed=0, k=0)
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError for k=0")

    try:
        inject_incidents(base, seed=0, k=n + 1, incident_len=1)
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError for k * incident_len > n")
