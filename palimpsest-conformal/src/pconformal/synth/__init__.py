"""Deterministic synthetic fixture generators for the G1-G8 gates.

Every generator is `gen(seed, ...) -> (Frame, GroundTruth)`, seeded via
`numpy.random.default_rng(seed)` only, so the same seed always reproduces
byte-identical output (see `tests/synth/test_determinism.py`).
"""

from pconformal.synth._frame import Frame
from pconformal.synth.bimodal_fleet import BimodalFleetGroundTruth, bimodal_fleet
from pconformal.synth.heavy_tail import HeavyTailGroundTruth, heavy_tail
from pconformal.synth.heteroscedastic import HeteroscedasticGroundTruth, heteroscedastic_pair
from pconformal.synth.incidents import IncidentGroundTruth, inject_incidents
from pconformal.synth.regime_shift import Kind, RegimeShiftGroundTruth, Target, regime_shift
from pconformal.synth.seasonal import (
    SeasonalGroundTruth,
    default_hour_of_week_profile,
    seasonal_latency,
)
from pconformal.synth.slow_burn import SlowBurnGroundTruth, slow_burn

__all__ = [
    "Frame",
    "Kind",
    "Target",
    "seasonal_latency",
    "SeasonalGroundTruth",
    "default_hour_of_week_profile",
    "heteroscedastic_pair",
    "HeteroscedasticGroundTruth",
    "heavy_tail",
    "HeavyTailGroundTruth",
    "regime_shift",
    "RegimeShiftGroundTruth",
    "slow_burn",
    "SlowBurnGroundTruth",
    "bimodal_fleet",
    "BimodalFleetGroundTruth",
    "inject_incidents",
    "IncidentGroundTruth",
]
