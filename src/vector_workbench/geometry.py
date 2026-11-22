from __future__ import annotations

import math
from typing import Iterable, Sequence

import numpy as np

from .errors import ErrorCode, WorkbenchError
from .model import CubicSegment, Scene, VectorPath

_GL_X = (
    -0.9324695142031521,
    -0.6612093864662645,
    -0.2386191860831969,
    0.2386191860831969,
    0.6612093864662645,
    0.9324695142031521,
)
_GL_W = (
    0.1713244923791704,
    0.3607615730481386,
    0.4679139345726910,
    0.4679139345726910,
    0.3607615730481386,
    0.1713244923791704,
)


def segment_array(segment: CubicSegment, coord_scale: int, scale: float = 1.0) -> np.ndarray:
    denominator = float(coord_scale)
    return (
        np.array(
            [segment.start, segment.control1, segment.control2, segment.end],
            dtype=np.float64,
        )
        / denominator
        * float(scale)
    )


def evaluate_cubic(points: np.ndarray, t: float) -> np.ndarray:
    u = 1.0 - t
    return (
        (u * u * u) * points[0]
        + (3.0 * u * u * t) * points[1]
        + (3.0 * u * t * t) * points[2]
        + (t * t * t) * points[3]
    )


def evaluate_path_at(path: VectorPath, coord_scale: int, segment: int, t: float) -> tuple[float, float]:
    point = evaluate_cubic(segment_array(path.segments[segment], coord_scale), t)
    return float(point[0]), float(point[1])


def _cubic_extrema(p0: float, p1: float, p2: float, p3: float) -> list[float]:
    a = -p0 + 3.0 * p1 - 3.0 * p2 + p3
    b = 3.0 * p0 - 6.0 * p1 + 3.0 * p2
    c = -3.0 * p0 + 3.0 * p1
    roots: list[float] = []
    if abs(a) < 1e-14:
        if abs(b) > 1e-14:
            t = -c / b
            if 0.0 < t < 1.0:
                roots.append(t)
        return roots
    discriminant = b * b - 4.0 * a * c
    if discriminant < 0.0:
        return roots
    root = math.sqrt(max(0.0, discriminant))
    for t in ((-b - root) / (2.0 * a), (-b + root) / (2.0 * a)):
        if 0.0 < t < 1.0:
            roots.append(t)
    return roots


def cubic_bbox_scene(segment: CubicSegment, coord_scale: int) -> tuple[float, float, float, float]:
    points = segment_array(segment, coord_scale)
    candidates_x = [0.0, 1.0]
    candidates_y = [0.0, 1.0]
    candidates_x.extend(_cubic_extrema(*(float(v) for v in points[:, 0])))
    candidates_y.extend(_cubic_extrema(*(float(v) for v in points[:, 1])))
    evaluated = np.array(
        [evaluate_cubic(points, t) for t in sorted(set(candidates_x + candidates_y))]
    )
    return (
        float(evaluated[:, 0].min()),
        float(evaluated[:, 1].min()),
        float(evaluated[:, 0].max()),
        float(evaluated[:, 1].max()),
    )


def path_bbox_scene(path: VectorPath, coord_scale: int) -> tuple[float, float, float, float]:
    boxes = [cubic_bbox_scene(segment, coord_scale) for segment in path.segments]
    return (
        min(box[0] for box in boxes),
        min(box[1] for box in boxes),
        max(box[2] for box in boxes),
        max(box[3] for box in boxes),
    )


def scene_contour_bbox(paths: Iterable[VectorPath], coord_scale: int) -> tuple[float, float, float, float]:
    boxes = [path_bbox_scene(path, coord_scale) for path in paths]
    return (
        min(box[0] for box in boxes),
        min(box[1] for box in boxes),
        max(box[2] for box in boxes),
        max(box[3] for box in boxes),
    )


def _cubic_area_scene(segment: CubicSegment, coord_scale: int) -> float:
    points = segment_array(segment, coord_scale)
    total = 0.0
    for x, weight in zip(_GL_X, _GL_W):
        t = 0.5 * (x + 1.0)
        w = 0.5 * weight
        u = 1.0 - t
        p = (
            (u * u * u) * points[0]
            + (3.0 * u * u * t) * points[1]
            + (3.0 * u * t * t) * points[2]
            + (t * t * t) * points[3]
        )
        dp = (
            (3.0 * u * u) * (points[1] - points[0])
            + (6.0 * u * t) * (points[2] - points[1])
            + (3.0 * t * t) * (points[3] - points[2])
        )
        total += w * float(p[0] * dp[1] - p[1] * dp[0])
    return 0.5 * total


def signed_path_area_scene(path: VectorPath, coord_scale: int) -> float:
    return sum(_cubic_area_scene(segment, coord_scale) for segment in path.segments)


def path_area_scene(path: VectorPath, coord_scale: int) -> float:
    return abs(signed_path_area_scene(path, coord_scale))


def _flatness(points: np.ndarray) -> float:
    chord = points[3] - points[0]
    length = float(np.hypot(chord[0], chord[1]))
    if length < 1e-14:
        return max(
            float(np.hypot(*(points[1] - points[0]))),
            float(np.hypot(*(points[2] - points[0]))),
            float(np.hypot(*(points[3] - points[0]))),
        )
    d1 = abs(_cross2(chord, points[1] - points[0])) / length
    d2 = abs(_cross2(chord, points[2] - points[0])) / length
    return max(d1, d2)


def _split_cubic(points: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    p01 = 0.5 * (points[0] + points[1])
    p12 = 0.5 * (points[1] + points[2])
    p23 = 0.5 * (points[2] + points[3])
    p012 = 0.5 * (p01 + p12)
    p123 = 0.5 * (p12 + p23)
    middle = 0.5 * (p012 + p123)
    return (
        np.array([points[0], p01, p012, middle], dtype=np.float64),
        np.array([middle, p123, p23, points[3]], dtype=np.float64),
    )


def _append_unique(points: list[np.ndarray], point: np.ndarray) -> None:
    if not points:
        points.append(point.copy())
        return
    delta = point - points[-1]
    if float(np.hypot(delta[0], delta[1])) > 1e-12:
        points.append(point.copy())


def flatten_path(
    path: VectorPath,
    coord_scale: int,
    scale: float,
    tolerance: float,
    *,
    max_depth: int = 24,
) -> np.ndarray:
    """Flatten a closed cubic path to an Nx2 polyline in render pixels.

    The returned array does not repeat the first point at the end. The contour
    is still closed logically because every cubic segment is chained.
    """

    if (
        isinstance(scale, bool)
        or not isinstance(scale, (int, float))
        or not math.isfinite(float(scale))
        or scale <= 0
    ):
        raise WorkbenchError(ErrorCode.E_PARA, "scale must be finite and > 0", field="scale")
    if (
        isinstance(tolerance, bool)
        or not isinstance(tolerance, (int, float))
        or not math.isfinite(float(tolerance))
        or tolerance <= 0
    ):
        raise WorkbenchError(
            ErrorCode.E_PARA, "tolerance must be finite and > 0", field="tolerance"
        )

    out: list[np.ndarray] = []

    def recursive(sub: np.ndarray, depth: int) -> None:
        if _flatness(sub) <= tolerance or depth >= max_depth:
            _append_unique(out, sub[3])
            return
        left, right = _split_cubic(sub)
        recursive(left, depth + 1)
        recursive(right, depth + 1)

    for index, segment in enumerate(path.segments):
        sub = segment_array(segment, coord_scale, scale)
        if not out:
            _append_unique(out, sub[0])
        elif float(np.hypot(*(sub[0] - out[-1]))) > 1e-12:
            raise WorkbenchError(
                ErrorCode.E_GEOM,
                "segment start does not continue previous contour endpoint",
                path_id=path.id,
                segment=index,
            )
        recursive(sub, 0)

    if len(out) > 1 and float(np.hypot(*(out[-1] - out[0]))) <= 1e-12:
        out.pop()

    cleaned: list[np.ndarray] = []
    for point in out:
        _append_unique(cleaned, point)
    if len(cleaned) > 1 and float(np.hypot(*(cleaned[-1] - cleaned[0]))) <= 1e-12:
        cleaned.pop()
    return np.asarray(cleaned, dtype=np.float64)


def close_points(points: np.ndarray) -> np.ndarray:
    if len(points) == 0:
        return points
    return np.vstack([points, points[0]])


def polygon_area(points: np.ndarray) -> float:
    if len(points) < 3:
        return 0.0
    x = points[:, 0]
    y = points[:, 1]
    return float(abs(0.5 * np.sum(x * np.roll(y, -1) - y * np.roll(x, -1))))


def polygon_bbox(points: np.ndarray) -> tuple[float, float, float, float]:
    return (
        float(points[:, 0].min()),
        float(points[:, 1].min()),
        float(points[:, 0].max()),
        float(points[:, 1].max()),
    )


def _cross2(a: np.ndarray, b: np.ndarray) -> float:
    return float(a[0] * b[1] - a[1] * b[0])


def _orientation(a: np.ndarray, b: np.ndarray, c: np.ndarray) -> float:
    return _cross2(b - a, c - a)


def _on_segment(a: np.ndarray, b: np.ndarray, p: np.ndarray, eps: float = 1e-10) -> bool:
    return (
        min(a[0], b[0]) - eps <= p[0] <= max(a[0], b[0]) + eps
        and min(a[1], b[1]) - eps <= p[1] <= max(a[1], b[1]) + eps
    )


def _segments_intersect(a: np.ndarray, b: np.ndarray, c: np.ndarray, d: np.ndarray) -> bool:
    o1 = _orientation(a, b, c)
    o2 = _orientation(a, b, d)
    o3 = _orientation(c, d, a)
    o4 = _orientation(c, d, b)
    eps = 1e-9
    if ((o1 > eps and o2 < -eps) or (o1 < -eps and o2 > eps)) and (
        (o3 > eps and o4 < -eps) or (o3 < -eps and o4 > eps)
    ):
        return True
    if abs(o1) <= eps and _on_segment(a, b, c):
        return True
    if abs(o2) <= eps and _on_segment(a, b, d):
        return True
    if abs(o3) <= eps and _on_segment(c, d, a):
        return True
    if abs(o4) <= eps and _on_segment(c, d, b):
        return True
    return False


def find_self_intersection(points: np.ndarray) -> tuple[int, int] | None:
    """Return the first pair of intersecting non-adjacent closed edges."""

    count = len(points)
    if count < 4:
        return None
    for i in range(count):
        a = points[i]
        b = points[(i + 1) % count]
        for j in range(i + 1, count):
            if j == i + 1 or (i == 0 and j == count - 1):
                continue
            c = points[j]
            d = points[(j + 1) % count]
            if _segments_intersect(a, b, c, d):
                return i, j
    return None


def point_to_polyline_distance(points: np.ndarray, sample: Sequence[float]) -> float:
    p = np.asarray(sample, dtype=np.float64)
    closed = close_points(points)
    best = math.inf
    for start, end in zip(closed[:-1], closed[1:]):
        edge = end - start
        norm2 = float(np.dot(edge, edge))
        if norm2 <= 1e-20:
            distance = float(np.hypot(*(p - start)))
        else:
            t = max(0.0, min(1.0, float(np.dot(p - start, edge) / norm2)))
            distance = float(np.hypot(*(p - (start + t * edge))))
        best = min(best, distance)
    return best


def truth_curve_samples(scene: Scene, path: VectorPath, t_values: Sequence[float]) -> list[dict[str, float]]:
    samples: list[dict[str, float]] = []
    for segment_index in range(len(path.segments)):
        for t in t_values:
            x, y = evaluate_path_at(path, scene.coord_scale, segment_index, t)
            samples.append(
                {
                    "segment": segment_index,
                    "t": float(t),
                    "point": [x, y],
                }
            )
    return samples

