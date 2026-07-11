# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Shared forecaster -> score -> split-conformal pipeline for the coverage gates.

One reviewed implementation of the ADR-002 scoring path so that G1
(tests/score), G2 (tests/forecast), and the shuffled control (here) all
exercise the *same* path. Two properties this module deliberately has:

- **Honest chronological split.** The forecaster is fit ONLY on a training
  prefix; calibration and evaluation windows are strictly out-of-sample
  forecasts. Fitting on the full series and splitting scores afterwards is
  in-sample leakage — benign on this cyclostationary fixture (HW extrapolates
  it near-perfectly), but production data is not that kind, so the gate runs
  the deploy-time protocol.
- **Multi-bucket calibration.** Calibration scores are spread across several
  time buckets so the weighted-merge quantile path (BucketRing) is exercised
  through calibration, not bypassed by a single bucket. At `decay = 1.0` this
  is textbook split conformal; `decay < 1` is the tracked, non-gating regime.

Not a test module.
"""

from __future__ import annotations

import math
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from typing import Literal

import numpy as np
import numpy.typing as npt
from scipy.stats import t as student_t

from pconformal.calibrate import SplitConformal
from pconformal.forecast import Forecaster, RollingMAD
from pconformal.score import score
from pconformal.synth import Frame, seasonal_latency

_WEEKS = 8
_NOISE = 0.05
MAD_WINDOW = 64
EPS = 1e-6
RELATIVE_ACCURACY = 0.005  # DDSketch accuracy for the calibration score quantile
TRAIN_FRAC = 0.5  # first half fits the forecaster (out-of-sample cal + test follow)
CAL_FRAC = 0.5  # of the out-of-sample region, first half calibrates, rest evaluates
N_BUCKETS = 8  # spread calibration across buckets so the merge path is exercised

Statistic = Literal["p50", "p95", "p99"]
STATISTICS: tuple[Statistic, ...] = ("p50", "p95", "p99")
MakeForecaster = Callable[[], Forecaster]


def _series(frame: Frame, statistic: Statistic) -> npt.NDArray[np.float64]:
    return {"p50": frame.p50, "p95": frame.p95, "p99": frame.p99}[statistic]


def _oos_scores(
    y: npt.NDArray[np.float64], forecaster: Forecaster, *, train_frac: float
) -> tuple[npt.NDArray[np.float64], npt.NDArray[np.float64], npt.NDArray[np.float64]]:
    """Fit on the train prefix; return post-warmup OOS (scores, band_width, mad)."""
    n = y.shape[0]
    n_train = int(train_frac * n)
    forecaster.fit(y[:n_train])
    forecaster.predict_band(n - 1)  # pre-warm the forecast cache to the full horizon
    idx = range(n_train, n)
    bands = np.array([forecaster.predict_band(int(t)) for t in idx], dtype=np.float64)
    q_lo, q_hi = bands[:, 0], bands[:, 1]
    y_eval = y[n_train:]
    center = 0.5 * (q_lo + q_hi)
    resid = y_eval - center
    mad = RollingMAD(window=MAD_WINDOW, epsilon=EPS).scale(resid)
    scores = score(y_eval, q_lo, q_hi, mad, eps=EPS)
    keep = slice(MAD_WINDOW, None)  # drop the causal-MAD warmup before splitting
    return scores[keep], (q_hi - q_lo)[keep], mad[keep]


@dataclass(frozen=True)
class Coverage:
    covered: int
    n_test: int
    n_cal: int
    median_width: float
    radius: float

    @property
    def rate(self) -> float:
        return self.covered / self.n_test


def _calibrate_and_evaluate(
    scores: npt.NDArray[np.float64],
    band_width: npt.NDArray[np.float64],
    mad: npt.NDArray[np.float64],
    *,
    alpha: float,
    n_buckets: int,
    decay: float,
    cal_frac: float,
) -> Coverage:
    n_cal = int(cal_frac * scores.shape[0])
    cal, test = scores[:n_cal], scores[n_cal:]
    sc = SplitConformal(capacity=n_buckets, decay=decay, relative_accuracy=RELATIVE_ACCURACY)
    for chunk in np.array_split(cal, n_buckets):  # oldest -> newest time buckets
        sc.open_bucket()
        sc.extend(chunk)
    radius = sc.radius(alpha)
    covered = int(np.count_nonzero(test <= radius))
    widths = band_width[n_cal:] + 2.0 * radius * mad[n_cal:]
    return Coverage(covered, test.shape[0], n_cal, float(np.median(widths)), radius)


def coverage_on_seasonal(
    seed: int,
    make_forecaster: MakeForecaster,
    *,
    alpha: float = 0.1,
    statistic: Statistic = "p50",
    n_buckets: int = N_BUCKETS,
    decay: float = 1.0,
    train_frac: float = TRAIN_FRAC,
    cal_frac: float = CAL_FRAC,
) -> Coverage:
    """Out-of-sample split-conformal coverage + median band width on seasonal_latency."""
    frame, _ = seasonal_latency(seed, weeks=_WEEKS, noise=_NOISE)
    scores, band_width, mad = _oos_scores(
        _series(frame, statistic), make_forecaster(), train_frac=train_frac
    )
    return _calibrate_and_evaluate(
        scores, band_width, mad, alpha=alpha, n_buckets=n_buckets, decay=decay, cal_frac=cal_frac
    )


def coverage_t_interval(rates: Sequence[float], conf: float = 0.95) -> tuple[float, float, float]:
    """Two-sided t-interval for the mean of per-seed coverage rates. Returns (lo, hi, mean)."""
    sample = np.asarray(rates, dtype=np.float64)
    n = sample.shape[0]
    mean = float(sample.mean())
    if n < 2:
        return mean, mean, mean
    se = float(sample.std(ddof=1) / math.sqrt(n))
    half = float(student_t.ppf(0.5 + conf / 2.0, df=n - 1)) * se
    return mean - half, mean + half, mean


def t_lower_bound(rates: Sequence[float], conf: float = 0.95) -> float:
    """One-sided lower confidence bound on the mean of per-seed coverage rates."""
    sample = np.asarray(rates, dtype=np.float64)
    n = sample.shape[0]
    mean = float(sample.mean())
    if n < 2:
        return mean
    se = float(sample.std(ddof=1) / math.sqrt(n))
    return mean - float(student_t.ppf(conf, df=n - 1)) * se


def acf1(x: npt.NDArray[np.float64]) -> float:
    """Lag-1 autocorrelation of ``x``."""
    centered = x - x.mean()
    denom = float(np.sum(centered * centered))
    if denom == 0.0:
        return 0.0
    return float(np.sum(centered[1:] * centered[:-1]) / denom)


def _residuals_in_sample(
    y: npt.NDArray[np.float64], forecaster: Forecaster
) -> npt.NDArray[np.float64]:
    forecaster.fit(y)
    n = y.shape[0]
    bands = np.array([forecaster.predict_band(int(t)) for t in range(n)], dtype=np.float64)
    return y - 0.5 * (bands[:, 0] + bands[:, 1])


def shuffled_control(
    seed: int, make_forecaster: MakeForecaster, *, alpha: float = 0.1
) -> tuple[float, float, Coverage]:
    """Return (ordered residual acf1, shuffled residual acf1, shuffled coverage)."""
    frame, _ = seasonal_latency(seed, weeks=_WEEKS, noise=_NOISE)
    y = frame.p50
    rng = np.random.default_rng(seed + 1000)
    y_shuffled = y[rng.permutation(y.shape[0])]

    resid_ordered = _residuals_in_sample(y, make_forecaster())
    resid_shuffled = _residuals_in_sample(y_shuffled, make_forecaster())
    scores, band_width, mad = _oos_scores(y_shuffled, make_forecaster(), train_frac=TRAIN_FRAC)
    cov = _calibrate_and_evaluate(
        scores, band_width, mad, alpha=alpha, n_buckets=N_BUCKETS, decay=1.0, cal_frac=CAL_FRAC
    )
    return acf1(resid_ordered), acf1(resid_shuffled), cov
