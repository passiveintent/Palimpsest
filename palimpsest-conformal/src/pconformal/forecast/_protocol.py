# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""The Forecaster protocol — the swap-a-model boundary (ADR-002).

ADR-002 puts a Holt-Winters-class model behind a `Forecaster` protocol so
that "the conformal core only ever depends on the protocol's predicted
quantiles, never on the concrete forecaster implementation." Swapping the
forecaster (Holt-Winters today, a customer's own model under the future
bring-your-own-model line tomorrow) must never touch the score, calibration,
or drift code. This protocol is that boundary, and G2 falsifies it: the
coverage guarantee must survive replacing the forecaster with a zero-skill
`ConstantForecaster`.

A forecaster predicts, for a window, a nominal quantile band `(q_lo, q_hi)`
around the monitored window summary statistic. It makes no coverage
promise — conformal calibration (ADR-003/004) inflates or deflates this band
to hit the target. Once coverage is fixed there, band *width* is the score,
so a skilled forecaster earns its complexity by predicting a tighter nominal
band than the zero-skill control.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable

import numpy as np
import numpy.typing as npt


@runtime_checkable
class Forecaster(Protocol):
    """Predicts a nominal quantile band for a monitored window statistic."""

    def fit(self, series: npt.NDArray[np.float64]) -> None:
        """Fit to a 1-D history of the window statistic (e.g. per-window p95)."""
        ...

    def predict_band(self, t: int) -> tuple[float, float]:
        """Return the predicted ``(q_lo, q_hi)`` band for window index ``t``.

        ``q_lo <= q_hi``. ``t`` is a window index (ADR-013: an index, not a
        counter); indices within the fitted range read the in-sample fit,
        indices beyond it are forecast forward.
        """
        ...
