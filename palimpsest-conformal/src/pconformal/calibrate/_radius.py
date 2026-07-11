# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Shared type alias: a band-radius function of the miscoverage level.

DtACI's experts each need the band radius their own ``alpha`` would produce
this step; `SplitConformal.radius` (with calibration weights bound) is the
canonical implementation.
"""

from __future__ import annotations

from collections.abc import Callable

RadiusFn = Callable[[float], float]
