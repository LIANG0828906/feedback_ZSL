"""Deterministic integer-seeded pseudo random generation.

PCG32 is implemented on fixed-width integer arithmetic so the sequence is
identical on every supported Python build regardless of platform word size.
No global/unseeded random state is ever touched.
"""

from __future__ import annotations

import numpy as np

_MASK64 = (1 << 64) - 1
_MASK32 = (1 << 32) - 1


class PCG32:
    """A minimal, deterministic PCG32 stream tied to one integer seed."""

    def __init__(self, seed: int) -> None:
        seed = int(seed) & _MASK64
        self._state = 0
        self._inc = 0x14057B7EF767814F
        self._advance()
        self._state = (self._state + seed) & _MASK64
        self._advance()

    def _advance(self) -> None:
        self._state = (
            self._state * 6364136223846793005 + self._inc
        ) & _MASK64

    def next_u32(self) -> int:
        old = self._state
        self._advance()
        xorshifted = (((old >> 18) ^ old) >> 27) & _MASK32
        rot = (old >> 59) & 31
        return ((xorshifted >> rot) | (xorshifted << ((-rot) & 31))) & _MASK32

    def uniform_float(self) -> float:
        return self.next_u32() / float(1 << 32)

    def randrange(self, low: int, high: int) -> int:
        """Uniform integer in the half-open range [low, high)."""

        span = int(high) - int(low)
        if span <= 0:
            raise ValueError("empty randrange")
        return low + (self.next_u32() % span)

    def fill(self, shape: tuple[int, ...]) -> np.ndarray:
        count = int(np.prod(shape))
        out = np.empty(count, dtype=np.uint32)
        for i in range(count):
            out[i] = self.next_u32()
        return out.reshape(shape)


def _mix32(value: int) -> int:
    """32-bit integer avalanche mix used for lattice hashing."""

    value &= _MASK32
    value = (value ^ (value >> 16)) & _MASK32
    value = (value * 0x7FEB352D) & _MASK32
    value = (value ^ (value >> 15)) & _MASK32
    value = (value * 0x846CA68B) & _MASK32
    value = (value ^ (value >> 16)) & _MASK32
    return value


def lattice_hash(ix: np.ndarray, iy: np.ndarray, iz: np.ndarray, seed: int) -> np.ndarray:
    """Hash integer lattice coordinates to deterministic floats in [0, 1)."""

    h = np.full(np.broadcast(ix, iy, iz).shape, int(seed) & _MASK32, dtype=np.int64)
    h = (h ^ (ix.astype(np.int64) * 0x9E3779B1)) & 0xFFFFFFFF
    h = (h ^ (iy.astype(np.int64) * 0x85EBCA77)) & 0xFFFFFFFF
    h = (h ^ (iz.astype(np.int64) * 0xC2B2AE3D)) & 0xFFFFFFFF
    h = (h ^ (h >> 16)) * 0x7FEB352D & 0xFFFFFFFF
    h = (h ^ (h >> 15)) * 0x846CA68B & 0xFFFFFFFF
    h = (h ^ (h >> 16)) & 0xFFFFFFFF
    return (h.astype(np.float64) / 4294967296.0)


def smoothstep(t: np.ndarray) -> np.ndarray:
    t = np.clip(t, 0.0, 1.0)
    return t * t * (3.0 - 2.0 * t)


def value_noise_3d(points: np.ndarray, seed: int) -> np.ndarray:
    """Tricubic-interpolated value noise for arbitrary 3D point arrays.

    ``points`` has shape (..., 3); output shape matches the leading axes.
    The value depends only on coordinates and ``seed``, never on traversal
    order or any global RNG state.
    """

    pts = np.asarray(points, dtype=np.float64)
    base = np.floor(pts).astype(np.int64)
    frac = pts - base
    sx = smoothstep(frac[..., 0])
    sy = smoothstep(frac[..., 1])
    sz = smoothstep(frac[..., 2])

    def corner(dx: int, dy: int, dz: int) -> np.ndarray:
        return lattice_hash(base[..., 0] + dx, base[..., 1] + dy,
                            base[..., 2] + dz, seed)

    c000 = corner(0, 0, 0)
    c100 = corner(1, 0, 0)
    c010 = corner(0, 1, 0)
    c110 = corner(1, 1, 0)
    c001 = corner(0, 0, 1)
    c101 = corner(1, 0, 1)
    c011 = corner(0, 1, 1)
    c111 = corner(1, 1, 1)

    x00 = c000 + (c100 - c000) * sx
    x10 = c010 + (c110 - c010) * sx
    x01 = c001 + (c101 - c001) * sx
    x11 = c011 + (c111 - c011) * sx
    y0 = x00 + (x10 - x00) * sy
    y1 = x01 + (x11 - x01) * sy
    return y0 + (y1 - y0) * sz
