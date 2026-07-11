# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Non-gate contract tests for the CQR nonconformity score (ADR-002).

Covers the two properties the score's shape is chosen for — affine-scaling
invariance and the sign convention — plus vectorization, the eps floor, and
the no-pandas discipline. (Coverage itself is G1, which stays blocked on
calibration.)
"""

from __future__ import annotations

import subprocess
import sys

import numpy as np
import pytest
from hypothesis import given, settings
from hypothesis import strategies as st

from pconformal.score import score


def test_formula_matches_definition() -> None:
    # y below, inside, above a band of [10, 20] with mad=2.
    result = score(np.array([5.0, 15.0, 26.0]), 10.0, 20.0, 2.0)
    # below: max(10-5, 5-20)=5 -> 2.5 ; inside: max(-5,-5)=-5 -> -2.5 ; above: max(-16,6)=6 -> 3.0
    assert np.allclose(result, [2.5, -2.5, 3.0])


def test_sign_convention_inside_band_is_negative() -> None:
    q_lo, q_hi, mad = 10.0, 20.0, 3.0
    assert score(15.0, q_lo, q_hi, mad) < 0.0  # strictly inside
    assert score(10.0, q_lo, q_hi, mad) == 0.0  # on the lower edge
    assert score(20.0, q_lo, q_hi, mad) == 0.0  # on the upper edge
    assert score(9.0, q_lo, q_hi, mad) > 0.0  # below the band
    assert score(21.0, q_lo, q_hi, mad) > 0.0  # above the band


@given(
    y=st.floats(min_value=-1e3, max_value=1e3, allow_nan=False),
    lo=st.floats(min_value=-1e3, max_value=1e3, allow_nan=False),
    width=st.floats(min_value=0.0, max_value=1e3, allow_nan=False),
    mad=st.floats(min_value=1e-2, max_value=1e2, allow_nan=False),
    c=st.floats(min_value=0.1, max_value=10.0, allow_nan=False),
    b=st.floats(min_value=-1e3, max_value=1e3, allow_nan=False),
)
@settings(max_examples=300, deadline=None)
def test_affine_scaling_invariance(
    y: float, lo: float, width: float, mad: float, c: float, b: float
) -> None:
    hi = lo + width
    plain = score(y, lo, hi, mad)
    # scale by c > 0 and shift by b; MAD scales with the data (shift-invariant).
    scaled = score(c * y + b, c * lo + b, c * hi + b, c * mad)
    assert np.allclose(plain, scaled, rtol=1e-6, atol=1e-9)


def test_vectorized_matches_elementwise() -> None:
    rng = np.random.default_rng(0)
    y = rng.normal(100, 20, 256)
    q_lo = rng.normal(80, 5, 256)
    q_hi = q_lo + rng.uniform(5, 40, 256)
    mad = rng.uniform(0.5, 5.0, 256)

    vectorized = score(y, q_lo, q_hi, mad)
    elementwise = np.array(
        [float(score(y[i], q_lo[i], q_hi[i], mad[i])) for i in range(y.size)]
    )
    assert np.array_equal(vectorized, elementwise)


def test_eps_floor_keeps_zero_mad_finite() -> None:
    result = score(30.0, 10.0, 20.0, 0.0, eps=1e-3)
    assert np.isfinite(result)
    assert result == pytest.approx((30.0 - 20.0) / 1e-3)  # numerator / eps


def test_rejects_nonpositive_eps() -> None:
    with pytest.raises(ValueError, match="eps"):
        score(1.0, 0.0, 2.0, 1.0, eps=0.0)


def test_score_module_does_not_import_pandas() -> None:
    # statsmodels pulls pandas in-process, so check a clean subprocess: the
    # score path itself must be numpy-only (ADR-002 "no pandas in hot paths").
    proc = subprocess.run(
        [sys.executable, "-c", "import pconformal.score, sys; assert 'pandas' not in sys.modules"],
        capture_output=True,
        text=True,
    )
    assert proc.returncode == 0, proc.stderr
