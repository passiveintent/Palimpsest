"""G7: counterfactual audit. See docs/GATES.md#g7--counterfactual-audit."""

import pytest


@pytest.mark.gate
def test_g7_proposed_alpha_never_suppresses_a_confirmed_incident() -> None:
    pytest.skip("G7 not yet implemented — blocked on flywheel; see docs/GATES.md")
