"""Seeded, order-independent glass texture tables.

No NumPy or Python random state is used.  Every value comes from a stable
integer hash of (scene seed, quadrant, piece index, table coordinate).
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass

import numpy as np

from .scene import GlassPiece, TextureSettings


_U64 = np.uint64
_C1 = _U64(0xBF58476D1CE4E5B9)
_C2 = _U64(0x94D049BB133111EB)
_M1 = _U64(0x9E3779B97F4A7C15)
_S30 = _U64(30)
_S27 = _U64(27)
_S31 = _U64(31)
_S11 = _U64(11)


def _mix64(value: np.ndarray | np.uint64) -> np.ndarray:
    value = np.asarray(value, dtype=np.uint64)
    with np.errstate(over="ignore"):
        value = (value ^ (value >> _S30)) * _C1
        value = (value ^ (value >> _S27)) * _C2
        return value ^ (value >> _S31)


def _piece_seed(seed: int, quadrant: int, piece_index: int, salt: int) -> np.uint64:
    value = _U64(seed & 0xFFFFFFFFFFFFFFFF)
    value = _mix64(value ^ _U64((quadrant + 1) * 0xD1B54A32D192ED03 & 0xFFFFFFFFFFFFFFFF))
    value = _mix64(value ^ _U64((piece_index + 1) * 0x94D049BB133111EB & 0xFFFFFFFFFFFFFFFF))
    value = _mix64(value ^ _U64(salt & 0xFFFFFFFFFFFFFFFF))
    return value


def _hash_noise_table(
    seed_value: np.uint64, size: int, salt: int
) -> np.ndarray:
    y, x = np.mgrid[0:size, 0:size]
    x_u = x.astype(np.uint64)
    y_u = y.astype(np.uint64)
    hashed = _mix64(seed_value ^ _U64(salt & 0xFFFFFFFFFFFFFFFF))
    hashed = _mix64(hashed ^ (x_u * _M1))
    hashed = _mix64(hashed ^ (y_u * _C1))
    # Keep the high 24 bits, then map to [0, 1).
    return ((hashed >> _U64(40)).astype(np.float64) / float(1 << 24)).astype(
        np.float64
    )


def _cyclic_smooth(field: np.ndarray, passes: int) -> np.ndarray:
    result = field
    for _ in range(passes):
        result = (
            np.roll(result, 1, axis=0)
            + 2.0 * result
            + np.roll(result, -1, axis=0)
            + np.roll(result, 1, axis=1)
            + 2.0 * result
            + np.roll(result, -1, axis=1)
        ) / 8.0
    return result


def _normalize_field(field: np.ndarray) -> np.ndarray:
    low = float(field.min())
    high = float(field.max())
    if high - low <= 1e-15:
        return np.full_like(field, 0.5, dtype=np.float64)
    return (field - low) / (high - low)


@dataclass(frozen=True)
class PieceTexture:
    serial: int
    transmission: np.ndarray
    reflection: np.ndarray

    def digest(self) -> str:
        digest = hashlib.sha256()
        digest.update(np.ascontiguousarray(self.transmission, dtype=np.float32).tobytes())
        digest.update(np.ascontiguousarray(self.reflection, dtype=np.float32).tobytes())
        return digest.hexdigest()


class TextureBank:
    """Holds one deterministic mottling table per glass piece."""

    def __init__(
        self,
        seed: int,
        settings: TextureSettings,
        pieces: tuple[GlassPiece, ...],
    ) -> None:
        self.seed = seed
        self.settings = settings
        self._by_serial = {
            piece.serial: self._generate(piece) for piece in pieces
        }

    def _generate(self, piece: GlassPiece) -> PieceTexture:
        size = self.settings.size
        base_seed = _piece_seed(
            self.seed, piece.quadrant, piece.piece_index, 0x5EED_5EED
        )
        transmission_noise = _cyclic_smooth(
            _hash_noise_table(base_seed, size, 1), passes=2
        )
        reflection_noise = _cyclic_smooth(
            _hash_noise_table(base_seed, size, 2), passes=1
        )
        # A third field mixes two spatial scales so neighbouring pieces cannot
        # collapse to the same flat tint even when they share a base colour.
        coarse = _hash_noise_table(base_seed, size, 3)
        coarse = _cyclic_smooth(coarse, passes=5)
        transmission_noise = _normalize_field(0.68 * transmission_noise + 0.32 * coarse)
        reflection_noise = _normalize_field(0.73 * reflection_noise + 0.27 * coarse)

        trans_low = self.settings.transmission_low
        trans_high = self.settings.transmission_high
        refl_low = self.settings.reflection_low
        refl_high = self.settings.reflection_high
        transmission = (
            trans_low + (trans_high - trans_low) * transmission_noise
        ).astype(np.float32)
        reflection = (
            refl_low + (refl_high - refl_low) * reflection_noise
        ).astype(np.float32)
        return PieceTexture(
            serial=piece.serial,
            transmission=transmission,
            reflection=reflection,
        )

    def get(self, serial: int) -> PieceTexture:
        return self._by_serial[serial]

    def digests(self) -> dict[str, str]:
        return {
            str(serial): self._by_serial[serial].digest()
            for serial in sorted(self._by_serial)
        }


def _wrap_indices(indices: np.ndarray, size: int) -> np.ndarray:
    return np.mod(indices, size).astype(np.intp)


def bilinear_sample(
    table: np.ndarray, u: np.ndarray, v: np.ndarray
) -> np.ndarray:
    """Periodic bilinear sample. u/v are in [0, 1]."""
    size_y, size_x = table.shape
    x = np.mod(u, 1.0) * size_x - 0.5
    y = np.mod(v, 1.0) * size_y - 0.5
    x0 = np.floor(x).astype(np.intp)
    y0 = np.floor(y).astype(np.intp)
    fx = (x - x0).astype(np.float32)
    fy = (y - y0).astype(np.float32)
    x1 = _wrap_indices(x0 + 1, size_x)
    y1 = _wrap_indices(y0 + 1, size_y)
    x0w = _wrap_indices(x0, size_x)
    y0w = _wrap_indices(y0, size_y)
    top = table[y0w, x0w] * (1.0 - fx) + table[y0w, x1] * fx
    bottom = table[y1, x0w] * (1.0 - fx) + table[y1, x1] * fx
    return top * (1.0 - fy) + bottom * fy
