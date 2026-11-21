"""Small dependency-free 3D math layer used by the deterministic pipeline."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable, Sequence

from .errors import fail

Vec3 = tuple[float, float, float]
Mat4 = tuple[Vec3, Vec3, Vec3, Vec3]

EPS = 1.0e-9


def v3(values: Sequence[float]) -> Vec3:
    return (float(values[0]), float(values[1]), float(values[2]))


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


def length(a: Vec3) -> float:
    return math.sqrt(dot(a, a))


def normalize(a: Vec3) -> Vec3:
    size = length(a)
    if not math.isfinite(size) or size <= EPS:
        return (0.0, 0.0, 0.0)
    return (a[0] / size, a[1] / size, a[2] / size)


def all_finite(values: Iterable[float]) -> bool:
    return all(math.isfinite(float(item)) for item in values)


def vec_all_finite(value: Vec3) -> bool:
    return all_finite(value)


def vmin(a: Vec3, b: Vec3) -> Vec3:
    return (min(a[0], b[0]), min(a[1], b[1]), min(a[2], b[2]))


def vmax(a: Vec3, b: Vec3) -> Vec3:
    return (max(a[0], b[0]), max(a[1], b[1]), max(a[2], b[2]))


def mat4_vec(matrix: Mat4, value: Vec3, w: float = 1.0) -> Vec3:
    x, y, z = value
    return (
        matrix[0][0] * x + matrix[0][1] * y + matrix[0][2] * z + matrix[0][3] * w,
        matrix[1][0] * x + matrix[1][1] * y + matrix[1][2] * z + matrix[1][3] * w,
        matrix[2][0] * x + matrix[2][1] * y + matrix[2][2] * z + matrix[2][3] * w,
    )


def mat4_det(matrix: Mat4) -> float:
    a, b, c, d = matrix
    return (
        a[0] * (b[1] * (c[2] * d[3] - c[3] * d[2]) - b[2] * (c[1] * d[3] - c[3] * d[1]) + b[3] * (c[1] * d[2] - c[2] * d[1]))
        - a[1] * (b[0] * (c[2] * d[3] - c[3] * d[2]) - b[2] * (c[0] * d[3] - c[3] * d[0]) + b[3] * (c[0] * d[2] - c[2] * d[0]))
        + a[2] * (b[0] * (c[1] * d[3] - c[3] * d[1]) - b[1] * (c[0] * d[3] - c[3] * d[0]) + b[3] * (c[0] * d[1] - c[1] * d[0]))
        - a[3] * (b[0] * (c[1] * d[2] - c[2] * d[1]) - b[1] * (c[0] * d[2] - c[2] * d[0]) + b[2] * (c[0] * d[1] - c[1] * d[0]))
    )


@dataclass(frozen=True)
class Ray:
    origin: Vec3
    direction: Vec3


def validate_ray(ray: Ray) -> None:
    if not vec_all_finite(ray.origin):
        fail("E_RAY_INVALID", "ray origin is not finite", "ray.origin")
    if not vec_all_finite(ray.direction):
        fail("E_RAY_INVALID", "ray direction is not finite", "ray.direction")
    if length(ray.direction) <= EPS:
        fail("E_RAY_INVALID", "ray has zero length", "ray.direction")


def ray_aabb(
    origin: Vec3,
    direction: Vec3,
    lower: Vec3,
    upper: Vec3,
    max_distance: float = math.inf,
) -> tuple[float, Vec3] | None:
    t_min = 0.0
    t_max = max_distance
    entry_axis = -1
    entry_sign = 0.0

    for axis in range(3):
        if abs(direction[axis]) <= EPS:
            if origin[axis] < lower[axis] or origin[axis] > upper[axis]:
                return None
            continue
        inv = 1.0 / direction[axis]
        near = (lower[axis] - origin[axis]) * inv
        far = (upper[axis] - origin[axis]) * inv
        sign = -1.0
        if near > far:
            near, far = far, near
            sign = 1.0
        if near > t_min:
            t_min = near
            entry_axis = axis
            entry_sign = sign
        if far < t_max:
            t_max = far
        if t_min > t_max:
            return None

    if t_max < 0.0 or t_max < t_min:
        return None
    if entry_axis < 0:
        normal = normalize((-direction[0], -direction[1], -direction[2]))
        return (0.0, normal)
    normal = [0.0, 0.0, 0.0]
    normal[entry_axis] = entry_sign
    return (t_min, (normal[0], normal[1], normal[2]))


def ray_triangle(
    origin: Vec3,
    direction: Vec3,
    a: Vec3,
    b: Vec3,
    c: Vec3,
) -> tuple[float, Vec3] | None:
    edge1 = sub(b, a)
    edge2 = sub(c, a)
    pvec = cross(direction, edge2)
    det = dot(edge1, pvec)
    if abs(det) <= EPS:
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
    distance = dot(edge2, qvec) * inv_det
    if distance < -EPS:
        return None
    distance = max(0.0, distance)
    geometric = normalize(cross(edge1, edge2))
    if dot(geometric, direction) > 0.0:
        geometric = mul(geometric, -1.0)
    return (distance, geometric)


def _closest_point_on_segment(p: Vec3, a: Vec3, b: Vec3) -> Vec3:
    ab = sub(b, a)
    denom = dot(ab, ab)
    if denom <= EPS:
        return a
    t = max(0.0, min(1.0, dot(sub(p, a), ab) / denom))
    return add(a, mul(ab, t))


def point_segment_distance(p: Vec3, a: Vec3, b: Vec3) -> float:
    return length(sub(p, _closest_point_on_segment(p, a, b)))


def ray_sphere(origin: Vec3, direction: Vec3, center: Vec3, radius: float) -> float | None:
    oc = sub(origin, center)
    half_b = dot(oc, direction)
    c = dot(oc, oc) - radius * radius
    disc = half_b * half_b - c
    if disc < -EPS:
        return None
    root = math.sqrt(max(0.0, disc))
    near = -half_b - root
    far = -half_b + root
    if near >= -EPS:
        return max(0.0, near)
    if far >= -EPS:
        return max(0.0, far)
    return None


def ray_capsule(
    origin: Vec3,
    direction: Vec3,
    a: Vec3,
    b: Vec3,
    radius: float,
) -> tuple[float, Vec3] | None:
    """Intersect a finite capsule, returning the closest entry and outward normal."""
    ba = sub(b, a)
    oa = sub(origin, a)
    baba = dot(ba, ba)
    if baba <= EPS or radius <= 0.0:
        return None

    candidates: list[float] = []

    # Infinite cylinder around the capsule segment.
    d_parallel = dot(ba, direction) / baba
    o_parallel = dot(ba, oa) / baba
    d_perp = sub(direction, mul(ba, d_parallel))
    o_perp = sub(oa, mul(ba, o_parallel))
    qa = dot(d_perp, d_perp)
    qb = 2.0 * dot(o_perp, d_perp)
    qc = dot(o_perp, o_perp) - radius * radius
    if qa > EPS:
        disc = qb * qb - 4.0 * qa * qc
        if disc >= -EPS:
            root = math.sqrt(max(0.0, disc))
            for t in ((-qb - root) / (2.0 * qa), (-qb + root) / (2.0 * qa)):
                if t >= -EPS:
                    along = o_parallel + t * d_parallel
                    if -EPS <= along <= 1.0 + EPS:
                        candidates.append(max(0.0, t))

    # End caps.
    for center in (a, b):
        t = ray_sphere(origin, direction, center, radius)
        if t is not None:
            point = add(origin, mul(direction, t))
            along = dot(sub(point, a), ba) / baba
            cap_ok = along <= EPS if center == a else along >= 1.0 - EPS
            if cap_ok:
                candidates.append(t)

    if not candidates:
        return None

    for t in sorted(set(round(value, 12) for value in candidates)):
        point = add(origin, mul(direction, t))
        closest = _closest_point_on_segment(point, a, b)
        normal = normalize(sub(point, closest))
        if length(normal) > EPS:
            return (t, normal)
    return None


def ensure_finite_vec(value: Vec3, code: str, message: str, point: str) -> None:
    if not vec_all_finite(value):
        fail(code, message, point)

