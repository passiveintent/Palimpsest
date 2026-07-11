# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""DtACI: Dynamically-tuned ACI (Gibbs & Candès 2022).

An ensemble of ACI experts, one per candidate step size `gamma`, aggregated by
exponential weights so the method tracks distribution shift without hand-tuning
a single learning rate. Reference: Gibbs & Candès, "Conformal Inference for
Online Prediction with Arbitrary Distribution Shifts" (2022), the DtACI /
aggregated-ACI algorithm.

=====================================================================
DEVIATION-CHECK — review this block line-by-line against the paper before
production (the build playbook flags DtACI weighting as a known failure point):

1. LOSS (scale-verified). Experts are weighted by the PINBALL (quantile) loss
   at the target level tau = 1 - alpha_target,
   `pinball(S_t, r_k) = tau*(S_t-r_k)_+ + (1-tau)*(r_k-S_t)_+`, NOT the raw 0/1
   coverage indicator. This is the paper's choice; the prompt's phrase
   "coverage loss" was imprecise. The loss is evaluated on the SCORE/RADIUS
   scale — realized nonconformity score `S_t` vs the predicted quantile
   `r_k = radius_fn(alpha_k)` — NOT on the alpha scale. Verified.
2. AGGREGATION (variant shipped). The output alpha is the DETERMINISTIC
   weight-average of the experts' alphas (`alpha = w . alphas`). Gibbs & Candès
   also give a RANDOMIZED variant (sample one expert with prob w_k); their
   regret bound is stated for the randomized draw, the weighted average is the
   common deterministic practical choice. We ship the weighted average. If you
   need the paper's exact regret guarantee, switch to sampling.
3. WEIGHT UPDATE. `w_k <- normalize(w_k * exp(-eta * loss_k))` followed by the
   fixed-share mixing `w_k <- (1-sigma)*w_k + sigma/K`. Mixing is what stops a
   single expert's weight from collapsing to ~0 and never recovering after a
   regime change.
4. HYPERPARAMETERS (no magic numbers). `eta` and `sigma` are NOT constants:
   they are the closed forms in `dtaci_hyperparameters`, parameterized by the
   expert count and a declared localization `interval` (bind it to the
   calibration window length). Overridable only for tests. The exact algebraic
   form of eta/sigma still needs verifying against the paper's equations — see
   `dtaci_hyperparameters`.
5. EXPERT ALPHAS. Each expert runs the plain ACI recursion on its own alpha
   using its own coverage error `err_k = 1{S_t > r_k}`.
=====================================================================
"""

from __future__ import annotations

import math
from collections.abc import Sequence

import numpy as np
import numpy.typing as npt

from pconformal.calibrate._radius import RadiusFn

_MIN_ALPHA = 1e-6

_DEFAULT_GAMMAS: tuple[float, ...] = (0.002, 0.008, 0.032, 0.128)


def dtaci_hyperparameters(
    alpha_target: float, n_experts: int, interval: int
) -> tuple[float, float]:
    """Gibbs & Candès (2022) closed forms for the DtACI step size and mixing rate.

    Parameterized by the expert count ``k = n_experts`` and a localization
    horizon ``interval`` (natural binding: the calibration window length).
    Returns ``(eta, sigma)``:

        sigma = 1 / (2 * interval)
        eta   = sqrt(3 * (ln(k * interval) + 2) / interval) / (alpha * (1 - alpha))

    The `alpha*(1-alpha)` denominator is the simplified form of the paper's
    loss-scale term `(1-alpha)^2 * alpha^3 + alpha^2 * (1-alpha)^3`, which
    factors exactly to `(alpha * (1-alpha))^2` under the square root.

    DEVIATION-CHECK: verify this algebraic form against the paper's stated
    equations before production; the horizon binding (interval := calibration
    window) is ours, not the paper's.
    """
    if not 0.0 < alpha_target < 1.0:
        raise ValueError("alpha_target must be in (0, 1)")
    if n_experts < 1:
        raise ValueError("n_experts must be >= 1")
    if interval < 2:
        raise ValueError("interval must be >= 2")
    sigma = 1.0 / (2.0 * interval)
    eta = math.sqrt(3.0 * (math.log(n_experts * interval) + 2.0) / interval) / (
        alpha_target * (1.0 - alpha_target)
    )
    return eta, sigma


class DtACI:
    """Ensemble of ACI experts aggregated by exponential pinball-loss weights."""

    def __init__(
        self,
        *,
        alpha_target: float,
        alpha_max: float,
        interval: int,
        gammas: Sequence[float] = _DEFAULT_GAMMAS,
        eta: float | None = None,
        sigma: float | None = None,
        alpha_init: float | None = None,
    ) -> None:
        if not 0.0 < alpha_target < 1.0:
            raise ValueError("alpha_target must be in (0, 1)")
        if not 0.0 < alpha_max < 1.0:
            raise ValueError("alpha_max must be in (0, 1)")
        if alpha_target > alpha_max:
            raise ValueError("alpha_target must be <= alpha_max")
        if len(gammas) == 0:
            raise ValueError("need at least one gamma")
        if any(g <= 0.0 for g in gammas):
            raise ValueError("all gammas must be > 0")

        # eta/sigma default to the paper's closed forms bound to `interval`
        # (no magic constants); an explicit value overrides only for tests.
        default_eta, default_sigma = dtaci_hyperparameters(alpha_target, len(gammas), interval)
        self._eta = default_eta if eta is None else eta
        self._sigma = default_sigma if sigma is None else sigma
        if self._eta <= 0.0 or not 0.0 <= self._sigma <= 1.0:
            raise ValueError("eta must be > 0 and sigma in [0, 1]")

        self._alpha_target = alpha_target
        self._alpha_max = alpha_max
        self._interval = interval
        self._gammas = np.asarray(gammas, dtype=np.float64)
        k = self._gammas.shape[0]
        init = alpha_target if alpha_init is None else alpha_init
        self._alphas = np.full(k, self._clip(init), dtype=np.float64)
        self._weights = np.full(k, 1.0 / k, dtype=np.float64)

    @property
    def alpha(self) -> float:
        """Aggregated miscoverage level.

        DEVIATION-CHECK item 2: the DETERMINISTIC weight-average of the
        experts' alphas (paper also has a randomized-selection variant).
        """
        return float(self._weights @ self._alphas)

    @property
    def eta(self) -> float:
        return self._eta

    @property
    def sigma(self) -> float:
        return self._sigma

    @property
    def weights(self) -> npt.NDArray[np.float64]:
        """The (normalized) expert weights — a copy, for inspection/tests."""
        return self._weights.copy()

    @property
    def expert_alphas(self) -> npt.NDArray[np.float64]:
        return self._alphas.copy()

    def update(self, score: float, radius_fn: RadiusFn) -> float:
        """Fold in the realized ``score``; return the new aggregated alpha.

        ``radius_fn(alpha)`` returns the band radius an expert running at
        ``alpha`` would use this step (typically ``SplitConformal.radius``).
        """
        tau = 1.0 - self._alpha_target
        radii = np.array([radius_fn(float(a)) for a in self._alphas], dtype=np.float64)

        # (1) pinball loss per expert on the SCORE/RADIUS scale: realized score
        # vs the expert's predicted quantile r_k. (5) coverage error.
        diff = score - radii
        losses = np.where(diff > 0.0, tau * diff, (tau - 1.0) * diff)
        errs = (diff > 0.0).astype(np.float64)

        # (3) exponential weighting + fixed-share mixing. A +inf radius (empty
        # calibration fallback) yields loss 0 and never destabilizes the update.
        finite = np.isfinite(losses)
        scaled = np.where(finite, np.exp(-self._eta * losses), 1.0)
        self._weights = self._weights * scaled
        total = float(self._weights.sum())
        if total <= 0.0 or not np.isfinite(total):
            self._weights = np.full_like(self._weights, 1.0 / self._weights.shape[0])
        else:
            self._weights = self._weights / total
        self._weights = (1.0 - self._sigma) * self._weights + self._sigma / self._weights.shape[0]

        # (5) each expert steps its own alpha by the plain ACI recursion.
        self._alphas = np.clip(
            self._alphas + self._gammas * (self._alpha_target - errs),
            _MIN_ALPHA,
            self._alpha_max,
        )
        return self.alpha

    def _clip(self, alpha: float) -> float:
        return min(max(alpha, _MIN_ALPHA), self._alpha_max)
