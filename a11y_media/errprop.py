"""误差传播报告(阶段 4)。

沿 "包络检测 -> 窗口推导 -> 字符排程 -> 朗读输出" 链路逐句记录:
- envelope  : 边界量化误差, 上界 B1 = hop // 2 (仅由帧移推导)
- window    : 窗口推导累积偏差, 新增量化 0, 累积上界 C2 = B1
- schedule  : 字符排程取整残差 < 1 个时间单位, 累积上界 C3 = C2 + 1
- total     : 整工程总朗读时长偏差, 上界 C4 = 2 * 句数 * C2 + 1
上界只允许由帧长、帧移、时间单位量化步长与权重分母推导, 不得从真值反推。
实测超出上界时抛出 E_ERR 并指出句号与误差项名称。
"""

from __future__ import annotations

from fractions import Fraction

from .envelope import boundary_quant_bound
from .errors import err_error
from .model import Project
from .schedule import char_residual

UNIT_QUANT_STEP = 1  # 统一时间单位(采样点)的量化步长


def cumulative_bounds(project: Project) -> dict:
    """各级累积上界(采样点), 仅由帧移与时间单位量化步长推导。"""
    b1 = boundary_quant_bound(project.hop)
    c2 = b1                      # 窗口推导不引入新的量化(同一整型时间单位)
    c3 = c2 + UNIT_QUANT_STEP    # 字符排程取整残差 < 1 步长
    c4 = 2 * len(project.subtitles) * c2 + UNIT_QUANT_STEP
    return {"envelope": b1, "window": c2, "schedule": c3, "total": c4}


def sentence_error_entry(
    project: Project,
    sub_id: int,
    detected: tuple,
    window: tuple,
    weights: tuple,
    char_starts: tuple,
) -> dict:
    """单句误差统计: 包络边界量化误差 / 窗口累积偏差 / 字符取整残差。"""
    seg = project.segments[project.subtitles[sub_id].segment]
    env_start = abs(detected[0] - seg.start)
    env_end = abs(detected[1] - seg.end)
    if project.subtitles[sub_id].window is None:
        window_dev = max(env_start, env_end)
    else:
        window_dev = 0  # 显式窗口直接采用清单值, 不继承检测量化误差
    res_max = Fraction(0)
    for i, actual in enumerate(char_starts):
        r = char_residual(window, weights, i, actual)
        if r > res_max:
            res_max = r
    return {
        "sub": sub_id,
        "segment": seg.seg_id,
        "envelope_err_start": env_start,
        "envelope_err_end": env_end,
        "window_dev": window_dev,
        "char_res_max": [res_max.numerator, res_max.denominator],
    }


def check_sentence_bounds(entry: dict, bounds: dict) -> None:
    """实测超界 -> E_ERR, 指出句号与误差项名称。"""
    sub = entry["sub"]
    if entry["envelope_err_start"] > bounds["envelope"]:
        raise err_error(
            "包络起始边界量化误差超界", sub=sub, item="envelope_err_start",
            measured=entry["envelope_err_start"], bound=bounds["envelope"],
        )
    if entry["envelope_err_end"] > bounds["envelope"]:
        raise err_error(
            "包络结束边界量化误差超界", sub=sub, item="envelope_err_end",
            measured=entry["envelope_err_end"], bound=bounds["envelope"],
        )
    if entry["window_dev"] > bounds["window"]:
        raise err_error(
            "窗口推导累积偏差超界", sub=sub, item="window_dev",
            measured=entry["window_dev"], bound=bounds["window"],
        )
    res = Fraction(*entry["char_res_max"])
    if res > bounds["schedule"]:
        raise err_error(
            "字符排程取整残差超界", sub=sub, item="char_res_max",
            measured=float(res), bound=bounds["schedule"],
        )


def check_total_bound(actual: int, truth: int, bounds: dict) -> None:
    dev = abs(actual - truth)
    if dev > bounds["total"]:
        raise err_error(
            "整工程总朗读时长偏差超界", sub=-1, item="total_read",
            measured=dev, bound=bounds["total"],
        )
