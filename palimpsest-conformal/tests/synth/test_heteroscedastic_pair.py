# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

from __future__ import annotations

import numpy as np
from hypothesis import given, settings
from hypothesis import strategies as st

from pconformal.synth import heteroscedastic_pair


@given(
    n=st.integers(min_value=10, max_value=200),
    sigma_a=st.floats(min_value=0.1, max_value=10.0, allow_nan=False),
    variance_ratio=st.floats(min_value=1.0, max_value=50.0, allow_nan=False),
    seed=st.integers(min_value=0, max_value=2**32 - 1),
)
@settings(max_examples=25, deadline=None)
def test_heteroscedastic_pair_invariants(
    n: int, sigma_a: float, variance_ratio: float, seed: int
) -> None:
    frame, gt = heteroscedastic_pair(seed, n=n, sigma_a=sigma_a, variance_ratio=variance_ratio)

    assert frame.entity is not None
    assert frame.p50.shape == (2 * n,)
    assert np.all(frame.p50 <= frame.p95)
    assert np.all(frame.p95 <= frame.p99)
    assert np.isclose(gt.sigma_b, sigma_a * np.sqrt(variance_ratio))
    assert set(np.unique(frame.entity)) == {"service_a", "service_b"}


def test_service_b_is_the_noisier_one_by_default() -> None:
    _, gt = heteroscedastic_pair(0)
    assert gt.sigma_b > gt.sigma_a
    assert np.isclose(gt.variance_ratio, (gt.sigma_b / gt.sigma_a) ** 2)


def test_rejects_invalid_parameters() -> None:
    for kwargs in ({"n": 0}, {"sigma_a": 0.0}, {"variance_ratio": 0.0}):
        try:
            heteroscedastic_pair(0, **kwargs)
        except ValueError:
            continue
        raise AssertionError(f"expected ValueError for {kwargs}")
