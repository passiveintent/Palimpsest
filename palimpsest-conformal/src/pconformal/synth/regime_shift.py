"""regime_shift: shifts EITHER input features OR the score channel, never both.

This is the shared G3/G4 substrate. `target="inputs"` is G4's fixture
(input-feature changepoint, benign scores); `target="scores"` is a
sharper/general-purpose stand-in for G3's boiling-frog scenario
(`slow_burn` is the dedicated, gradual version of the same idea). The
`score` channel is a synthetic nonconformity-score-like residual —
the real score module doesn't exist yet, so this generator produces a
stand-in with the property that matters for drift tests: it drifts (or
doesn't) independently of `input_features`.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal, get_args

import numpy as np
import numpy.typing as npt

from pconformal.synth._frame import Frame

Kind = Literal["step", "ramp"]
Target = Literal["inputs", "scores"]

_KINDS = get_args(Kind)
_TARGETS = get_args(Target)


@dataclass(frozen=True)
class RegimeShiftGroundTruth:
    t0: int
    kind: Kind
    target: Target
    magnitude: float
    ramp_windows: int
    progress: npt.NDArray[np.float64]
    true_score_mean: npt.NDArray[np.float64]
    true_feature_mix: npt.NDArray[np.float64]


def _progress(n: int, t0: int, kind: Kind, ramp_windows: int) -> npt.NDArray[np.float64]:
    t = np.arange(n, dtype=np.float64)
    if kind == "step":
        return np.where(t >= t0, 1.0, 0.0)
    raw = (t - t0) / max(ramp_windows, 1)
    return np.clip(raw, 0.0, 1.0)


def regime_shift(
    seed: int,
    t0: int,
    kind: Kind,
    target: Target,
    *,
    n: int = 400,
    magnitude: float = 2.0,
    ramp_windows: int = 40,
    n_features: int = 3,
    base: float = 100.0,
) -> tuple[Frame, RegimeShiftGroundTruth]:
    if kind not in _KINDS:
        raise ValueError(f"kind must be one of {_KINDS}, got {kind!r}")
    if target not in _TARGETS:
        raise ValueError(f"target must be one of {_TARGETS}, got {target!r}")
    if not 0 <= t0 < n:
        raise ValueError("t0 must be within [0, n)")
    if n_features < 2:
        raise ValueError("n_features must be >= 2")
    if ramp_windows < 1:
        raise ValueError("ramp_windows must be >= 1")

    rng = np.random.default_rng(seed)
    progress = _progress(n, t0, kind, ramp_windows)

    score_shift = progress * magnitude if target == "scores" else np.zeros(n)
    score = score_shift + rng.standard_normal(n)

    alpha_pre = np.full(n_features, 5.0)
    post_multiplier = np.array([3.0] + [1.0] * (n_features - 1))
    alpha_post = alpha_pre * post_multiplier

    if target == "inputs":
        alpha_t = alpha_pre[None, :] * (1 - progress)[:, None] + alpha_post[None, :] * progress[
            :, None
        ]
    else:
        alpha_t = np.tile(alpha_pre, (n, 1))

    input_features = np.empty((n, n_features), dtype=np.float64)
    for i in range(n):
        input_features[i] = rng.dirichlet(alpha_t[i])

    true_feature_mix = alpha_t / alpha_t.sum(axis=1, keepdims=True)

    spread_95, spread_99 = 15.0, 25.0
    p50 = base + score
    p95 = p50 + spread_95
    p99 = p50 + spread_99
    t = np.arange(n, dtype=np.int64)

    frame = Frame(t=t, p50=p50, p95=p95, p99=p99, input_features=input_features, score=score)
    ground_truth = RegimeShiftGroundTruth(
        t0=t0,
        kind=kind,
        target=target,
        magnitude=magnitude,
        ramp_windows=ramp_windows,
        progress=progress,
        true_score_mean=score_shift,
        true_feature_mix=true_feature_mix,
    )
    return frame, ground_truth
