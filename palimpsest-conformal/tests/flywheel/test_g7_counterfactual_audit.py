# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G7: counterfactual audit. See docs/GATES.md#g7--counterfactual-audit."""

import pytest


@pytest.mark.gate
def test_g7_proposed_alpha_never_suppresses_a_confirmed_incident() -> None:
    pytest.skip("G7 not yet implemented — blocked on flywheel; see docs/GATES.md")
