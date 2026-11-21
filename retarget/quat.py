"""Quaternion and rotation-matrix helpers (float64, [w, x, y, z] convention)."""

import numpy as np


def qnormalize(q):
    q = np.asarray(q, dtype=np.float64)
    n = float(np.linalg.norm(q))
    if n == 0.0:
        raise ValueError("zero-norm quaternion")
    return q / n


def qmul(a, b):
    aw, ax, ay, az = a
    bw, bx, by, bz = b
    return np.array([
        aw * bw - ax * bx - ay * by - az * bz,
        aw * bx + ax * bw + ay * bz - az * by,
        aw * by - ax * bz + ay * bw + az * bx,
        aw * bz + ax * by - ay * bx + az * bw,
    ], dtype=np.float64)


def qconj(q):
    q = np.asarray(q, dtype=np.float64)
    return np.array([q[0], -q[1], -q[2], -q[3]], dtype=np.float64)


def qrotate(q, v):
    q = np.asarray(q, dtype=np.float64)
    v = np.asarray(v, dtype=np.float64)
    qv = q[1:]
    t = 2.0 * np.cross(qv, v)
    return v + q[0] * t + np.cross(qv, t)


def qangle_between(a, b):
    """Absolute rotation angle (radians) between two quaternions."""
    qa = qnormalize(a)
    qb = qnormalize(b)
    d = abs(float(np.dot(qa, qb)))
    d = min(1.0, d)
    return 2.0 * float(np.arccos(d))


def qfrom_matrix(m):
    """Convert a 3x3 rotation matrix to a quaternion (Shepperd's method)."""
    m = np.asarray(m, dtype=np.float64)
    tr = m[0, 0] + m[1, 1] + m[2, 2]
    if tr > 0.0:
        s = np.sqrt(tr + 1.0) * 2.0
        w = 0.25 * s
        x = (m[2, 1] - m[1, 2]) / s
        y = (m[0, 2] - m[2, 0]) / s
        z = (m[1, 0] - m[0, 1]) / s
    elif m[0, 0] > m[1, 1] and m[0, 0] > m[2, 2]:
        s = np.sqrt(1.0 + m[0, 0] - m[1, 1] - m[2, 2]) * 2.0
        w = (m[2, 1] - m[1, 2]) / s
        x = 0.25 * s
        y = (m[0, 1] + m[1, 0]) / s
        z = (m[0, 2] + m[2, 0]) / s
    elif m[1, 1] > m[2, 2]:
        s = np.sqrt(1.0 + m[1, 1] - m[0, 0] - m[2, 2]) * 2.0
        w = (m[0, 2] - m[2, 0]) / s
        x = (m[0, 1] + m[1, 0]) / s
        y = 0.25 * s
        z = (m[1, 2] + m[2, 1]) / s
    else:
        s = np.sqrt(1.0 + m[2, 2] - m[0, 0] - m[1, 1]) * 2.0
        w = (m[1, 0] - m[0, 1]) / s
        x = (m[0, 2] + m[2, 0]) / s
        y = (m[1, 2] + m[2, 1]) / s
        z = 0.25 * s
    return qnormalize(np.array([w, x, y, z], dtype=np.float64))


def mat_from_quat(q):
    w, x, y, z = qnormalize(q)
    return np.array([
        [1 - 2 * (y * y + z * z), 2 * (x * y - z * w), 2 * (x * z + y * w)],
        [2 * (x * y + z * w), 1 - 2 * (x * x + z * z), 2 * (y * z - x * w)],
        [2 * (x * z - y * w), 2 * (y * z + x * w), 1 - 2 * (x * x + y * y)],
    ], dtype=np.float64)
