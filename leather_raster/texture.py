from __future__ import annotations

import numpy as np


_U64 = np.uint64


def _hash_u64(values: np.ndarray) -> np.ndarray:
    with np.errstate(over="ignore"):
        values = np.asarray(values, dtype=np.uint64)
        values ^= values >> _U64(30)
        values *= _U64(0xBF58476D1CE4E5B9)
        values ^= values >> _U64(27)
        values *= _U64(0x94D049BB133111EB)
        values ^= values >> _U64(31)
    return values


def _hash_lattice(ix: np.ndarray, iy: np.ndarray, seed: int, channel: int) -> np.ndarray:
    x = np.asarray(ix, dtype=np.int64).astype(np.uint64)
    y = np.asarray(iy, dtype=np.int64).astype(np.uint64)
    with np.errstate(over="ignore"):
        mixed = (
            x * _U64(0x9E3779B185EBCA87)
            ^ y * _U64(0xC2B2AE3D27D4EB4F)
            ^ _U64((int(seed) & 0xFFFFFFFFFFFFFFFF) + channel * 0xD6E8FEB86659FD93 & 0xFFFFFFFFFFFFFFFF)
        )
    hashed = _hash_u64(mixed)
    return (hashed & _U64(0xFFFFFFFF)).astype(np.float64) / 4294967295.0


def value_noise(u: np.ndarray, v: np.ndarray, seed: int, channel: int) -> np.ndarray:
    ix = np.floor(u).astype(np.int64)
    iy = np.floor(v).astype(np.int64)
    fx = u - ix
    fy = v - iy
    sx = fx * fx * fx * (fx * (fx * 6.0 - 15.0) + 10.0)
    sy = fy * fy * fy * (fy * (fy * 6.0 - 15.0) + 10.0)
    n00 = _hash_lattice(ix, iy, seed, channel)
    n10 = _hash_lattice(ix + 1, iy, seed, channel)
    n01 = _hash_lattice(ix, iy + 1, seed, channel)
    n11 = _hash_lattice(ix + 1, iy + 1, seed, channel)
    nx0 = n00 * (1.0 - sx) + n10 * sx
    nx1 = n01 * (1.0 - sx) + n11 * sx
    return nx0 * (1.0 - sy) + nx1 * sy


def fiber_texture(u: np.ndarray, v: np.ndarray, seed: int) -> np.ndarray:
    """Return seeded [0, 1] leather fiber texture.

    The function is analytic in UV space. It has no RNG state and therefore gives
    identical values for identical coordinates regardless of fragment traversal.
    """

    u = np.asarray(u, dtype=np.float64)
    v = np.asarray(v, dtype=np.float64)

    broad = value_noise(u * 17.0 + v * 0.7, v * 43.0, seed, 1)
    fine = value_noise(u * 61.0 - v * 0.4, v * 137.0, seed, 2)
    cross = value_noise(u * 103.0 + v * 31.0, v * 97.0 - u * 9.0, seed, 3)
    warp = value_noise(u * 11.0, v * 19.0, seed, 4)
    striation = 0.5 + 0.5 * np.sin(
        2.0 * np.pi * (v * 38.0 + u * 3.5 + 2.8 * warp)
    )

    texture = 0.38 * broad + 0.24 * fine + 0.14 * cross + 0.24 * striation
    return np.clip(texture, 0.0, 1.0)

