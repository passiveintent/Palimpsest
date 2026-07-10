# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G4: input shift. See docs/GATES.md#g4--input-shift."""

import pytest


@pytest.mark.gate
def test_g4_recalibration_completes_within_budget_with_bounded_false_pages() -> None:
    pytest.skip("G4 not yet implemented — blocked on drift/calibrate; see docs/GATES.md")
