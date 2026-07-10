"""G2: zero-skill control. See docs/GATES.md#g2--zero-skill-control."""

import pytest


@pytest.mark.gate
def test_g2_holt_winters_beats_constant_forecaster_width_at_equal_coverage() -> None:
    pytest.skip("G2 not yet implemented — blocked on forecast/score; see docs/GATES.md")
