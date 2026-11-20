from __future__ import annotations

import binascii
import struct
import zlib
from pathlib import Path

import numpy as np

from .codec import atomic_write_bytes
from .pipeline import AtlasPipeline, Preview


def _u8(values: np.ndarray) -> np.ndarray:
    return np.floor(np.clip(values, 0.0, 1.0) * 255.0 + 0.5).astype(np.uint8)


def _heat_colors(normalized: np.ndarray) -> np.ndarray:
    # Deterministic black -> red -> yellow -> white heat ramp.
    value = np.clip(normalized, 0.0, 1.0)
    red = np.clip(value * 3.0, 0.0, 1.0)
    green = np.clip(value * 3.0 - 1.0, 0.0, 1.0)
    blue = np.clip(value * 3.0 - 2.0, 0.0, 1.0)
    return np.stack([red, green, blue], axis=-1)


def preview_rgb(preview: Preview, *, heatmap: bool = False) -> np.ndarray:
    base = np.asarray(preview.srgb[..., :3], dtype=np.float64)
    if not heatmap:
        return _u8(base)
    heat = np.asarray(preview.error_heatmap, dtype=np.float64)
    peak = float(np.max(heat)) if heat.size else 0.0
    if peak > 0.0:
        heat = heat / peak
    colors = _heat_colors(heat)
    return _u8(base * 0.42 + colors * 0.58)


def preview_rgba_float(preview: Preview, *, heatmap: bool = False) -> np.ndarray:
    base = np.asarray(preview.srgb, dtype=np.float64)
    output = np.array(base, dtype=np.float32, copy=True)
    if heatmap:
        heat = np.asarray(preview.error_heatmap, dtype=np.float64)
        peak = float(np.max(heat)) if heat.size else 0.0
        if peak > 0.0:
            heat = heat / peak
        colors = _heat_colors(heat)
        output[..., :3] = base[..., :3] * 0.42 + colors * 0.58
    output[..., 3] = np.clip(base[..., 3], 0.0, 1.0)
    return output


def preview_heatmap_rgb(preview: Preview) -> np.ndarray:
    heat = np.asarray(preview.error_heatmap, dtype=np.float64)
    peak = float(np.max(heat)) if heat.size else 0.0
    if peak > 0.0:
        heat = heat / peak
    return _u8(_heat_colors(heat))


def scale_nearest(rgb: np.ndarray, scale: int) -> np.ndarray:
    if scale <= 1:
        return rgb
    return np.repeat(np.repeat(rgb, scale, axis=0), scale, axis=1)


def _png_chunk(kind: bytes, data: bytes) -> bytes:
    return (
        struct.pack(">I", len(data))
        + kind
        + data
        + struct.pack(">I", binascii.crc32(kind + data) & 0xFFFFFFFF)
    )


def encode_png_rgb(rgb: np.ndarray) -> bytes:
    array = np.asarray(rgb, dtype=np.uint8)
    if array.ndim != 3 or array.shape[2] != 3:
        raise ValueError("PNG preview must have shape (height, width, 3)")
    height, width, _ = array.shape
    raw = b"".join(
        b"\x00" + array[row].tobytes(order="C") for row in range(height)
    )
    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + _png_chunk(b"IHDR", header)
        + _png_chunk(b"IDAT", zlib.compress(raw, 9))
        + _png_chunk(b"IEND", b"")
    )


def render_preview_png(
    pipeline: AtlasPipeline,
    path: str | Path,
    *,
    version: int | None = None,
    heatmap: bool = False,
    scale: int = 1,
    renderer: str = "cpu",
) -> str:
    preview = pipeline.get_preview(version)
    if renderer == "opengl":
        from .opengl_preview import render_rgba_opengl

        try:
            rgba = render_rgba_opengl(preview_rgba_float(preview, heatmap=heatmap))
            rgb = scale_nearest(rgba[..., :3], scale)
        except Exception:
            rgb = scale_nearest(preview_rgb(preview, heatmap=heatmap), scale)
    else:
        rgb = scale_nearest(preview_rgb(preview, heatmap=heatmap), scale)
    payload = encode_png_rgb(rgb)
    atomic_write_bytes(path, payload)
    return binascii.crc32(payload) & 0xFFFFFFFF
