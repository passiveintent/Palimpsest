# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Non-gate contract tests for SplitConformal, ACI, and DtACI (ADR-002/004)."""

from __future__ import annotations

import math

import numpy as np

from pconformal.calibrate import ACI, DtACI, SplitConformal, dtaci_hyperparameters
from pconformal.synth import inject_incidents, seasonal_latency


def _filled(scores: list[float]) -> SplitConformal:
    sc = SplitConformal(capacity=1, decay=1.0, relative_accuracy=0.001)
    sc.open_bucket()
    sc.extend(np.asarray(scores, dtype=np.float64))
    return sc


# -- SplitConformal ------------------------------------------------------


def test_radius_is_the_finite_sample_conformal_quantile() -> None:
    scores = list(np.arange(1.0, 101.0))  # 100 scores: 1..100
    sc = _filled(scores)
    # level = ceil((100+1)*0.9)/100 = ceil(90.9)/100 = 91/100 -> ~91st value.
    radius = sc.radius(0.1)
    assert 90.0 <= radius <= 92.0


def test_radius_is_infinite_when_calibration_is_too_small() -> None:
    # n=5, alpha=0.1: level = ceil(6*0.9)/5 = ceil(5.4)/5 = 6/5 > 1 -> +inf.
    sc = _filled([1.0, 2.0, 3.0, 4.0, 5.0])
    assert math.isinf(sc.radius(0.1))


def test_masking_injected_incidents_tightens_band_and_preserves_data() -> None:
    # Invariant 4's structural guarantee (non-influence) is tested at the ring
    # level (G6: mask == weight zero, data survives). Here we test the
    # *empirical* consequence on real injected incidents: inject_incidents
    # plants elevated values, so masking their bucket lowers the calibration
    # quantile -> the band tightens. Data must survive (invariant 5).
    frame, _ = seasonal_latency(0, weeks=2)
    injected, gt = inject_incidents(frame, seed=0, k=3, magnitude=6.0)
    clean = injected.p50[~gt.mask]
    incident_scores = injected.p50[gt.mask]

    sc = SplitConformal(capacity=2, decay=1.0, relative_accuracy=0.001)
    sc.open_bucket()
    sc.extend(clean)
    incident = sc.open_bucket()
    sc.extend(incident_scores)

    unmasked = sc.radius(0.1)
    mask = np.array([bid == incident for bid in sc.ring.bucket_ids()])
    masked = sc.radius(0.1, sc.mask_incident_windows(mask))

    assert masked < unmasked  # elevated incident scores dropped -> strictly tighter
    assert sc.ring.sketch_at(incident).n() == int(gt.mask.sum())  # data survives


def test_provenance_carries_the_decay_so_the_audit_can_distinguish_bands() -> None:
    # ADR-004: decay=1 bands are finite-sample guaranteed; decay<1 bands are
    # adaptively weighted. The audit must tell them apart, so decay is on the
    # provenance record.
    assert _filled([1.0, 2.0, 3.0]).provenance().decay == 1.0
    adaptive = SplitConformal(capacity=3, decay=0.9, relative_accuracy=0.001)
    for _ in range(3):
        adaptive.open_bucket()
        adaptive.add(1.0)
    assert adaptive.provenance().decay == 0.9


# -- ACI -----------------------------------------------------------------


def test_aci_widens_on_miss_and_tightens_on_streak() -> None:
    aci = ACI(alpha_target=0.1, gamma=0.05, alpha_max=0.3)
    start = aci.alpha
    aci.update(covered=False)  # a miss lowers alpha (widens the band)
    assert aci.alpha < start
    lowered = aci.alpha
    for _ in range(20):
        aci.update(covered=True)  # a clean streak raises alpha back toward target
    assert aci.alpha > lowered


def test_aci_clips_to_alpha_max_and_stays_positive() -> None:
    aci = ACI(alpha_target=0.2, gamma=0.5, alpha_max=0.25)
    for _ in range(50):
        aci.update(covered=True)  # push alpha up relentlessly
    assert 0.0 < aci.alpha <= 0.25
    aci2 = ACI(alpha_target=0.05, gamma=0.9, alpha_max=0.3)
    for _ in range(50):
        aci2.update(covered=False)  # push alpha down relentlessly
    assert aci2.alpha > 0.0


# -- DtACI ---------------------------------------------------------------


def test_dtaci_hyperparameters_match_the_closed_form_and_are_used() -> None:
    alpha, k, interval = 0.1, 4, 500
    eta, sigma = dtaci_hyperparameters(alpha, k, interval)
    # closed form only — no magic constants to forget to pin.
    assert sigma == 1.0 / (2.0 * interval)
    expected_eta = math.sqrt(3.0 * (math.log(k * interval) + 2.0) / interval) / (
        alpha * (1.0 - alpha)
    )
    assert math.isclose(eta, expected_eta, rel_tol=1e-12)
    # DtACI with the 4 default gammas derives eta/sigma from `interval`, not literals.
    dt = DtACI(alpha_target=alpha, alpha_max=0.3, interval=interval)
    assert math.isclose(dt.eta, eta, rel_tol=1e-12)
    assert math.isclose(dt.sigma, sigma, rel_tol=1e-12)


def test_dtaci_weights_stay_normalized_and_alpha_bounded() -> None:
    sc = _filled(list(np.random.default_rng(0).exponential(1.0, 500)))
    dt = DtACI(alpha_target=0.1, alpha_max=0.3, interval=500)
    rng = np.random.default_rng(1)
    for _ in range(200):
        dt.update(float(rng.exponential(1.0)), sc.radius)
        assert math.isclose(float(dt.weights.sum()), 1.0, rel_tol=1e-9)
        assert np.all(dt.weights >= 0.0)
        assert 0.0 < dt.alpha <= 0.3


def test_dtaci_adapts_alpha_down_after_an_upward_score_shift() -> None:
    sc = _filled(list(np.random.default_rng(0).exponential(1.0, 500)))
    dt = DtACI(alpha_target=0.1, alpha_max=0.3, interval=500)
    rng = np.random.default_rng(2)

    for _ in range(300):  # stationary phase: scores ~ Exp(1), calibrated regime
        dt.update(float(rng.exponential(1.0)), sc.radius)
    alpha_before = dt.alpha
    for _ in range(300):  # regime shift: scores double -> systematic under-coverage
        dt.update(float(rng.exponential(2.0)), sc.radius)
    alpha_after = dt.alpha

    # under-coverage should drive alpha DOWN (a wider band) to recover coverage.
    assert alpha_after < alpha_before
