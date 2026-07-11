# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""HoltWintersForecaster: the production forecaster of ADR-002.

A Holt-Winters-class model (statsmodels `ExponentialSmoothing`, additive
seasonal, weekly period on window stats) forecasts the seasonal mean of the
monitored statistic. The nominal band is that point forecast plus the
in-sample residual quantiles at `nominal_alpha`: because the model has
already absorbed the hour-of-week seasonality, its residuals are small, so
its nominal band is tight. That tightness is the whole point of G2 — the
zero-skill `ConstantForecaster` leaves the seasonal swing in its residuals
and pays for it in width.
"""

from __future__ import annotations

import numpy as np
import numpy.typing as npt
from statsmodels.tsa.holtwinters import ExponentialSmoothing

_HOURS_PER_WEEK = 168


class HoltWintersForecaster:
    """Additive-seasonal exponential smoothing behind the `Forecaster` protocol."""

    def __init__(
        self,
        *,
        seasonal_periods: int = _HOURS_PER_WEEK,
        nominal_alpha: float = 0.1,
    ) -> None:
        if seasonal_periods < 2:
            raise ValueError("seasonal_periods must be >= 2")
        if not 0.0 < nominal_alpha < 1.0:
            raise ValueError("nominal_alpha must be in (0, 1)")
        self._seasonal_periods = seasonal_periods
        self._nominal_alpha = nominal_alpha
        self._n = 0
        self._fitted: npt.NDArray[np.float64] = np.empty(0, dtype=np.float64)
        self._resid_lo = 0.0
        self._resid_hi = 0.0
        self._forecast_cache: npt.NDArray[np.float64] = np.empty(0, dtype=np.float64)

    def fit(self, series: npt.NDArray[np.float64]) -> None:
        y = np.asarray(series, dtype=np.float64)
        if y.ndim != 1:
            raise ValueError("series must be 1-D")
        if y.shape[0] < 2 * self._seasonal_periods:
            # statsmodels needs >= 2 full seasons to estimate a seasonal
            # component; refuse loudly rather than return a degenerate fit.
            raise ValueError(
                f"need >= {2 * self._seasonal_periods} points to fit "
                f"{self._seasonal_periods}-period seasonality, got {y.shape[0]}"
            )
        self._n = y.shape[0]
        results = ExponentialSmoothing(
            y,
            trend=None,
            seasonal="add",
            seasonal_periods=self._seasonal_periods,
            initialization_method="estimated",
        ).fit()
        self._results = results
        self._fitted = np.asarray(results.fittedvalues, dtype=np.float64)
        residuals = y - self._fitted
        self._resid_lo = float(np.quantile(residuals, self._nominal_alpha / 2.0))
        self._resid_hi = float(np.quantile(residuals, 1.0 - self._nominal_alpha / 2.0))
        self._forecast_cache = np.empty(0, dtype=np.float64)

    def predict_band(self, t: int) -> tuple[float, float]:
        mu = self._point_forecast(t)
        return (mu + self._resid_lo, mu + self._resid_hi)

    def _point_forecast(self, t: int) -> float:
        if self._n == 0:
            raise RuntimeError("forecaster is not fitted")
        if t < 0:
            raise ValueError("t must be >= 0")
        if t < self._n:
            return float(self._fitted[t])
        horizon = t - self._n + 1
        if horizon > self._forecast_cache.shape[0]:
            self._forecast_cache = np.asarray(self._results.forecast(horizon), dtype=np.float64)
        return float(self._forecast_cache[horizon - 1])
