# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G1: coverage. See docs/GATES.md#g1--coverage.

The marginal half is implemented here (prompt 8: split conformal). The
per-stratum half needs the Mondrian stratified calibrator (ADR-003, prompt 9)
and stays a tracked skip until then.

Gate shape (theory-derived, no hand-set bands). Split conformal gives an
ASYMMETRIC two-sided theorem, `1 - alpha <= coverage <= 1 - alpha +
1/(n_cal+1)` under exchangeability. Run honestly out-of-sample (the forecaster
is fit only on a training prefix — see `_pipeline`), coverage sits AT nominal,
not comfortably above it, so we test:

  * nominal is inside the per-seed coverage t-interval — catches systematic
    under-coverage (e.g. 0.885), which a single pooled binomial CI's ~+/-0.003
    band and its within-seed autocorrelation blindness would miss;
  * the mean does not exceed `1 - alpha + 1/(n_cal+1) + eps_sketch` — catches
    band-width waste, where `eps_sketch` is the DDSketch discretization plus
    the `(1+gamma)` conservative radius inflation, derived from the sketch's
    own relative accuracy, not picked by hand.
"""

from __future__ import annotations

import numpy as np
import pytest

from pconformal.forecast import HoltWintersForecaster
from tests.calibrate._pipeline import (
    RELATIVE_ACCURACY,
    coverage_on_seasonal,
    coverage_t_interval,
)
from tests.seeds import paired_seeds

_ALPHA = 0.1
_NOMINAL = 1.0 - _ALPHA
_EPS_SKETCH = 2.0 * RELATIVE_ACCURACY  # sketch discretization + (1+gamma) inflation allowance
_SEEDS = 8


@pytest.mark.gate
def test_g1_marginal_coverage_on_seasonal_latency() -> None:
    results = [
        coverage_on_seasonal(int(window_seed % (2**32)), HoltWintersForecaster, alpha=_ALPHA)
        for window_seed, _ in paired_seeds("g1-coverage", _SEEDS)
    ]
    rates = [r.rate for r in results]
    n_cal = results[0].n_cal
    lo, hi, mean = coverage_t_interval(rates)
    upper_theory = _NOMINAL + 1.0 / (n_cal + 1) + _EPS_SKETCH

    assert lo <= _NOMINAL <= hi, (
        f"nominal {_NOMINAL} outside coverage t-interval [{lo:.4f}, {hi:.4f}] "
        f"(mean={mean:.4f}, per-seed={np.round(rates, 4).tolist()}) — under/over-coverage"
    )
    assert mean <= upper_theory, (
        f"mean coverage {mean:.4f} exceeds theory upper bound {upper_theory:.4f} "
        f"(= 1-alpha + 1/(n_cal+1) + eps_sketch, n_cal={n_cal}) — band-width waste"
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
