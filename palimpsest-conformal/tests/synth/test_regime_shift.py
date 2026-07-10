# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

from __future__ import annotations

import numpy as np
from hypothesis import given, settings
from hypothesis import strategies as st

from pconformal.synth import regime_shift

_kinds = st.sampled_from(["step", "ramp"])
_targets = st.sampled_from(["inputs", "scores"])


@given(
    n=st.integers(min_value=20, max_value=300),
    kind=_kinds,
    target=_targets,
    magnitude=st.floats(min_value=-5.0, max_value=5.0, allow_nan=False),
    ramp_windows=st.integers(min_value=1, max_value=50),
    n_features=st.integers(min_value=2, max_value=5),
    seed=st.integers(min_value=0, max_value=2**32 - 1),
    data=st.data(),
)
@settings(max_examples=25, deadline=None)
def test_regime_shift_invariants(
    n: int,
    kind: str,
    target: str,
    magnitude: float,
    ramp_windows: int,
    n_features: int,
    seed: int,
    data: st.DataObject,
) -> None:
    t0 = data.draw(st.integers(min_value=0, max_value=n - 1))
    frame, gt = regime_shift(
        seed,
        t0,
        kind,  # type: ignore[arg-type]
        target,  # type: ignore[arg-type]
        n=n,
        magnitude=magnitude,
        ramp_windows=ramp_windows,
        n_features=n_features,
    )

    assert frame.score is not None
    assert frame.input_features is not None
    assert frame.input_features.shape == (n, n_features)
    assert np.allclose(frame.input_features.sum(axis=1), 1.0)
    assert np.all(gt.progress >= 0.0) and np.all(gt.progress <= 1.0)
    if t0 > 0:
        assert gt.progress[0] == 0.0

    if target == "inputs":
        # Score is never touched when the shift targets inputs.
        assert np.all(gt.true_score_mean == 0.0)
    else:
        # Input-feature mix is never touched when the shift targets scores.
        first_row = gt.true_feature_mix[0]
        assert np.allclose(gt.true_feature_mix, first_row)


def test_step_is_discontinuous_and_ramp_is_gradual() -> None:
    _f_step, gt_step = regime_shift(0, t0=100, kind="step", target="scores", n=300)
    assert set(np.unique(gt_step.progress)) <= {0.0, 1.0}

    _f_ramp, gt_ramp = regime_shift(0, t0=100, kind="ramp", target="scores", n=300, ramp_windows=40)
    mid = gt_ramp.progress[120]
    assert 0.0 < mid < 1.0


def test_rejects_invalid_parameters() -> None:
    try:
        regime_shift(0, t0=500, kind="step", target="inputs", n=100)
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError for t0 >= n")

    try:
        regime_shift(0, t0=0, kind="bogus", target="inputs", n=100)  # type: ignore[arg-type]
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError for invalid kind")

    try:
        regime_shift(0, t0=0, kind="step", target="bogus", n=100)  # type: ignore[arg-type]
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError for invalid target")
