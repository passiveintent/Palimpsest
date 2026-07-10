# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Smoke tests for the "for eyeballing" plot helpers.

Not statistical assertions — just confirms every generator's plot helper
runs end-to-end and writes a non-empty PNG.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from pconformal.synth.plotting import ALL_PLOTS


@pytest.mark.parametrize("plot_fn", ALL_PLOTS, ids=[p.__name__ for p in ALL_PLOTS])
def test_plot_writes_nonempty_png(plot_fn: object, tmp_path: Path) -> None:
    path = plot_fn(out_dir=tmp_path)  # type: ignore[operator]
    assert path.exists()
    assert path.suffix == ".png"
    assert path.stat().st_size > 0
