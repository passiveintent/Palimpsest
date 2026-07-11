# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""SplitConformal: the conformal band radius over a sketch ring (ADR-002/004).

Holds a `BucketRing` of nonconformity-score sketches (ADR-002's studentized
CQR scores) and turns them into a band radius: the weighted-merge quantile of
the calibration scores at the finite-sample split-conformal level
`ceil((n+1)*(1-alpha)) / n`. A new observation with score `s` is covered iff
`s <= radius`, i.e. iff `y` lands in `[q_lo - radius*mad, q_hi + radius*mad]`.

Two invariants live here:

- **Invariant 4 (non-influence / fail-blind).** `mask_incident_windows`
  returns a weight vector with the confirmed-incident buckets zeroed. The
  structural guarantee is *non-influence*: a masked bucket contributes zero
  weight to the merge, so confirmed-incident scores can never widen a band
  *through inclusion* — the channel by which an incident could inflate its
  own calibration set is closed by construction. (Whether masking then
  *tightens* the band is a separate, empirical fact — true only when the
  masked scores are high, which they are for real incidents; it is asserted
  on injected incidents in the tests, not claimed as a theorem here.)
- **Invariant 5 (reweight, never delete).** Masking is a pure weight
  operation on the ring (`BucketRing.reweight`); the score data is untouched
  and the pre-mask radius stays reconstructible from the base weights.
"""

from __future__ import annotations

import math

import numpy as np
import numpy.typing as npt

from pconformal.sketch import BucketRing, Provenance

_Weights = npt.NDArray[np.float64]


class SplitConformal:
    """Split-conformal band radius backed by a reweightable sketch ring."""

    def __init__(
        self,
        *,
        capacity: int = 1,
        decay: float = 1.0,
        relative_accuracy: float = 0.01,
        calibration_horizon: int = 0,
        audit_horizon: int = 0,
    ) -> None:
        # decay defaults to 1.0 (uniform weights = textbook split conformal);
        # ADR-004 recalibration passes decay < 1 to down-weight stale history.
        self._relative_accuracy = float(relative_accuracy)
        self._ring = BucketRing(
            capacity,
            decay=decay,
            relative_accuracy=relative_accuracy,
            calibration_horizon=calibration_horizon,
            audit_horizon=audit_horizon,
        )

    @property
    def ring(self) -> BucketRing:
        return self._ring

    # -- ingestion -------------------------------------------------------

    def open_bucket(self) -> int:
        """Open a new time bucket for incoming calibration scores."""
        return self._ring.open_bucket()

    def add(self, score: float) -> None:
        """Add one nonconformity score to the current bucket."""
        self._ring.add(score)

    def extend(self, scores: npt.NDArray[np.float64]) -> None:
        """Add many nonconformity scores to the current bucket."""
        self._ring.extend(scores)

    # -- calibration -----------------------------------------------------

    def n(self, weights: _Weights | None = None) -> int:
        """Effective (unweighted) count of contributing calibration scores.

        Buckets at zero weight (e.g. masked incident windows) are excluded —
        they are out of calibration, so they must not count toward ``n`` in
        the finite-sample level either.
        """
        ids = self._ring.bucket_ids()
        w = self._ring.base_weights() if weights is None else np.asarray(weights, dtype=np.float64)
        if w.shape != (len(ids),):
            raise ValueError("weights length must equal the number of live buckets")
        return int(sum(self._ring.sketch_at(b).n() for b, wi in zip(ids, w, strict=True) if wi > 0))

    def radius(self, alpha: float, weights: _Weights | None = None) -> float:
        """Band radius = weighted-merge score quantile at the split-conformal level.

        The DDSketch quantile carries a relative-value error of +/- gamma
        (``relative_accuracy``). We convert that two-sided sketch error into a
        one-sided CONSERVATIVE guarantee by inflating the radius toward wider
        coverage by ``gamma * |radius|`` (which is exactly ``(1 + gamma) *
        radius`` for the usual positive radius, and correctly moves a negative
        radius toward +inf too). Shipped guarantee: coverage >= 1 - alpha under
        this inflation, i.e. the sketch approximation can only over-cover, never
        silently under-cover — the only acceptable direction for this product.

        Returns ``+inf`` when the finite-sample level exceeds 1 (too few
        calibration points to certify ``1 - alpha`` — the band covers
        everything, the correct conservative fallback).
        """
        if not 0.0 < alpha < 1.0:
            raise ValueError("alpha must be in (0, 1)")
        n = self.n(weights)
        if n == 0:
            raise ValueError("no calibration scores available for a radius")
        level = math.ceil((n + 1) * (1.0 - alpha)) / n
        if level > 1.0:
            return math.inf
        raw = self._ring.merged_quantile(level, weights)
        return raw + self._relative_accuracy * abs(raw)

    def mask_incident_windows(self, mask: npt.NDArray[np.bool_]) -> _Weights:
        """Zero the weights of confirmed-incident buckets; return the new weights.

        ``mask[i]`` True marks bucket ``i`` as a confirmed incident to drop
        from calibration (invariant 4). The structural guarantee is
        non-influence: masked buckets contribute zero weight, so incident
        scores cannot widen the band through inclusion. Pure and reversible
        (invariant 5): the ring is untouched and `base_weights` re-derives the
        pre-mask state.
        """
        keep = ~np.asarray(mask, dtype=np.bool_)
        return self._ring.reweight(keep)

    def provenance(self, weights: _Weights | None = None) -> Provenance:
        """Which buckets/weights produced the current calibration (invariant 6)."""
        return self._ring.provenance(weights)
