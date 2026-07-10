# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G2: zero-skill control. See docs/GATES.md#g2--zero-skill-control."""

import pytest


@pytest.mark.gate
def test_g2_holt_winters_beats_constant_forecaster_width_at_equal_coverage() -> None:
    pytest.skip("G2 not yet implemented — blocked on forecast/score; see docs/GATES.md")
