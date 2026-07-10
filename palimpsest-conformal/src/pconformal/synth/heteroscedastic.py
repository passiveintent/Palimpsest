"""heteroscedastic_pair: two services with a known variance ratio.

ADR-002 studentizes the CQR score by a rolling MAD specifically so that
scores from different-volatility services/strata are comparable. This
fixture is the minimal case that claim needs to survive: two services with
identical location but a declared variance ratio (10x by default), for
tests that check studentization actually equalizes them rather than
comparing raw residuals across a heteroscedastic pair.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from pconformal.synth._constants import Z95, Z99
from pconformal.synth._frame import Frame


@dataclass(frozen=True)
class HeteroscedasticGroundTruth:
    base: float
    sigma_a: float
    sigma_b: float
    variance_ratio: float


def heteroscedastic_pair(
    seed: int,
    *,
    n: int = 500,
    base: float = 100.0,
    sigma_a: float = 2.0,
    variance_ratio: float = 10.0,
) -> tuple[Frame, HeteroscedasticGroundTruth]:
    """Two same-mean services (`service_a`, `service_b`) with `Var(b) = variance_ratio * Var(a)`.

    Each service's p50 is `base + Normal(0, sigma)`; p95/p99 are p50 plus
    the service's own analytic Normal offsets, so within a service
    `p50 <= p95 <= p99` always holds regardless of the noise draw.
    """
    if n < 1:
        raise ValueError("n must be >= 1")
    if sigma_a <= 0:
        raise ValueError("sigma_a must be > 0")
    if variance_ratio <= 0:
        raise ValueError("variance_ratio must be > 0")

    sigma_b = sigma_a * np.sqrt(variance_ratio)
    rng = np.random.default_rng(seed)

    t = np.tile(np.arange(n, dtype=np.int64), 2)
    entity = np.array(["service_a"] * n + ["service_b"] * n, dtype=np.str_)

    p50_a = base + sigma_a * rng.standard_normal(n)
    p50_b = base + sigma_b * rng.standard_normal(n)
    p50 = np.concatenate([p50_a, p50_b])

    sigma_per_row = np.concatenate([np.full(n, sigma_a), np.full(n, sigma_b)])
    p95 = p50 + Z95 * sigma_per_row
    p99 = p50 + Z99 * sigma_per_row

    frame = Frame(t=t, p50=p50, p95=p95, p99=p99, entity=entity)
    ground_truth = HeteroscedasticGroundTruth(
        base=base, sigma_a=sigma_a, sigma_b=sigma_b, variance_ratio=variance_ratio
    )
    return frame, ground_truth
