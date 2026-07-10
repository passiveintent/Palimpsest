from __future__ import annotations

import numpy as np
from hypothesis import given, settings
from hypothesis import strategies as st

from pconformal.synth import heavy_tail


@given(
    xi=st.floats(min_value=-0.5, max_value=1.0, allow_nan=False),
    sigma=st.floats(min_value=0.1, max_value=20.0, allow_nan=False),
    u=st.floats(min_value=1.0, max_value=100.0, allow_nan=False),
    exceed_prob=st.floats(min_value=0.01, max_value=0.2, allow_nan=False),
    n=st.integers(min_value=200, max_value=500),
    seed=st.integers(min_value=0, max_value=2**32 - 1),
)
@settings(max_examples=25, deadline=None)
def test_heavy_tail_invariants(
    xi: float, sigma: float, u: float, exceed_prob: float, n: int, seed: int
) -> None:
    frame, gt = heavy_tail(seed, xi=xi, sigma=sigma, u=u, n=n, exceed_prob=exceed_prob)

    assert gt.raw.shape == (n,)
    assert gt.is_exceedance.sum() == gt.exceedances.shape[0]
    assert np.all(gt.exceedances >= 0)
    assert np.all(gt.raw[~gt.is_exceedance] < u)
    assert np.all(gt.raw[gt.is_exceedance] >= u)
    assert np.all(np.isfinite(frame.p50))
    assert np.all(frame.p50 <= frame.p95)
    assert np.all(frame.p95 <= frame.p99)


def test_exceedances_match_generating_parameters_directly() -> None:
    _frame, gt = heavy_tail(0, xi=0.3, sigma=2.0, u=10.0, n=5000, exceed_prob=0.1)
    # exceedances = raw - u for exceedance points, so this is exact, not
    # approximate: it's the definition, not an estimate of it.
    assert np.allclose(gt.raw[gt.is_exceedance] - gt.u, gt.exceedances)


def test_rejects_invalid_parameters() -> None:
    for kwargs in ({"sigma": 0.0}, {"exceed_prob": 0.0}, {"exceed_prob": 1.0}, {"n": 1}):
        try:
            heavy_tail(0, **kwargs)
        except ValueError:
            continue
        raise AssertionError(f"expected ValueError for {kwargs}")
