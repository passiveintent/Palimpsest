# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""The v1 nonconformity score — studentized CQR (ADR-002).

`score = max(q_lo - y, y - q_hi) / max(mad, eps)`: the standard conformalized
quantile-regression nonconformity of the observed window statistic `y`
against the forecaster's band `(q_lo, q_hi)` (ADR-002's `Forecaster`),
studentized by a rolling MAD so scores are comparable across strata of
different volatility.

Two properties this shape buys, both falsified in tests:

- **Sign convention.** Inside the band (`q_lo < y < q_hi`) both terms are
  negative, so the score is negative; it is 0 at an edge and positive only
  on a violation. Downstream layers read "> 0" as "outside the band".
- **Affine invariance.** Under `y -> c*y + b` (`c > 0`) with the band and MAD
  carried along, numerator and denominator both scale by `c` and the shift
  cancels in every difference, so the score is unchanged — the scoring path
  has no hidden dependence on the raw units of the metric.

Pure and fully vectorized (numpy only, no pandas): the single scoring path
ADR-002 requires backtest and the live product to share.
"""

from __future__ import annotations

import numpy as np
import numpy.typing as npt

_DEFAULT_EPS = 1e-6


def score(
    y: npt.ArrayLike,
    q_lo: npt.ArrayLike,
    q_hi: npt.ArrayLike,
    mad: npt.ArrayLike,
    *,
    eps: float = _DEFAULT_EPS,
) -> npt.NDArray[np.float64]:
    """Studentized CQR nonconformity score; broadcasts over its inputs.

    ``eps`` floors the MAD denominator so a near-zero-volatility regime can't
    divide by ~0 and manufacture an enormous score (ADR-002).
    """
    if eps <= 0.0:
        raise ValueError("eps must be > 0")
    y_a = np.asarray(y, dtype=np.float64)
    q_lo_a = np.asarray(q_lo, dtype=np.float64)
    q_hi_a = np.asarray(q_hi, dtype=np.float64)
    mad_a = np.asarray(mad, dtype=np.float64)
    numerator = np.maximum(q_lo_a - y_a, y_a - q_hi_a)
    return np.asarray(numerator / np.maximum(mad_a, eps), dtype=np.float64)
