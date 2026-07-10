from __future__ import annotations

import numpy as np
from hypothesis import given, settings
from hypothesis import strategies as st

from pconformal.synth import seasonal_latency
from pconformal.synth.seasonal import default_hour_of_week_profile


def test_default_profile_shape_and_positivity() -> None:
    profile = default_hour_of_week_profile()
    assert profile.shape == (168,)
    assert np.all(profile > 0)


def test_quantile_ordering_and_positivity() -> None:
    frame, gt = seasonal_latency(0, weeks=4, noise=0.1)
    assert np.all(frame.p50 <= frame.p95)
    assert np.all(frame.p95 <= frame.p99)
    assert np.all(frame.p50 > 0)
    assert np.all(gt.true_p50 <= gt.true_p95)
    assert np.all(gt.true_p95 <= gt.true_p99)


@given(
    weeks=st.integers(min_value=1, max_value=10),
    noise=st.floats(min_value=0.0, max_value=0.3, allow_nan=False),
    sigma=st.floats(min_value=0.05, max_value=0.5, allow_nan=False),
    base=st.floats(min_value=1.0, max_value=1000.0, allow_nan=False),
    seed=st.integers(min_value=0, max_value=2**32 - 1),
)
@settings(max_examples=25, deadline=None)
def test_seasonal_latency_invariants_hold_across_parameter_range(
    weeks: int, noise: float, sigma: float, base: float, seed: int
) -> None:
    frame, gt = seasonal_latency(seed, weeks=weeks, noise=noise, sigma=sigma, base=base)

    n = weeks * 168
    assert frame.t.shape == (n,)
    assert frame.p50.shape == frame.p95.shape == frame.p99.shape == (n,)
    assert np.all(frame.p50 <= frame.p95)
    assert np.all(frame.p95 <= frame.p99)
    assert np.all(np.isfinite(frame.p50))
    assert gt.sigma == sigma
    assert np.all(gt.true_p50 <= gt.true_p95)
    assert np.all(gt.true_p95 <= gt.true_p99)


def test_rejects_invalid_parameters() -> None:
    for kwargs in ({"weeks": 0}, {"noise": -0.1}, {"sigma": 0.0}, {"base": 0.0}):
        try:
            seasonal_latency(0, **kwargs)
        except ValueError:
            continue
        raise AssertionError(f"expected ValueError for {kwargs}")


def test_rejects_wrong_shaped_profile() -> None:
    try:
        seasonal_latency(0, hour_of_week_profile=np.ones(10))
    except ValueError:
        return
    raise AssertionError("expected ValueError for wrong-shaped profile")
