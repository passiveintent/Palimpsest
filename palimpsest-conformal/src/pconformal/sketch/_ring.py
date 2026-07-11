# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""BucketRing: the time-bucketed sketch ring buffer and its provenance.

This is ADR-004's recalibration substrate and the object invariant 5 is
about: "Recalibration = weight operations on the time-bucketed sketch ring
buffer. Calibration data is never deleted." Every recalibration move ADR-004
names — exponential-decay weighting of history, zeroing pre-changepoint
buckets on a changepoint, annealing back to steady state — is expressed here
as a pure transform of a *weight vector*. The sketches (the data) are never
touched, so any "what did the band look like before this changepoint" query
stays answerable, and reweighting is exactly reversible (G6).

`reweight` and `boost` return new weight vectors; they never mutate the ring
or the weights handed to them. Capacity-driven eviction of the oldest bucket
is the buffer's finite retention boundary — a separate concern from
recalibration, which is what invariant 5 forbids deleting through.
"""

from __future__ import annotations

from collections.abc import Iterable, Sequence
from dataclasses import dataclass
from enum import Enum

import numpy as np
import numpy.typing as npt

from pconformal.sketch._ddsketch import DDSketchQuantile
from pconformal.sketch._protocol import MergeableQuantileSketch

_Weights = npt.NDArray[np.float64]
_WeightsLike = Sequence[float] | npt.NDArray[np.float64]
_MaskLike = Sequence[bool] | npt.NDArray[np.bool_]


class CalibrationProvenance(Enum):
    """Which weighting regime produced a band (ADR-003, invariant 6).

    A single `BucketRing` is one stratum's own history, so it always reports
    BUCKET_LOCAL. The pooled tiers are assigned by the hierarchical-shrinkage
    layer (ADR-003, `calibrate/strata.py`) when it merges this ring with its
    service- and fleet-scoped parents to rescue a sparse bucket.
    """

    BUCKET_LOCAL = "bucket-local"
    SERVICE_POOLED = "service-pooled"
    FLEET_POOLED = "fleet-pooled"


@dataclass(frozen=True)
class Provenance:
    """Audit record of which buckets and weights produced a merged query.

    `effective_n` is the weighted sample count (sum of ``weight * n`` over
    contributing buckets) — the quantity the shrinkage layer thresholds on
    to decide whether a bucket is rich enough to stand alone (ADR-003).

    `decay` is the ring's decay factor in force. It is on the audit record
    because it changes the *meaning* of the band (ADR-004): at ``decay == 1``
    the weights are uniform and the split-conformal finite-sample guarantee
    holds; at ``decay < 1`` the weights are not likelihood ratios, so that
    guarantee formally lapses and coverage is DtACI/ACI's responsibility. A
    3am reviewer must be able to tell the two apart at a glance.
    """

    bucket_ids: tuple[int, ...]
    weights: tuple[float, ...]
    effective_n: float
    tier: CalibrationProvenance
    decay: float


class BucketRing:
    """A fixed-capacity ring of time-bucketed quantile sketches.

    Buckets are ordered oldest-to-newest; the newest bucket has decay age 0
    and weight 1, older buckets decay by ``decay ** age``. All weight vectors
    (`base_weights`, `merged_quantile`, `reweight`, `boost`, `provenance`)
    are aligned to `bucket_ids()` in that same oldest-to-newest order.
    """

    def __init__(
        self,
        capacity: int,
        *,
        decay: float = 0.9,
        relative_accuracy: float = 0.01,
        calibration_horizon: int = 0,
        audit_horizon: int = 0,
    ) -> None:
        if capacity < 1:
            raise ValueError("capacity must be >= 1")
        if not 0.0 < decay <= 1.0:
            raise ValueError("decay must be in (0, 1]")
        if calibration_horizon < 0 or audit_horizon < 0:
            raise ValueError("horizons must be >= 0")
        # Retention contract (ADR-004, invariant 5): capacity-driven eviction is
        # a distinct, explicit policy from recalibration, and it must never drop
        # a bucket still needed to recalibrate or to answer a "what did the band
        # look like before this changepoint" audit. Enforced at construction so
        # the auditability promise cannot silently break by mis-sizing the ring.
        required = calibration_horizon + audit_horizon
        if capacity < required:
            raise ValueError(
                f"capacity ({capacity}) must be >= calibration_horizon + "
                f"audit_horizon ({calibration_horizon} + {audit_horizon} = {required}); "
                "eviction would drop calibration/audit data the ADR-004 mechanism needs"
            )
        self._capacity = capacity
        self._decay = float(decay)
        self._relative_accuracy = float(relative_accuracy)
        self._calibration_horizon = calibration_horizon
        self._audit_horizon = audit_horizon
        self._ids: list[int] = []
        self._sketches: list[MergeableQuantileSketch] = []
        self._next_id = 0

    # -- shape -----------------------------------------------------------

    def __len__(self) -> int:
        return len(self._sketches)

    @property
    def capacity(self) -> int:
        return self._capacity

    @property
    def decay(self) -> float:
        return self._decay

    def bucket_ids(self) -> tuple[int, ...]:
        """Live bucket ids, oldest to newest."""
        return tuple(self._ids)

    def sketch_at(self, bucket_id: int) -> MergeableQuantileSketch:
        """The live sketch for ``bucket_id`` (read-only by convention)."""
        return self._sketches[self._index_of(bucket_id)]

    # -- ingestion -------------------------------------------------------

    def open_bucket(self) -> int:
        """Open a new most-recent bucket, evicting the oldest if at capacity.

        Returns the new bucket's stable id. Eviction is retention, not
        deletion-through-recalibration (invariant 5): reweighting never
        removes a bucket, only the buffer aging past `capacity` does.
        """
        bucket_id = self._next_id
        self._next_id += 1
        self._ids.append(bucket_id)
        self._sketches.append(DDSketchQuantile(self._relative_accuracy))
        if len(self._sketches) > self._capacity:
            self._ids.pop(0)
            self._sketches.pop(0)
        return bucket_id

    def add(self, x: float) -> None:
        """Add one observation to the current (newest) bucket, opening one if empty."""
        if not self._sketches:
            self.open_bucket()
        self._sketches[-1].add(x)

    def extend(self, values: Iterable[float]) -> None:
        """Add many observations to the current (newest) bucket."""
        if not self._sketches:
            self.open_bucket()
        sketch = self._sketches[-1]
        for value in values:
            sketch.add(value)

    # -- weights ---------------------------------------------------------

    def base_weights(self) -> _Weights:
        """Steady-state exponential-decay weights, oldest to newest.

        This is also the "annealed" weight vector recalibration returns to
        after a changepoint transient (ADR-004): it is a pure function of
        `decay` and the live bucket count, so it is always reconstructible.
        """
        count = len(self._sketches)
        ages = np.arange(count - 1, -1, -1, dtype=np.float64)  # oldest -> newest
        return np.asarray(self._decay**ages, dtype=np.float64)

    def merged_quantile(self, q: float, weights: _WeightsLike | None = None) -> float:
        """Weighted-merge quantile at level ``q`` (default: decay weights).

        Buckets with zero weight or no data contribute nothing — a masked
        bucket is exactly a weight-zero bucket. Raises if no bucket
        contributes (the visible-degrade path the shrinkage layer catches by
        pooling upward).
        """
        resolved = self._resolve_weights(weights)
        merged: MergeableQuantileSketch | None = None
        for sketch, weight in zip(self._sketches, resolved, strict=True):
            if weight <= 0.0 or sketch.n() == 0:
                continue
            part = sketch.scaled(float(weight))
            merged = part if merged is None else merged.merge(part)
        if merged is None:
            raise ValueError("no calibration data contributes to this query")
        return merged.quantile(q)

    def reweight(self, mask: _MaskLike, weights: _WeightsLike | None = None) -> _Weights:
        """Zero out the weights of masked-out buckets; return a NEW vector.

        ``mask[i]`` True keeps bucket i, False zeroes it (ADR-004's
        pre-changepoint zero-out). Pure: the ring and ``weights`` are
        untouched, and the underlying sketches are never deleted, so
        annealing back (`base_weights`) restores the original query exactly.
        """
        resolved = self._resolve_weights(weights)
        keep = np.asarray(mask, dtype=np.bool_)
        if keep.shape != (len(self._sketches),):
            raise ValueError("mask length must equal the number of live buckets")
        return np.asarray(np.where(keep, resolved, 0.0), dtype=np.float64)

    def boost(
        self,
        bucket_ids: Iterable[int],
        factor: float,
        weights: _WeightsLike | None = None,
    ) -> _Weights:
        """Multiply the given buckets' weights by ``factor``; return a NEW vector.

        Reversibility is by SNAPSHOT RESTORE, not algebraic inverse: because
        this is a pure op that leaves its input and the ring untouched, the
        exact way to undo any recalibration sequence is to restore the
        pre-image weight vector (or recompute `base_weights`), which is
        bit-identical. ``boost(ids, 1 / factor)`` only approximately inverts
        (it accumulates float error), so it must not be relied on for exact
        restoration. ``factor > 0`` is required regardless (use `reweight` to
        zero a bucket).
        """
        if factor <= 0.0:
            raise ValueError("factor must be > 0")
        resolved = self._resolve_weights(weights).copy()
        for bucket_id in bucket_ids:
            resolved[self._index_of(bucket_id)] *= factor
        return resolved

    def provenance(self, weights: _WeightsLike | None = None) -> Provenance:
        """Report which buckets and weights produced a query (invariant 6)."""
        resolved = self._resolve_weights(weights)
        ids: list[int] = []
        used: list[float] = []
        effective_n = 0.0
        for bucket_id, sketch, weight in zip(self._ids, self._sketches, resolved, strict=True):
            if weight <= 0.0 or sketch.n() == 0:
                continue
            ids.append(bucket_id)
            used.append(float(weight))
            effective_n += float(weight) * sketch.n()
        return Provenance(
            bucket_ids=tuple(ids),
            weights=tuple(used),
            effective_n=effective_n,
            tier=CalibrationProvenance.BUCKET_LOCAL,
            decay=self._decay,
        )

    # -- internals -------------------------------------------------------

    def _index_of(self, bucket_id: int) -> int:
        try:
            return self._ids.index(bucket_id)
        except ValueError:
            raise KeyError(f"bucket {bucket_id} is not live in the ring") from None

    def _resolve_weights(self, weights: _WeightsLike | None) -> _Weights:
        if weights is None:
            return self.base_weights()
        resolved = np.asarray(weights, dtype=np.float64)
        if resolved.shape != (len(self._sketches),):
            raise ValueError("weights length must equal the number of live buckets")
        if bool(np.any(resolved < 0.0)):
            raise ValueError("weights must be non-negative")
        return resolved
