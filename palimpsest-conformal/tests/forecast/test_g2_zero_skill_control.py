# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G2: zero-skill control. See docs/GATES.md#g2--zero-skill-control.

Both the production Holt-Winters forecaster and the zero-skill
`ConstantForecaster` run through the identical rolling-origin out-of-sample
split-conformal path on the identical seasonal windows (paired seeds, equal
budget). Neither may under-cover — proving the guarantee is a property of the
conformal wrapper, not forecaster skill — while Holt-Winters earns its
complexity with a >= 20% narrower median band. That relative width comparison
is also G1's band-width-waste guard: an absolute coverage ceiling would
conflict with split conformal's legitimate conservatism.

Coverage uses G1's exact one-sided shape (fail iff the upper confidence bound
of mean per-seed coverage sits below nominal).
"""

from __future__ import annotations

import numpy as np
import pytest

from pconformal.forecast import ConstantForecaster, HoltWintersForecaster
from tests.calibrate._pipeline import MakeForecaster, coverage_on_seasonal, t_upper_bound
from tests.seeds import paired_seeds

_ALPHA = 0.1
_NOMINAL = 1.0 - _ALPHA
_MIN_WIDTH_REDUCTION = 0.20  # HW must be >= 20% narrower than the zero-skill control
_SEEDS = 8


def _coverage_and_widths(make_forecaster: MakeForecaster) -> tuple[list[float], list[float]]:
    rates: list[float] = []
    widths: list[float] = []
    for window_seed, draw_seed in paired_seeds("g2-zero-skill", _SEEDS):
        cov = coverage_on_seasonal(
            int(window_seed % (2**32)),
            make_forecaster,
            alpha=_ALPHA,
            split_seed=int(draw_seed % (2**32)),  # exchangeable cal/eval split
        )
        rates.append(cov.rate)
        widths.append(cov.median_width)
    return rates, widths


@pytest.mark.gate
def test_g2_holt_winters_beats_constant_forecaster_width_at_equal_coverage() -> None:
    hw_rates, hw_widths = _coverage_and_widths(HoltWintersForecaster)
    const_rates, const_widths = _coverage_and_widths(ConstantForecaster)

    # (1) neither forecaster under-covers (same one-sided guard as G1).
    assert t_upper_bound(hw_rates) >= _NOMINAL, f"HW under-covers (mean={np.mean(hw_rates):.4f})"
    assert t_upper_bound(const_rates) >= _NOMINAL, (
        f"Const under-covers (mean={np.mean(const_rates):.4f})"
    )

    # (2) HW earns its complexity: >= 20% narrower median width (record the actual).
    hw_width = float(np.median(hw_widths))
    const_width = float(np.median(const_widths))
    width_reduction = 1.0 - hw_width / const_width
    assert width_reduction >= _MIN_WIDTH_REDUCTION, (
        f"HW only {width_reduction:.1%} narrower than the zero-skill control "
        f"(HW={hw_width:.2f}, Const={const_width:.2f}); need >= {_MIN_WIDTH_REDUCTION:.0%}"
    )
