# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G1: coverage. See docs/GATES.md#g1--coverage.

Final gate shape — one-sided, zero hand-set constants. The product claim is
that the band never *under-covers*: `coverage >= 1 - alpha`. So G1 fails only
on statistically detectable under-coverage — iff even the one-sided UPPER
confidence bound of the mean per-seed coverage sits below `1 - alpha`. This
passes at nominal, passes legitimate conservatism (split conformal is meant to
over-cover slightly), fails confident slippage, and tightens automatically as
seed count grows. A two-sided "nominal inside the CI" test would wrongly reject
that legitimate conservatism as the sample grows; a lower-bound-clears-nominal
test would demand wasteful over-inflation. This is the honest middle.

Protocol: honest rolling-origin out-of-sample (periodic refit, short bounded
horizon — see `_pipeline`), so calibration and evaluation residuals are
near-exchangeable. Fixed-alpha split conformal is then conservative (coverage
~0.915 here), the safe direction. Band-width waste is caught by G2's relative
width comparison, not an absolute coverage ceiling (which would conflict with
this legitimate conservatism).

The per-stratum half needs the Mondrian stratified calibrator (ADR-003, prompt
9) and stays a tracked skip until then. In-repo GATES.md is canonical; the
build-playbook's original G1 text is superseded.
"""

from __future__ import annotations

import numpy as np
import pytest

from pconformal.forecast import HoltWintersForecaster
from tests.calibrate._pipeline import coverage_on_seasonal, t_upper_bound
from tests.seeds import paired_seeds

_ALPHA = 0.1
_NOMINAL = 1.0 - _ALPHA
_SEEDS = 8


@pytest.mark.gate
def test_g1_marginal_coverage_on_seasonal_latency() -> None:
    rates = [
        coverage_on_seasonal(
            int(window_seed % (2**32)),
            HoltWintersForecaster,
            alpha=_ALPHA,
            split_seed=int(draw_seed % (2**32)),  # exchangeable cal/eval split
        ).rate
        for window_seed, draw_seed in paired_seeds("g1-coverage", _SEEDS)
    ]
    upper_cb = t_upper_bound(rates)  # one-sided 95% upper confidence bound
    assert upper_cb >= _NOMINAL, (
        f"upper confidence bound {upper_cb:.4f} < nominal {_NOMINAL}: statistically "
        f"confident marginal under-coverage (mean={np.mean(rates):.4f}, "
        f"per-seed={np.round(rates, 4).tolist()})"
    )


@pytest.mark.gate
def test_g1_per_stratum_coverage() -> None:
    pytest.skip("G1 per-stratum blocked on the Mondrian stratified calibrator (ADR-003, prompt 9)")


def test_g1_helper_reports_sane_shapes() -> None:
    # A cheap non-gate guard that the pipeline wiring itself is intact.
    cov = coverage_on_seasonal(0, HoltWintersForecaster, alpha=_ALPHA)
    assert cov.n_test > 0
    assert np.isfinite(cov.radius)
    assert cov.median_width > 0.0
