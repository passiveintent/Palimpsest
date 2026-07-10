"""G8: percentile-merge falsification. See docs/GATES.md#g8--percentile-merge-falsification."""

import pytest


@pytest.mark.gate
def test_g8_naive_per_host_p95_average_diverges_from_true_fleet_p95() -> None:
    pytest.skip("G8 not yet implemented — blocked on triage/sketch; see docs/GATES.md")
