"""Small deterministic vector and quaternion helpers."""

from __future__ import annotations

import numpy as np


EPS = 1.0e-10


def as_vec3(value: object, label: str) -> np.ndarray:
    try:
        vec = np.asarray(value, dtype=np.float64)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{label} must be a finite three-component vector") from exc
    if vec.shape != (3,) or not np.all(np.isfinite(vec)):
        raise ValueError(f"{label} must be a finite three-component vector")
    return vec


def normalize(vec: np.ndarray, label: str) -> np.ndarray:
    norm = float(np.linalg.norm(vec))
    if not np.isfinite(norm) or norm <= EPS:
        raise ValueError(f"{label} must have non-zero length")
    return vec / norm


def quaternion_matrix(quat: np.ndarray) -> np.ndarray:
    """Return the rotation matrix for a normalized (w, x, y, z) quaternion."""

    w, x, y, z = quat
    return np.array(
        [
            [1.0 - 2.0 * (y * y + z * z), 2.0 * (x * y - z * w), 2.0 * (x * z + y * w)],
            [2.0 * (x * y + z * w), 1.0 - 2.0 * (x * x + z * z), 2.0 * (y * z - x * w)],
            [2.0 * (x * z - y * w), 2.0 * (y * z + x * w), 1.0 - 2.0 * (x * x + y * y)],
        ],
        dtype=np.float64,
    )


def smoothstep(edge0: float, edge1: float, x: np.ndarray | float) -> np.ndarray:
    if edge0 == edge1:
        return np.asarray(x >= edge1, dtype=np.float64)
    t = np.clip((np.asarray(x, dtype=np.float64) - edge0) / (edge1 - edge0), 0.0, 1.0)
    return t * t * (3.0 - 2.0 * t)


def tonemap_srgb(linear: np.ndarray, exposure: float, max_display: float) -> np.ndarray:
    """Hue-preserving Reinhard tone map followed by an sRGB transfer."""

    x = np.maximum(np.asarray(linear, dtype=np.float64) * exposure, 0.0)
    peak = np.max(x, axis=-1, keepdims=True)
    mapped = x / (1.0 + peak)
    mapped = np.clip(mapped, 0.0, max_display)
    srgb = np.where(
        mapped <= 0.0031308,
        12.92 * mapped,
        1.055 * np.power(mapped, 1.0 / 2.4) - 0.055,
    )
    return np.clip(srgb, 0.0, 1.0)


def sha256_bytes(data: bytes) -> str:
    import hashlib

    return "sha256:" + hashlib.sha256(data).hexdigest()


def sha256_array(array: np.ndarray) -> str:
    contiguous = np.ascontiguousarray(array)
    return sha256_bytes(contiguous.tobytes(order="C"))
