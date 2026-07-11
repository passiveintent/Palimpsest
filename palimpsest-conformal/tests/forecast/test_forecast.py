# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Non-gate contract tests for the forecast layer (ADR-002).

These are the falsifiable claims ADR-002 asks for outside the named gates:
Holt-Winters recovers the seasonal profile of `synth.seasonal_latency`; the
`ConstantForecaster` control is deliberately bad on it; and RollingMAD
studentization makes two heteroscedastic services comparable. (G2 itself —
equal coverage, >= 20% width reduction — lives in tests/forecast and stays
blocked until score + calibration land.)
"""

from __future__ import annotations

import numpy as np
import numpy.typing as npt
import pytest

from pconformal.forecast import (
    ConstantForecaster,
    Forecaster,
    HoltWintersForecaster,
    RollingMAD,
)
from pconformal.synth import Frame, SeasonalGroundTruth, heteroscedastic_pair, seasonal_latency

_PERIOD = 168


def _band_midpoints(f: Forecaster, ts: range) -> npt.NDArray[np.float64]:
    return np.array([sum(f.predict_band(t)) / 2.0 for t in ts], dtype=np.float64)


@pytest.fixture(scope="module")
def seasonal() -> tuple[Frame, SeasonalGroundTruth, HoltWintersForecaster]:
    frame, gt = seasonal_latency(0, weeks=6, noise=0.05)
    hw = HoltWintersForecaster(seasonal_periods=_PERIOD)
    hw.fit(frame.p50)
    return frame, gt, hw


def test_forecasters_satisfy_the_protocol() -> None:
    assert isinstance(HoltWintersForecaster(), Forecaster)
    assert isinstance(ConstantForecaster(), Forecaster)


def test_holt_winters_recovers_the_seasonal_profile(
    seasonal: tuple[Frame, SeasonalGroundTruth, HoltWintersForecaster],
) -> None:
    _frame, gt, hw = seasonal
    ts = range(_PERIOD, gt.true_p50.shape[0])  # skip the first-season warmup
    mids = _band_midpoints(hw, ts)
    truth = gt.true_p50[_PERIOD:]

    corr = float(np.corrcoef(mids, truth)[0, 1])
    median_rel_err = float(np.median(np.abs(mids - truth) / truth))

    assert corr > 0.95, f"seasonal shape not recovered (corr={corr:.3f})"
    assert median_rel_err < 0.05, f"seasonal level not recovered (rel_err={median_rel_err:.3f})"
    # a real forecaster tracks the seasonal swing: the band moves across t.
    assert mids.std() > 0.05 * truth.mean()


def test_constant_forecaster_is_deliberately_bad_on_seasonal(
    seasonal: tuple[Frame, SeasonalGroundTruth, HoltWintersForecaster],
) -> None:
    frame, gt, hw = seasonal
    const = ConstantForecaster()
    const.fit(frame.p50)

    ts = range(_PERIOD, gt.true_p50.shape[0])
    truth = gt.true_p50[_PERIOD:]
    const_mids = _band_midpoints(const, ts)
    hw_mids = _band_midpoints(hw, ts)

    const_err = float(np.median(np.abs(const_mids - truth) / truth))
    hw_err = float(np.median(np.abs(hw_mids - truth) / truth))

    # zero-skill: the band never changes with t...
    assert const_mids.std() == 0.0
    # ...and it cannot track the seasonal swing, so it is much worse than HW.
    assert const_err > 0.12, f"control unexpectedly good (rel_err={const_err:.3f})"
    assert hw_err < 0.4 * const_err, f"HW ({hw_err:.3f}) did not dominate control ({const_err:.3f})"


def test_predict_band_is_ordered_in_and_out_of_sample(
    seasonal: tuple[Frame, SeasonalGroundTruth, HoltWintersForecaster],
) -> None:
    frame, _gt, hw = seasonal
    n = frame.p50.shape[0]
    for t in (0, 1, _PERIOD, n - 1, n, n + 5):
        q_lo, q_hi = hw.predict_band(t)
        assert np.isfinite(q_lo) and np.isfinite(q_hi)
        assert q_lo <= q_hi


def test_holt_winters_refuses_a_series_shorter_than_two_seasons() -> None:
    hw = HoltWintersForecaster(seasonal_periods=_PERIOD)
    with pytest.raises(ValueError, match="seasonality"):
        hw.fit(np.ones(2 * _PERIOD - 1, dtype=np.float64))


def test_rolling_mad_makes_heteroscedastic_services_comparable() -> None:
    frame, gt = heteroscedastic_pair(7, n=500)
    resid_a = frame.p50[frame.entity == "service_a"] - gt.base
    resid_b = frame.p50[frame.entity == "service_b"] - gt.base

    mad = RollingMAD(window=64)
    stud_a = mad.studentize(resid_a)
    stud_b = mad.studentize(resid_b)

    warmup = 64  # let the trailing window fill before measuring scale
    raw_ratio = float(np.std(resid_b[warmup:]) / np.std(resid_a[warmup:]))
    stud_ratio = float(np.std(stud_b[warmup:]) / np.std(stud_a[warmup:]))

    # raw residuals live on very different scales (~sqrt(variance_ratio))...
    assert raw_ratio > 2.5, f"fixture not heteroscedastic enough (raw_ratio={raw_ratio:.2f})"
    # ...but studentized scores overlap: their scales are within ~20%.
    assert 0.8 < stud_ratio < 1.25, f"studentization did not equalize (ratio={stud_ratio:.2f})"
    # and both studentized series are centered near zero (distributions overlap).
    assert abs(float(np.median(stud_a[warmup:]))) < 0.3
    assert abs(float(np.median(stud_b[warmup:]))) < 0.3


def test_rolling_mad_epsilon_floor_on_a_constant_series() -> None:
    mad = RollingMAD(window=16, epsilon=1e-3)
    scale = mad.scale(np.full(50, 42.0, dtype=np.float64))
    assert np.all(scale == 1e-3)  # zero deviation -> floored, never zero
