# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Shared fixture container for the synth generators.

ADR-002's universal input schema is windowed p50/p95/p99, so every `Frame`
carries those. Everything else is optional and populated only by the
generators whose gate substrate needs it: `entity` for multi-entity
fixtures (heteroscedastic_pair, bimodal_fleet), `input_features` /
`score` for the drift substrate (regime_shift, slow_burn) where the
forecaster/score modules don't exist yet to derive a real nonconformity
score from raw stats.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import numpy.typing as npt


@dataclass(frozen=True)
class Frame:
    t: npt.NDArray[np.int64]
    p50: npt.NDArray[np.float64]
    p95: npt.NDArray[np.float64]
    p99: npt.NDArray[np.float64]
    entity: npt.NDArray[np.str_] | None = None
    input_features: npt.NDArray[np.float64] | None = None
    score: npt.NDArray[np.float64] | None = None
