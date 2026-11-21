from __future__ import annotations

import hashlib
import math
import os
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import BusinessError, ErrorCode
from .model import BooleanResult, Point, combined_bounds


@dataclass
class PreviewImage:
    width: int
    height: int
    rgba: bytes
    png: bytes
    rgba_sha256: str
    png_sha256: str
    world_bounds: tuple[float, float, float, float]

    def write_png(self, filename: str | Path) -> Path:
        path = Path(filename)
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary = path.with_name(path.name + ".tmp")
        temporary.write_bytes(self.png)
        os.replace(temporary, path)
        return path


def _fill_row(
    row: np.ndarray,
    y: float,
    segments: list[tuple[Point, Point]],
    x_min: float,
    x_max: float,
    width: int,
    fill: tuple[int, int, int, int],
) -> None:
    events: list[tuple[float, int]] = []
    for start, end in segments:
        if (start.y > y) == (end.y > y):
            continue
        x = start.x + (y - start.y) * (
            (end.x - start.x) / (end.y - start.y)
        )
        events.append((x, 1 if end.y > start.y else -1))
    if not events:
        return
    events.sort(key=lambda item: (item[0], item[1]))
    winding = 0
    previous_x = x_min
    index = 0
    while index < len(events):
        x = events[index][0]
        if winding != 0:
            left = max(0, math.ceil((previous_x - x_min) / (x_max - x_min) * width))
            right = min(
                width,
                math.ceil((x - x_min) / (x_max - x_min) * width),
            )
            if right > left:
                row[left:right] = fill
        while index < len(events) and events[index][0] == x:
            winding += events[index][1]
            index += 1
        previous_x = x
    if winding != 0:
        left = max(0, math.ceil((previous_x - x_min) / (x_max - x_min) * width))
        row[left:] = fill


def rasterize_contours(
    contours: list[list[Point]],
    bounds: tuple[float, float, float, float],
    *,
    width: int = 512,
    height: int = 512,
    margin: int = 16,
    background: tuple[int, int, int, int] = (18, 22, 30, 255),
    fill: tuple[int, int, int, int] = (88, 196, 255, 255),
) -> np.ndarray:
    if width <= 2 * margin or height <= 2 * margin:
        raise BusinessError(ErrorCode.PATH_INVALID, "preview canvas is too small")
    segments, _, _ = _world_to_canvas_segments(
        contours,
        bounds,
        width=width,
        height=height,
        margin=margin,
    )
    return _rasterize_canvas_segments(
        segments,
        width=width,
        height=height,
        background=background,
        fill=fill,
    )


def _world_to_canvas_segments(
    contours: list[list[Point]],
    bounds: tuple[float, float, float, float],
    *,
    width: int,
    height: int,
    margin: int,
) -> tuple[list[tuple[Point, Point]], float, float]:
    x_min, y_min, x_max, y_max = bounds
    world_width = max(x_max - x_min, 1.0e-12)
    world_height = max(y_max - y_min, 1.0e-12)
    inner_width = width - margin * 2
    inner_height = height - margin * 2
    scale = min(inner_width / world_width, inner_height / world_height)
    draw_width = world_width * scale
    draw_height = world_height * scale
    offset_x = (width - draw_width) * 0.5
    offset_y = (height - draw_height) * 0.5

    def transform(point: Point) -> Point:
        return Point(
            offset_x + (point.x - x_min) * scale,
            height - (offset_y + (point.y - y_min) * scale),
        )

    segments: list[tuple[Point, Point]] = []
    for points in contours:
        if len(points) < 2:
            continue
        transformed = [transform(point) for point in points]
        segments.extend(zip(transformed, transformed[1:] + transformed[:1]))
    return segments, offset_x, offset_y


def _rasterize_canvas_segments(
    segments: list[tuple[Point, Point]],
    *,
    width: int,
    height: int,
    background: tuple[int, int, int, int],
    fill: tuple[int, int, int, int],
) -> np.ndarray:
    canvas = np.empty((height, width, 4), dtype=np.uint8)
    canvas[:, :] = background
    for row_index in range(height):
        _fill_row(
            canvas[row_index],
            row_index + 0.5,
            segments,
            0.0,
            float(width),
            width,
            fill,
        )
    return canvas


def render_preview(
    path_a,
    path_b,
    result: BooleanResult,
    *,
    tolerance: float,
    width: int = 512,
    height: int = 512,
    bounds: tuple[float, float, float, float] | None = None,
) -> PreviewImage:
    if result.report is None or not getattr(result.report, "accepted", False):
        raise BusinessError(
            ErrorCode.RESULT_DEGRADED,
            "degraded or uncommitted result cannot be rendered",
        )
    if bounds is None:
        bounds = combined_bounds(
            [path_a, path_b],
            result.contours,
            tolerance,
        )
    contours = [list(contour.points) for contour in result.contours]
    segments, _, _ = _world_to_canvas_segments(
        contours,
        bounds,
        width=width,
        height=height,
        margin=16,
    )
    canvas = _rasterize_canvas_segments(
        segments,
        width=width,
        height=height,
        background=(18, 22, 30, 255),
        fill=(88, 196, 255, 255),
    )
    rgba = canvas.tobytes(order="C")
    png = _encode_png_qt(canvas, width, height)
    return PreviewImage(
        width=width,
        height=height,
        rgba=rgba,
        png=png,
        rgba_sha256=hashlib.sha256(rgba).hexdigest(),
        png_sha256=hashlib.sha256(png).hexdigest(),
        world_bounds=bounds,
    )


def _encode_png_qt(canvas: np.ndarray, width: int, height: int) -> bytes:
    from PySide6.QtCore import QBuffer, QByteArray, QIODevice
    from PySide6.QtGui import QImage

    contiguous = np.ascontiguousarray(canvas)
    image = QImage(
        contiguous.data,
        width,
        height,
        int(contiguous.strides[0]),
        QImage.Format.Format_RGBA8888,
    )
    byte_array = QByteArray()
    buffer = QBuffer(byte_array)
    if not buffer.open(QIODevice.OpenModeFlag.WriteOnly):
        raise BusinessError(ErrorCode.IO, "cannot open offscreen PNG buffer")
    try:
        if not image.save(buffer, "PNG"):
            raise BusinessError(ErrorCode.IO, "Qt failed to encode preview PNG")
        return bytes(byte_array.data())
    finally:
        buffer.close()
        image = None
        contiguous = None
        buffer = None

