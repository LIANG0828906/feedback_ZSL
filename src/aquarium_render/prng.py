"""Small fixed PRNG, independent of NumPy/random implementation versions."""

from __future__ import annotations

import math

from .canonical import fnv1a64

MASK64 = 0xFFFFFFFFFFFFFFFF


def splitmix64(value: int) -> int:
    value = (value + 0x9E3779B97F4A7C15) & MASK64
    value = ((value ^ (value >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
    value = ((value ^ (value >> 27)) * 0x94D049BB133111EB) & MASK64
    return (value ^ (value >> 31)) & MASK64


class StableRandom:
    """SplitMix64 stream keyed by seed, component id and a channel name."""

    def __init__(self, seed: int, component_id: str, channel: str):
        mixed = (
            (int(seed) & MASK64)
            ^ fnv1a64(component_id)
            ^ fnv1a64(channel)
            ^ 0xA0761D6478BD642F
        )
        self.state = splitmix64(mixed)

    def next_u64(self) -> int:
        self.state = (self.state + 0x9E3779B97F4A7C15) & MASK64
        return splitmix64(self.state)

    def random(self) -> float:
        return (self.next_u64() >> 11) * (1.0 / 9007199254740992.0)

    def uniform(self, low: float, high: float) -> float:
        return low + (high - low) * self.random()

    def gaussian(self) -> float:
        # Box-Muller with one fixed stream. The clamp prevents log(0).
        u1 = max(self.random(), 1e-15)
        u2 = self.random()
        return math.sqrt(-2.0 * math.log(u1)) * math.cos(2.0 * math.pi * u2)

