from __future__ import annotations

import numpy as np
from hypothesis import given, settings
from hypothesis import strategies as st

from pconformal.synth import slow_burn


@given(
    rate=st.floats(min_value=0.0, max_value=1.0, allow_nan=False),
    n=st.integers(min_value=10, max_value=300),
    n_features=st.integers(min_value=2, max_value=5),
    seed=st.integers(min_value=0, max_value=2**32 - 1),
)
@settings(max_examples=25, deadline=None)
def test_slow_burn_invariants(rate: float, n: int, n_features: int, seed: int) -> None:
    frame, gt = slow_burn(seed, rate, n=n, n_features=n_features)

    assert frame.input_features is not None
    assert frame.input_features.shape == (n, n_features)
    # Stationary inputs: every row's true mix is identical.
    assert np.allclose(gt.true_feature_mix, gt.true_feature_mix[0])
    # Monotonically non-decreasing drift (rate >= 0).
    assert np.all(np.diff(gt.true_score_mean) >= -1e-12)
    assert np.isclose(gt.true_score_mean[0], 0.0)
    assert np.isclose(gt.true_score_mean[-1], rate * (n - 1))


def test_zero_rate_is_stationary() -> None:
    _frame, gt = slow_burn(0, rate=0.0, n=100)
    assert np.all(gt.true_score_mean == 0.0)


def test_rejects_negative_rate() -> None:
    try:
        slow_burn(0, -0.1)
    except ValueError:
        return
    raise AssertionError("expected ValueError for negative rate")


def test_rejects_invalid_parameters() -> None:
    for kwargs in ({"n": 0}, {"n_features": 1}):
        try:
            slow_burn(0, 0.01, **kwargs)
        except ValueError:
            continue
        raise AssertionError(f"expected ValueError for {kwargs}")
