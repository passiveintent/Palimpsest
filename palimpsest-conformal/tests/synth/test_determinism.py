"""Same seed => byte-identical output, for every synth generator.

Each generator is called twice with the same seed and once with a
different seed; the fingerprint (tests/synth/_helpers.py) of (frame,
ground_truth) must match across the two same-seed calls and differ from
the different-seed call.
"""

from __future__ import annotations

from pconformal.synth import (
    bimodal_fleet,
    heavy_tail,
    heteroscedastic_pair,
    inject_incidents,
    regime_shift,
    seasonal_latency,
    slow_burn,
)
from tests.synth._helpers import fingerprint


def test_seasonal_latency_deterministic() -> None:
    a = seasonal_latency(42)
    b = seasonal_latency(42)
    c = seasonal_latency(43)
    assert fingerprint(a[0]) == fingerprint(b[0])
    assert fingerprint(a[1]) == fingerprint(b[1])
    assert fingerprint(a[0]) != fingerprint(c[0])


def test_heteroscedastic_pair_deterministic() -> None:
    a = heteroscedastic_pair(42)
    b = heteroscedastic_pair(42)
    c = heteroscedastic_pair(43)
    assert fingerprint(a[0]) == fingerprint(b[0])
    assert fingerprint(a[1]) == fingerprint(b[1])
    assert fingerprint(a[0]) != fingerprint(c[0])


def test_heavy_tail_deterministic() -> None:
    a = heavy_tail(42)
    b = heavy_tail(42)
    c = heavy_tail(43)
    assert fingerprint(a[0]) == fingerprint(b[0])
    assert fingerprint(a[1]) == fingerprint(b[1])
    assert fingerprint(a[0]) != fingerprint(c[0])


def test_regime_shift_deterministic() -> None:
    a = regime_shift(42, t0=50, kind="step", target="inputs")
    b = regime_shift(42, t0=50, kind="step", target="inputs")
    c = regime_shift(43, t0=50, kind="step", target="inputs")
    assert fingerprint(a[0]) == fingerprint(b[0])
    assert fingerprint(a[1]) == fingerprint(b[1])
    assert fingerprint(a[0]) != fingerprint(c[0])


def test_slow_burn_deterministic() -> None:
    a = slow_burn(42, rate=0.03)
    b = slow_burn(42, rate=0.03)
    c = slow_burn(43, rate=0.03)
    assert fingerprint(a[0]) == fingerprint(b[0])
    assert fingerprint(a[1]) == fingerprint(b[1])
    assert fingerprint(a[0]) != fingerprint(c[0])


def test_bimodal_fleet_deterministic() -> None:
    a = bimodal_fleet(42, hosts=12, mix=0.3)
    b = bimodal_fleet(42, hosts=12, mix=0.3)
    c = bimodal_fleet(43, hosts=12, mix=0.3)
    assert fingerprint(a[0]) == fingerprint(b[0])
    assert fingerprint(a[1]) == fingerprint(b[1])
    assert fingerprint(a[0]) != fingerprint(c[0])


def test_inject_incidents_deterministic() -> None:
    base, _ = seasonal_latency(0, weeks=2)
    a = inject_incidents(base, seed=42, k=3)
    b = inject_incidents(base, seed=42, k=3)
    c = inject_incidents(base, seed=43, k=3)
    assert fingerprint(a[0]) == fingerprint(b[0])
    assert fingerprint(a[1]) == fingerprint(b[1])
    assert fingerprint(a[0]) != fingerprint(c[0])
