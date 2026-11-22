"""阈值化与形态学清理。"""
from __future__ import annotations

from typing import Dict

import numpy as np

from segworkbench.pipeline.morphology import closing, component_masks, opening
from segworkbench.pipeline.parameters import SegmentationParameters


def clean_mask(raw_mask: np.ndarray, params: SegmentationParameters) -> Dict[str, np.ndarray]:
    """先闭后开，再按最小像素数保留连通域。

    输入已是颜色距离阈值化的结果，这里再用距离阈值做一次显式二值化
    以固化“阈值化”阶段语义；结构元素半径为 0 时形态学为恒等操作。
    """
    binary = raw_mask.astype(bool).copy()
    closed = closing(binary, params.struct_radius)
    opened = opening(closed, params.struct_radius)
    components = component_masks(opened)
    kept = [component for component in components if int(component.sum()) >= params.min_region_pixels]
    final_mask = np.zeros_like(raw_mask, dtype=bool)
    for component in kept:
        final_mask |= component
    return {"final_mask": final_mask, "kept_component_count": np.array([len(kept)], dtype=np.int32)}
