# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Tracked (non-gating) measurement of coverage drift under decay < 1.

ADR-004: at ``decay < 1`` the ring weights are exponential-decay weights, not
likelihood ratios, so weighted-exchangeability theory does not apply and the
split-conformal finite-sample guarantee formally lapses — maintaining coverage
then becomes DtACI/ACI's job. On *stationary* seasonal data the drift is
small; this test pins it so a decay regression (or a coverage cliff at some
decay) becomes visible, without gating the build on the exact number.
"""

from __future__ import annotations

from pconformal.forecast import ConstantForecaster
from tests.calibrate._pipeline import coverage_on_seasonal
from tests.seeds import paired_seeds

_ALPHA = 0.1


def test_coverage_under_decay_is_tracked_and_stays_bounded() -> None:
    for decay in (1.0, 0.98, 0.95):
        covered = 0
        n_test = 0
        for window_seed, _ in paired_seeds("decay-drift", 4):
            cov = coverage_on_seasonal(
                int(window_seed % (2**32)),
                ConstantForecaster,
                alpha=_ALPHA,
                decay=decay,
                n_buckets=8,
            )
            covered += cov.covered
            n_test += cov.n_test
        rate = covered / n_test
        # Loose, non-gating band: on stationary data decay must not collapse
        # coverage. A tighter regression signal is DtACI's job under real drift
        # (G3/G4, prompt 11); this only guards against a decay-induced cliff.
        assert 0.85 <= rate <= 0.94, f"decay={decay}: coverage {rate:.4f} over {n_test} windows"
