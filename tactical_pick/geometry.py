"""Small deterministic vector and intersection helpers.

All vectors are plain 3-tuples of floats.  Keeping this layer dependency-free
makes command-line replay bit-for-bit reproducible and also avoids NumPy edge
case differences between versions.
"""

from __future__ import annotations

import math
from typing import Iterable, Sequence

Vec3 = tuple[float, float, float]
Vec2 = tuple[float, float]

EPS = 1e-12
PARALLEL_EPS = 1e-12
GRAZING_EPS = 1e-7


def v3(values: Sequence[float]) -> Vec3:
    if len(values) != 3:
        raise ValueError("expected three coordinates")
    return (float(values[0]), float(values[1]), float(values[2]))


def v2(values: Sequence[float]) -> Vec2:
    if len(values) != 2:
        raise ValueError("expected two coordinates")
    return (float(values[0]), float(values[1]))


def add(a: Vec3, b: Vec3) -> Vec3:
    return (a[0] + b[0], a[1] + b[1], a[2] + b[2])


def sub(a: Vec3, b: Vec3) -> Vec3:
    return (a[0] - b[0], a[1] - b[1], a[2] - b[2])


def mul(a: Vec3, scalar: float) -> Vec3:
    return (a[0] * scalar, a[1] * scalar, a[2] * scalar)


def dot(a: Vec3, b: Vec3) -> float:
    return a[0] * b[0] + a[1] * b[1] + a[2] * b[2]


def cross(a: Vec3, b: Vec3) -> Vec3:
    return (
        a[1] * b[2] - a[2] * b[1],
        a[2] * b[0] - a[0] * b[2],
        a[0] * b[1] - a[1] * b[0],
    )


def length_sq(a: Vec3) -> float:
    return dot(a, a)


def length(a: Vec3) -> float:
    return math.sqrt(length_sq(a))


def distance(a: Vec3, b: Vec3) -> float:
    return length(sub(a, b))


def normalize(a: Vec3) -> Vec3:
    n = length(a)
    if not math.isfinite(n) or n <= EPS:
        raise ValueError("cannot normalize a zero or non-finite vector")
    return (a[0] / n, a[1] / n, a[2] / n)


def clamp(value: float, low: float, high: float) -> float:
    return low if value < low else high if value > high else value


def point_segment_distance_sq(point: Vec3, a: Vec3, b: Vec3) -> float:
    ab = sub(b, a)
    denom = dot(ab, ab)
    if denom <= EPS:
        return length_sq(sub(point, a))
    t = clamp(dot(sub(point, a), ab) / denom, 0.0, 1.0)
    closest = add(a, mul(ab, t))
    return length_sq(sub(point, closest))


def point_segment_distance_sq_2d(point: Vec2, a: Vec2, b: Vec2) -> float:
    abx = b[0] - a[0]
    aby = b[1] - a[1]
    denom = abx * abx + aby * aby
    if denom <= EPS:
        dx = point[0] - a[0]
        dy = point[1] - a[1]
        return dx * dx + dy * dy
    t = ((point[0] - a[0]) * abx + (point[1] - a[1]) * aby) / denom
    t = clamp(t, 0.0, 1.0)
    dx = point[0] - (a[0] + abx * t)
    dy = point[1] - (a[1] + aby * t)
    return dx * dx + dy * dy


def aabb_contains_point(minimum: Vec3, maximum: Vec3, point: Vec3) -> bool:
    return all(minimum[i] - EPS <= point[i] <= maximum[i] + EPS for i in range(3))


def ray_aabb_intersection(
    origin: Vec3,
    direction: Vec3,
    minimum: Vec3,
    maximum: Vec3,
) -> tuple[float, Vec3] | None:
    """Return the nearest ray/AABB entry and its outward normal."""

    t_near = -math.inf
    t_far = math.inf
    near_axis = -1
    for axis in range(3):
        o = origin[axis]
        d = direction[axis]
        lo = minimum[axis]
        hi = maximum[axis]
        if abs(d) <= PARALLEL_EPS:
            if o < lo - EPS or o > hi + EPS:
                return None
            continue
        inv = 1.0 / d
        t1 = (lo - o) * inv
        t2 = (hi - o) * inv
        if t1 > t2:
            t1, t2 = t2, t1
        if t1 > t_near:
            t_near = t1
            near_axis = axis
        if t2 < t_far:
            t_far = t2
        if t_near > t_far + EPS:
            return None
    if t_far < -EPS:
        return None
    if t_near < 0.0:
        if t_far < 0.0:
            return None
        t_near = 0.0
    if near_axis < 0:
        normal = (0.0, 0.0, -1.0)
    else:
        normal_list = [0.0, 0.0, 0.0]
        normal_list[near_axis] = 1.0 if direction[near_axis] < 0.0 else -1.0
        normal = (normal_list[0], normal_list[1], normal_list[2])
    return (t_near, normal)


def ray_triangle_intersection(
    origin: Vec3,
    direction: Vec3,
    a: Vec3,
    b: Vec3,
    c: Vec3,
) -> tuple[float, Vec3] | None:
    """Two-sided Moller-Trumbore intersection with a grazing tolerance."""

    edge1 = sub(b, a)
    edge2 = sub(c, a)
    pvec = cross(direction, edge2)
    det = dot(edge1, pvec)
    if abs(det) <= PARALLEL_EPS:
        return None
    inv_det = 1.0 / det
    tvec = sub(origin, a)
    u = dot(tvec, pvec) * inv_det
    if u < -EPS or u > 1.0 + EPS:
        return None
    qvec = cross(tvec, edge1)
    v = dot(direction, qvec) * inv_det
    if v < -EPS or u + v > 1.0 + EPS:
        return None
    t = dot(edge2, qvec) * inv_det
    if t < -EPS:
        return None
    t = max(0.0, t)
    normal = normalize(cross(edge1, edge2))
    if dot(normal, direction) > 0.0:
        normal = (-normal[0], -normal[1], -normal[2])
    return (t, normal)


def ray_sphere_intersection(
    origin: Vec3,
    direction: Vec3,
    center: Vec3,
    radius: float,
) -> tuple[float, Vec3] | None:
    oc = sub(origin, center)
    b = dot(oc, direction)
    c = dot(oc, oc) - radius * radius
    disc = b * b - c
    if disc < -EPS:
        return None
    root = math.sqrt(max(0.0, disc))
    candidates = (-b - root, -b + root)
    for t in candidates:
        if t >= -EPS:
            t = max(0.0, t)
            point = add(origin, mul(direction, t))
            normal = normalize(sub(point, center))
            if dot(normal, direction) > 0.0:
                normal = (-normal[0], -normal[1], -normal[2])
            return (t, normal)
    return None


def ray_capsule_intersection(
    origin: Vec3,
    direction: Vec3,
    capsule_a: Vec3,
    capsule_b: Vec3,
    radius: float,
) -> tuple[float, Vec3] | None:
    """Ray intersection against a closed capsule (cylinder plus two caps)."""

    axis = sub(capsule_b, capsule_a)
    axis_len_sq = length_sq(axis)
    candidates: list[tuple[float, Vec3]] = []

    if axis_len_sq <= EPS:
        hit = ray_sphere_intersection(origin, direction, capsule_a, radius)
        return hit

    axis_len = math.sqrt(axis_len_sq)
    axis_dir = mul(axis, 1.0 / axis_len)
    m = sub(origin, capsule_a)
    md = dot(m, axis_dir)
    nd = sub(direction, mul(axis_dir, dot(direction, axis_dir)))
    radial = sub(m, mul(axis_dir, md))
    a = dot(nd, nd)
    b = 2.0 * dot(nd, radial)
    c = dot(radial, radial) - radius * radius

    if a > PARALLEL_EPS:
        disc = b * b - 4.0 * a * c
        if disc >= -EPS:
            root = math.sqrt(max(0.0, disc))
            for t in ((-b - root) / (2.0 * a), (-b + root) / (2.0 * a)):
                if t < -EPS:
                    continue
                t = max(0.0, t)
                along = md + t * dot(direction, axis_dir)
                if -EPS <= along <= axis_len + EPS:
                    point = add(origin, mul(direction, t))
                    closest = add(capsule_a, mul(axis_dir, clamp(along, 0.0, axis_len)))
                    normal = normalize(sub(point, closest))
                    if dot(normal, direction) > 0.0:
                        normal = (-normal[0], -normal[1], -normal[2])
                    candidates.append((t, normal))

    for center, is_a in ((capsule_a, True), (capsule_b, False)):
        hit = ray_sphere_intersection(origin, direction, center, radius)
        if hit is None:
            continue
        t, normal = hit
        point = add(origin, mul(direction, t))
        along = dot(sub(point, capsule_a), axis_dir)
        if is_a and along <= EPS:
            candidates.append((t, normal))
        elif not is_a and along >= axis_len - EPS:
            candidates.append((t, normal))

    if not candidates:
        return None
    candidates.sort(key=lambda item: (item[0], item[1]))
    return candidates[0]


def ray_plane_z_intersection(
    origin: Vec3,
    direction: Vec3,
    plane_z: float,
) -> tuple[float, Vec3] | None:
    if abs(direction[2]) <= PARALLEL_EPS:
        return None
    t = (plane_z - origin[2]) / direction[2]
    if t < -EPS:
        return None
    t = max(0.0, t)
    normal: Vec3 = (0.0, 0.0, 1.0)
    if dot(normal, direction) > 0.0:
        normal = (0.0, 0.0, -1.0)
    return (t, normal)


def near_grazing(direction: Vec3, normal: Vec3, threshold: float = GRAZING_EPS) -> bool:
    return abs(dot(direction, normal)) <= threshold


def finite_vec(values: Iterable[float]) -> bool:
    return all(math.isfinite(float(value)) for value in values)
