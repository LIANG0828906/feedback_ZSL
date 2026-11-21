"""Small deterministic 2D geometry helpers used by stroke generation."""

import math

import numpy as np


def polyline_arclength(points):
    diffs = np.diff(points, axis=0)
    return float(np.sqrt((diffs ** 2).sum(axis=1)).sum())


def point_at_arclength(points, target):
    """Uniform-arc-length sampling on a flattened polyline."""
    remaining = float(target)
    for i in range(len(points) - 1):
        a = points[i]
        b = points[i + 1]
        seg_len = float(np.hypot(b[0] - a[0], b[1] - a[1]))
        if seg_len <= 1e-12:
            continue
        if remaining <= seg_len or i == len(points) - 2:
            t = min(1.0, max(0.0, remaining / seg_len))
            p = a + (b - a) * t
            tangent = (b - a) / seg_len
            return p, tangent
        remaining -= seg_len
    p = points[-2]
    b = points[-1]
    seg_len = float(np.hypot(b[0] - p[0], b[1] - p[1]))
    return points[-1], (b - p) / seg_len


def segment_intersect(p1, p2, p3, p4):
    """Return True for a proper intersection; touching endpoints -> False."""
    d1 = _cross(p3 - p1, p4 - p1)
    d2 = _cross(p3 - p2, p4 - p2)
    d3 = _cross(p1 - p3, p2 - p3)
    d4 = _cross(p1 - p4, p2 - p4)
    eps = 1e-10
    return ((d1 > eps and d2 < -eps) or (d1 < -eps and d2 > eps)) and (
        (d3 > eps and d4 < -eps) or (d3 < -eps and d4 > eps)
    )


def point_segment_distance(point, a, b):
    ab = b - a
    denom = float(ab @ ab)
    if denom <= 1e-18:
        return float(np.hypot(*(point - a)))
    t = float(np.clip((point - a) @ ab / denom, 0.0, 1.0))
    proj = a + t * ab
    return float(np.hypot(*(point - proj)))


def distance_to_ring(point, ring):
    best = float("inf")
    n = len(ring)
    for i in range(n):
        d = point_segment_distance(point, ring[i], ring[(i + 1) % n])
        if d < best:
            best = d
    return best


def point_in_ring(point, ring):
    inside = False
    x, y = float(point[0]), float(point[1])
    n = len(ring)
    j = n - 1
    for i in range(n):
        xi, yi = ring[i]
        xj, yj = ring[j]
        if (yi > y) != (yj > y):
            x_cross = (xj - xi) * (y - yi) / (yj - yi) + xi
            if x_cross > x:
                inside = not inside
        j = i
    return inside


def clip_line_to_ring(origin, direction, ring):
    """Liang-Barsky style clipping of a ray against each convex segment pair.

    Works for arbitrary simple polygons by intersecting the infinite line with
    every edge and sorting the inside intervals via a reference point.
    """
    direction = np.asarray(direction, dtype=float)
    origin = np.asarray(origin, dtype=float)
    cuts = []
    n = len(ring)
    for i in range(n):
        a = ring[i]
        b = ring[(i + 1) % n]
        edge = b - a
        denom = _cross(direction, edge)
        if abs(denom) <= 1e-12:
            continue
        diff = a - origin
        t = _cross(diff, edge) / denom
        u = _cross(diff, direction) / denom
        if -1e-10 <= u <= 1.0 + 1e-10:
            cuts.append(float(t))
    cuts.sort()
    intervals = []
    for k in range(0, len(cuts) - 1, 2):
        t0, t1 = cuts[k], cuts[k + 1]
        if t1 - t0 <= 1e-9:
            continue
        mid = origin + direction * (0.5 * (t0 + t1))
        if point_in_ring(mid, ring):
            intervals.append((max(0.0, t0), t1))
    return intervals


def _cross(a, b):
    return float(a[0] * b[1] - a[1] * b[0])


def unit_angle(angle_deg):
    rad = math.radians(angle_deg)
    return np.array([math.cos(rad), math.sin(rad)], dtype=float)


def perpendicular(direction):
    return np.array([-direction[1], direction[0]], dtype=float)
