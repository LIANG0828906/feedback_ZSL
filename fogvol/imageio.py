"""Deterministic image and array serialization."""

from __future__ import annotations

import io
import os
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image, PngImagePlugin


def rgba8(encoded_rgb: np.ndarray, opacity: np.ndarray) -> np.ndarray:
    height, width, _ = encoded_rgb.shape
    result = np.empty((height, width, 4), dtype=np.uint8)
    result[..., :3] = np.rint(
        np.clip(encoded_rgb, 0.0, 1.0) * 255.0
    ).astype(np.uint8)
    result[..., 3] = np.rint(
        np.clip(opacity, 0.0, 1.0) * 255.0
    ).astype(np.uint8)
    return result


def png_bytes_rgba(
    pixels: np.ndarray, metadata: dict[str, Any]
) -> bytes:
    info = PngImagePlugin.PngInfo()
    for key in sorted(metadata):
        value = metadata[key]
        if isinstance(value, float):
            text = format(value, ".17g")
        else:
            text = str(value)
        info.add_text(key, text)
    buffer = io.BytesIO()
    Image.fromarray(pixels, mode="RGBA").save(
        buffer, format="PNG", pnginfo=info
    )
    return buffer.getvalue()


def png_bytes_gray16(values: np.ndarray) -> bytes:
    uint16 = np.rint(
        np.clip(values, 0.0, 65535.0)
    ).astype(np.uint16)
    buffer = io.BytesIO()
    Image.fromarray(uint16).save(buffer, format="PNG")
    return buffer.getvalue()


def npy_bytes(values: np.ndarray) -> bytes:
    buffer = io.BytesIO()
    np.save(buffer, values, allow_pickle=False)
    return buffer.getvalue()


def atomic_write_bytes(path: Path, payload: bytes) -> None:
    temp = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with temp.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temp, path)

