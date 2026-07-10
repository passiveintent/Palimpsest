"""Shared test helpers for the synth package (not itself a test module)."""

from __future__ import annotations

import dataclasses
import hashlib
from typing import Any

import numpy as np


def _hash_value(value: object) -> bytes:
    if isinstance(value, np.ndarray):
        return value.tobytes()
    if isinstance(value, bool):
        return b"\x01" if value else b"\x00"
    if isinstance(value, (int, float, str)):
        return repr(value).encode()
    if value is None:
        return b"\xff"
    if isinstance(value, tuple):
        return b"|".join(_hash_value(v) for v in value)
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return fingerprint(value).encode()
    raise TypeError(f"cannot fingerprint value of type {type(value)!r}")


def fingerprint(obj: Any) -> str:
    """A deterministic hex digest of every field of a (possibly nested) dataclass.

    Used to assert "same seed => byte-identical output": two independent
    calls to a generator with the same seed must fingerprint identically,
    covering every ndarray/scalar field of both the Frame and its
    GroundTruth without hand-listing them per generator.
    """
    parts = [_hash_value(getattr(obj, f.name)) for f in dataclasses.fields(obj)]
    return hashlib.sha256(b"\x00".join(parts)).hexdigest()
