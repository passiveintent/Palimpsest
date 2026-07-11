# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G1b: adaptive-stack long-run coverage — the layer that carries the product claim.

See docs/GATES.md#g1b. G1 asserts the fixed-alpha wrapper never under-covers
under exchangeability. G1b asserts the FULL composed stack (forecaster ->
studentized CQR score -> split-conformal calibration -> DtACI adaptive
miscoverage) holds long-run coverage within a deterministic Gibbs & Candes
bound REGARDLESS of exchangeability — including on the adversarial frozen-
horizon protocol, where a single multi-step extrapolation makes evaluation
residuals stochastically dominate calibration's and fixed alpha catastrophically
under-covers. Surviving that break is ACI's reason to exist.

Bound (Gibbs & Candes 2021, exact telescoping of the ACI recursion): for step
`gamma` over `T` windows, `|empirical_miscoverage - alpha| = |alpha_{T+1} -
alpha_1|/(gamma*T) <= (max(alpha_1, 1-alpha_1) + gamma)/(gamma*T)`, since
`alpha_t in [-gamma, 1+gamma]`. Non-vacuous only for large gamma, so we use the
DtACI grid's fastest gamma.

=====================================================================
DEVIATION-CHECK (verify against the paper before production): the exact bound
is derived for an UN-clipped single ACI. Our stack (a) floors alpha at
`alpha_max` (invariant 2), which breaks the clean telescoping, and (b)
aggregates an ensemble (DtACI), not one ACI. Both are absorbed into a declared
`_CLIP_ENSEMBLE_ALLOWANCE`. The gate therefore asserts the bound PLUS that
allowance; tighten once the clipped/ensemble constant is lifted from the paper.
=====================================================================
"""

from __future__ import annotations

import numpy as np
import pytest

from pconformal.calibrate import DtACI, RadiusFn, SplitConformal
from pconformal.forecast import HoltWintersForecaster
from pconformal.synth import seasonal_latency
from tests.calibrate._pipeline import ScoreProtocol, _protocol_scores
from tests.seeds import paired_seeds

_ALPHA = 0.1
_ALPHA_MAX = 0.4
_GAMMAS = (0.002, 0.008, 0.032, 0.128)
_GAMMA_FAST = max(_GAMMAS)
_CLIP_ENSEMBLE_ALLOWANCE = 0.02  # clip + DtACI-aggregation slack on the un-clipped ACI bound
_CONTROL_MARGIN = 0.01  # small slack: DtACI must be at least ~as calibrated as fixed alpha
_SEEDS = 6
_N_BUCKETS = 8


def _radius_fn(cal: np.ndarray) -> RadiusFn:
    sc = SplitConformal(capacity=_N_BUCKETS, decay=1.0, relative_accuracy=0.005)
    for chunk in np.array_split(cal, _N_BUCKETS):
        sc.open_bucket()
        sc.extend(chunk)
    return sc.radius


def _dtaci_miscoverage(stream: np.ndarray, radius_fn: RadiusFn) -> float:
    dt = DtACI(alpha_target=_ALPHA, alpha_max=_ALPHA_MAX, interval=stream.shape[0], gammas=_GAMMAS)
    err = 0
    for s in stream:
        err += int(not (s <= radius_fn(dt.alpha)))
        dt.update(float(s), radius_fn)
    return float(err / stream.shape[0])


def _fixed_alpha_miscoverage(stream: np.ndarray, radius_fn: RadiusFn) -> float:
    r = radius_fn(_ALPHA)
    return float(np.count_nonzero(stream > r) / stream.shape[0])


def _aci_bound(t_eval: int) -> float:
    return (max(_ALPHA, 1.0 - _ALPHA) + _GAMMA_FAST) / (_GAMMA_FAST * t_eval)


@pytest.mark.gate
@pytest.mark.parametrize("protocol", ["rolling", "frozen"])
def test_g1b_adaptive_stack_bounds_long_run_miscoverage(protocol: ScoreProtocol) -> None:
    dtaci_devs: list[float] = []
    fixed_devs: list[float] = []
    for window_seed, _ in paired_seeds("g1b-adaptive", _SEEDS):
        frame, _ = seasonal_latency(int(window_seed % (2**32)), weeks=8, noise=0.05)
        scores, _, _ = _protocol_scores(frame.p50, HoltWintersForecaster, protocol)
        n_cal = scores.shape[0] // 2
        cal, stream = scores[:n_cal], scores[n_cal:]
        radius_fn = _radius_fn(cal)

        dtaci_miscov = _dtaci_miscoverage(stream, radius_fn)
        bound = _aci_bound(stream.shape[0]) + _CLIP_ENSEMBLE_ALLOWANCE
        assert abs(dtaci_miscov - _ALPHA) <= bound, (
            f"[{protocol}] DtACI long-run |miscov - alpha| = "
            f"{abs(dtaci_miscov - _ALPHA):.4f} exceeds bound {bound:.4f} (seed {window_seed})"
        )
        dtaci_devs.append(abs(dtaci_miscov - _ALPHA))
        fixed_devs.append(abs(_fixed_alpha_miscoverage(stream, radius_fn) - _ALPHA))

    # Control (adversarial frozen protocol): adaptation must be at least as
    # well-calibrated as a non-adaptive fixed alpha, on average — |miscov -
    # alpha| is the calibration-quality metric, not raw miscoverage. (The clean
    # "fixed catastrophically under-covers" scenario is the DtACI regret gate.)
    if protocol == "frozen":
        assert float(np.mean(dtaci_devs)) <= float(np.mean(fixed_devs)) + _CONTROL_MARGIN, (
            f"[frozen] DtACI mean|miscov-alpha| {np.mean(dtaci_devs):.4f} worse than "
            f"fixed alpha {np.mean(fixed_devs):.4f} + margin {_CONTROL_MARGIN}"
        )
