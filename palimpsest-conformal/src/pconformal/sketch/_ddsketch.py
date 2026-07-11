# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""DDSketchQuantile: a MergeableQuantileSketch backed by Datadog's DDSketch.

We deliberately do NOT hand-roll the sketch (copilot-instructions). DDSketch
gives a relative-*value* error guarantee: the value returned for quantile q
is within ``relative_accuracy`` of the true value at that rank. Its merge
sums per-bucket counts, so merging is *exactly* associative and commutative
at the store level — differently-ordered merges of the same data return
bit-identical quantiles, comfortably inside the tolerance G6 asserts. (The
gate speaks of a "rank-error" bound generically; DDSketch's concrete bound
is relative value error, exposed as `relative_accuracy`.)

`merge` and `scaled` are pure: they return a new sketch and leave their
operands untouched, which is what lets `BucketRing` reweight without ever
mutating or deleting calibration data (invariant 5).

Weighted merges (ADR-004 exponential decay) and reversible reweighting need
to scale a bucket's counts by an arbitrary real weight, which DDSketch's
public API does not expose. Because a DDSketch store is just additive
real-valued counts, multiplying every count (positive store, negative store,
zero bucket, and the summary count/sum) by ``w`` is exactly "this sketch,
weighted by ``w``" — `scaled` does that on a deep copy.
"""

from __future__ import annotations

import copy

from ddsketch import DDSketch
from ddsketch.store import DenseStore

from pconformal.sketch._protocol import MergeableQuantileSketch

_DEFAULT_RELATIVE_ACCURACY = 0.01


class DDSketchQuantile:
    """A `MergeableQuantileSketch` wrapping :class:`ddsketch.DDSketch`."""

    def __init__(self, relative_accuracy: float = _DEFAULT_RELATIVE_ACCURACY) -> None:
        if not 0.0 < relative_accuracy < 1.0:
            raise ValueError("relative_accuracy must be in (0, 1)")
        self._relative_accuracy = float(relative_accuracy)
        self._sketch = DDSketch(relative_accuracy=self._relative_accuracy)

    @classmethod
    def _wrap(cls, sketch: DDSketch, relative_accuracy: float) -> DDSketchQuantile:
        """Wrap an already-built DDSketch without re-inserting data."""
        obj = cls.__new__(cls)
        obj._relative_accuracy = relative_accuracy
        obj._sketch = sketch
        return obj

    @property
    def relative_accuracy(self) -> float:
        return self._relative_accuracy

    def add(self, x: float) -> None:
        self._sketch.add(float(x))

    def merge(self, other: MergeableQuantileSketch) -> DDSketchQuantile:
        if not isinstance(other, DDSketchQuantile):
            raise TypeError("DDSketchQuantile can only merge with another DDSketchQuantile")
        if other._relative_accuracy != self._relative_accuracy:
            raise ValueError(
                "cannot merge sketches with different relative_accuracy "
                f"({self._relative_accuracy} vs {other._relative_accuracy})"
            )
        merged = copy.deepcopy(self._sketch)
        merged.merge(other._sketch)  # DDSketch.merge mutates `merged`, reads `other`
        return DDSketchQuantile._wrap(merged, self._relative_accuracy)

    def scaled(self, weight: float) -> DDSketchQuantile:
        if weight < 0.0:
            raise ValueError("weight must be >= 0")
        scaled = copy.deepcopy(self._sketch)
        _scale_counts(scaled, float(weight))
        return DDSketchQuantile._wrap(scaled, self._relative_accuracy)

    def quantile(self, q: float) -> float:
        if not 0.0 <= q <= 1.0:
            raise ValueError("q must be in [0, 1]")
        value = self._sketch.get_quantile_value(q)
        if value is None:  # DDSketch returns None for an empty sketch
            raise ValueError("quantile of an empty sketch is undefined")
        return float(value)

    def n(self) -> int:
        # Unit-weight inserts and count-summing merges keep this integer-valued.
        return int(round(self._sketch.count))


def _scale_counts(sketch: DDSketch, weight: float) -> None:
    """Multiply every count in ``sketch`` by ``weight`` in place.

    Scaling the additive count representation is an exact reweighting: the
    reported quantiles are unchanged when ``weight`` is uniform, and combine
    as a true weighted merge when different buckets carry different weights.
    """
    for store in (sketch._store, sketch._negative_store):
        # Our sketches are always built from the default (dense) store; fail
        # loud rather than silently mis-scale an unexpected store type.
        if not isinstance(store, DenseStore):
            raise TypeError("DDSketchQuantile requires DDSketch's default DenseStore backing")
        store.bins = [count * weight for count in store.bins]
        store.count *= weight
    sketch._zero_count *= weight
    sketch._count *= weight
    sketch._sum *= weight
