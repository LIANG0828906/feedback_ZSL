"""Deterministic, seed-only pseudo random primitives.

No global/global-seeded NumPy RNG is ever used.  Every value is a pure
function of explicit integer inputs (stress fringe seed, brick number,
sample index ...) so results never depend on call or triangle traversal
order and any single brick can be regenerated in isolation.

The mixing primitive is SplitMix64, evaluated in fully vectorised form on
NumPy uint64 arrays.
"""

from __future__ import annotations

import numpy as np

U64_MASK = np.uint64(0xFFFFFFFFFFFFFFFF)
_U32_MASK = np.uint64(0xFFFFFFFF)


def splitmix64(x: np.ndarray) -> np.ndarray:
    """Vectorised SplitMix64 mixing function for uint64 arrays."""
    with np.errstate(over="ignore"):
        x = (x + np.uint64(0x9E3779B97F4A7C15)) & U64_MASK
    z = x
    with np.errstate(over="ignore"):
        z = ((z ^ (z >> np.uint64(30))) * np.uint64(0xBF58476D1CE4E5B9)) & U64_MASK
        z = ((z ^ (z >> np.uint64(27))) * np.uint64(0x94D049BB133111EB)) & U64_MASK
    return (z ^ (z >> np.uint64(31))) & U64_MASK


def mix(*parts: int | np.ndarray) -> np.ndarray:
    """Hash an arbitrary set of integer parts into uint64 values."""
    acc = np.uint64(0x123456789ABCDEF0)
    for part in parts:
        acc ^= splitmix64(np.asarray(part, dtype=np.uint64))
        with np.errstate(over="ignore"):
            acc = (acc * np.uint64(0x100000001B3)) & U64_MASK
    return splitmix64(acc)


def uniform(seed: int, brick_id: int, channel: int, index: int | np.ndarray) -> np.ndarray:
    """Deterministic uniform samples in [0, 1) as float64.

    ``channel`` names the independent stream (fringe params, bubble table,
    jitter samples ...), keeping different sample tables independent while
    remaining a pure function of (seed, brick_id, channel, index).
    """
    idx = np.asarray(index, dtype=np.uint64)
    hashed = mix(np.uint64(seed), np.uint64(brick_id), np.uint64(channel), idx)
    return (hashed >> np.uint64(11)).astype(np.float64) / float(1 << 53)


def uniform_range(
    seed: int,
    brick_id: int,
    channel: int,
    index: int | np.ndarray,
    low: float,
    high: float,
) -> np.ndarray:
    """Deterministic uniform samples in [low, high)."""
    return low + (high - low) * uniform(seed, brick_id, channel, index)


# Independent stream identifiers used across the renderer.
CH_FREQUENCY = 1
CH_PHASE = 2
CH_AMPLITUDE = 3
CH_HUE = 4
CH_SCATTER = 5
CH_BUBBLE_X = 6
CH_BUBBLE_Y = 7
CH_BUBBLE_R = 8
CH_JITTER = 9
CH_JITTER_V = 10


def golden_spiral_samples(count: int) -> np.ndarray:
    """Return ``count`` fixed low-discrepancy unit-disc samples.

    The golden-angle spiral needs no random state at all; combined with the
    deterministic hash jitter streams it gives reproducible soft shadows.
    """
    if count <= 0:
        return np.zeros((0, 2), dtype=np.float64)
    i = np.arange(count, dtype=np.float64) + 0.5
    golden_angle = np.pi * (3.0 - np.sqrt(5.0))
    radius = np.sqrt(i / count)
    theta = i * golden_angle
    return np.stack([radius * np.cos(theta), radius * np.sin(theta)], axis=1)
