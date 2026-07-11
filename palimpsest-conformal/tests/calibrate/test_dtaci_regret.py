# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Interim DtACI regret gate (Gibbs & Candes 2022) — closes the validation gap
before the full drift stack (G3/G4, prompt 11).

The paper's guarantee is REGRET against the best fixed-gamma expert chosen in
hindsight, and that is testable now: on a score-distribution regime shift
(`regime_shift(target="scores")`), DtACI's coverage error must be no worse
than the best single-gamma ACI expert's, plus a small regret margin — and far
better than a non-adaptive fixed alpha (the control). This exercises the
ensemble aggregation itself, not just adaptation direction. The line-by-line
algorithm audit (pinball on the score/radius scale, exp-weighting +
fixed-share mixing, weighted-average vs randomized output) lives in the
DEVIATION-CHECK block of `calibrate/_dtaci.py`.

Scope note: calibration is a FIXED stationary set here, so DtACI can only
adapt alpha within what that set supports — full post-shift recovery needs
ring recalibration (ADR-004), which is G4. This gate isolates the ensemble.
"""

from __future__ import annotations

import numpy as np
import pytest

from pconformal.calibrate import ACI, DtACI, RadiusFn, SplitConformal
from pconformal.synth import regime_shift
from tests.seeds import paired_seeds

_ALPHA = 0.1
_TARGET_COVERAGE = 1.0 - _ALPHA
_ALPHA_MAX = 0.4
_GAMMAS = (0.002, 0.008, 0.032, 0.128)  # DtACI default expert grid
_REGRET_MARGIN = 0.03  # observed max gap ~0.015; 2x headroom
_SEEDS = 8
_T0, _N, _MAGNITUDE, _CAL_N = 200, 400, 2.0, 300


def _radius_fn(cal: np.ndarray) -> RadiusFn:
    sc = SplitConformal(capacity=1, decay=1.0, relative_accuracy=0.005)
    sc.open_bucket()
    sc.extend(cal)
    return sc.radius


def _err(covered: int, n: int) -> float:
    return abs(covered / n - _TARGET_COVERAGE)


def _dtaci_error(stream: np.ndarray, radius_fn: RadiusFn) -> float:
    dt = DtACI(alpha_target=_ALPHA, alpha_max=_ALPHA_MAX, interval=stream.shape[0], gammas=_GAMMAS)
    covered = 0
    for s in stream:
        covered += int(s <= radius_fn(dt.alpha))
        dt.update(float(s), radius_fn)
    return _err(covered, stream.shape[0])


def _aci_error(stream: np.ndarray, radius_fn: RadiusFn, gamma: float) -> float:
    aci = ACI(alpha_target=_ALPHA, gamma=gamma, alpha_max=_ALPHA_MAX)
    covered = 0
    for s in stream:
        hit = bool(s <= radius_fn(aci.alpha))
        covered += int(hit)
        aci.update(covered=hit)
    return _err(covered, stream.shape[0])


def _fixed_alpha_error(stream: np.ndarray, radius_fn: RadiusFn) -> float:
    r = radius_fn(_ALPHA)
    return _err(int(np.count_nonzero(stream <= r)), stream.shape[0])


@pytest.mark.gate
def test_dtaci_regret_no_worse_than_best_fixed_gamma_expert() -> None:
    for window_seed, draw_seed in paired_seeds("dtaci-regret", _SEEDS):
        frame, _ = regime_shift(
            int(window_seed % (2**32)),
            t0=_T0,
            kind="step",
            target="scores",
            n=_N,
            magnitude=_MAGNITUDE,
        )
        stream = frame.score
        assert stream is not None  # regime_shift always populates the score channel
        cal = np.random.default_rng(int(draw_seed % (2**32))).standard_normal(_CAL_N)
        radius_fn = _radius_fn(cal)

        dtaci = _dtaci_error(stream, radius_fn)
        best_expert = min(_aci_error(stream, radius_fn, g) for g in _GAMMAS)
        fixed = _fixed_alpha_error(stream, radius_fn)

        assert dtaci <= best_expert + _REGRET_MARGIN, (
            f"DtACI coverage error {dtaci:.4f} worse than best fixed-gamma expert "
            f"{best_expert:.4f} + margin {_REGRET_MARGIN} (seed {window_seed})"
        )
        # control: adaptation must beat a non-adaptive fixed alpha through the shift.
        assert dtaci < fixed, f"DtACI ({dtaci:.4f}) no better than fixed alpha ({fixed:.4f})"
