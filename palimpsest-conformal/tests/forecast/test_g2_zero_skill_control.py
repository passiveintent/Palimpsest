# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G2: zero-skill control. See docs/GATES.md#g2--zero-skill-control.

Both the production Holt-Winters forecaster and the zero-skill
`ConstantForecaster` run through the identical out-of-sample split-conformal
path on the identical seasonal windows (paired seeds, equal budget). The
coverage guarantee must hold for BOTH — proving it is a property of the
conformal wrapper, not forecaster skill — while Holt-Winters earns its
complexity with a >= 20% narrower median band.

Coverage is checked with G1's exact theory-derived shape (nominal inside the
per-seed coverage t-interval, mean under the theory upper bound).
"""

from __future__ import annotations

import numpy as np
import pytest

from pconformal.forecast import ConstantForecaster, HoltWintersForecaster
from tests.calibrate._pipeline import (
    RELATIVE_ACCURACY,
    MakeForecaster,
    coverage_on_seasonal,
    coverage_t_interval,
)
from tests.seeds import paired_seeds

_ALPHA = 0.1
_NOMINAL = 1.0 - _ALPHA
_EPS_SKETCH = 2.0 * RELATIVE_ACCURACY
_MIN_WIDTH_REDUCTION = 0.20  # HW must be >= 20% narrower than the zero-skill control
_SEEDS = 8


def _coverage_and_widths(
    make_forecaster: MakeForecaster,
) -> tuple[list[float], list[float], int]:
    rates: list[float] = []
    widths: list[float] = []
    n_cal = 0
    for window_seed, _ in paired_seeds("g2-zero-skill", _SEEDS):
        cov = coverage_on_seasonal(int(window_seed % (2**32)), make_forecaster, alpha=_ALPHA)
        rates.append(cov.rate)
        widths.append(cov.median_width)
        n_cal = cov.n_cal
    return rates, widths, n_cal


def _assert_coverage_holds(name: str, rates: list[float], n_cal: int) -> None:
    lo, hi, mean = coverage_t_interval(rates)
    upper_theory = _NOMINAL + 1.0 / (n_cal + 1) + _EPS_SKETCH
    assert lo <= _NOMINAL <= hi, (
        f"{name} coverage CI [{lo:.4f}, {hi:.4f}] inconsistent with nominal"
    )
    assert mean <= upper_theory, (
        f"{name} mean coverage {mean:.4f} > theory upper {upper_theory:.4f}"
    )


@pytest.mark.gate
def test_g2_holt_winters_beats_constant_forecaster_width_at_equal_coverage() -> None:
    hw_rates, hw_widths, hw_ncal = _coverage_and_widths(HoltWintersForecaster)
    const_rates, const_widths, const_ncal = _coverage_and_widths(ConstantForecaster)

    # (1) coverage is forecaster-agnostic: both pass G1's coverage shape.
    _assert_coverage_holds("HW", hw_rates, hw_ncal)
    _assert_coverage_holds("Const", const_rates, const_ncal)

    # (2) HW earns its complexity: >= 20% narrower median width (record the actual).
    hw_width = float(np.median(hw_widths))
    const_width = float(np.median(const_widths))
    width_reduction = 1.0 - hw_width / const_width
    assert width_reduction >= _MIN_WIDTH_REDUCTION, (
        f"HW only {width_reduction:.1%} narrower than the zero-skill control "
        f"(HW={hw_width:.2f}, Const={const_width:.2f}); need >= {_MIN_WIDTH_REDUCTION:.0%}"
    )
