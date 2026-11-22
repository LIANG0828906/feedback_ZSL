"""矩阵、3D LUT 三线性插值、Bradford 白点适配与渲染意图策略。"""

from __future__ import annotations

import numpy as np

from .errors import CwbError, E_INTENT, E_WP
from .model import (
    INTENT_ABSOLUTE,
    INTENT_PERCEPTUAL,
    INTENT_RELATIVE,
    ColorProfile,
    LUT3D,
    TraceRecord,
)

# PCS 采用 D50 参考白点（XYZ）
PCS_D50 = np.array([0.9642, 1.0, 0.8249], dtype=np.float64)

_BRADFORD = np.array([
    [0.8951, 0.2664, -0.1614],
    [-0.7502, 1.7135, 0.0367],
    [0.0389, -0.0685, 1.0296],
], dtype=np.float64)
_BRADFORD_INV = np.linalg.inv(_BRADFORD)


def apply_matrix(matrix: tuple[float, ...], rgb: np.ndarray) -> np.ndarray:
    """行主序 3x3 矩阵作用于 (...,3) 数组。"""
    m = np.asarray(matrix, dtype=np.float64).reshape(3, 3)
    return rgb @ m.T


def lut3d_index_coords(n: int) -> np.ndarray:
    """返回 n^3 x 3 的网格坐标，顺序与生成器约定一致 (R 最快)。"""
    r, g, b = np.meshgrid(np.arange(n), np.arange(n), np.arange(n),
                          indexing="ij")
    return np.stack([r.ravel(), g.ravel(), b.ravel()], axis=1)


def apply_lut3d(lut: LUT3D, rgb: np.ndarray) -> np.ndarray:
    """对 (...,3) 的归一化 RGB 做三线性插值。"""
    n = lut.size
    grid = np.asarray(lut.grid, dtype=np.float64).reshape(n, n, n, 3)
    x = np.clip(rgb, 0.0, 1.0) * (n - 1)
    lo = np.floor(x).astype(np.int64)
    hi = np.clip(lo + 1, 0, n - 1)
    frac = (x - lo)[..., None, :]

    def gather(c0, c1, c2):
        return grid[c0[..., 0], c1[..., 1], c2[..., 2]]

    rl, gl, bl = lo[..., 0], lo[..., 1], lo[..., 2]
    rh, gh, bh = hi[..., 0], hi[..., 1], hi[..., 2]
    c000 = grid[rl, gl, bl]
    c100 = grid[rh, gl, bl]
    c010 = grid[rl, gh, bl]
    c110 = grid[rh, gh, bl]
    c001 = grid[rl, gl, bh]
    c101 = grid[rh, gl, bh]
    c011 = grid[rl, gh, bh]
    c111 = grid[rh, gh, bh]
    fr, fg, fb = frac[..., 0], frac[..., 1], frac[..., 2]
    c00 = c000 * (1 - fr) + c100 * fr
    c01 = c001 * (1 - fr) + c101 * fr
    c10 = c010 * (1 - fr) + c110 * fr
    c11 = c011 * (1 - fr) + c111 * fr
    c0 = c00 * (1 - fg) + c10 * fg
    c1 = c01 * (1 - fg) + c11 * fg
    return c0 * (1 - fb) + c1 * fb


def bradford_matrix(src_wp: np.ndarray, dst_wp: np.ndarray) -> np.ndarray:
    """计算 src 白点 -> dst 白点的 Bradford 色度适配矩阵。"""
    src_cone = _BRADFORD @ np.asarray(src_wp, dtype=np.float64)
    dst_cone = _BRADFORD @ np.asarray(dst_wp, dtype=np.float64)
    scale = np.diag(dst_cone / src_cone)
    return _BRADFORD_INV @ scale @ _BRADFORD


def resolve_white_point(profile: ColorProfile) -> tuple[np.ndarray, TraceRecord | None]:
    """白点解析：优先显式字段，其次配置声明默认值，都没有则 E_WP。

    绝不内部猜测。
    """
    if profile.white_point is not None:
        return np.asarray(profile.white_point, dtype=np.float64), None
    if profile.declared_default_white_point is not None:
        wp = np.asarray(profile.declared_default_white_point, dtype=np.float64)
        trace = TraceRecord(
            stage="white_point",
            message="白点字段缺失，使用配置文件声明的默认白点",
            data={"white_point": wp.tolist(),
                  "source": "declared_default_white_point"})
        return wp, trace
    raise CwbError(E_WP,
                   f"配置 {profile.profile_id} 白点缺失且无声明默认值")


def adapt_and_render(pcs: np.ndarray, src_wp: np.ndarray,
                     dst_wp: np.ndarray, intent: str) -> np.ndarray:
    """在 PCS(XYZ) 内按渲染意图做白点适配/色域处理。

    三种意图共用同一解析模型，仅适配策略不同：
      - relative: Bradford 适配到目标白点（媒体白映射到媒体白）。
      - absolute: 不做白点适配，保留介质差异（恒等）。
      - perceptual: Bradford 适配 + 向目标白点做确定性、单调的感知压缩。
    """
    xyz = np.asarray(pcs, dtype=np.float64)
    if intent == INTENT_ABSOLUTE:
        return xyz.copy()
    adapt = bradford_matrix(src_wp, dst_wp)
    adapted = xyz @ adapt.T
    if intent == INTENT_RELATIVE:
        return adapted
    if intent == INTENT_PERCEPTUAL:
        # 感知压缩：逐元素 soft-knee，仅对超过膝点的正向高亮做单调收缩，
        # 保证逐像素不超过相对比色结果、方向可解释且完全确定。
        knee = 0.7
        return np.where(
            adapted <= knee,
            adapted,
            knee + (1.0 - knee) * (1.0 - np.exp(
                -(adapted - knee) / (1.0 - knee))))
    raise CwbError(E_INTENT, f"不支持的渲染意图: {intent}")
