from __future__ import annotations

import numpy as np
from hypothesis import given, settings
from hypothesis import strategies as st

from pconformal.synth import bimodal_fleet


@given(
    hosts=st.integers(min_value=2, max_value=50),
    mix=st.floats(min_value=0.0, max_value=1.0, allow_nan=False),
    n_per_host=st.integers(min_value=10, max_value=100),
    seed=st.integers(min_value=0, max_value=2**32 - 1),
)
@settings(max_examples=25, deadline=None)
def test_bimodal_fleet_invariants(hosts: int, mix: float, n_per_host: int, seed: int) -> None:
    frame, gt = bimodal_fleet(seed, hosts, mix, n_per_host=n_per_host)

    assert frame.p50.shape == (hosts,)
    assert np.all(frame.p50 <= frame.p95)
    assert np.all(frame.p95 <= frame.p99)
    assert gt.raw_by_host.shape == (hosts, n_per_host)
    assert (gt.host_mode == "slow").sum() == round(hosts * mix)
    assert gt.relative_error >= 0.0
    assert gt.naive_avg_p95 > 0.0 and gt.true_pooled_p95 > 0.0


def test_naive_average_diverges_sharply_from_true_pooled_p95_on_default_fixture() -> None:
    # The G8 substrate's whole point: naive per-host averaging should be
    # badly wrong relative to the true pooled quantile.
    _frame, gt = bimodal_fleet(0, hosts=24, mix=0.25)
    assert gt.relative_error >= 0.25


def test_zero_mix_is_a_uniform_fleet() -> None:
    _frame, gt = bimodal_fleet(0, hosts=10, mix=0.0)
    assert np.all(gt.host_mode == "fast")


def test_rejects_invalid_parameters() -> None:
    for args, kwargs in (
        ((0, 1, 0.5), {}),
        ((0, 10, -0.1), {}),
        ((0, 10, 1.5), {}),
        ((0, 10, 0.5), {"n_per_host": 1}),
    ):
        try:
            bimodal_fleet(*args, **kwargs)
        except ValueError:
            continue
        raise AssertionError(f"expected ValueError for {args}, {kwargs}")
