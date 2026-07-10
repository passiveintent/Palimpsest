# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

from __future__ import annotations

from tests.seeds import paired_seeds


def test_paired_seeds_deterministic_across_calls() -> None:
    first = paired_seeds("evt-tail-shuffle-control", 25)
    second = paired_seeds("evt-tail-shuffle-control", 25)
    assert first == second


def test_paired_seeds_shape_and_uniqueness() -> None:
    pairs = paired_seeds("drift-recalibration-ab", 10)
    assert len(pairs) == 10
    assert all(isinstance(p, tuple) and len(p) == 2 for p in pairs)
    assert len({p for p in pairs}) == len(pairs)
    assert all(a != b for a, b in pairs)


def test_paired_seeds_distinguishes_experiment_names() -> None:
    a = paired_seeds("experiment-a", 5)
    b = paired_seeds("experiment-b", 5)
    assert a != b


def test_paired_seeds_rejects_negative_n() -> None:
    try:
        paired_seeds("anything", -1)
    except ValueError:
        return
    raise AssertionError("expected ValueError for negative n")


def test_paired_seeds_empty() -> None:
    assert paired_seeds("anything", 0) == []
