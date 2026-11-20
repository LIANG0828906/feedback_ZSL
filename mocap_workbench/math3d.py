"""Small deterministic 3-D math layer.

Only float64 NumPy arrays are used for solver state and hashes.  Hash inputs
are quantized before serialization so platform-independent bytes can be
compared across runs while still preserving sub-micrometre pose detail.
"""

from __future__ import annotations

import hashlib
import math
from collections.abc import Iterable, Sequence

import numpy as np

AXIS_INDEX = {"x": 0, "X": 0, "y": 1, "Y": 1, "z": 2, "Z": 2}
HASH_QUANTUM = 1.0e-9


def axis_rotation(axis: str, angle: float) -> np.ndarray:
    """Return a 3x3 right-handed rotation matrix for one Cartesian axis."""

    idx = AXIS_INDEX[axis]
    c = math.cos(float(angle))
    s = math.sin(float(angle))
    out = np.eye(3, dtype=np.float64)
    if idx == 0:
        out[1, 1] = c
        out[1, 2] = -s
        out[2, 1] = s
        out[2, 2] = c
    elif idx == 1:
        out[0, 0] = c
        out[0, 2] = s
        out[2, 0] = -s
        out[2, 2] = c
    else:
        out[0, 0] = c
        out[0, 1] = -s
        out[1, 0] = s
        out[1, 1] = c
    return out


def euler_matrix(order: Sequence[str], angles: Sequence[float]) -> np.ndarray:
    """Compose BVH channel rotations.

    BVH channels are multiplied left-to-right.  Thus a ZXY channel vector
    yields ``Rz @ Rx @ Ry``.
    """

    out = np.eye(3, dtype=np.float64)
    for axis, angle in zip(order, angles):
        out = out @ axis_rotation(axis, angle)
    return out


def matrix_to_euler(order: Sequence[str], matrix: np.ndarray) -> np.ndarray:
    """Decompose a matrix into the supplied BVH rotation order.

    The formulas are derived for ``Ra(a) @ Rb(b) @ Rc(c)`` where the order is
    any permutation of X/Y/Z.  A deterministic zero-angle fallback handles the
    one-degree-of-freedom gimbal singularity.
    """

    key = "".join(a.upper() for a in order)
    if len(key) != 3 or set(key) != {"X", "Y", "Z"}:
        raise ValueError(f"unsupported Euler order {order!r}")
    r = np.asarray(matrix, dtype=np.float64)
    eps = 1.0e-12
    if key == "XYZ":
        y = math.asin(float(np.clip(r[0, 2], -1.0, 1.0)))
        if abs(math.cos(y)) > eps:
            x = math.atan2(-r[1, 2], r[2, 2])
            z = math.atan2(-r[0, 1], r[0, 0])
        else:
            x = 0.0
            z = math.atan2(r[1, 0], r[1, 1])
        return np.array([x, y, z], dtype=np.float64)
    if key == "XZY":
        z = math.asin(float(np.clip(-r[0, 1], -1.0, 1.0)))
        if abs(math.cos(z)) > eps:
            x = math.atan2(r[2, 1], r[1, 1])
            y = math.atan2(r[0, 2], r[0, 0])
        else:
            x = 0.0
            y = math.atan2(-r[2, 0], r[2, 2])
        return np.array([x, z, y], dtype=np.float64)
    if key == "YXZ":
        x = math.asin(float(np.clip(-r[1, 2], -1.0, 1.0)))
        if abs(math.cos(x)) > eps:
            y = math.atan2(r[0, 2], r[2, 2])
            z = math.atan2(r[1, 0], r[1, 1])
        else:
            y = 0.0
            z = math.atan2(-r[0, 1], r[0, 0])
        return np.array([y, x, z], dtype=np.float64)
    if key == "YZX":
        z = math.asin(float(np.clip(r[1, 0], -1.0, 1.0)))
        if abs(math.cos(z)) > eps:
            y = math.atan2(-r[2, 0], r[0, 0])
            x = math.atan2(-r[1, 2], r[1, 1])
        else:
            y = 0.0
            x = math.atan2(r[2, 1], r[2, 2])
        return np.array([y, z, x], dtype=np.float64)
    if key == "ZXY":
        x = math.asin(float(np.clip(r[2, 1], -1.0, 1.0)))
        if abs(math.cos(x)) > eps:
            z = math.atan2(-r[0, 1], r[1, 1])
            y = math.atan2(-r[2, 0], r[2, 2])
        else:
            z = 0.0
            y = math.atan2(r[0, 2], r[0, 0])
        return np.array([z, x, y], dtype=np.float64)
    # ZYX
    y = math.asin(float(np.clip(-r[2, 0], -1.0, 1.0)))
    if abs(math.cos(y)) > eps:
        z = math.atan2(r[1, 0], r[0, 0])
        x = math.atan2(r[2, 1], r[2, 2])
    else:
        z = 0.0
        x = math.atan2(-r[1, 2], r[1, 1])
    return np.array([z, y, x], dtype=np.float64)


def rotation_vector_to_matrix(vector: Sequence[float]) -> np.ndarray:
    """Rodrigues exponential map from a rotation vector to a 3x3 matrix."""

    v = np.asarray(vector, dtype=np.float64)
    theta = float(np.linalg.norm(v))
    if theta < 1.0e-14:
        wx, wy, wz = v
        return np.array(
            [[1.0, -wz, wy], [wz, 1.0, -wx], [-wy, wx, 1.0]],
            dtype=np.float64,
        )
    axis = v / theta
    x, y, z = axis
    c = math.cos(theta)
    s = math.sin(theta)
    t = 1.0 - c
    return np.array(
        [
            [t * x * x + c, t * x * y - s * z, t * x * z + s * y],
            [t * x * y + s * z, t * y * y + c, t * y * z - s * x],
            [t * x * z - s * y, t * y * z + s * x, t * z * z + c],
        ],
        dtype=np.float64,
    )


def matrix_to_rotation_vector(matrix: np.ndarray) -> np.ndarray:
    """Logarithm map from SO(3) to a rotation vector."""

    r = np.asarray(matrix, dtype=np.float64)
    trace = float(np.trace(r))
    cos_theta = max(-1.0, min(1.0, (trace - 1.0) * 0.5))
    theta = math.acos(cos_theta)
    if theta < 1.0e-12:
        return np.array(
            [
                (r[2, 1] - r[1, 2]) * 0.5,
                (r[0, 2] - r[2, 0]) * 0.5,
                (r[1, 0] - r[0, 1]) * 0.5,
            ],
            dtype=np.float64,
        )
    if math.pi - theta < 1.0e-8:
        # Stable axis extraction near pi.
        diag = np.maximum((np.diag(r) + 1.0) * 0.5, 0.0)
        axis = np.sqrt(diag)
        k = int(np.argmax(axis))
        if axis[k] > 1.0e-12:
            if k == 0:
                axis[1] = math.copysign(axis[1], r[0, 1] + r[1, 0])
                axis[2] = math.copysign(axis[2], r[0, 2] + r[2, 0])
            elif k == 1:
                axis[0] = math.copysign(axis[0], r[0, 1] + r[1, 0])
                axis[2] = math.copysign(axis[2], r[1, 2] + r[2, 1])
            else:
                axis[0] = math.copysign(axis[0], r[0, 2] + r[2, 0])
                axis[1] = math.copysign(axis[1], r[1, 2] + r[2, 1])
            axis = axis / np.linalg.norm(axis)
            return axis * theta
    scale = theta / (2.0 * math.sin(theta))
    return np.array(
        [
            (r[2, 1] - r[1, 2]) * scale,
            (r[0, 2] - r[2, 0]) * scale,
            (r[1, 0] - r[0, 1]) * scale,
        ],
        dtype=np.float64,
    )


def orthonormalize(matrix: np.ndarray) -> np.ndarray:
    """Project a noisy 3x3 matrix back onto SO(3)."""

    u, _, vt = np.linalg.svd(np.asarray(matrix, dtype=np.float64))
    out = u @ vt
    if np.linalg.det(out) < 0.0:
        u[:, -1] *= -1.0
        out = u @ vt
    return out


def transform_matrix(rotation: np.ndarray, translation: np.ndarray) -> np.ndarray:
    out = np.eye(4, dtype=np.float64)
    out[:3, :3] = rotation
    out[:3, 3] = np.asarray(translation, dtype=np.float64)
    return out


def quantize(values: np.ndarray, quantum: float = HASH_QUANTUM) -> np.ndarray:
    return np.rint(np.asarray(values, dtype=np.float64) / quantum).astype("<i8")


def canonical_hash(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def hash_array(values: np.ndarray) -> str:
    arr = np.ascontiguousarray(quantize(values))
    h = hashlib.sha256()
    h.update(str(arr.shape).encode("ascii"))
    h.update(b"|")
    h.update(arr.tobytes(order="C"))
    return h.hexdigest()


def hash_pose(joint_names: Iterable[str], transforms: np.ndarray) -> str:
    names = "\x1f".join(joint_names).encode("utf-8")
    h = hashlib.sha256()
    h.update(b"pose-v1\x00")
    h.update(names)
    h.update(b"\x00")
    h.update(np.ascontiguousarray(quantize(transforms)).tobytes(order="C"))
    return h.hexdigest()


def hash_text(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def hash_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def canonical_json_bytes(value: object) -> bytes:
    import json

    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def quaternion_from_matrix(matrix: np.ndarray) -> np.ndarray:
    """Convert SO(3) to a canonical [x,y,z,w] quaternion."""

    r = orthonormalize(matrix)
    trace = float(np.trace(r))
    if trace > 0.0:
        s = math.sqrt(trace + 1.0) * 2.0
        w = 0.25 * s
        x = (r[2, 1] - r[1, 2]) / s
        y = (r[0, 2] - r[2, 0]) / s
        z = (r[1, 0] - r[0, 1]) / s
    elif r[0, 0] > r[1, 1] and r[0, 0] > r[2, 2]:
        s = math.sqrt(1.0 + r[0, 0] - r[1, 1] - r[2, 2]) * 2.0
        w = (r[2, 1] - r[1, 2]) / s
        x = 0.25 * s
        y = (r[0, 1] + r[1, 0]) / s
        z = (r[0, 2] + r[2, 0]) / s
    elif r[1, 1] > r[2, 2]:
        s = math.sqrt(1.0 + r[1, 1] - r[0, 0] - r[2, 2]) * 2.0
        w = (r[0, 2] - r[2, 0]) / s
        x = (r[0, 1] + r[1, 0]) / s
        y = 0.25 * s
        z = (r[1, 2] + r[2, 1]) / s
    else:
        s = math.sqrt(1.0 + r[2, 2] - r[0, 0] - r[1, 1]) * 2.0
        w = (r[1, 0] - r[0, 1]) / s
        x = (r[0, 2] + r[2, 0]) / s
        y = (r[1, 2] + r[2, 1]) / s
        z = 0.25 * s
    q = np.array([x, y, z, w], dtype=np.float64)
    if q[3] < 0.0:
        q = -q
    return q


def transform_to_record(transform: np.ndarray) -> np.ndarray:
    """Pack a 4x4 transform into translation + quaternion (7 values)."""

    t = np.asarray(transform, dtype=np.float64)
    return np.concatenate([t[:3, 3], quaternion_from_matrix(t[:3, :3])])


def record_to_transform(record: np.ndarray) -> np.ndarray:
    x, y, z, qx, qy, qz, qw = np.asarray(record, dtype=np.float64)
    n = math.sqrt(qx * qx + qy * qy + qz * qz + qw * qw)
    if n < 1.0e-15:
        q = np.array([0.0, 0.0, 0.0, 1.0])
    else:
        q = np.array([qx, qy, qz, qw]) / n
    xq, yq, zq, wq = q
    r = np.array(
        [
            [
                1.0 - 2.0 * (yq * yq + zq * zq),
                2.0 * (xq * yq - zq * wq),
                2.0 * (xq * zq + yq * wq),
            ],
            [
                2.0 * (xq * yq + zq * wq),
                1.0 - 2.0 * (xq * xq + zq * zq),
                2.0 * (yq * zq - xq * wq),
            ],
            [
                2.0 * (xq * zq - yq * wq),
                2.0 * (yq * zq + xq * wq),
                1.0 - 2.0 * (xq * xq + yq * yq),
            ],
        ],
        dtype=np.float64,
    )
    return transform_matrix(r, np.array([x, y, z]))


def max_abs_diff(a: np.ndarray, b: np.ndarray) -> float:
    if np.asarray(a).shape != np.asarray(b).shape:
        return math.inf
    if np.asarray(a).size == 0:
        return 0.0
    return float(np.max(np.abs(np.asarray(a) - np.asarray(b))))
