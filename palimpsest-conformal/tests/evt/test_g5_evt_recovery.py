"""G5: EVT recovery. See docs/GATES.md#g5--evt-recovery."""

import pytest


@pytest.mark.gate
def test_g5_gpd_parameter_recovery_or_refuse_with_max_band_fallback() -> None:
    pytest.skip("G5 not yet implemented — blocked on evt/score; see docs/GATES.md")
