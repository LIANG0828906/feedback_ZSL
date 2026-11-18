"""Small deterministic and filesystem helpers used across the renderer."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Any, Iterable

import numpy as np

MASK64 = (1 << 64) - 1
U64 = np.uint64


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str | os.PathLike[str]) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            chunk = handle.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def object_digest(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def atomic_write_bytes(path: str | os.PathLike[str], data: bytes) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    with open(temporary, "wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    try:
        directory_fd = os.open(path.parent, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def atomic_write_json(path: str | os.PathLike[str], value: Any) -> None:
    data = json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        indent=2,
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(path, data)


def atomic_write_npy(path: str | os.PathLike[str], array: np.ndarray) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}.npy")
    with open(temporary, "wb") as handle:
        np.save(handle, np.ascontiguousarray(array), allow_pickle=False)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def mix64(value: int) -> int:
    value &= MASK64
    value ^= value >> 30
    value = (value * 0xBF58476D1CE4E5B9) & MASK64
    value ^= value >> 27
    value = (value * 0x94D049BB133111EB) & MASK64
    value ^= value >> 31
    return value & MASK64


def stable_u64(*values: int) -> int:
    state = 0x9E3779B97F4A7C15
    for value in values:
        state = mix64(state ^ (int(value) & MASK64))
    return state


def hash_unit(*values: int) -> float:
    return (stable_u64(*values) >> 11) * (1.0 / 9007199254740992.0)


def hash_u64_array(x: np.ndarray, y: np.ndarray, seed: int) -> np.ndarray:
    """Vectorised stable 64-bit hash for integer grid coordinates."""

    a = np.asarray(x, dtype=np.uint64) + U64(seed & MASK64)
    b = np.asarray(y, dtype=np.uint64) ^ U64(stable_u64(seed, 0xA5A5A5A5))
    z = a ^ (b + U64(0x9E3779B97F4A7C15) + (a << U64(6)) + (a >> U64(2)))
    z ^= z >> U64(30)
    z *= U64(0xBF58476D1CE4E5B9)
    z ^= z >> U64(27)
    z *= U64(0x94D049BB133111EB)
    z ^= z >> U64(31)
    return z


def hash_unit_array(x: np.ndarray, y: np.ndarray, seed: int) -> np.ndarray:
    return ((hash_u64_array(x, y, seed) >> U64(11)).astype(np.float64)) * (
        1.0 / 9007199254740992.0
    )


def smoothstep(edge0: float | np.ndarray, edge1: float | np.ndarray, x: Any) -> np.ndarray:
    x_arr = np.asarray(x, dtype=np.float64)
    edge0_arr = np.asarray(edge0, dtype=np.float64)
    edge1_arr = np.asarray(edge1, dtype=np.float64)
    denom = edge1_arr - edge0_arr
    denom = np.where(np.abs(denom) < 1e-15, 1e-15, denom)
    t = np.clip((x_arr - edge0_arr) / denom, 0.0, 1.0)
    return t * t * (3.0 - 2.0 * t)


def bilinear_sample(image: np.ndarray, u: Any, v: Any, wrap: bool = False) -> np.ndarray:
    """Bilinearly sample an HxW or HxWxC image with finite UVs."""

    img = np.asarray(image)
    if img.ndim not in (2, 3):
        raise ValueError("bilinear_sample expects HxW or HxWxC")
    height, width = img.shape[:2]
    u_arr = np.asarray(u, dtype=np.float64)
    v_arr = np.asarray(v, dtype=np.float64)
    if wrap:
        u_arr = np.mod(u_arr, 1.0)
        v_arr = np.mod(v_arr, 1.0)
    else:
        u_arr = np.clip(u_arr, 0.0, 1.0)
        v_arr = np.clip(v_arr, 0.0, 1.0)
    x = u_arr * (width - 1)
    y = v_arr * (height - 1)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    if wrap:
        x1 = (x0 + 1) % width
        y1 = (y0 + 1) % height
        x0 = np.mod(x0, width)
        y0 = np.mod(y0, height)
    else:
        x1 = np.minimum(x0 + 1, width - 1)
        y1 = np.minimum(y0 + 1, height - 1)
    fx = x - x0
    fy = y - y0
    if img.ndim == 3:
        fx = fx[..., None]
        fy = fy[..., None]
    top = img[y0, x0] * (1.0 - fx) + img[y0, x1] * fx
    bottom = img[y1, x0] * (1.0 - fx) + img[y1, x1] * fx
    return top * (1.0 - fy) + bottom * fy


def normalise(vector: Any, axis: int = -1, eps: float = 1e-15) -> np.ndarray:
    arr = np.asarray(vector, dtype=np.float64)
    length = np.sqrt(np.sum(arr * arr, axis=axis, keepdims=True))
    return arr / np.maximum(length, eps)


def srgb_encode(linear: np.ndarray) -> np.ndarray:
    value = np.clip(linear, 0.0, 1.0)
    return np.where(
        value <= 0.0031308,
        12.92 * value,
        1.055 * np.power(value, 1.0 / 2.4) - 0.055,
    )


def sorted_light_ids(lights: Iterable[Any]) -> list[str]:
    return sorted((light.id for light in lights), key=lambda item: item.encode("utf-8"))
