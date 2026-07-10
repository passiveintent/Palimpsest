# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G1: coverage. See docs/GATES.md#g1--coverage."""

import pytest


@pytest.mark.gate
def test_g1_marginal_and_per_stratum_coverage() -> None:
    pytest.skip("G1 not yet implemented — blocked on score/calibrate; see docs/GATES.md")
