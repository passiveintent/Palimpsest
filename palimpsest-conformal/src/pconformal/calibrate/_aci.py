# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""ACI: Adaptive Conformal Inference (Gibbs & Candès 2021).

The adaptive-miscoverage recursion that is the "I" term of the PID framing:

    alpha_{t+1} = alpha_t + gamma * (alpha_target - err_t),
    err_t = 1{ y_t outside the band at alpha_t },

with `alpha` clipped to `(0, alpha_max]`. A miss (`err_t = 1`) pushes alpha
down, widening the next band; a run of clean windows lets alpha drift back up
toward `alpha_target`, tightening it. `alpha_max` is the floor on band width
(a ceiling on alpha) — the guardrail that stops adaptation from narrowing the
band into uselessness.
"""

from __future__ import annotations

_MIN_ALPHA = 1e-6  # keep alpha strictly positive so the quantile level stays < 1


class ACI:
    """Single-rate adaptive conformal miscoverage controller."""

    def __init__(
        self,
        *,
        alpha_target: float,
        gamma: float,
        alpha_max: float,
        alpha_init: float | None = None,
    ) -> None:
        if not 0.0 < alpha_target < 1.0:
            raise ValueError("alpha_target must be in (0, 1)")
        if gamma <= 0.0:
            raise ValueError("gamma must be > 0")
        if not 0.0 < alpha_max < 1.0:
            raise ValueError("alpha_max must be in (0, 1)")
        if alpha_target > alpha_max:
            raise ValueError("alpha_target must be <= alpha_max")
        self._alpha_target = alpha_target
        self._gamma = gamma
        self._alpha_max = alpha_max
        self._alpha = self._clip(alpha_target if alpha_init is None else alpha_init)

    @property
    def alpha(self) -> float:
        """The current miscoverage level to form this window's band with."""
        return self._alpha

    def update(self, covered: bool) -> float:
        """Fold in whether the last window was covered; return the new alpha."""
        err = 0.0 if covered else 1.0
        self._alpha = self._clip(self._alpha + self._gamma * (self._alpha_target - err))
        return self._alpha

    def _clip(self, alpha: float) -> float:
        return min(max(alpha, _MIN_ALPHA), self._alpha_max)
