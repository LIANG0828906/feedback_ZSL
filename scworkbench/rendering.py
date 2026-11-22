from __future__ import annotations

import hashlib
from typing import Any

import numpy as np


def _parse_color(value: str) -> tuple[int, int, int]:
    value = value.lstrip("#")
    return int(value[0:2], 16), int(value[2:4], 16), int(value[4:6], 16)


def _scale(value: int, source_total: int, target_total: int) -> int:
    if source_total <= 0:
        return 0
    return int(value) * int(target_total) // int(source_total)


def _rect_mask(
    height: int, width: int, x: int, y: int, w: int, h: int
) -> tuple[slice, slice] | None:
    x0 = max(0, min(width, x))
    x1 = max(0, min(width, x + w))
    y0 = max(0, min(height, y))
    y1 = max(0, min(height, y + h))
    if x1 <= x0 or y1 <= y0:
        return None
    return slice(y0, y1), slice(x0, x1)


def _blend_rect(
    image: np.ndarray,
    x: int,
    y: int,
    w: int,
    h: int,
    color: tuple[int, int, int],
    alpha: float = 1.0,
) -> None:
    region = _rect_mask(image.shape[0], image.shape[1], x, y, w, h)
    if region is None:
        return
    target = image[region][:, :, :3].astype(np.float32)
    source = np.asarray(color, dtype=np.float32)
    image[region][:, :, :3] = np.clip(
        target * (1.0 - alpha) + source * alpha, 0, 255
    ).astype(np.uint8)


def _stroke_rect(
    image: np.ndarray,
    x: int,
    y: int,
    w: int,
    h: int,
    color: tuple[int, int, int],
    thickness: int = 1,
) -> None:
    _blend_rect(image, x, y, w, max(1, thickness), color)
    _blend_rect(image, x, y + h - thickness, w, max(1, thickness), color)
    _blend_rect(image, x, y, max(1, thickness), h, color)
    _blend_rect(image, x + w - thickness, y, max(1, thickness), h, color)


def _disc(
    image: np.ndarray,
    cx: int,
    cy: int,
    radius: int,
    color: tuple[int, int, int],
    alpha: float = 1.0,
) -> None:
    if radius <= 0:
        return
    x0 = max(0, cx - radius)
    x1 = min(image.shape[1], cx + radius + 1)
    y0 = max(0, cy - radius)
    y1 = min(image.shape[0], cy + radius + 1)
    if x1 <= x0 or y1 <= y0:
        return
    yy, xx = np.ogrid[y0:y1, x0:x1]
    mask = (xx - cx) ** 2 + (yy - cy) ** 2 <= radius * radius
    region = image[y0:y1, x0:x1]
    target = region[:, :, :3].astype(np.float32)
    source = np.asarray(color, dtype=np.float32)
    blended = target * (1.0 - alpha) + source * alpha
    region[:, :, :3][mask] = np.clip(blended[mask], 0, 255).astype(np.uint8)


def _line(
    image: np.ndarray,
    x0: int,
    y0: int,
    x1: int,
    y1: int,
    color: tuple[int, int, int],
    thickness: int = 1,
) -> None:
    steps = max(abs(x1 - x0), abs(y1 - y0), 1)
    xs = np.linspace(x0, x1, steps + 1).astype(np.int32)
    ys = np.linspace(y0, y1, steps + 1).astype(np.int32)
    half = max(0, thickness // 2)
    for x, y in zip(xs, ys, strict=True):
        _blend_rect(
            image,
            int(x) - half,
            int(y) - half,
            max(1, thickness),
            max(1, thickness),
            color,
        )


def render_scene(
    scene: dict[str, Any],
    output_width: int,
    output_height: int,
    source_width: int,
    source_height: int,
    modifiers: dict[str, Any] | None = None,
) -> np.ndarray:
    modifiers = modifiers or {}
    height, width = output_height, output_width
    top = np.asarray(_parse_color(scene["background"][0]), dtype=np.float32)
    bottom = np.asarray(_parse_color(scene["background"][1]), dtype=np.float32)
    y_gradient = np.linspace(0.0, 1.0, height, dtype=np.float32)[:, None, None]
    image = np.empty((height, width, 4), dtype=np.uint8)
    image[:, :, :3] = np.clip(
        top[None, None, :] * (1.0 - y_gradient)
        + bottom[None, None, :] * y_gradient,
        0,
        255,
    ).astype(np.uint8)
    image[:, :, 3] = 255

    glow_x = width * 0.72
    glow_y = height * 0.28
    yy, xx = np.ogrid[:height, :width]
    glow = np.clip(
        1.0 - np.sqrt((xx - glow_x) ** 2 + (yy - glow_y) ** 2) / (width * 0.8),
        0.0,
        1.0,
    )
    image[:, :, :3] = np.clip(
        image[:, :, :3].astype(np.float32) + glow[:, :, None] * 28.0, 0, 255
    ).astype(np.uint8)

    grid = (xx + yy) % 64 < 1
    image[grid, :3] = np.clip(
        image[grid, :3].astype(np.int16) + np.array([10, 18, 25]), 0, 255
    ).astype(np.uint8)

    for particle in scene.get("particles", []):
        px = _scale(particle["x"], source_width, width)
        py = _scale(particle["y"], source_height, height)
        radius = max(1, _scale(int(particle["r"]), source_width, width))
        _disc(
            image,
            px,
            py,
            radius,
            _parse_color(particle["color"]),
            alpha=0.35,
        )

    window_offsets = modifiers.get("window_offsets", {})
    active_window = modifiers.get("active_window")
    scroll_offset = int(modifiers.get("scroll_offset", 0))
    for index, window in enumerate(scene["windows"]):
        offset = window_offsets.get(str(window["id"]), window_offsets.get(window["id"], (0, 0)))
        if isinstance(offset, dict):
            offset_x = int(offset.get("x", 0))
            offset_y = int(offset.get("y", 0))
        else:
            offset_x, offset_y = int(offset[0]), int(offset[1])
        x = _scale(window["x"], source_width, width) + offset_x
        y = _scale(window["y"], source_height, height) + offset_y
        w = max(16, _scale(window["w"], source_width, width))
        h = max(16, _scale(window["h"], source_height, height))
        color = _parse_color(window["color"])
        _blend_rect(image, x + 5, y + 6, w, h, (0, 0, 0), alpha=0.38)
        _blend_rect(image, x, y, w, h, (13, 20, 31), alpha=0.96)
        _stroke_rect(
            image,
            x,
            y,
            w,
            h,
            color if window["id"] == active_window else (58, 76, 98),
            thickness=2 if window["id"] == active_window else 1,
        )
        _blend_rect(image, x + 1, y + 1, w - 2, 22, color, alpha=0.82)
        title_hash = hashlib.sha256(window["title"].encode("utf-8")).digest()
        for dot in range(3):
            dot_color = (255, 94, 98) if dot == 0 else (
                (255, 202, 58) if dot == 1 else (39, 201, 63)
            )
            _disc(image, x + 11 + dot * 12, y + 11, 4, dot_color, alpha=0.9)
        for bar in range(5):
            bar_width = max(4, (w - 28) * (35 + (title_hash[bar] % 45)) // 100)
            bar_y = y + 36 + bar * 18 - (scroll_offset % 18 if window["id"] == active_window else 0)
            if y < bar_y < y + h - 8:
                bar_color = (
                    min(255, color[0] + 35),
                    min(255, color[1] + 35),
                    min(255, color[2] + 35),
                )
                _blend_rect(
                    image,
                    x + 14,
                    bar_y,
                    bar_width,
                    6,
                    bar_color,
                    alpha=0.42,
                )
        for marker in range(4):
            bx = x + 16 + marker * 28
            by = y + h - 34
            if x < bx < x + w - 18:
                _blend_rect(
                    image,
                    bx,
                    by,
                    18,
                    18,
                    (color[0] // 2, color[1] // 2, color[2] // 2),
                    alpha=0.65,
                )

    cursor = modifiers.get(
        "cursor",
        (
            _scale(scene["cursor"][0], source_width, width),
            _scale(scene["cursor"][1], source_height, height),
        ),
    )
    cursor_x, cursor_y = int(cursor[0]), int(cursor[1])
    _line(image, cursor_x, cursor_y, cursor_x + 13, cursor_y + 13, (255, 244, 198), 2)
    _line(image, cursor_x + 13, cursor_y, cursor_x, cursor_y + 13, (255, 244, 198), 2)
    _disc(image, cursor_x, cursor_y, 3, (255, 255, 255), alpha=0.9)

    key_buffer = str(modifiers.get("key_buffer", ""))
    if key_buffer:
        digest = hashlib.sha256(key_buffer.encode("utf-8")).digest()
        for index in range(min(12, len(key_buffer))):
            shade = digest[index % len(digest)]
            _blend_rect(
                image,
                width - 28 - index * 13,
                height - 14,
                9,
                7,
                (80 + shade // 2, 190, 220),
                alpha=0.8,
            )

    frame_seq = int(modifiers.get("frame_seq", 0))
    frame_bits = f"{frame_seq:024b}"[-16:]
    for index, bit in enumerate(frame_bits):
        if bit == "1":
            _blend_rect(
                image,
                8 + index * 8,
                height - 10,
                6,
                4,
                (84, 214, 255),
                alpha=0.9,
            )
    _blend_rect(
        image,
        0,
        0,
        max(2, width // 6),
        2,
        (74, 210, 255),
        alpha=0.8,
    )
    return image


def image_digest(image: np.ndarray) -> str:
    return hashlib.sha256(image.tobytes(order="C")).hexdigest()


def render_missing_frame(
    output_width: int,
    output_height: int,
    frame_seq: int,
    timestamp_ms: int,
) -> np.ndarray:
    height, width = output_height, output_width
    image = np.zeros((height, width, 4), dtype=np.uint8)
    image[:, :, :3] = np.array([22, 10, 30], dtype=np.uint8)
    image[:, :, 3] = 255
    _stroke_rect(image, 3, 3, width - 6, height - 6, (235, 58, 123), 3)
    _line(image, 0, 0, width - 1, height - 1, (120, 24, 64), 2)
    _line(image, width - 1, 0, 0, height - 1, (120, 24, 64), 2)
    bits = f"{frame_seq:032b}"
    for index, bit in enumerate(bits):
        if bit == "1":
            _blend_rect(
                image,
                width // 2 - 72 + index * 5,
                height // 2 + 42,
                3,
                12,
                (255, 125, 167),
                alpha=1.0,
            )
    stamp = timestamp_ms % 4096
    for index in range(12):
        if stamp & (1 << index):
            _blend_rect(
                image,
                width // 2 - 60 + index * 10,
                height // 2 + 62,
                7,
                7,
                (255, 188, 208),
            )
    return image
