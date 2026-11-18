"""Deterministic integer random primitives.

All randomness in the project comes from ``splitmix64`` hashes of explicit
integer seeds.  The helpers never touch NumPy's global RNG and are therefore
reproducible byte-for-byte across processes, platforms and call ordering.
"""

from __future__ import annotations

import numpy as np

UINT64_MASK = np.uint64(0xFFFFFFFFFFFFFFFF)
UINT32_MASK = np.uint64(0xFFFFFFFF)


def _as_u64(value: int | np.integer | np.ndarray) -> np.uint64 | np.ndarray:
    return np.asarray(value, dtype=np.uint64)


def splitmix64(seed: int | np.integer | np.ndarray) -> np.uint64 | np.ndarray:
    """Return the splitmix64 hash of one or many unsigned 64-bit seeds."""
    with np.errstate(over="ignore"):
        state = (_as_u64(seed) + np.uint64(0x9E3779B97F4A7C15)) & UINT64_MASK
        state = ((state ^ (state >> np.uint64(30))) *
                 np.uint64(0xBF58476D1CE4E5B9)) & UINT64_MASK
        state = ((state ^ (state >> np.uint64(27))) *
                 np.uint64(0x94D049BB133111EB)) & UINT64_MASK
        return (state ^ (state >> np.uint64(31))) & UINT64_MASK


def hash_mix(*parts: int | np.integer | np.ndarray) -> np.uint64 | np.ndarray:
    """Mix an ordered tuple of integer parts into one deterministic hash."""
    if not parts:
        return np.uint64(0)
    acc = np.uint64(0xCBF29CE484222325)
    with np.errstate(over="ignore"):
        for part in parts:
            value = _as_u64(part)
            acc = (acc ^ splitmix64(value)) * np.uint64(0x100000001B3)
            acc &= UINT64_MASK
        return splitmix64(acc)


def uniform_float(seed: int, *parts: int) -> np.float32 | np.ndarray:
    """Map deterministic hashes to floats in the half-open range [0, 1)."""
    bits = hash_mix(seed, *parts) if parts else splitmix64(seed)
    return (bits >> np.uint64(8)).astype(np.float32) / np.float32(1 << 56)


def uniform_grid(seed: int, layer_index: int, resolution: int) -> np.ndarray:
    """Build a ``resolution x resolution`` float32 table in [0, 1).

    The table depends only on ``seed``, ``layer_index`` and ``resolution``;
    vectorising the hash keeps creation fast while remaining independent of
    any iteration order.
    """
    axis = np.arange(resolution, dtype=np.uint64)
    row, col = np.meshgrid(axis, axis, indexing="ij")
    bits = hash_mix(
        np.uint64(seed),
        np.uint64(layer_index),
        np.uint64(resolution),
        row,
        col,
    )
    return (bits >> np.uint64(8)).astype(np.float32) / np.float32(1 << 56)


def integer_in(seed: int, *parts: int, bound: int) -> np.uint64 | np.ndarray:
    """Return deterministic integers in [0, bound)."""
    bits = hash_mix(seed, *parts)
    return bits % np.uint64(bound)
