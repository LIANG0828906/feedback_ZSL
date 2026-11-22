"""字符级高亮排程与时长归一化(阶段 3)。

把每句台词的显示窗口切分为逐字符高亮区间:
- 权重为正整数, 归一化使用整型有理运算(分母 W = 权重和);
- 区间首尾相接、总时长恰等于台词窗口、无重叠无空洞;
- 取整残差相对实数精确解 < 1 个时间单位(采样点)。
"""

from __future__ import annotations

import math
from fractions import Fraction

from .errors import para_error
from .windows import allocate


def validate_speed(speed: float) -> None:
    if not isinstance(speed, (int, float)) or isinstance(speed, bool):
        raise para_error("语速必须为数值", field="assist.speed", value=speed)
    if not math.isfinite(speed) or speed <= 0:
        raise para_error("语速必须为正且有限", field="assist.speed", value=speed)


def validate_weights(weights: tuple[int, ...], text_len: int, sub_id: int) -> None:
    if len(weights) != text_len:
        raise para_error(
            "逐字权重个数须与台词字符数一致",
            field="assist.char_weights",
            sub=sub_id,
            weights=len(weights),
            text_len=text_len,
        )
    for w in weights:
        if not isinstance(w, int) or isinstance(w, bool) or w <= 0:
            raise para_error(
                "逐字权重必须为正整数", field="assist.char_weights", sub=sub_id, value=w
            )
    if sum(weights) == 0:
        raise para_error("逐字权重之和为零", field="assist.char_weights", sub=sub_id)


def effective_weights(weights: tuple[int, ...], mode: str) -> tuple[int, ...]:
    """按高亮推进方式取有效权重: uniform 视为等权。"""
    if mode == "uniform":
        return tuple(1 for _ in weights)
    if mode == "weighted":
        return weights
    raise para_error("未知的高亮推进方式", field="assist.highlight_mode", value=mode)


def schedule_chars(window: tuple[int, int], weights: tuple[int, ...]) -> tuple[int, ...]:
    """逐字符高亮起始时刻(采样点)。首尾相接, 总时长 == 窗口长度。"""
    start, end = window
    total = end - start
    denom = sum(weights)
    if denom <= 0:
        raise para_error("逐字权重之和为零", field="assist.char_weights")
    lengths = allocate(total, list(weights))
    starts = []
    cursor = start
    for ln in lengths:
        starts.append(cursor)
        cursor += ln
    return tuple(starts)


def char_residual(
    window: tuple[int, int], weights: tuple[int, ...], idx: int, actual: int
) -> Fraction:
    """第 idx 字符实际高亮起始相对实数精确解的残差(采样点, 有理数)。"""
    start, end = window
    denom = sum(weights)
    cum = sum(weights[:idx])
    exact = Fraction(start) + Fraction((end - start) * cum, denom)
    return Fraction(actual) - exact


def total_read_samples(total_window: int, speed: float) -> int:
    """整工程总朗读时长(采样点): 窗口总长 / 语速, 最近取整(半值向上)。"""
    validate_speed(speed)
    exact = Fraction(total_window, 1) / Fraction(speed)
    return int(exact + Fraction(1, 2))
