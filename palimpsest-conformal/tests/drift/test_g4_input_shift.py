"""G4: input shift. See docs/GATES.md#g4--input-shift."""

import pytest


@pytest.mark.gate
def test_g4_recalibration_completes_within_budget_with_bounded_false_pages() -> None:
    pytest.skip("G4 not yet implemented — blocked on drift/calibrate; see docs/GATES.md")
