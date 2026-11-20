"""法线、材质混合与接缝闭合算法。

所有函数均为纯函数：相同输入字节产生完全相同的输出字节，因此局部重算
与整段全量重算可以逐位一致（增量/全量共享同一份实现）。
"""

from __future__ import annotations

import numpy as np

from .errors import ErrorCode, TerrainError
from .quadtree import QuadNode


def compute_normals(elevations: np.ndarray, cell_size: float) -> np.ndarray:
    """中心差分法线；边界使用单侧差分。

    返回 float32 单位法线数组，shape = (H, W, 3)。
    """
    h = elevations.astype(np.float64, copy=False)
    dy = np.zeros_like(h)
    dx = np.zeros_like(h)
    # 中心差分（内部）
    dy[1:-1, :] = (h[2:, :] - h[:-2, :]) * 0.5
    dx[:, 1:-1] = (h[:, 2:] - h[:, :-2]) * 0.5
    # 边界单侧差分
    dy[0, :] = h[1, :] - h[0, :]
    dy[-1, :] = h[-1, :] - h[-2, :]
    dx[:, 0] = h[:, 1] - h[:, 0]
    dx[:, -1] = h[:, -1] - h[:, -2]

    dx /= cell_size
    dy /= cell_size
    # 法线 = (-dz/dx, -dz/dy, 1)
    nx = -dx
    ny = -dy
    nz = np.ones_like(h)
    norm = np.sqrt(nx * nx + ny * ny + nz * nz)
    normals = np.stack((nx / norm, ny / norm, nz / norm), axis=-1)
    return normals.astype(np.float32)


def compute_material_blend(indices: np.ndarray, palette_size: int) -> np.ndarray:
    """按材质索引做双线性混合权重。

    对每个像素取其 3x3 邻域（边界 clamp），按双线性权重累计各材质的权重，
    输出每个像素对 ``palette_size`` 种材质的归一化权重（float32）。
    增量与全量调用同一函数，结果逐位一致。
    """
    idx = indices.astype(np.int64, copy=False)
    height, width = idx.shape
    padded = np.pad(idx, 1, mode="edge")
    # 3x3 双线性权重（角 0.25，边 0.5，中心 1.0，归一化系数 4）
    kernel = np.array(
        [[0.25, 0.5, 0.25], [0.5, 1.0, 0.5], [0.25, 0.5, 0.25]], dtype=np.float64
    )
    weights = np.zeros((height, width, palette_size), dtype=np.float64)
    for ky in range(3):
        for kx in range(3):
            w = kernel[ky, kx]
            neigh = padded[ky : ky + height, kx : kx + width]
            # np.add.at 等价于按索引累加（邻域内可能有重复索引）
            np.add.at(weights, (np.arange(height)[:, None], np.arange(width)[None, :], neigh), w)
    weights /= 4.0
    return weights.astype(np.float32)


def normal_angle_degrees(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    """两个法线数组逐像素夹角（度），自动夹紧到合法余弦区间。"""
    # 先在 float32 做点积（与法线存储精度一致），再提升到 float64 求反余弦，
    # 保证两条计算路径产出的相同法线其自夹角严格为 0
    cosine = np.sum(a.astype(np.float32) * b.astype(np.float32), axis=-1).astype(np.float64)
    cosine = np.clip(cosine, -1.0, 1.0)
    angles = np.degrees(np.arccos(cosine))
    # 余弦在 float32 精度内等于 1 时，夹角置 0，消除 arccos 舍入放大
    angles[np.isclose(cosine, 1.0, atol=5e-7, rtol=0.0)] = 0.0
    return angles


def close_seams(
    root: QuadNode,
    elevations: np.ndarray,
    chunk_pixel: int,
    cell_size: float,
    vertical_tolerance: float,
) -> tuple[dict[tuple[int, int], float], list[dict]]:
    """对层级不一致的四叉树边界做匹配顶点闭合。

    规则：粗粒度叶子的边界高程视为该世界坐标处的参考高度，细粒度一侧的
    边界顶点向其线性插值结果对齐（匹配顶点 + 裙边修正量）。返回每个细粒度
    chunk 的闭合修正量与检测报告；若闭合后两侧同一世界坐标的垂直偏差仍超过
    容差，抛 E_CRACK_UNRESOLVED 并定位接缝世界坐标。
    """
    corrections: dict[tuple[int, int], float] = {}
    report: list[dict] = []
    leaves = root.leaves()

    for leaf in leaves:
        for side, neighbor_leaves in (
            ("right", leaf.edge_neighbors(root, "right")),
            ("bottom", leaf.edge_neighbors(root, "bottom")),
        ):
            for neighbor in neighbor_leaves:
                if neighbor.depth == leaf.depth:
                    continue  # 同级，顶点天然匹配
                coarse, fine = (leaf, neighbor) if leaf.depth < neighbor.depth else (neighbor, leaf)
                gap = _boundary_gap(coarse, fine, side, elevations, chunk_pixel, cell_size)
                report.append(
                    {
                        "coarse": (coarse.cx0, coarse.cy0, coarse.cx1, coarse.cy1),
                        "fine": (fine.cx0, fine.cy0, fine.cx1, fine.cy1),
                        "side": side,
                        "max_gap": float(gap["max_gap"]),
                        "world_xy": gap["world_xy"],
                        "closed": gap["max_gap"] <= vertical_tolerance,
                    }
                )
                if gap["max_gap"] > vertical_tolerance:
                    raise TerrainError(
                        ErrorCode.E_CRACK_UNRESOLVED,
                        f"接缝垂直偏差 {gap['max_gap']:.6g} 超过容差 {vertical_tolerance:.6g}",
                        {
                            "world_xy": gap["world_xy"],
                            "max_gap": float(gap["max_gap"]),
                            "tolerance": float(vertical_tolerance),
                        },
                    )
                for cy in range(fine.cy0, fine.cy1):
                    for cx in range(fine.cx0, fine.cx1):
                        key = (cx, cy)
                        corrections[key] = max(
                            corrections.get(key, 0.0), float(gap["max_gap"])
                        )
    return corrections, report


def _boundary_gap(
    coarse: QuadNode,
    fine: QuadNode,
    side: str,
    elevations: np.ndarray,
    chunk_pixel: int,
    cell_size: float,
) -> dict:
    """计算共享边界上细粒度顶点相对粗边线性插值的最大垂直偏差。

    chunk 网格到像素的映射为 ``pixel = chunk * chunk_pixel``（边缘 chunk 由
    栅格尺寸截断）。垂直边共享一列像素、水平边共享一行像素；粗边在两个粗
    端点之间按世界距离线性插值，与细边每个顶点的真实高程比较。
    """
    height, width = elevations.shape

    def px_x(cx: int) -> int:
        return min(cx * chunk_pixel, width)

    def px_y(cy: int) -> int:
        return min(cy * chunk_pixel, height)

    max_gap = 0.0
    worst_world = (0.0, 0.0)

    if side == "right":
        # 共享边界列；若位于栅格右缘则 clamp 到最后一个有效像素
        edge_x = min(px_x(coarse.cx1), width - 1)
        coarse_y0, coarse_y1 = px_y(coarse.cy0), px_y(coarse.cy1)
        fine_y0, fine_y1 = px_y(fine.cy0), px_y(fine.cy1)
        h0 = float(elevations[coarse_y0, edge_x])
        h1 = float(elevations[min(coarse_y1, height) - 1, edge_x])
        span = max(1, coarse_y1 - 1 - coarse_y0)
        for py in range(fine_y0, fine_y1):
            t = (py - coarse_y0) / span
            reference = h0 + (h1 - h0) * t
            actual = float(elevations[py, edge_x])
            gap = abs(actual - reference)
            if gap > max_gap:
                max_gap = gap
                worst_world = (edge_x * cell_size, py * cell_size)
    else:  # bottom，水平边界
        edge_y = min(px_y(coarse.cy1), height - 1)
        coarse_x0, coarse_x1 = px_x(coarse.cx0), px_x(coarse.cx1)
        fine_x0, fine_x1 = px_x(fine.cx0), px_x(fine.cx1)
        h0 = float(elevations[edge_y, coarse_x0])
        h1 = float(elevations[edge_y, min(coarse_x1, width) - 1])
        span = max(1, coarse_x1 - 1 - coarse_x0)
        for px in range(fine_x0, fine_x1):
            t = (px - coarse_x0) / span
            reference = h0 + (h1 - h0) * t
            actual = float(elevations[edge_y, px])
            gap = abs(actual - reference)
            if gap > max_gap:
                max_gap = gap
                worst_world = (px * cell_size, edge_y * cell_size)

    return {"max_gap": float(max_gap), "world_xy": worst_world}
