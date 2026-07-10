"""Deterministic seed derivation for paired A/B stochastic tests.

Engineering discipline (Copilot instructions): "All stochastic tests use
paired seeds from tests/seeds.py. A/B comparisons share seeds. Equal-budget:
compared policies see identical windows." A "pair" here is the two seeds a
single trial needs — one to draw the window/scenario, one for any secondary
stochastic choice (e.g. model init) — and both arms of an A/B comparison
reuse the *same* pair for trial i, so they are exercised against identical
draws and differ only in the policy under test.

Seeds are derived from SHA256(experiment name, trial index, branch label),
never from OS randomness or wall-clock time, so paired_seeds is a pure
function: same (experiment, n) in, same seed pairs out, forever.
"""

from __future__ import annotations

import hashlib

_SEED_BYTES = 8  # low 8 bytes of the digest -> uint64-range seed


def _derive_seed(experiment: str, index: int, branch: str) -> int:
    message = f"{experiment}\0{index}\0{branch}".encode()
    digest = hashlib.sha256(message).digest()
    return int.from_bytes(digest[:_SEED_BYTES], byteorder="big", signed=False)


def paired_seeds(experiment: str, n: int) -> list[tuple[int, int]]:
    """Return `n` deterministic (window_seed, draw_seed) pairs for `experiment`.

    Both seeds in pair `i` must be reused, unmodified, by every policy
    compared under `experiment` so the comparison is equal-budget.
    """
    if n < 0:
        raise ValueError("n must be non-negative")
    return [
        (_derive_seed(experiment, i, "window"), _derive_seed(experiment, i, "draw"))
        for i in range(n)
    ]
