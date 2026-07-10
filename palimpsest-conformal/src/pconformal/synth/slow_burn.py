# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""slow_burn: gradual score drift under stationary inputs — the G3 boiling-frog fixture.

The canonical, purpose-built substrate for G3 (`docs/GATES.md`): the score
channel drifts linearly from the very first window at a declared `rate`,
while `input_features` are drawn from one fixed Dirichlet concentration for
the entire series. A drift detector that only watches inputs must never
fire on this fixture; the ordinary band mechanism must eventually catch the
drift. `regime_shift(target="scores")` is the sharper, changepoint-style
sibling of this same idea.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import numpy.typing as npt

from pconformal.synth._frame import Frame


@dataclass(frozen=True)
class SlowBurnGroundTruth:
    rate: float
    true_score_mean: npt.NDArray[np.float64]
    true_feature_mix: npt.NDArray[np.float64]


def slow_burn(
    seed: int,
    rate: float,
    *,
    n: int = 300,
    n_features: int = 3,
    base: float = 100.0,
) -> tuple[Frame, SlowBurnGroundTruth]:
    if rate < 0:
        raise ValueError("rate must be >= 0")
    if n < 1:
        raise ValueError("n must be >= 1")
    if n_features < 2:
        raise ValueError("n_features must be >= 2")

    rng = np.random.default_rng(seed)
    t = np.arange(n, dtype=np.int64)

    true_score_mean = rate * t.astype(np.float64)
    score = true_score_mean + rng.standard_normal(n)

    alpha = np.full(n_features, 5.0)
    input_features = rng.dirichlet(alpha, size=n)
    true_feature_mix = np.tile(alpha / alpha.sum(), (n, 1))

    spread_95, spread_99 = 15.0, 25.0
    p50 = base + score
    p95 = p50 + spread_95
    p99 = p50 + spread_99

    frame = Frame(t=t, p50=p50, p95=p95, p99=p99, input_features=input_features, score=score)
    ground_truth = SlowBurnGroundTruth(
        rate=rate, true_score_mean=true_score_mean, true_feature_mix=true_feature_mix
    )
    return frame, ground_truth
