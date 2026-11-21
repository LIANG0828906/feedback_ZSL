"""Small deterministic geometry kernels used by projection and hit testing."""

from __future__ import annotations

import math
from typing import Iterable, Sequence

import numpy as np

Point2 = tuple[float, float]
Rect = tuple[float, float, float, float]

GEOMETRY_EPS = 1e-9
AREA_EPS = 1e-12


def normalize_rect(x0: float, y0: float, x1: float, y1: float) -> Rect:
    return (min(x0, x1), min(y0, y1), max(x0, x1), max(y0, y1))


def clip_rect_to_viewport(rect: Rect, width: int, height: int) -> Rect | None:
    xmin = max(0.0, rect[0])
    ymin = max(0.0, rect[1])
    xmax = min(float(width), rect[2])
    ymax = min(float(height), rect[3])
    if xmax <= xmin or ymax <= ymin:
        return None
    return (xmin, ymin, xmax, ymax)


def rect_intersects(a: Rect, b: Rect) -> bool:
    return a[0] < b[2] and a[2] > b[0] and a[1] < b[3] and a[3] > b[1]


def convex_hull(points: Iterable[Sequence[float]]) -> list[Point2]:
    unique = sorted({(float(point[0]), float(point[1])) for point in points})
    if len(unique) <= 1:
        return unique

    def cross(o: Point2, a: Point2, b: Point2) -> float:
        return (a[0] - o[0]) * (b[1] - o[1]) - (a[1] - o[1]) * (b[0] - o[0])

    lower: list[Point2] = []
    for point in unique:
        while len(lower) >= 2 and cross(lower[-2], lower[-1], point) <= GEOMETRY_EPS:
            lower.pop()
        lower.append(point)
    upper: list[Point2] = []
    for point in reversed(unique):
        while len(upper) >= 2 and cross(upper[-2], upper[-1], point) <= GEOMETRY_EPS:
            upper.pop()
        upper.append(point)
    hull = lower[:-1] + upper[:-1]
    if len(hull) < 3:
        return unique
    return hull


def polygon_area(points: Sequence[Sequence[float]]) -> float:
    if len(points) < 3:
        return 0.0
    total = 0.0
    for index, point in enumerate(points):
        following = points[(index + 1) % len(points)]
        total += float(point[0]) * float(following[1]) - float(following[0]) * float(point[1])
    return abs(total) * 0.5


def _clip_against_line(
    polygon: list[Point2],
    *,
    axis: int,
    boundary: float,
    keep_greater: bool,
) -> list[Point2]:
    if not polygon:
        return []
    output: list[Point2] = []
    previous = polygon[-1]
    previous_inside = previous[axis] >= boundary if keep_greater else previous[axis] <= boundary
    for current in polygon:
        current_inside = current[axis] >= boundary if keep_greater else current[axis] <= boundary
        if current_inside != previous_inside:
            delta = current[axis] - previous[axis]
            if abs(delta) > GEOMETRY_EPS:
                t = (boundary - previous[axis]) / delta
                intersection = (
                    previous[0] + t * (current[0] - previous[0]),
                    previous[1] + t * (current[1] - previous[1]),
                )
                if abs(intersection[axis] - boundary) <= 1e-7:
                    if axis == 0:
                        intersection = (boundary, intersection[1])
                    else:
                        intersection = (intersection[0], boundary)
                output.append(intersection)
        if current_inside:
            output.append(current)
        previous = current
        previous_inside = current_inside
    return output


def clip_polygon_to_rect(points: Sequence[Sequence[float]], rect: Rect) -> list[Point2]:
    polygon: list[Point2] = [(float(point[0]), float(point[1])) for point in points]
    if len(polygon) < 3:
        return []
    xmin, ymin, xmax, ymax = rect
    polygon = _clip_against_line(polygon, axis=0, boundary=xmin, keep_greater=True)
    polygon = _clip_against_line(polygon, axis=0, boundary=xmax, keep_greater=False)
    polygon = _clip_against_line(polygon, axis=1, boundary=ymin, keep_greater=True)
    polygon = _clip_against_line(polygon, axis=1, boundary=ymax, keep_greater=False)
    if polygon_area(polygon) <= AREA_EPS:
        return []
    return polygon


def point_in_convex_polygon(point: Sequence[float], polygon: Sequence[Sequence[float]], eps: float = 1e-7) -> bool:
    if len(polygon) < 3:
        return False
    px, py = float(point[0]), float(point[1])
    sign = 0
    for index, current in enumerate(polygon):
        following = polygon[(index + 1) % len(polygon)]
        edge_x = float(following[0]) - float(current[0])
        edge_y = float(following[1]) - float(current[1])
        rel_x = px - float(current[0])
        rel_y = py - float(current[1])
        cross = edge_x * rel_y - edge_y * rel_x
        length = math.hypot(edge_x, edge_y)
        if length <= GEOMETRY_EPS:
            continue
        distance = cross / length
        if distance > eps:
            if sign < 0:
                return False
            sign = 1
        elif distance < -eps:
            if sign > 0:
                return False
            sign = -1
    return True


def distance_to_polygon_boundary(point: Sequence[float], polygon: Sequence[Sequence[float]]) -> float:
    if len(polygon) < 2:
        return math.inf
    px, py = float(point[0]), float(point[1])
    best = math.inf
    for index, current in enumerate(polygon):
        following = polygon[(index + 1) % len(polygon)]
        ax, ay = float(current[0]), float(current[1])
        bx, by = float(following[0]), float(following[1])
        dx, dy = bx - ax, by - ay
        length_sq = dx * dx + dy * dy
        if length_sq <= GEOMETRY_EPS:
            distance = math.hypot(px - ax, py - ay)
        else:
            t = max(0.0, min(1.0, ((px - ax) * dx + (py - ay) * dy) / length_sq))
            distance = math.hypot(px - (ax + t * dx), py - (ay + t * dy))
        best = min(best, distance)
    return best


def polygon_bbox(points: Sequence[Sequence[float]]) -> Rect | None:
    if not points:
        return None
    xs = [float(point[0]) for point in points]
    ys = [float(point[1]) for point in points]
    return (min(xs), min(ys), max(xs), max(ys))


def _signed_polygon_area(points: Sequence[Sequence[float]]) -> float:
    total = 0.0
    for index, point in enumerate(points):
        following = points[(index + 1) % len(points)]
        total += float(point[0]) * float(following[1]) - float(following[0]) * float(point[1])
    return total * 0.5


def convex_polygon_intersection(
    subject: Sequence[Sequence[float]],
    clip: Sequence[Sequence[float]],
) -> list[Point2]:
    """Intersect two convex polygons with deterministic Sutherland-Hodgman."""

    output: list[Point2] = [(float(point[0]), float(point[1])) for point in subject]
    clipper: list[Point2] = [(float(point[0]), float(point[1])) for point in clip]
    if len(output) < 3 or len(clipper) < 3:
        return []
    if _signed_polygon_area(clipper) < 0.0:
        clipper.reverse()
    for index, start in enumerate(clipper):
        if not output:
            break
        end = clipper[(index + 1) % len(clipper)]
        edge_x = end[0] - start[0]
        edge_y = end[1] - start[1]

        def inside(point: Point2) -> bool:
            return edge_x * (point[1] - start[1]) - edge_y * (point[0] - start[0]) >= -1e-9

        previous = output[-1]
        previous_inside = inside(previous)
        next_output: list[Point2] = []
        for current in output:
            current_inside = inside(current)
            if current_inside != previous_inside:
                dx = current[0] - previous[0]
                dy = current[1] - previous[1]
                denominator = edge_x * dy - edge_y * dx
                if abs(denominator) > GEOMETRY_EPS:
                    t = (edge_y * (previous[0] - start[0]) - edge_x * (previous[1] - start[1])) / denominator
                    t = max(0.0, min(1.0, t))
                    next_output.append((previous[0] + t * dx, previous[1] + t * dy))
            if current_inside:
                next_output.append(current)
            previous = current
            previous_inside = current_inside
        output = next_output
    if polygon_area(output) <= AREA_EPS:
        return []
    return output


def bbox_area(rect: Rect) -> float:
    return max(0.0, rect[2] - rect[0]) * max(0.0, rect[3] - rect[1])


def capsule_surface_samples(
    a: Sequence[float],
    b: Sequence[float],
    radius: float,
    *,
    axis_samples: int = 5,
    radial_samples: int = 16,
) -> tuple[np.ndarray, list[tuple[int, int]]]:
    """Tessellate a capsule surface into deterministic points and edges."""

    endpoint_a = np.asarray(a, dtype=np.float64)
    endpoint_b = np.asarray(b, dtype=np.float64)
    direction = endpoint_b - endpoint_a
    length = float(np.linalg.norm(direction))
    if length > GEOMETRY_EPS:
        direction /= length
        reference = np.array([1.0, 0.0, 0.0]) if abs(direction[0]) < 0.9 else np.array([0.0, 1.0, 0.0])
        u = np.cross(direction, reference)
        u /= np.linalg.norm(u)
        v = np.cross(direction, u)
    else:
        direction = np.array([0.0, 0.0, 1.0])
        u = np.array([1.0, 0.0, 0.0])
        v = np.array([0.0, 1.0, 0.0])

    radial_samples = max(6, int(radial_samples))
    axis_samples = max(2, int(axis_samples))
    angles = np.arange(radial_samples, dtype=np.float64) * (2.0 * math.pi / radial_samples)
    radial_offsets = radius * (
        np.cos(angles)[:, None] * u[None, :]
        + np.sin(angles)[:, None] * v[None, :]
    )
    axis_t = np.linspace(0.0, 1.0, axis_samples)
    ring_centers = endpoint_a[None, :] + direction[None, :] * (length * axis_t)[:, None]
    ring_points = ring_centers[:, None, :] + radial_offsets[None, :, :]

    latitude_values = np.asarray([math.pi / 6.0, math.pi / 3.0], dtype=np.float64)
    cap_rings: list[np.ndarray] = []
    for outward, center in ((-direction, endpoint_a), (direction, endpoint_b)):
        latitude_offsets = (
            radius
            * np.cos(latitude_values)[:, None, None]
            * outward[None, None, :]
            + radius
            * np.sin(latitude_values)[:, None, None]
            * (
                np.cos(angles)[None, :, None] * u[None, None, :]
                + np.sin(angles)[None, :, None] * v[None, None, :]
            )
        )
        cap_rings.append(center[None, None, :] + latitude_offsets)

    all_rings = [
        cap_rings[0][1],
        cap_rings[0][0],
        *[ring_points[index] for index in range(axis_samples)],
        cap_rings[1][0],
        cap_rings[1][1],
    ]
    points = np.concatenate(all_rings, axis=0)
    ring_indices = [
        list(range(ring_index * radial_samples, (ring_index + 1) * radial_samples))
        for ring_index in range(len(all_rings))
    ]
    edges: list[tuple[int, int]] = []
    for ring in ring_indices:
        for index in range(len(ring)):
            edges.append((ring[index], ring[(index + 1) % len(ring)]))
    for left, right in zip(ring_indices, ring_indices[1:]):
        for index in range(radial_samples):
            edges.append((left[index], right[index]))
    return np.asarray(points, dtype=np.float64), edges
