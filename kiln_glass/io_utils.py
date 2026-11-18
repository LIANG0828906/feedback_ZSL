"""Byte-stable image and ownership-map IO using dependency-free formats."""

from __future__ import annotations

import json
import os
import numpy as np


def write_pam(path: str, rgba: np.ndarray) -> None:
    """Write a deterministic 8-bit RGBA PAM (P7) file."""
    if rgba.dtype != np.uint8 or rgba.ndim != 3 or rgba.shape[2] != 4:
        raise ValueError("write_pam expects an (H, W, 4) uint8 array")
    height, width = rgba.shape[:2]
    header = (
        f"P7\nWIDTH {width}\nHEIGHT {height}\nDEPTH 4\n"
        "MAXVAL 255\nTUPLTYPE RGB_ALPHA\nENDHDR\n"
    ).encode("ascii")
    tmp = f"{path}.tmp"
    with open(tmp, "wb") as handle:
        handle.write(header)
        handle.write(rgba.tobytes(order="C"))
    os.replace(tmp, path)


def read_pam(path: str) -> np.ndarray:
    with open(path, "rb") as handle:
        blob = handle.read()
    end = blob.index(b"ENDHDR\n") + len(b"ENDHDR\n")
    header = blob[:end].decode("ascii").split()
    values = dict(zip(header[1::2], header[2::2]))
    width, height = int(values["WIDTH"]), int(values["HEIGHT"])
    return np.frombuffer(blob[end:], dtype=np.uint8).reshape(height, width, 4).copy()


def write_ownership(path: str, ownership: np.ndarray) -> None:
    """Write int32 brick-ownership map with a stable binary header."""
    if ownership.dtype != np.int32 or ownership.ndim != 2:
        raise ValueError("ownership must be an (H, W) int32 array")
    height, width = ownership.shape
    header = f"KGOWN\n{width}\n{height}\n".encode("ascii")
    tmp = f"{path}.tmp"
    with open(tmp, "wb") as handle:
        handle.write(header)
        handle.write(ownership.tobytes(order="C"))
    os.replace(tmp, path)


def read_ownership(path: str) -> np.ndarray:
    with open(path, "rb") as handle:
        blob = handle.read()
    lines = blob.split(b"\n", 3)
    if lines[0] != b"KGOWN":
        raise ValueError("invalid ownership file")
    width, height = int(lines[1]), int(lines[2])
    payload = blob[len(b"KGOWN\n") + len(str(width).encode()) + 1
                   + len(str(height).encode()) + 1:]
    return np.frombuffer(payload, dtype=np.int32).reshape(height, width).copy()


def write_json(path: str, payload: dict | list) -> None:
    tmp = f"{path}.tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, sort_keys=True)
        handle.write("\n")
    os.replace(tmp, path)


def read_json(path: str) -> dict | list:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def color_digest(rgba: np.ndarray) -> dict:
    """Deterministic per-tile colour summary."""
    flat = rgba.reshape(-1, 4).astype(np.float64)
    mean = flat.mean(axis=0)
    return {
        "pixels": int(flat.shape[0]),
        "mean_rgba": [round(float(v), 6) for v in mean],
        "min_rgba": [int(v) for v in flat.min(axis=0)],
        "max_rgba": [int(v) for v in flat.max(axis=0)],
        "sha256": _sha256(rgba.tobytes(order="C")),
    }


def _sha256(data: bytes) -> str:
    import hashlib

    return hashlib.sha256(data).hexdigest()
