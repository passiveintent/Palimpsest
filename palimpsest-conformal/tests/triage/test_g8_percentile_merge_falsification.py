# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""G8: percentile-merge falsification. See docs/GATES.md#g8--percentile-merge-falsification."""

import pytest


@pytest.mark.gate
def test_g8_naive_per_host_p95_average_diverges_from_true_fleet_p95() -> None:
    pytest.skip("G8 not yet implemented — blocked on triage/sketch; see docs/GATES.md")
