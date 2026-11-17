"""Deterministic, dependency-light pseudo random generator (SplitMix64).

The same (seed, token...) tuple always produces the same byte sequence on
every supported Python/NumPy version, which is the backbone of the
renderer's byte-level reproducibility guarantee.
"""

from __future__ import annotations

import numpy as np

MASK64 = (1 << 64) - 1
GOLDEN = 0x9E3779B97F4A7C15


def splitmix64(state: int) -> int:
    state = (state + GOLDEN) & MASK64
    z = state
    z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
    z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & MASK64
    return z ^ (z >> 31)


def seed_mix(seed: int, *tokens: object) -> int:
    """FNV-1a style mixing of an integer seed with string/int tokens."""
    h = seed & MASK64
    for token in tokens:
        if isinstance(token, str):
            data = token.encode("utf-8")
        elif isinstance(token, (int, np.integer)):
            data = int(token).to_bytes(8, "little", signed=False)
        elif isinstance(token, float):
            data = repr(token).encode("utf-8")
        else:
            data = str(token).encode("utf-8")
        for byte in data:
            h ^= byte
            h = (h * 0x100000001B3) & MASK64
    return h


class DeterministicRng:
    """SplitMix64 based generator with stable float/noise helpers."""

    def __init__(self, seed: int) -> None:
        self.state = seed & MASK64

    def next_u64(self) -> int:
        self.state = splitmix64(self.state)
        return self.state

    def uniform(self) -> float:
        # 53-bit mantissa float in [0, 1)
        return (self.next_u64() >> 11) / float(1 << 53)

    def uniform_range(self, low: float, high: float) -> float:
        return low + (high - low) * self.uniform()

    def gaussian(self) -> float:
        # Box-Muller transform, guarded against log(0)
        u1 = max(self.uniform(), 1e-15)
        u2 = self.uniform()
        return float(np.sqrt(-2.0 * np.log(u1)) * np.cos(2.0 * np.pi * u2))

    def noise(self, n: int) -> np.ndarray:
        return np.array([self.gaussian() for _ in range(int(n))], dtype=np.float64)

