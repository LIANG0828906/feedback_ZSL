"""Deterministic offscreen rasterizer (numpy only) and PNG writer."""

import struct
import zlib

import numpy as np

from . import LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR

LAYER_COLORS = {
    LAYER_OUTLINE: np.array([40, 34, 30], dtype=np.uint8),
    LAYER_SHADOW: np.array([70, 60, 95], dtype=np.uint8),
    LAYER_SMEAR: np.array([210, 160, 90], dtype=np.uint8),
}


def render_version(store, manifest, width=640, height=480, margin=24):
    """Composite every committed layer of one manifest into an RGBA buffer."""
    canvas = np.zeros((height, width, 4), dtype=np.uint8)
    min_x, min_y, max_x, max_y = _manifest_bounds(manifest)
    span_x = max(1e-9, max_x - min_x)
    span_y = max(1e-9, max_y - min_y)
    scale = min((width - 2 * margin) / span_x, (height - 2 * margin) / span_y)
    offset_x = 0.5 * (width - span_x * scale)
    offset_y = 0.5 * (height - span_y * scale)
    for layer in (LAYER_SMEAR, LAYER_SHADOW, LAYER_OUTLINE):
        strokes = store.read_layer_strokes(manifest["version"], layer)
        _raster_layer(
            canvas,
            strokes,
            LAYER_COLORS[layer],
            min_x,
            min_y,
            scale,
            offset_x,
            offset_y,
        )
    return canvas


def _manifest_bounds(manifest):
    # Bounds are derived from stroke coordinates, never from file metadata.
    # Manifest entries only carry chunk ids; derive bounds from chunk grid.
    chunks = [tuple(e["chunk"]) for e in manifest["entries"]]
    if not chunks:
        return 0.0, 0.0, 1.0, 1.0
    from .storage import CHUNK_SIZE

    cx = min(c[0] for c in chunks)
    cy = min(c[1] for c in chunks)
    ex = max(c[0] for c in chunks)
    ey = max(c[1] for c in chunks)
    return (
        cx * CHUNK_SIZE,
        cy * CHUNK_SIZE,
        (ex + 1) * CHUNK_SIZE,
        (ey + 1) * CHUNK_SIZE,
    )


def _raster_layer(canvas, strokes, color, min_x, min_y, scale, offset_x, offset_y):
    height, width = canvas.shape[:2]
    for stroke in strokes:
        radius = max(0.7, stroke["width"] * scale * 0.5)
        x = offset_x + (stroke["x"] - min_x) * scale
        y = offset_y + (stroke["y"] - min_y) * scale
        angle = np.deg2rad(stroke["angle"])
        half_len = 0.5 * stroke["length"] * scale
        dx, dy = np.cos(angle) * half_len, np.sin(angle) * half_len
        r = int(np.ceil(radius + max(abs(dx), abs(dy))))
        x0, x1 = max(0, int(x) - r), min(width - 1, int(x) + r)
        y0, y1 = max(0, int(y) - r), min(height - 1, int(y) + r)
        if x0 > x1 or y0 > y1:
            continue
        yy, xx = np.mgrid[y0 : y1 + 1, x0 : x1 + 1]
        dist = _segment_distance(
            xx.astype(np.float64),
            yy.astype(np.float64),
            x - dx,
            y - dy,
            x + dx,
            y + dy,
        )
        mask = dist <= radius
        alpha = float(stroke["alpha"]) * 0.55
        _blend(canvas, yy, xx, mask, color, alpha)


def _segment_distance(px, py, ax, ay, bx, by):
    abx, aby = bx - ax, by - ay
    denom = abx * abx + aby * aby
    if denom <= 1e-12:
        return np.hypot(px - ax, py - ay)
    t = np.clip(((px - ax) * abx + (py - ay) * aby) / denom, 0.0, 1.0)
    proj_x = ax + t * abx
    proj_y = ay + t * aby
    return np.hypot(px - proj_x, py - proj_y)


def _blend(canvas, yy, xx, mask, color, alpha):
    existing = canvas[yy, xx].astype(np.float32)
    src = np.zeros_like(existing)
    src[..., 0] = color[0]
    src[..., 1] = color[1]
    src[..., 2] = color[2]
    src[..., 3] = 255.0 * alpha
    out_alpha = src[..., 3:4] / 255.0 + existing[..., 3:4] / 255.0 * (1 - src[..., 3:4] / 255.0)
    blended_rgb = (
        src[..., :3] * (src[..., 3:4] / 255.0)
        + existing[..., :3] * (existing[..., 3:4] / 255.0) * (1 - src[..., 3:4] / 255.0)
    )
    safe_alpha = np.where(out_alpha > 1e-9, out_alpha, 1.0)
    rgb = np.where(mask[..., None], blended_rgb / safe_alpha, existing[..., :3])
    a = np.where(mask[..., None], np.clip(out_alpha * 255.0, 0, 255), existing[..., 3:4])
    region = np.concatenate([rgb, a], axis=-1).astype(np.uint8)
    canvas[yy, xx] = np.where(mask[..., None], region, canvas[yy, xx])


def write_png(path, canvas):
    height, width = canvas.shape[:2]
    raw = bytearray()
    for row in canvas:
        raw.append(0)
        raw.extend(row.tobytes())
    png = bytearray(b"\x89PNG\r\n\x1a\n")
    png += _chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0))
    png += _chunk(b"IDAT", zlib.compress(bytes(raw), 9))
    png += _chunk(b"IEND", b"")
    with open(path, "wb") as fh:
        fh.write(png)


def _chunk(tag, data):
    return (
        struct.pack(">I", len(data))
        + tag
        + data
        + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
    )


def canvas_hash(canvas):
    import hashlib

    return hashlib.sha256(canvas.tobytes()).hexdigest()
