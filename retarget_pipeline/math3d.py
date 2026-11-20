"""确定性的四元数 / 旋转 / 层级变换数学。

约定
----
* 四元数布局为 ``[w, x, y, z]``，全部为单位四元数（除显式归一化入口外）。
* 矩阵为 3x3 旋转矩阵或 4x4 刚体/仿射矩阵，行主序 NumPy 数组。
* 局部变换: 关节相对父关节；全局变换: 相对骨架根（世界）。
* 本模块不引入随机源，所有分支均为确定性计算。
"""

from __future__ import annotations

import math
from typing import Sequence

import numpy as np

Quat = np.ndarray
Vec3 = np.ndarray

EPS = 1.0e-10
# 近重合旋转判定阈值（弧度，约 0.5 度）。样本生成器与求解器共用。
NEAR_COINCIDENT_ANGLE = 8.72664626e-3


def as_vec3(v: Sequence[float] | np.ndarray) -> np.ndarray:
    arr = np.asarray(v, dtype=np.float64)
    if arr.shape != (3,):
        raise ValueError(f"期望长度为 3 的向量，得到 shape={arr.shape}")
    return arr


def as_quat(q: Sequence[float] | np.ndarray) -> np.ndarray:
    arr = np.asarray(q, dtype=np.float64)
    if arr.shape != (4,):
        raise ValueError(f"期望长度为 4 的四元数 [w,x,y,z]，得到 shape={arr.shape}")
    return arr


def quat_identity() -> np.ndarray:
    return np.array([1.0, 0.0, 0.0, 0.0], dtype=np.float64)


def quat_normalize(q: Sequence[float]) -> np.ndarray:
    """归一化；退化（近零）四元数返回恒等旋转，由调用方决定是否报告。"""

    arr = as_quat(q)
    norm = float(np.linalg.norm(arr))
    if norm < EPS:
        return quat_identity()
    return arr / norm


def quat_is_degenerate(q: Sequence[float]) -> bool:
    return float(np.linalg.norm(as_quat(q))) < EPS


def quat_mul(a: Sequence[float], b: Sequence[float]) -> np.ndarray:
    """四元数乘法 ``a * b``（先应用 b 再应用 a）。"""

    aw, ax, ay, az = as_quat(a)
    bw, bx, by, bz = as_quat(b)
    return np.array(
        [
            aw * bw - ax * bx - ay * by - az * bz,
            aw * bx + ax * bw + ay * bz - az * by,
            aw * by - ax * bz + ay * bw + az * bx,
            aw * bz + ax * by - ay * bx + az * bw,
        ],
        dtype=np.float64,
    )


def quat_conj(q: Sequence[float]) -> np.ndarray:
    w, x, y, z = as_quat(q)
    return np.array([w, -x, -y, -z], dtype=np.float64)


def quat_inv(q: Sequence[float]) -> np.ndarray:
    n = as_quat(q)
    return quat_conj(n) / float(np.dot(n, n))


def quat_rotate(q: Sequence[float], v: Sequence[float]) -> np.ndarray:
    """把四元数旋转作用于向量。"""

    qn = quat_normalize(q)
    _, x, y, z = qn
    vec = as_vec3(v)
    u = np.array([x, y, z], dtype=np.float64)
    return vec + 2.0 * np.cross(u, np.cross(u, vec) + qn[0] * vec)


def quat_to_matrix(q: Sequence[float]) -> np.ndarray:
    """单位四元数 -> 3x3 旋转矩阵。"""

    w, x, y, z = quat_normalize(q)
    return np.array(
        [
            [1 - 2 * (y * y + z * z), 2 * (x * y - z * w), 2 * (x * z + y * w)],
            [2 * (x * y + z * w), 1 - 2 * (x * x + z * z), 2 * (y * z - x * w)],
            [2 * (x * z - y * w), 2 * (y * z + x * w), 1 - 2 * (x * x + y * y)],
        ],
        dtype=np.float64,
    )


def matrix_to_quat(m: np.ndarray) -> np.ndarray:
    """3x3 旋转矩阵 -> 四元数 ``[w,x,y,z]``（Shepperd 方法，数值稳定）。

    若矩阵非正交/退化（无法求解旋转）抛出 ``ValueError``，
    由重定向层转换为 ``E_POSE_UNRESOLVED``。
    """

    mat = np.asarray(m, dtype=np.float64)
    if mat.shape != (3, 3):
        raise ValueError(f"期望 3x3 旋转矩阵，得到 shape={mat.shape}")
    if not np.allclose(mat @ mat.T, np.eye(3), atol=1e-5) or abs(np.linalg.det(mat) - 1.0) > 1e-5:
        raise ValueError("旋转矩阵非正交或行列式不为 1，无法求解四元数")

    trace = mat[0, 0] + mat[1, 1] + mat[2, 2]
    if trace > 0.0:
        s = math.sqrt(trace + 1.0) * 2.0
        w = 0.25 * s
        x = (mat[2, 1] - mat[1, 2]) / s
        y = (mat[0, 2] - mat[2, 0]) / s
        z = (mat[1, 0] - mat[0, 1]) / s
    elif mat[0, 0] > mat[1, 1] and mat[0, 0] > mat[2, 2]:
        s = math.sqrt(1.0 + mat[0, 0] - mat[1, 1] - mat[2, 2]) * 2.0
        w = (mat[2, 1] - mat[1, 2]) / s
        x = 0.25 * s
        y = (mat[0, 1] + mat[1, 0]) / s
        z = (mat[0, 2] + mat[2, 0]) / s
    elif mat[1, 1] > mat[2, 2]:
        s = math.sqrt(1.0 + mat[1, 1] - mat[0, 0] - mat[2, 2]) * 2.0
        w = (mat[0, 2] - mat[2, 0]) / s
        x = (mat[0, 1] + mat[1, 0]) / s
        y = 0.25 * s
        z = (mat[1, 2] + mat[2, 1]) / s
    else:
        s = math.sqrt(1.0 + mat[2, 2] - mat[0, 0] - mat[1, 1]) * 2.0
        w = (mat[1, 0] - mat[0, 1]) / s
        x = (mat[0, 2] + mat[2, 0]) / s
        y = (mat[1, 2] + mat[2, 1]) / s
        z = 0.25 * s
    return quat_normalize([w, x, y, z])


def quat_between_vectors(a: Sequence[float], b: Sequence[float]) -> np.ndarray:
    """求解把单位向量 a 转到单位向量 b 的最短弧四元数。"""

    va = as_vec3(a)
    vb = as_vec3(b)
    na = float(np.linalg.norm(va))
    nb = float(np.linalg.norm(vb))
    if na < EPS or nb < EPS:
        raise ValueError("零向量无法构造旋转")
    va = va / na
    vb = vb / nb
    dot = float(np.clip(np.dot(va, vb), -1.0, 1.0))
    if dot < -1.0 + 1e-9:
        # 相反向量：任选一个正交轴
        axis = np.cross([1.0, 0.0, 0.0], va)
        if np.linalg.norm(axis) < EPS:
            axis = np.cross([0.0, 1.0, 0.0], va)
        axis = axis / np.linalg.norm(axis)
        return np.array([0.0, axis[0], axis[1], axis[2]], dtype=np.float64)
    axis = np.cross(va, vb)
    w = 1.0 + dot
    return quat_normalize([w, axis[0], axis[1], axis[2]])


def quat_angle(a: Sequence[float], b: Sequence[float]) -> float:
    """两个朝向之间的测地角度（弧度，非负）。"""

    qa = quat_normalize(a)
    qb = quat_normalize(b)
    inner = abs(float(np.clip(np.dot(qa, qb), -1.0, 1.0)))
    return 2.0 * math.acos(min(1.0, inner))


def quat_relative(global_q: Sequence[float], parent_global_q: Sequence[float]) -> np.ndarray:
    """由父全局朝向与本关节全局朝向求局部朝向。"""

    return quat_mul(quat_inv(quat_normalize(parent_global_q)), quat_normalize(global_q))


def make_transform(rotation: np.ndarray, translation: Sequence[float], scale: float = 1.0) -> np.ndarray:
    """构造 4x4 仿射变换 T = R * S，平移放在最后一列。"""

    m = np.eye(4, dtype=np.float64)
    m[:3, :3] = np.asarray(rotation, dtype=np.float64) * float(scale)
    m[:3, 3] = as_vec3(translation)
    return m


def transform_rotation(m: np.ndarray) -> np.ndarray:
    """从 4x4 变换提取纯旋转（对含缩放矩阵做列归一化）。"""

    m = np.asarray(m, dtype=np.float64)
    r = m[:3, :3].copy()
    col_norms = np.linalg.norm(r, axis=0)
    if np.any(col_norms < EPS):
        raise ValueError("变换包含退化缩放轴，无法提取旋转")
    return r / col_norms


def transform_translation(m: np.ndarray) -> np.ndarray:
    return np.asarray(m, dtype=np.float64)[:3, 3].copy()


def mat4_mul(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    return np.asarray(a, dtype=np.float64) @ np.asarray(b, dtype=np.float64)


def order_hierarchy(parents: Sequence[int | None]) -> list[int]:
    """返回保证父先于子的拓扑顺序；根父索引为 -1/None。

    成环或父索引越域抛出 ``ValueError``（由 codec 层转业务码）。
    """

    n = len(parents)
    ordered: list[int] = []
    state = [0] * n  # 0 未访问, 1 访问中, 2 完成

    def visit(idx: int, stack: tuple[int, ...]) -> None:
        if idx < 0 or idx >= n:
            raise ValueError(f"父关节索引越域: {idx}")
        if state[idx] == 1:
            cycle = " -> ".join(str(j) for j in stack + (idx,))
            raise ValueError(f"层级父子关系成环: {cycle}")
        if state[idx] == 2:
            return
        state[idx] = 1
        p = parents[idx]
        if p is not None and p >= 0:
            visit(int(p), stack + (idx,))
        state[idx] = 2
        ordered.append(idx)

    for i in range(n):
        if state[i] == 0:
            visit(i, ())
    return ordered


def forward_kinematics(
    parents: Sequence[int | None],
    local_rotations: Sequence[np.ndarray],
    local_translations: Sequence[np.ndarray],
    bone_scales: Sequence[float] | None = None,
) -> tuple[list[np.ndarray], list[np.ndarray]]:
    """层级正向运动学。

    返回 ``(全局旋转四元数列表, 全局平移列表)``。

    缩放语义：每个关节的局部缩放 ``s_i`` 只作用于该关节自身的绑定偏移，
    通过把缩放烘焙进局部平移实现，因此父关节的缩放不会对子关节旋转
    重复施加——旋转始终以纯四元数级联，避免“父子重复缩放”。
    """

    n = len(parents)
    order = order_hierarchy(parents)
    global_rot: list[np.ndarray | None] = [None] * n
    global_pos: list[np.ndarray | None] = [None] * n
    scales = list(bone_scales) if bone_scales is not None else [1.0] * n

    for idx in order:
        parent = parents[idx]
        loc_r = quat_normalize(local_rotations[idx])
        loc_t = as_vec3(local_translations[idx]) * float(scales[idx])
        if parent is None or parent < 0:
            global_rot[idx] = loc_r
            global_pos[idx] = loc_t
        else:
            gp = global_pos[parent]
            gr = global_rot[parent]
            assert gp is not None and gr is not None
            global_rot[idx] = quat_mul(gr, loc_r)
            global_pos[idx] = gp + quat_rotate(gr, loc_t)
    return [q for q in global_rot if q is not None], [p for p in global_pos if p is not None]


def nearest_rotation_matrix(m: np.ndarray) -> np.ndarray:
    """通过 SVD 把含误差的 3x3 矩阵投影到最近正交旋转（极分解）。"""

    m = np.asarray(m, dtype=np.float64)
    u, _, vt = np.linalg.svd(m)
    r = u @ vt
    if np.linalg.det(r) < 0.0:
        u[:, -1] *= -1.0
        r = u @ vt
    return r
