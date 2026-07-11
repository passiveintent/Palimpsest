# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""The coverage pipeline runs per window-statistic (p50/p95/p99), not just medians.

An SRE backtest that only watches medians is worthless — the tail is the
product. The pipeline therefore takes a `statistic` selector. On
`seasonal_latency` specifically, p95/p99 are constant-ratio scalings of p50
(a lognormal with fixed sigma) and the CQR score is affine-invariant, so
coverage is identical across the three here. That is a property of THIS
fixture, not of the pipeline: meaningful tail-vs-median differentiation needs
a fixture with a genuine, non-affine tail (heavy_tail; the EVT path, prompt
10). This test documents both facts.
"""

from __future__ import annotations

from pconformal.forecast import HoltWintersForecaster
from tests.calibrate._pipeline import STATISTICS, coverage_on_seasonal


def test_pipeline_runs_per_statistic_affine_on_seasonal() -> None:
    rates = [
        coverage_on_seasonal(
            0, HoltWintersForecaster, alpha=0.1, statistic=stat, protocol="frozen"
        ).rate
        for stat in STATISTICS
    ]
    # affine copies on this fixture => coverage is (near-)identical across stats.
    assert max(rates) - min(rates) < 0.02
