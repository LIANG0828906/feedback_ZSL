"""TRC 反演（编码 -> 线性）与编码（线性 -> 编码）。

同时支持参数式（gamma / sRGB 分段）与表格式（等间距采样 + 线性插值）。
所有函数对 0..1 的浮点数组运算，保持确定性（无随机分支、float64 计算）。
"""

from __future__ import annotations

import numpy as np

from .model import TRC


def _srgb_to_linear(x: np.ndarray) -> np.ndarray:
    x = np.asarray(x, dtype=np.float64)
    return np.where(x <= 0.04045,
                    x / 12.92,
                    np.power(np.clip((x + 0.055) / 1.055, 0.0, None), 2.4))


def _linear_to_srgb(x: np.ndarray) -> np.ndarray:
    x = np.clip(np.asarray(x, dtype=np.float64), 0.0, None)
    return np.where(x <= 0.0031308,
                    x * 12.92,
                    1.055 * np.power(x, 1.0 / 2.4) - 0.055)


def linearize(trc: TRC, encoded: np.ndarray) -> np.ndarray:
    """编码值 -> 线性光。"""
    x = np.clip(np.asarray(encoded, dtype=np.float64), 0.0, 1.0)
    if trc.kind == "parametric":
        if trc.curve == "srgb":
            return _srgb_to_linear(x)
        return np.power(x, trc.gamma)
    # 表格 TRC：编码轴等间距，线性插值得到线性值
    table = np.asarray(trc.table, dtype=np.float64)
    pos = x * (len(table) - 1)
    lo = np.floor(pos).astype(np.int64)
    hi = np.clip(lo + 1, 0, len(table) - 1)
    frac = pos - lo
    return table[lo] * (1.0 - frac) + table[hi] * frac


def encode(trc: TRC, linear: np.ndarray) -> np.ndarray:
    """线性光 -> 编码值（输出限制在 0..1）。"""
    x = np.clip(np.asarray(linear, dtype=np.float64), 0.0, 1.0)
    if trc.kind == "parametric":
        if trc.curve == "srgb":
            return np.clip(_linear_to_srgb(x), 0.0, 1.0)
        return np.power(x, 1.0 / trc.gamma)
    # 表格 TRC 反查：在线性轴上反插值求编码位置
    table = np.asarray(trc.table, dtype=np.float64)
    idx = np.clip(np.searchsorted(table, x, side="left") - 1, 0,
                  len(table) - 2)
    y0 = table[idx]
    y1 = table[idx + 1]
    span = np.where(y1 > y0, y1 - y0, 1.0)
    frac = np.clip((x - y0) / span, 0.0, 1.0)
    enc = (idx + frac) / (len(table) - 1)
    # 末段平台处理
    enc = np.where(y1 <= y0, idx / (len(table) - 1), enc)
    return np.clip(enc, 0.0, 1.0)


def trc_inversion_difference(parametric: TRC, table: TRC,
                             samples: int = 4096) -> dict:
    """对同一编码网格，记录参数式与表格式反演的可测数值差异。"""
    grid = np.linspace(0.0, 1.0, samples, dtype=np.float64)
    a = linearize(parametric, grid)
    b = linearize(table, grid)
    diff = np.abs(a - b)
    return {
        "samples": samples,
        "max_abs_diff": float(diff.max()),
        "mean_abs_diff": float(diff.mean()),
        "argmax_encoded": float(grid[int(diff.argmax())]),
        "parametric_kind": "parametric",
        "table_kind": "table",
    }
