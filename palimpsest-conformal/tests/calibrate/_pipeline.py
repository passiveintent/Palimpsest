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
TRAIN_FRAC = 0.5  # frozen protocol: first half fits the forecaster
CAL_FRAC = 0.5  # of the out-of-sample region, first half calibrates, rest evaluates
N_BUCKETS = 8  # spread calibration across buckets so the merge path is exercised
TRAIN_WINDOW = 504  # rolling protocol: fixed 3-week refit window (>= 2 seasons)
BLOCK = 48  # rolling protocol: refit every 2 days; forecast horizons stay 1..BLOCK

Statistic = Literal["p50", "p95", "p99"]
STATISTICS: tuple[Statistic, ...] = ("p50", "p95", "p99")
# "rolling" is the honest deployment default: periodic refit, one-step-ahead-ish
# (short bounded horizon), so calibration and evaluation residuals share a
# horizon distribution -> near-exchangeable. "frozen" is a single multi-step
# extrapolation whose eval residuals live at longer horizons than calibration's
# -> deliberately non-exchangeable; kept as the adversarial substrate for G1b.
ScoreProtocol = Literal["rolling", "frozen"]
MakeForecaster = Callable[[], Forecaster]


def _series(frame: Frame, statistic: Statistic) -> npt.NDArray[np.float64]:
    return {"p50": frame.p50, "p95": frame.p95, "p99": frame.p99}[statistic]


def _finish_scores(
    q_lo: npt.NDArray[np.float64],
    q_hi: npt.NDArray[np.float64],
    y_eval: npt.NDArray[np.float64],
) -> tuple[npt.NDArray[np.float64], npt.NDArray[np.float64], npt.NDArray[np.float64]]:
    center = 0.5 * (q_lo + q_hi)
    resid = y_eval - center
    mad = RollingMAD(window=MAD_WINDOW, epsilon=EPS).scale(resid)
    scores = score(y_eval, q_lo, q_hi, mad, eps=EPS)
    keep = slice(MAD_WINDOW, None)  # drop the causal-MAD warmup before splitting
    return scores[keep], (q_hi - q_lo)[keep], mad[keep]


def _frozen_oos_scores(
    y: npt.NDArray[np.float64], forecaster: Forecaster, *, train_frac: float = TRAIN_FRAC
) -> tuple[npt.NDArray[np.float64], npt.NDArray[np.float64], npt.NDArray[np.float64]]:
    """Single fit on the train prefix, multi-step extrapolation over all of cal+eval.

    Deliberately non-exchangeable (eval at longer horizons than cal); the G1b
    adversarial substrate, NOT the honest default.
    """
    n = y.shape[0]
    n_train = int(train_frac * n)
    forecaster.fit(y[:n_train])
    forecaster.predict_band(n - 1)  # pre-warm the forecast cache to the full horizon
    bands = np.array([forecaster.predict_band(int(t)) for t in range(n_train, n)], dtype=np.float64)
    return _finish_scores(bands[:, 0], bands[:, 1], y[n_train:])


def _rolling_oos_scores(
    y: npt.NDArray[np.float64],
    make_forecaster: MakeForecaster,
    *,
    train_window: int = TRAIN_WINDOW,
    block: int = BLOCK,
) -> tuple[npt.NDArray[np.float64], npt.NDArray[np.float64], npt.NDArray[np.float64]]:
    """Rolling-origin, periodic-refit, short-horizon scores (the honest default).

    Refit on the trailing ``train_window`` every ``block`` windows and forecast
    the next ``block`` (horizons 1..block), so calibration and evaluation scores
    live at the same short horizons — restoring the near-exchangeability a
    single multi-step extrapolation destroys.
    """
    n = y.shape[0]
    q_lo: list[float] = []
    q_hi: list[float] = []
    y_eval: list[float] = []
    t = train_window
    while t < n:
        end = min(t + block, n)
        forecaster = make_forecaster()
        forecaster.fit(y[t - train_window : t])  # forecaster._n == train_window
        forecaster.predict_band(train_window + (end - t) - 1)  # warm cache to horizon end-t
        for tt in range(t, end):
            lo, hi = forecaster.predict_band(train_window + (tt - t))  # horizon (tt-t)+1
            q_lo.append(lo)
            q_hi.append(hi)
            y_eval.append(float(y[tt]))
        t = end
    return _finish_scores(
        np.asarray(q_lo, dtype=np.float64),
        np.asarray(q_hi, dtype=np.float64),
        np.asarray(y_eval, dtype=np.float64),
    )


def _protocol_scores(
    y: npt.NDArray[np.float64], make_forecaster: MakeForecaster, protocol: ScoreProtocol
) -> tuple[npt.NDArray[np.float64], npt.NDArray[np.float64], npt.NDArray[np.float64]]:
    if protocol == "rolling":
        return _rolling_oos_scores(y, make_forecaster)
    return _frozen_oos_scores(y, make_forecaster())


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
    split_seed: int | None = None,
) -> Coverage:
    # A wrapper-coverage gate needs cal and eval EXCHANGEABLE. A seeded random
    # split delivers that for ANY forecaster — including one (ConstantForecaster)
    # whose scores stay seasonal, where a contiguous split of a non-integer
    # number of weeks would give cal and eval different phase mixes and spurious
    # under-coverage. split_seed=None keeps the contiguous split (decay-drift
    # test, where time-ordered buckets are the point).
    if split_seed is not None:
        perm = np.random.default_rng(split_seed).permutation(scores.shape[0])
        scores, band_width, mad = scores[perm], band_width[perm], mad[perm]
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
    cal_frac: float = CAL_FRAC,
    protocol: ScoreProtocol = "rolling",
    split_seed: int | None = None,
) -> Coverage:
    """Out-of-sample split-conformal coverage + median band width on seasonal_latency."""
    frame, _ = seasonal_latency(seed, weeks=_WEEKS, noise=_NOISE)
    scores, band_width, mad = _protocol_scores(
        _series(frame, statistic), make_forecaster, protocol
    )
    return _calibrate_and_evaluate(
        scores,
        band_width,
        mad,
        alpha=alpha,
        n_buckets=n_buckets,
        decay=decay,
        cal_frac=cal_frac,
        split_seed=split_seed,
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


def t_upper_bound(rates: Sequence[float], conf: float = 0.95) -> float:
    """One-sided upper confidence bound on the mean of per-seed coverage rates.

    G1's under-coverage guard fails iff even this bound sits below 1 - alpha —
    i.e. we are statistically confident coverage is below nominal. It passes
    at-nominal and passes legitimate conservatism, and it tightens (gains
    power) automatically as seed count grows. Zero hand-set constants.
    """
    sample = np.asarray(rates, dtype=np.float64)
    n = sample.shape[0]
    mean = float(sample.mean())
    if n < 2:
        return mean
    se = float(sample.std(ddof=1) / math.sqrt(n))
    return mean + float(student_t.ppf(conf, df=n - 1)) * se


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
    # shuffled data has no temporal structure, so a single fit (frozen) is the
    # right, cheap choice here — rolling refit would be pointless on noise.
    scores, band_width, mad = _frozen_oos_scores(y_shuffled, make_forecaster())
    cov = _calibrate_and_evaluate(
        scores, band_width, mad, alpha=alpha, n_buckets=N_BUCKETS, decay=1.0, cal_frac=CAL_FRAC
    )
    return acf1(resid_ordered), acf1(resid_shuffled), cov
