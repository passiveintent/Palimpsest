# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""RollingMAD: the scale-normalizer of the CQR score (ADR-002).

ADR-002 studentizes the nonconformity score by a rolling median absolute
deviation "so that the score is scale-normalized across regimes with
different volatility instead of comparing raw residuals across
heteroscedastic strata." This is that studentizer: a causal (trailing-window)
robust scale estimate with an epsilon floor, so a low-variance regime can't
divide a residual by ~0 and manufacture a spurious violation.

It operates on a residual/score series that is already located near zero
(the CQR score is 0 at the band edge); the MAD is computed about each
window's own median, so it is a pure spread estimate regardless of any
residual location left in the input.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import numpy.typing as npt


@dataclass(frozen=True)
class RollingMAD:
    """Trailing-window median-absolute-deviation studentizer with an epsilon floor."""

    window: int = 64
    epsilon: float = 1e-6

    def __post_init__(self) -> None:
        if self.window < 1:
            raise ValueError("window must be >= 1")
        if self.epsilon <= 0.0:
            raise ValueError("epsilon must be > 0")

    def scale(self, series: npt.NDArray[np.float64]) -> npt.NDArray[np.float64]:
        """Per-index trailing-window MAD, floored at ``epsilon``.

        Index ``i`` uses the causal window ``series[max(0, i-window+1) : i+1]``
        (expanding until the window fills), so the estimate never peeks ahead.
        """
        x = np.asarray(series, dtype=np.float64)
        if x.ndim != 1:
            raise ValueError("series must be 1-D")
        out = np.empty(x.shape[0], dtype=np.float64)
        for i in range(x.shape[0]):
            window = x[max(0, i - self.window + 1) : i + 1]
            mad = float(np.median(np.abs(window - np.median(window))))
            out[i] = max(mad, self.epsilon)
        return out

    def studentize(self, series: npt.NDArray[np.float64]) -> npt.NDArray[np.float64]:
        """Divide each value by its trailing-window MAD scale.

        Studentizing a heteroscedastic residual series by its own rolling MAD
        collapses regimes of differing volatility onto a common scale, which
        is what makes cross-stratum scores comparable (ADR-002).
        """
        x = np.asarray(series, dtype=np.float64)
        return x / self.scale(x)
