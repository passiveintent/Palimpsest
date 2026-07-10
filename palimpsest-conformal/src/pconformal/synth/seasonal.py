"""seasonal_latency: a Holt-Winters-recoverable seasonal series of window stats.

ADR-002's Forecaster is Holt-Winters-class; ADR-003's Mondrian strata exist
only to repair exchangeability the forecaster can't learn, on the premise
that a Holt-Winters-class model *can* learn hour-of-week seasonality. This
generator is the fixture that premise stands on: a series whose seasonal
shape is a known, fixed, deterministic `hour_of_week_profile`, so a
forecaster fit against it can be checked against ground truth rather than
against another model's opinion.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import numpy.typing as npt

from pconformal.synth._constants import Z95, Z99
from pconformal.synth._frame import Frame

_HOURS_PER_WEEK = 168


def default_hour_of_week_profile() -> npt.NDArray[np.float64]:
    """A deterministic, strictly-positive multiplicative seasonal profile.

    Business-hours weekday windows run hot, off-hours weekday windows run
    warm, weekends run cool, with a smooth daily wave layered on top so the
    profile is a curve a Holt-Winters-class model can actually fit, not a
    step function.
    """
    hours = np.arange(_HOURS_PER_WEEK)
    hour_of_day = hours % 24
    day_of_week = hours // 24
    is_weekday = day_of_week < 5
    is_business_hours = (hour_of_day >= 9) & (hour_of_day < 17)

    base = np.where(
        is_weekday & is_business_hours,
        1.3,
        np.where(is_weekday, 0.85, 0.6),
    )
    daily_wave = 0.08 * np.sin(2 * np.pi * (hour_of_day - 6) / 24)
    return (base + daily_wave).astype(np.float64)


@dataclass(frozen=True)
class SeasonalGroundTruth:
    hour_of_week_profile: npt.NDArray[np.float64]
    mu: npt.NDArray[np.float64]
    sigma: float
    true_p50: npt.NDArray[np.float64]
    true_p95: npt.NDArray[np.float64]
    true_p99: npt.NDArray[np.float64]


def seasonal_latency(
    seed: int,
    *,
    weeks: int = 8,
    hour_of_week_profile: npt.NDArray[np.float64] | None = None,
    noise: float = 0.05,
    base: float = 100.0,
    sigma: float = 0.15,
) -> tuple[Frame, SeasonalGroundTruth]:
    """Lognormal per-window latency, seasonal in log-mean, observed with noise.

    The per-window latency distribution is `LogNormal(mu(t), sigma)`, with
    `mu(t) = log(base) + log(profile[t % 168])`. `true_p50/p95/p99` are that
    distribution's exact analytic quantiles; the returned frame multiplies
    them by a single shared per-window noise factor `(1 + noise * z)` (same
    `z` across all three quantiles) so observation noise never inverts
    `p50 <= p95 <= p99`.
    """
    if weeks < 1:
        raise ValueError("weeks must be >= 1")
    if noise < 0:
        raise ValueError("noise must be >= 0")
    if sigma <= 0:
        raise ValueError("sigma must be > 0")
    if base <= 0:
        raise ValueError("base must be > 0")

    profile = (
        default_hour_of_week_profile()
        if hour_of_week_profile is None
        else np.asarray(hour_of_week_profile, dtype=np.float64)
    )
    if profile.shape != (_HOURS_PER_WEEK,):
        raise ValueError(f"hour_of_week_profile must have shape ({_HOURS_PER_WEEK},)")
    if np.any(profile <= 0):
        raise ValueError("hour_of_week_profile must be strictly positive")

    rng = np.random.default_rng(seed)
    n = weeks * _HOURS_PER_WEEK
    t = np.arange(n, dtype=np.int64)
    mu = np.log(base) + np.log(profile[t % _HOURS_PER_WEEK])

    true_p50 = np.exp(mu)
    true_p95 = np.exp(mu + sigma * Z95)
    true_p99 = np.exp(mu + sigma * Z99)

    factor = np.clip(1.0 + noise * rng.standard_normal(n), 1e-3, None)
    p50 = true_p50 * factor
    p95 = true_p95 * factor
    p99 = true_p99 * factor

    frame = Frame(t=t, p50=p50, p95=p95, p99=p99)
    ground_truth = SeasonalGroundTruth(
        hour_of_week_profile=profile,
        mu=mu,
        sigma=sigma,
        true_p50=true_p50,
        true_p95=true_p95,
        true_p99=true_p99,
    )
    return frame, ground_truth
