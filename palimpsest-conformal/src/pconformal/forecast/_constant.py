# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""ConstantForecaster: the zero-skill control G2 is defined against (ADR-002).

This is not a toy — it is the load-bearing null model. It predicts the
*training median band* (the marginal `[alpha/2, 1-alpha/2]` quantiles of the
training series) for every window, ignoring `t` entirely. Because it cannot
track hour-of-week seasonality, its nominal band must swallow the whole
seasonal swing to stay calibrated, which is exactly why G2 expects
Holt-Winters to beat it on width at equal coverage. If the conformal
guarantee did *not* hold for this forecaster, the guarantee would be a
property of the forecaster, not of the conformal wrapper — that is the bug
G2 exists to catch.
"""

from __future__ import annotations

import numpy as np
import numpy.typing as npt


class ConstantForecaster:
    """Predicts a fixed, seasonality-blind band behind the `Forecaster` protocol."""

    def __init__(self, *, nominal_alpha: float = 0.1) -> None:
        if not 0.0 < nominal_alpha < 1.0:
            raise ValueError("nominal_alpha must be in (0, 1)")
        self._nominal_alpha = nominal_alpha
        self._fitted = False
        self._median = 0.0
        self._q_lo = 0.0
        self._q_hi = 0.0

    def fit(self, series: npt.NDArray[np.float64]) -> None:
        y = np.asarray(series, dtype=np.float64)
        if y.ndim != 1:
            raise ValueError("series must be 1-D")
        if y.shape[0] < 1:
            raise ValueError("series must be non-empty")
        self._median = float(np.median(y))
        self._q_lo = float(np.quantile(y, self._nominal_alpha / 2.0))
        self._q_hi = float(np.quantile(y, 1.0 - self._nominal_alpha / 2.0))
        self._fitted = True

    def predict_band(self, t: int) -> tuple[float, float]:
        if not self._fitted:
            raise RuntimeError("forecaster is not fitted")
        # The band is independent of t: that seasonality-blindness is the
        # control, not an oversight.
        return (self._q_lo, self._q_hi)

    @property
    def median(self) -> float:
        """The training median the band is centered on (for introspection/tests)."""
        return self._median
