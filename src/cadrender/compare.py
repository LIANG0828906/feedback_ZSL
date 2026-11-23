"""预览帧与精确帧的自动一致性比对（业务要求 4）。"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Dict, Optional, Tuple

import numpy as np

from .errors import E_DIVERGE, DivergeError
from .renderer import RenderedFrame


@dataclass
class DivergenceReport:
    coverage_delta: float
    bbox_delta_px: float
    shade_level_delta: int
    first_mismatch: Optional[Tuple[int, int]]
    mismatch_tile: Optional[Tuple[int, int]]
    passed: bool
    measured: Dict[str, Any]

    def to_dict(self) -> Dict[str, Any]:
        return {
            "passed": self.passed,
            "coverage_delta": self.coverage_delta,
            "bbox_delta_px": self.bbox_delta_px,
            "shade_level_delta": self.shade_level_delta,
            "first_mismatch": (None if self.first_mismatch is None
                               else list(self.first_mismatch)),
            "mismatch_tile": (None if self.mismatch_tile is None
                              else list(self.mismatch_tile)),
            "measured": self.measured,
        }


def _bbox_delta(a: Optional[Tuple[int, int, int, int]],
                b: Optional[Tuple[int, int, int, int]]) -> float:
    if a is None and b is None:
        return 0.0
    if a is None or b is None:
        return float("inf")
    return float(max(abs(x - y) for x, y in zip(a, b)))


def compare_frames(preview: RenderedFrame, precise: RenderedFrame,
                   tol: Dict[str, float],
                   pixel_tol: float = 0.06) -> DivergenceReport:
    # 不得比对缺失数据
    if preview is None or precise is None:
        raise DivergeError("存在缺失帧，拒绝比对（不得基于缺失数据）",
                           module="compare",
                           context={"preview_none": preview is None,
                                    "precise_none": precise is None})
    if preview.color is None or precise.color is None:
        raise DivergeError("像素缓冲缺失，拒绝比对", module="compare")
    if preview.camera_sig != precise.camera_sig:
        raise DivergeError("两帧相机参数快照不一致，无法比对",
                           module="compare",
                           context={"preview_cam": preview.camera_sig,
                                    "precise_cam": precise.camera_sig})
    if preview.color.shape != precise.color.shape:
        raise DivergeError("两帧分辨率不一致，无法比对", module="compare",
                           context={"shapes": [preview.color.shape,
                                               precise.color.shape]})

    coverage_delta = abs(precise.coverage - preview.coverage)
    bbox_delta = _bbox_delta(preview.bbox_projection,
                             precise.bbox_projection)
    shade_delta = abs(precise.shade_levels - preview.shade_levels)

    # 轮廓内逐像素找首个不一致（按行主序扫描，确定性）
    mask = precise.silhouette_mask() | preview.silhouette_mask()
    diff = np.abs(precise.color.astype(np.float64)
                  - preview.color.astype(np.float64)).max(axis=2)
    mismatch = mask & (diff > pixel_tol)
    first: Optional[Tuple[int, int]] = None
    tile: Optional[Tuple[int, int]] = None
    if mismatch.any():
        yy, xx = np.where(mismatch)
        order = np.lexsort((xx, yy))  # 行优先
        fy, fx = int(yy[order[0]]), int(xx[order[0]])
        first = (fx, fy)
        tile = (fx // precise.tile_size, fy // precise.tile_size)

    measured = {
        "preview_coverage": preview.coverage,
        "precise_coverage": precise.coverage,
        "preview_bbox": preview.bbox_projection,
        "precise_bbox": precise.bbox_projection,
        "preview_shade_levels": preview.shade_levels,
        "precise_shade_levels": precise.shade_levels,
        "mismatch_pixel_count": int(mismatch.sum()),
    }
    passed = (coverage_delta <= tol["coverage"]
              and bbox_delta <= tol["bbox_projection_px"]
              and shade_delta <= int(tol["shade_levels"]))
    report = DivergenceReport(coverage_delta, bbox_delta, shade_delta,
                              first, tile, passed, measured)
    if not passed:
        raise DivergeError(
            "预览与精确帧一致性比对失败 E_DIVERGE",
            module="compare",
            context=report.to_dict(),
        )
    return report


def diff_heatmap(preview: RenderedFrame,
                 precise: RenderedFrame) -> np.ndarray:
    """返回归一化差异热力图 (H,W) float32。"""
    diff = np.abs(precise.color.astype(np.float64)
                  - preview.color.astype(np.float64)).mean(axis=2)
    return diff.astype(np.float32)
