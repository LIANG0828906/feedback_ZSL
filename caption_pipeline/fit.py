"""排版耗时与可读时长的双向约束校验与受控取舍。"""
from __future__ import annotations

import math
from dataclasses import dataclass

from .errors import E_FIT, PipelineError
from .layout import flatten, line_char_counts, split_screens

# 取舍策略常量（算法版本的一部分，改动需升 ALGO_VERSION）。
MIN_MARGIN = 4          # 压缩留白的下限
SOFT_MAX_SCREENS = 2    # 软上限：超过则记录"增加屏幕切分"取舍
HARD_MAX_SCREENS = 4    # 硬上限：超过则判定无法收敛
LINE_OVERHEAD_S = 0.15  # 每行注视/扫视开销（秒）
SCREEN_SWITCH_S = 0.30  # 每增加一屏的切换开销（秒）


@dataclass
class Tradeoff:
    line_id: str
    means: str    # compress_margin | increase_splits | reclaim_gap
    reason: str

    def to_dict(self):
        return {"line_id": self.line_id, "means": self.means,
                "reason": self.reason}


def required_samples(screens: list, max_cps: float, sample_rate: int) -> int:
    """由折行后的行数与每行字符数反推该句最短可读时长（采样点）。"""
    total = 0
    for count in line_char_counts(screens):
        total += math.ceil(count / max_cps * sample_rate)
        total += int(LINE_OVERHEAD_S * sample_rate)
    if screens:
        total += (len(screens) - 1) * int(SCREEN_SWITCH_S * sample_rate)
    return total


# 取舍按可解释优先级依次尝试：压缩留白 -> 增加屏幕切分 -> 回收空隙，
# 每次取舍记录台词编号、手段与理由；全部手段用尽仍冲突时抛 E_FIT，
# details 中给出台词编号与实测差额（采样点）。
def reconcile(line, window, screens, presentation, sample_rate, gap_available):
    """校验并在需要时执行受控取舍，返回 (window, screens, tradeoffs)。"""
    pres = presentation
    tradeoffs = []
    window_len = window.end - window.start
    need = required_samples(screens, line.max_cps, sample_rate)

    # 手段一：压缩留白，提升单屏行数上限，减少切屏开销。
    if (need > window_len or len(screens) > SOFT_MAX_SCREENS) \
            and pres.margin > MIN_MARGIN:
        compressed = split_screens(flatten(screens), pres.line_height,
                                   MIN_MARGIN, pres.screen_height)
        if len(compressed) < len(screens):
            tradeoffs.append(Tradeoff(
                line.id, "compress_margin",
                f"留白 {pres.margin}->{MIN_MARGIN}，单屏行数提升，"
                f"屏幕数 {len(screens)}->{len(compressed)}，降低切屏开销"))
            screens = compressed
            need = required_samples(screens, line.max_cps, sample_rate)

    # 手段二：增加屏幕切分（接受超出软上限的切分，硬上限内放行）。
    if len(screens) > SOFT_MAX_SCREENS and len(screens) <= HARD_MAX_SCREENS:
        tradeoffs.append(Tradeoff(
            line.id, "increase_splits",
            f"屏幕数 {len(screens)} 超出软上限 {SOFT_MAX_SCREENS}，"
            f"在硬上限 {HARD_MAX_SCREENS} 内接受更多切分以保证可行"))

    # 手段三：回收空隙，延长显示窗口覆盖残余差额。
    deficit = need - (window.end - window.start)
    if deficit > 0 and gap_available > 0:
        take = min(deficit, gap_available)
        window.end += take
        deficit -= take
        tradeoffs.append(Tradeoff(
            line.id, "reclaim_gap",
            f"回收后续空隙 {take} 采样点以覆盖最短可读时长差额"))

    if deficit > 0 or len(screens) > HARD_MAX_SCREENS:
        raise PipelineError(
            E_FIT, "排版与可读时长冲突无法收敛",
            details={"lines": [{"line_id": line.id,
                                "deficit_samples": max(0, deficit),
                                "screens": len(screens)}]})
    return window, screens, tradeoffs


def merge_fit_errors(errors: list) -> PipelineError:
    """把多句台词的 E_FIT 明细合并为一个异常。"""
    lines = []
    for err in errors:
        lines.extend(err.details.get("lines", []))
    return PipelineError(E_FIT, "排版与可读时长冲突无法收敛",
                         details={"lines": lines})
