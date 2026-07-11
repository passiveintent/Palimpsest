# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""The mergeable quantile-sketch contract (ADR-003/ADR-004, invariant 5).

Every calibration set in this system is stored as a quantile sketch rather
than a raw sample so that (a) hierarchical shrinkage can *merge* a sparse
bucket with its service/fleet parents (ADR-003) and (b) recalibration can
*reweight* the time-bucketed ring buffer instead of refitting or deleting
(ADR-004, invariant 5). Both operations demand the same algebra, captured
here.

The prompt's contract is the four core operations `add / merge / quantile /
n`. Two more — `scaled` and `relative_accuracy` — are the reweighting
extension ADR-004 needs: a `BucketRing` forms exponential-decay weighted
merges and reversible reweightings by scaling per-bucket counts, and it
derives its G6 comparison tolerance from the sketch's own error bound. They
live on the protocol (not just the concrete class) so the ring stays
sketch-agnostic; any drop-in KLL/t-digest backing must supply them too.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable


@runtime_checkable
class MergeableQuantileSketch(Protocol):
    """A quantile sketch whose merge is associative and commutative.

    Implementations MUST wrap an existing sketch library, never hand-roll
    one (copilot-instructions). The merge must combine the underlying
    frequency representation so that merge order does not change any
    reported quantile beyond the sketch's own rank/relative-error bound —
    the property G6 falsifies.
    """

    @property
    def relative_accuracy(self) -> float:
        """The sketch's relative-value-error bound; G6's comparison tolerance."""
        ...

    def add(self, x: float) -> None:
        """Insert a single observation into this sketch (in place)."""
        ...

    def merge(self, other: MergeableQuantileSketch) -> MergeableQuantileSketch:
        """Return a NEW sketch encoding self + other; both operands untouched."""
        ...

    def scaled(self, weight: float) -> MergeableQuantileSketch:
        """Return a NEW sketch with every underlying count multiplied by ``weight``.

        ``weight`` must be >= 0. This is the reweighting primitive ADR-004's
        ring is built on: a bucket weighted by ``w`` is exactly this sketch
        with its counts scaled by ``w``, and ``weight == 0`` yields an
        empty-mass sketch (a masked-out bucket) without touching the
        original data (invariant 5).
        """
        ...

    def quantile(self, q: float) -> float:
        """Approximate value at quantile ``q`` in [0, 1]; raise if empty."""
        ...

    def n(self) -> int:
        """Number of observations inserted (unit-weight count)."""
        ...
