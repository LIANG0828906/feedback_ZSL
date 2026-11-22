from __future__ import annotations

import math

import numpy as np

from .errors import ErrorCode, WorkbenchError
from .geometry import close_points, polygon_area
from .model import RenderParams, Scene, VectorPath


def canvas_size(scene: Scene, params: RenderParams) -> tuple[int, int]:
    width = scene.render_bounds[2] - scene.render_bounds[0]
    height = scene.render_bounds[3] - scene.render_bounds[1]
    return (
        max(1, int(math.ceil(width * params.scale - 1e-9))),
        max(1, int(math.ceil(height * params.scale - 1e-9))),
    )


def _subpixel_center_to_coordinate(
    index: int, subsamples: int, scale: float, origin: float
) -> float:
    return origin + (index + 0.5) / (subsamples * scale)


def _fill_polygon_into_mask(
    mask: np.ndarray,
    polygon: np.ndarray,
    *,
    width: int,
    height: int,
    subsamples: int,
    scale: float,
    origin: tuple[float, float],
) -> None:
    if len(polygon) < 3:
        return
    closed = close_points(np.asarray(polygon, dtype=np.float64))
    x0 = closed[:, 0]
    y0 = closed[:, 1]
    x1 = np.roll(x0, -1)
    y1 = np.roll(y0, -1)
    sub_width = width * subsamples
    sub_height = height * subsamples
    for row in range(sub_height):
        y = _subpixel_center_to_coordinate(row, subsamples, scale, origin[1])
        crossing = ((y0 <= y) & (y < y1)) | ((y1 <= y) & (y < y0))
        if not crossing.any():
            continue
        indices = np.nonzero(crossing)[0]
        dy = y1[indices] - y0[indices]
        intersection = x0[indices] + (y - y0[indices]) * (x1[indices] - x0[indices]) / dy
        intersection.sort()
        for left, right in zip(intersection[0::2], intersection[1::2]):
            start = int(
                math.ceil(
                    (left - origin[0]) * subsamples * scale - 0.5 - 1e-12
                )
            )
            stop = int(
                math.ceil(
                    (right - origin[0]) * subsamples * scale - 0.5 - 1e-12
                )
            )
            start = max(0, min(sub_width, start))
            stop = max(0, min(sub_width, stop))
            if stop > start:
                mask[row, start:stop] = True


def _circle_polygon(
    center: np.ndarray, radius: float, segments: int = 32
) -> np.ndarray:
    angles = np.linspace(0.0, 2.0 * math.pi, segments, endpoint=False)
    return np.column_stack(
        [center[0] + radius * np.cos(angles), center[1] + radius * np.sin(angles)]
    )


def _segment_normal(start: np.ndarray, end: np.ndarray) -> np.ndarray:
    direction = end - start
    length = float(np.hypot(direction[0], direction[1]))
    if length < 1e-14:
        return np.array([0.0, 0.0], dtype=np.float64)
    return np.array([-direction[1] / length, direction[0] / length], dtype=np.float64)


def _stroke_polygons(points: np.ndarray, path: VectorPath) -> list[np.ndarray]:
    radius = path.style.stroke_width / 2.0
    if radius <= 0.0:
        return []
    count = len(points)
    polygons: list[np.ndarray] = []
    for index in range(count):
        start = points[index]
        end = points[(index + 1) % count]
        normal = _segment_normal(start, end)
        if float(np.dot(normal, normal)) <= 1e-20:
            continue
        offset = normal * radius
        polygons.append(np.array([start + offset, end + offset, end - offset, start - offset]))

    if path.style.join == "round":
        for point in points:
            polygons.append(_circle_polygon(point, radius))
        return polygons

    miter_limit = 4.0 * radius
    for index in range(count):
        previous = points[(index - 1) % count]
        vertex = points[index]
        following = points[(index + 1) % count]
        normal_prev = _segment_normal(previous, vertex)
        normal_next = _segment_normal(vertex, following)
        if (
            float(np.dot(normal_prev, normal_prev)) <= 1e-20
            or float(np.dot(normal_next, normal_next)) <= 1e-20
        ):
            continue
        for sign in (-1.0, 1.0):
            offset_prev = normal_prev * radius * sign
            offset_next = normal_next * radius * sign
            first = vertex + offset_prev
            second = vertex + offset_next
            if path.style.join == "bevel":
                polygons.append(np.array([vertex, first, second]))
                continue
            direction = offset_prev + offset_next
            length = float(np.hypot(direction[0], direction[1]))
            if length < 1e-12:
                polygons.append(np.array([vertex, first, second]))
                continue
            direction /= length
            cosine = float(np.dot(direction, offset_prev / radius))
            if abs(cosine) < 1e-8:
                polygons.append(np.array([vertex, first, second]))
                continue
            distance = min(radius / cosine, miter_limit)
            tip = vertex + direction * distance
            polygons.append(np.array([vertex, first, tip, second]))
    return polygons


def _buffer_requirement(width: int, height: int, subsamples: int) -> int:
    sub_pixels = width * subsamples * height * subsamples
    coverage = width * height * np.dtype(np.float32).itemsize
    return int(sub_pixels + coverage)


def path_coverage(
    scene: Scene,
    path: VectorPath,
    flattened: np.ndarray,
    params: RenderParams,
) -> np.ndarray:
    width, height = canvas_size(scene, params)
    required = _buffer_requirement(width, height, params.subsamples)
    if required > params.coverage_buffer_limit:
        raise WorkbenchError(
            ErrorCode.E_CLIP,
            "coverage buffer exceeds the per-thread limit",
            path_id=path.id,
            stage="coverage",
            detail={
                "required_bytes": required,
                "limit_bytes": params.coverage_buffer_limit,
                "subsamples": params.subsamples,
            },
        )

    if path.kind == "stroke":
        stroke_pixels = path.style.stroke_width * params.scale
        if stroke_pixels > params.max_stroke_pixels:
            raise WorkbenchError(
                ErrorCode.E_CLIP,
                "stroke width exceeds the coverage clip limit",
                path_id=path.id,
                stage="coverage",
                detail={
                    "stroke_pixels": stroke_pixels,
                    "max_stroke_pixels": params.max_stroke_pixels,
                },
            )

    if polygon_area(flattened) <= 1e-10:
        raise WorkbenchError(
            ErrorCode.E_DEGEN,
            "closed contour has zero area",
            path_id=path.id,
            stage="coverage",
        )

    mask = np.zeros(
        (height * params.subsamples, width * params.subsamples), dtype=np.bool_
    )
    origin = (scene.render_bounds[0], scene.render_bounds[1])
    if path.kind == "fill":
        _fill_polygon_into_mask(
            mask,
            flattened,
            width=width,
            height=height,
            subsamples=params.subsamples,
            scale=params.scale,
            origin=origin,
        )
    elif path.kind == "stroke":
        for polygon in _stroke_polygons(flattened, path):
            _fill_polygon_into_mask(
                mask,
                polygon,
                width=width,
                height=height,
                subsamples=params.subsamples,
                scale=params.scale,
                origin=origin,
            )
    else:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            f"unsupported path kind {path.kind!r}",
            path_id=path.id,
            field="kind",
        )

    samples_per_pixel = params.subsamples * params.subsamples
    counts = mask.reshape(
        height, params.subsamples, width, params.subsamples
    ).sum(axis=(1, 3), dtype=np.uint32)
    coverage = counts.astype(np.float32) / np.float32(samples_per_pixel)
    return np.clip(coverage, np.float32(0.0), np.float32(1.0))


def composite_coverage(canvas: np.ndarray, path: VectorPath, coverage: np.ndarray) -> np.ndarray:
    if canvas.shape[:2] != coverage.shape:
        raise WorkbenchError(
            ErrorCode.E_STATE,
            "coverage dimensions do not match composition canvas",
            path_id=path.id,
            stage="compose",
        )
    color = (
        path.style.fill_color if path.kind == "fill" else path.style.stroke_color
    )
    source = np.asarray(color, dtype=np.float32)
    source_alpha = (coverage * np.float32(path.style.opacity)).astype(np.float32)
    alpha = source_alpha[..., None]
    out = canvas.copy()
    if path.style.blend == "normal":
        out[..., :3] = source * alpha + out[..., :3] * (np.float32(1.0) - alpha)
        out[..., 3] = source_alpha + out[..., 3] * (np.float32(1.0) - source_alpha)
    elif path.style.blend == "add":
        out[..., :3] = np.clip(
            out[..., :3] + source * alpha, np.float32(0.0), np.float32(1.0)
        )
        out[..., 3] = np.clip(
            out[..., 3] + source_alpha, np.float32(0.0), np.float32(1.0)
        )
    elif path.style.blend == "multiply":
        out[..., :3] = out[..., :3] * (
            np.float32(1.0) - alpha + alpha * source
        )
        out[..., 3] = source_alpha + out[..., 3] * (np.float32(1.0) - source_alpha)
    else:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            f"unsupported blend mode {path.style.blend!r}",
            path_id=path.id,
            stage="compose",
            field="blend",
        )
    return np.clip(out, np.float32(0.0), np.float32(1.0)).astype(np.float32, copy=False)


def empty_canvas(scene: Scene, params: RenderParams) -> np.ndarray:
    width, height = canvas_size(scene, params)
    return np.zeros((height, width, 4), dtype=np.float32)


def rgb8(image: np.ndarray) -> np.ndarray:
    rgb = np.clip(image[..., :3], 0.0, 1.0)
    return np.round(rgb * 255.0).astype(np.uint8)


def rgba8(image: np.ndarray) -> np.ndarray:
    rgba = np.clip(image, 0.0, 1.0)
    return np.round(rgba * 255.0).astype(np.uint8)

