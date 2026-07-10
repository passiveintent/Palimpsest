"""G3: boiling frog. See docs/GATES.md#g3--boiling-frog."""

import pytest


@pytest.mark.gate
def test_g3_score_drift_never_triggers_sw_and_violation_fires_before_bound() -> None:
    pytest.skip("G3 not yet implemented — blocked on drift/score; see docs/GATES.md")
