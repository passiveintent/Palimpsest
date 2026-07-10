# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""inject_incidents: plants labeled confirmed-incident windows onto an existing frame.

Invariant 4 requires confirmed-incident windows to be masked out of
calibration. This generator produces the mask a calibration routine would
need to do that: given any single-time-axis `Frame` (e.g. from
`seasonal_latency`), it plants `k` non-overlapping incident windows and
returns both the perturbed frame and the ground-truth boolean mask marking
exactly those windows.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import numpy.typing as npt

from pconformal.synth._frame import Frame


@dataclass(frozen=True)
class IncidentGroundTruth:
    mask: npt.NDArray[np.bool_]
    starts: npt.NDArray[np.int64]
    incident_len: int
    magnitude: float


def inject_incidents(
    series: Frame,
    seed: int,
    k: int,
    *,
    incident_len: int = 5,
    magnitude: float = 5.0,
) -> tuple[Frame, IncidentGroundTruth]:
    """Plant `k` non-overlapping `incident_len`-window spikes into `series`.

    `p50/p95/p99` are multiplied by `magnitude` on incident windows (best
    suited to positive-valued series such as `seasonal_latency`'s output);
    `score`, if present, gets an additive `+magnitude` spike instead, since
    a score channel is centered near zero and a multiplicative spike there
    would do nothing. Assumes a single logical time axis — for a
    multi-entity frame (`entity` set), slice per entity before calling
    this, since incidents are placed in absolute array-index space.
    """
    n = series.p50.shape[0]
    if k < 1:
        raise ValueError("k must be >= 1")
    if incident_len < 1:
        raise ValueError("incident_len must be >= 1")
    if k * incident_len > n:
        raise ValueError("k * incident_len must not exceed len(series)")

    rng = np.random.default_rng(seed)
    block = n // k
    if block < incident_len:
        raise ValueError("not enough room to place k non-overlapping incidents")

    starts = np.array(
        [rng.integers(i * block, i * block + block - incident_len + 1) for i in range(k)],
        dtype=np.int64,
    )

    mask = np.zeros(n, dtype=np.bool_)
    for s in starts:
        mask[s : s + incident_len] = True

    p50 = series.p50.copy()
    p95 = series.p95.copy()
    p99 = series.p99.copy()
    p50[mask] *= magnitude
    p95[mask] *= magnitude
    p99[mask] *= magnitude

    score = None
    if series.score is not None:
        score = series.score.copy()
        score[mask] += magnitude

    frame = Frame(
        t=series.t,
        p50=p50,
        p95=p95,
        p99=p99,
        entity=series.entity,
        input_features=series.input_features,
        score=score,
    )
    ground_truth = IncidentGroundTruth(
        mask=mask, starts=starts, incident_len=incident_len, magnitude=magnitude
    )
    return frame, ground_truth
