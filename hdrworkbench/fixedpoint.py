"""定点 Q 格式引擎。

线性化值与矩阵中间结果统一量化到 QF 有符号定点数（F 位小数），
步长 ``q = 2**-F``。每次量化都记录逐元素舍入方向：

- -1：向下舍入（floor）
-  0：恰好落在量化网格上
- +1：向上舍入（ceil）

提供两种确定性模式：

- ``nearest``  就近舍入，偶数小数用偶数舍入（banker's rounding），无随机源
- ``stochastic`` 由种子驱动的确定性随机舍入，暗部抖动可复现
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .errors import ModeInvalidError

VALID_ROUNDING = ("nearest", "stochastic")
MIN_FRAC_BITS = 1
MAX_FRAC_BITS = 48


@dataclass(frozen=True)
class Quantizer:
    frac_bits: int
    rounding: str = "nearest"
    seed: int = 0

    def __post_init__(self):
        if not isinstance(self.frac_bits, (int, np.integer)) or not (
            MIN_FRAC_BITS <= int(self.frac_bits) <= MAX_FRAC_BITS
        ):
            raise ModeInvalidError(
                f"小数位必须位于 [{MIN_FRAC_BITS}, {MAX_FRAC_BITS}]",
                mode="fixed",
                reason="bad_frac_bits",
            )
        if self.rounding not in VALID_ROUNDING:
            raise ModeInvalidError(
                f"未知舍入模式 {self.rounding!r}",
                mode="fixed",
                reason="bad_rounding",
            )

    @property
    def step(self) -> float:
        return 2.0 ** (-self.frac_bits)

    def quantize(self, values: np.ndarray, salt: int = 0) -> np.ndarray:
        """返回量化后的 float64 数组（数值仍以浮点承载 QF 格点）。"""
        scaled = np.asarray(values, dtype=np.float64) * (1 << self.frac_bits)
        if self.rounding == "nearest":
            integers = np.rint(scaled)
        else:
            rng = np.random.default_rng(
                np.random.SeedSequence([self.seed, salt])
            )
            floors = np.floor(scaled)
            draws = rng.random(size=scaled.shape, dtype=np.float64)
            integers = floors + (draws < (scaled - floors))
        return integers / (1 << self.frac_bits)

    def rounding_direction(
        self, before: np.ndarray, after: np.ndarray
    ) -> np.ndarray:
        return np.sign(after - before).astype(np.int8)


def quantize_matrix(matrix: np.ndarray, quantizer: Quantizer) -> np.ndarray:
    """矩阵系数按同一 QF 网格量化。"""
    return quantizer.quantize(np.asarray(matrix, dtype=np.float64), salt=1)


def fixed_matmul(
    vector: np.ndarray, matrix: np.ndarray, quantizer: Quantizer, salt: int
) -> np.ndarray:
    """定点矩阵乘：操作数已在 QF 网格，乘积后重新量化回 QF。"""
    product = np.matmul(vector, matrix)
    return quantizer.quantize(product, salt=salt)

