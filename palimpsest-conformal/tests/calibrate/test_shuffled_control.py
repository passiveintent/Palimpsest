# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Shuffled control: the coverage guarantee does not depend on time order.

Required control experiment (ADR-002 / engineering discipline): permute the
series in time, rerun the whole scoring + split-conformal path, and confirm
(a) marginal coverage still holds and (b) the forecaster's residual
autocorrelation has collapsed to ~0. If coverage survived the destruction of
all temporal structure, it never secretly relied on it.
"""

from __future__ import annotations

from pconformal.forecast import ConstantForecaster
from tests.calibrate._pipeline import shuffled_control
from tests.seeds import paired_seeds

_ALPHA = 0.1
_NOMINAL = 1.0 - _ALPHA


def test_time_shuffle_keeps_coverage_and_kills_residual_autocorrelation() -> None:
    ordered_acfs: list[float] = []
    shuffled_acfs: list[float] = []
    covered = 0
    n_test = 0

    for window_seed, _ in paired_seeds("shuffled-control", 8):
        acf_ordered, acf_shuffled, cov = shuffled_control(
            int(window_seed % (2**32)), ConstantForecaster, alpha=_ALPHA
        )
        ordered_acfs.append(acf_ordered)
        shuffled_acfs.append(acf_shuffled)
        covered += cov.covered
        n_test += cov.n_test

    rate = covered / n_test
    min_ordered = min(ordered_acfs)
    max_shuffled_abs = max(abs(a) for a in shuffled_acfs)

    # ordered data carries strong serial structure...
    assert min_ordered > 0.3, f"ordered residual acf1 unexpectedly low ({min_ordered:.3f})"
    # ...which shuffling destroys...
    assert max_shuffled_abs < 0.1, f"shuffle left residual autocorrelation ({max_shuffled_abs:.3f})"
    # ...yet marginal coverage survives (exchangeability restored).
    assert 0.87 <= rate <= 0.93, f"shuffled coverage {rate:.4f} over {n_test} windows"
