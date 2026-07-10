# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""heavy_tail: body + GPD exceedances with known (xi, sigma) — the G5 substrate.

By the Pickands-Balkema-de Haan theorem, exceedances over a high threshold
`u` are approximately GPD(xi, sigma) regardless of the parent distribution.
This generator builds that setup directly and exactly rather than relying
on the asymptotic approximation: every point above `u` *is* `u + GPD(xi,
sigma)` by construction, so G5's "recovers (xi, sigma) within tolerance"
claim has an exact target to check against.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import numpy.typing as npt

from pconformal.synth._frame import Frame


def _gpd_ppf(p: npt.NDArray[np.float64], xi: float, sigma: float) -> npt.NDArray[np.float64]:
    """Quantile function of GPD(xi, sigma), loc=0, via direct inversion.

    Implemented by hand (rather than via scipy.stats.genpareto.rvs) so
    determinism depends only on `numpy.random.default_rng`, never on a
    second library's RNG-consumption behavior.
    """
    if abs(xi) < 1e-12:
        return -sigma * np.log1p(-p)
    return (sigma / xi) * (np.power(1.0 - p, -xi) - 1.0)


@dataclass(frozen=True)
class HeavyTailGroundTruth:
    xi: float
    sigma: float
    u: float
    raw: npt.NDArray[np.float64]
    is_exceedance: npt.NDArray[np.bool_]
    exceedances: npt.NDArray[np.float64]


def heavy_tail(
    seed: int,
    *,
    xi: float = 0.2,
    sigma: float = 1.0,
    u: float = 10.0,
    n: int = 2000,
    exceed_prob: float = 0.05,
    window_size: int = 100,
) -> tuple[Frame, HeavyTailGroundTruth]:
    """`n` raw points: a bounded body below `u`, and `GPD(xi, sigma)` above it.

    `frame.p50/p95/p99` are ordinary empirical quantiles over non-overlapping
    blocks of `window_size` raw points (what a real windowed telemetry
    aggregation would report); the generator's real payload for G5 is in
    `ground_truth` — the raw stream, the exceedance mask, and the exact
    generating `(xi, sigma, u)`.
    """
    if sigma <= 0:
        raise ValueError("sigma must be > 0")
    if not 0 < exceed_prob < 1:
        raise ValueError("exceed_prob must be in (0, 1)")
    if n < 10:
        raise ValueError("n must be >= 10")
    if not 1 <= window_size <= n:
        raise ValueError("window_size must be in [1, n]")

    rng = np.random.default_rng(seed)
    n_exceed = max(1, round(n * exceed_prob))
    n_exceed = min(n_exceed, n - 1)
    n_body = n - n_exceed

    # Body: strictly in (0, u) by construction (Beta(2, 5) scaled).
    body = u * rng.beta(2.0, 5.0, size=n_body)

    exceed_amounts = _gpd_ppf(rng.random(n_exceed), xi, sigma)
    exceedance_values = u + exceed_amounts

    raw = np.concatenate([body, exceedance_values])
    is_exceedance = np.concatenate(
        [np.zeros(n_body, dtype=np.bool_), np.ones(n_exceed, dtype=np.bool_)]
    )

    order = rng.permutation(n)
    raw = raw[order]
    is_exceedance = is_exceedance[order]

    n_windows = n // window_size
    trimmed = raw[: n_windows * window_size].reshape(n_windows, window_size)
    t = np.arange(n_windows, dtype=np.int64)
    p50 = np.quantile(trimmed, 0.50, axis=1)
    p95 = np.quantile(trimmed, 0.95, axis=1)
    p99 = np.quantile(trimmed, 0.99, axis=1)

    frame = Frame(t=t, p50=p50, p95=p95, p99=p99)
    ground_truth = HeavyTailGroundTruth(
        xi=xi,
        sigma=sigma,
        u=u,
        raw=raw,
        is_exceedance=is_exceedance,
        exceedances=raw[is_exceedance] - u,
    )
    return frame, ground_truth
