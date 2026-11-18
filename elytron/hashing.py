"""Stateless integer hashing used by all deterministic sample generators."""

from __future__ import annotations

import math

MASK64 = (1 << 64) - 1
_GOLDEN = 0x9E3779B97F4A7C15
_MIX1 = 0xBF58476D1CE4E5B9
_MIX2 = 0x94D049BB133111EB


def mix64(value: int) -> int:
    """SplitMix64 finalizer over a fixed-width integer."""
    z = value & MASK64
    z = ((z ^ (z >> 30)) * _MIX1) & MASK64
    z = ((z ^ (z >> 27)) * _MIX2) & MASK64
    return (z ^ (z >> 31)) & MASK64


def hash_u64(*values: int) -> int:
    """Hash an ordered tuple without Python's process-randomized hash()."""
    state = 0xD1B54A32D192ED03
    for index, value in enumerate(values):
        word = (int(value) + _GOLDEN * (index + 1)) & MASK64
        state = mix64(state ^ mix64(word))
    return state


def unit_float(*values: int) -> float:
    """Return a deterministic value in [0, 1)."""
    return (hash_u64(*values) >> 11) * (1.0 / float(1 << 53))


def signed_float(*values: int) -> float:
    """Return a deterministic value in [-1, 1)."""
    return unit_float(*values) * 2.0 - 1.0


def table_seed(seed: int, face_id: int, face_seed: int, channel: int) -> int:
    return hash_u64(seed, face_id, face_seed, channel)


def smooth_value_noise(
    seed: int,
    face_id: int,
    face_seed: int,
    channel: int,
    u: float,
    v: float,
    lattice: int = 4,
) -> float:
    """Smooth bilinear value noise, independently recomputable per sample."""
    x = min(max(u, 0.0), 1.0) * lattice
    y = min(max(v, 0.0), 1.0) * lattice
    ix = min(int(math.floor(x)), lattice - 1)
    iy = min(int(math.floor(y)), lattice - 1)
    fx = x - ix
    fy = y - iy
    sx = fx * fx * (3.0 - 2.0 * fx)
    sy = fy * fy * (3.0 - 2.0 * fy)
    x1 = min(ix + 1, lattice)
    y1 = min(iy + 1, lattice)
    v00 = unit_float(seed, face_id, face_seed, channel, ix, iy)
    v10 = unit_float(seed, face_id, face_seed, channel, x1, iy)
    v01 = unit_float(seed, face_id, face_seed, channel, ix, y1)
    v11 = unit_float(seed, face_id, face_seed, channel, x1, y1)
    a = v00 + (v10 - v00) * sx
    b = v01 + (v11 - v01) * sx
    return a + (b - a) * sy
