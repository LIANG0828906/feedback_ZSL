from __future__ import annotations

import math
from typing import Iterable, Sequence

Vec3 = tuple[float, float, float]
Quat = tuple[float, float, float, float]
Mat3 = tuple[
    float, float, float,
    float, float, float,
    float, float, float,
]

EPSILON = 1e-12


def vec3(x: float, y: float, z: float) -> Vec3:
    return (float(x), float(y), float(z))


def v_add(a: Vec3, b: Vec3) -> Vec3:
    return (a[0] + b[0], a[1] + b[1], a[2] + b[2])


def v_sub(a: Vec3, b: Vec3) -> Vec3:
    return (a[0] - b[0], a[1] - b[1], a[2] - b[2])


def v_mul(a: Vec3, s: float) -> Vec3:
    return (a[0] * s, a[1] * s, a[2] * s)


def v_dot(a: Vec3, b: Vec3) -> float:
    return a[0] * b[0] + a[1] * b[1] + a[2] * b[2]


def v_length(a: Vec3) -> float:
    return math.sqrt(v_dot(a, a))


def v_distance(a: Vec3, b: Vec3) -> float:
    return v_length(v_sub(a, b))


def mat_identity() -> Mat3:
    return (1.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 1.0)


def mat_mul(a: Mat3, b: Mat3) -> Mat3:
    return (
        a[0] * b[0] + a[1] * b[3] + a[2] * b[6],
        a[0] * b[1] + a[1] * b[4] + a[2] * b[7],
        a[0] * b[2] + a[1] * b[5] + a[2] * b[8],
        a[3] * b[0] + a[4] * b[3] + a[5] * b[6],
        a[3] * b[1] + a[4] * b[4] + a[5] * b[7],
        a[3] * b[2] + a[4] * b[5] + a[5] * b[8],
        a[6] * b[0] + a[7] * b[3] + a[8] * b[6],
        a[6] * b[1] + a[7] * b[4] + a[8] * b[7],
        a[6] * b[2] + a[7] * b[5] + a[8] * b[8],
    )


def mat_vec(m: Mat3, v: Vec3) -> Vec3:
    return (
        m[0] * v[0] + m[1] * v[1] + m[2] * v[2],
        m[3] * v[0] + m[4] * v[1] + m[5] * v[2],
        m[6] * v[0] + m[7] * v[1] + m[8] * v[2],
    )


def quat_identity() -> Quat:
    return (1.0, 0.0, 0.0, 0.0)


def quat_normalize(q: Quat) -> Quat:
    n = math.sqrt(q[0] * q[0] + q[1] * q[1] + q[2] * q[2] + q[3] * q[3])
    if n < EPSILON:
        return quat_identity()
    return (q[0] / n, q[1] / n, q[2] / n, q[3] / n)


def quat_canonical(q: Quat) -> Quat:
    q = quat_normalize(q)
    if q[0] < -EPSILON:
        return (-q[0], -q[1], -q[2], -q[3])
    if abs(q[0]) <= EPSILON:
        for component in q[1:]:
            if abs(component) > EPSILON:
                if component < 0.0:
                    return (-q[0], -q[1], -q[2], -q[3])
                break
    return q


def quat_mul(a: Quat, b: Quat) -> Quat:
    aw, ax, ay, az = a
    bw, bx, by, bz = b
    return (
        aw * bw - ax * bx - ay * by - az * bz,
        aw * bx + ax * bw + ay * bz - az * by,
        aw * by - ax * bz + ay * bw + az * bx,
        aw * bz + ax * by - ay * bx + az * bw,
    )


def quat_axis_angle(axis: str, angle_rad: float) -> Quat:
    half = angle_rad * 0.5
    s = math.sin(half)
    c = math.cos(half)
    if axis == "X":
        return (c, s, 0.0, 0.0)
    if axis == "Y":
        return (c, 0.0, s, 0.0)
    if axis == "Z":
        return (c, 0.0, 0.0, s)
    raise ValueError(f"Unknown rotation axis: {axis}")


def quat_to_mat3(q: Quat) -> Mat3:
    w, x, y, z = quat_normalize(q)
    xx, yy, zz = x * x, y * y, z * z
    xy, xz, yz = x * y, x * z, y * z
    wx, wy, wz = w * x, w * y, w * z
    return (
        1.0 - 2.0 * (yy + zz), 2.0 * (xy - wz), 2.0 * (xz + wy),
        2.0 * (xy + wz), 1.0 - 2.0 * (xx + zz), 2.0 * (yz - wx),
        2.0 * (xz - wy), 2.0 * (yz + wx), 1.0 - 2.0 * (xx + yy),
    )


def quat_rotate_vec(q: Quat, v: Vec3) -> Vec3:
    return mat_vec(quat_to_mat3(q), v)


def quat_from_euler_order(angles_rad: Sequence[float], order: str) -> Quat:
    if len(angles_rad) != len(order):
        raise ValueError("Angle and order lengths differ")
    result = quat_identity()
    for angle, axis in zip(angles_rad, order):
        result = quat_mul(result, quat_axis_angle(axis, angle))
    return quat_canonical(result)


def _euler_xyz(m: Mat3) -> tuple[float, float, float]:
    b = math.asin(max(-1.0, min(1.0, m[2])))
    if abs(math.cos(b)) > 1e-8:
        a = math.atan2(-m[5], m[8])
        c = math.atan2(-m[1], m[0])
    else:
        a = math.atan2(m[7], m[4])
        c = 0.0
    return (a, b, c)


def _euler_zxy(m: Mat3) -> tuple[float, float, float]:
    b = math.asin(max(-1.0, min(1.0, m[7])))
    if abs(math.cos(b)) > 1e-8:
        a = math.atan2(-m[1], m[4])
        c = math.atan2(-m[6], m[8])
    else:
        a = math.atan2(m[3], m[4])
        c = 0.0
    return (a, b, c)


def _euler_yzx(m: Mat3) -> tuple[float, float, float]:
    b = math.asin(max(-1.0, min(1.0, m[3])))
    if abs(math.cos(b)) > 1e-8:
        a = math.atan2(-m[6], m[0])
        c = math.atan2(-m[5], m[4])
    else:
        a = math.atan2(m[7], m[8])
        c = 0.0
    return (a, b, c)


def _euler_xzy(m: Mat3) -> tuple[float, float, float]:
    b = math.asin(max(-1.0, min(1.0, -m[1])))
    if abs(math.cos(b)) > 1e-8:
        a = math.atan2(m[7], m[4])
        c = math.atan2(m[2], m[0])
    else:
        if b >= 0.0:
            a = math.atan2(m[6], m[3])
        else:
            a = math.atan2(-m[6], -m[3])
        c = 0.0
    return (a, b, c)


def _euler_zyx(m: Mat3) -> tuple[float, float, float]:
    b = math.asin(max(-1.0, min(1.0, -m[6])))
    if abs(math.cos(b)) > 1e-8:
        a = math.atan2(m[3], m[0])
        c = math.atan2(m[7], m[8])
    else:
        a = math.atan2(-m[5], m[4])
        c = 0.0
    return (a, b, c)


def _euler_yxz(m: Mat3) -> tuple[float, float, float]:
    b = math.asin(max(-1.0, min(1.0, -m[5])))
    if abs(math.cos(b)) > 1e-8:
        a = math.atan2(m[2], m[8])
        c = math.atan2(m[3], m[4])
    else:
        if b >= 0.0:
            a = math.atan2(m[2], m[0])
        else:
            a = math.atan2(-m[2], m[0])
        c = 0.0
    return (a, b, c)


_EULER_EXTRACTORS = {
    "XYZ": _euler_xyz,
    "ZXY": _euler_zxy,
    "YZX": _euler_yzx,
    "XZY": _euler_xzy,
    "ZYX": _euler_zyx,
    "YXZ": _euler_yxz,
}


def quat_to_euler_order(q: Quat, order: str) -> tuple[float, ...]:
    if len(order) == 1:
        axis = order
        m = quat_to_mat3(q)
        if axis == "X":
            return (math.atan2(m[7], m[4]),)
        if axis == "Y":
            return (math.atan2(m[2], m[0]),)
        return (math.atan2(m[3], m[0]),)
    extractor = _EULER_EXTRACTORS.get(order)
    if extractor is None:
        raise ValueError(f"Unsupported Euler order: {order}")
    return extractor(quat_to_mat3(q))


def validate_euler_order(order: Iterable[str]) -> str:
    text = "".join(order).upper()
    if not text or any(axis not in "XYZ" for axis in text):
        raise ValueError("Euler order must contain only X, Y, and Z")
    if len(set(text)) != len(text):
        raise ValueError("Euler order repeats an axis")
    if len(text) > 3:
        raise ValueError("Euler order is too long")
    return text
