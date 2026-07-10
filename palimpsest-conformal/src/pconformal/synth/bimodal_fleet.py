"""bimodal_fleet: per-host latency where averaged per-host p95s are badly wrong.

The G8 / ADR-007 substrate: `mix` fraction of hosts run in a "slow" mode
with a different mean and spread than the "fast" majority. Averaging each
host's own (correctly computed) p95 does not equal the true pooled p95 of
the fleet — that gap is precisely the ROLLUP_ARTIFACT failure mode ADR-007
triages away from NATIVELY_MERGEABLE aggregation.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import numpy.typing as npt

from pconformal.synth._frame import Frame


@dataclass(frozen=True)
class BimodalFleetGroundTruth:
    host_mode: npt.NDArray[np.str_]
    raw_by_host: npt.NDArray[np.float64]
    true_pooled_p95: float
    naive_avg_p95: float
    relative_error: float


def bimodal_fleet(
    seed: int,
    hosts: int,
    mix: float,
    *,
    n_per_host: int = 200,
    base: float = 100.0,
    fast_sigma: float = 0.2,
    slow_sigma: float = 0.5,
    slow_multiplier: float = 3.0,
) -> tuple[Frame, BimodalFleetGroundTruth]:
    """`hosts` hosts, `mix` fraction of them "slow" (higher mean and spread).

    Per-host raw samples are Lognormal; `frame` carries each host's own
    (correctly computed, empirical) p50/p95/p99. `ground_truth` carries the
    raw samples plus both the naive average-of-per-host-p95 (the
    ROLLUP_ARTIFACT computation) and the true pooled p95 (the
    NATIVELY_MERGEABLE computation), so G8 can assert their divergence
    directly instead of recomputing it.
    """
    if hosts < 2:
        raise ValueError("hosts must be >= 2")
    if not 0 <= mix <= 1:
        raise ValueError("mix must be in [0, 1]")
    if n_per_host < 10:
        raise ValueError("n_per_host must be >= 10")
    if fast_sigma <= 0 or slow_sigma <= 0:
        raise ValueError("fast_sigma and slow_sigma must be > 0")
    if slow_multiplier <= 0:
        raise ValueError("slow_multiplier must be > 0")

    rng = np.random.default_rng(seed)
    n_slow = min(max(round(hosts * mix), 0), hosts)
    is_slow = np.zeros(hosts, dtype=np.bool_)
    if n_slow > 0:
        slow_idx = rng.choice(hosts, size=n_slow, replace=False)
        is_slow[slow_idx] = True

    raw_by_host = np.empty((hosts, n_per_host), dtype=np.float64)
    for h in range(hosts):
        if is_slow[h]:
            mu, sigma = np.log(base * slow_multiplier), slow_sigma
        else:
            mu, sigma = np.log(base), fast_sigma
        raw_by_host[h] = rng.lognormal(mean=mu, sigma=sigma, size=n_per_host)

    per_host_p50 = np.quantile(raw_by_host, 0.50, axis=1)
    per_host_p95 = np.quantile(raw_by_host, 0.95, axis=1)
    per_host_p99 = np.quantile(raw_by_host, 0.99, axis=1)

    naive_avg_p95 = float(per_host_p95.mean())
    true_pooled_p95 = float(np.quantile(raw_by_host.ravel(), 0.95))
    relative_error = abs(naive_avg_p95 - true_pooled_p95) / true_pooled_p95

    host_mode = np.where(is_slow, "slow", "fast").astype(np.str_)
    t = np.arange(hosts, dtype=np.int64)
    entity = np.array([f"host_{i}" for i in range(hosts)], dtype=np.str_)

    frame = Frame(t=t, p50=per_host_p50, p95=per_host_p95, p99=per_host_p99, entity=entity)
    ground_truth = BimodalFleetGroundTruth(
        host_mode=host_mode,
        raw_by_host=raw_by_host,
        true_pooled_p95=true_pooled_p95,
        naive_avg_p95=naive_avg_p95,
        relative_error=relative_error,
    )
    return frame, ground_truth
