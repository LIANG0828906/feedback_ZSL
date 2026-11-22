"""Path-independent sub-RNG derivation.

Every random decision inside a match is drawn from a sub-generator that is
derived *only* from ``(match_seed, tick, purpose)``.  Because the derivation
never depends on how many random values were consumed before, the same
decision yields the same value on every execution path (realtime, resumed,
replayed).
"""

from __future__ import annotations

import hashlib

import numpy as np

_NAMESPACE = "syncdemo"


def derive_seed(seed: int, tick: int, purpose: str) -> int:
    """Derive a deterministic 128-bit sub-seed."""
    material = f"{_NAMESPACE}:{seed}:{tick}:{purpose}".encode("utf-8")
    digest = hashlib.sha256(material).digest()
    return int.from_bytes(digest[:16], "little")


def derive_rng(seed: int, tick: int, purpose: str) -> np.random.Generator:
    """Return a fresh generator for one specific decision point."""
    return np.random.Generator(np.random.PCG64(derive_seed(seed, tick, purpose)))
