"""台词显示窗口推导：段内映射 + 空隙延长 + 待取舍标记。"""
from __future__ import annotations

import math
from dataclasses import dataclass


def min_readable_samples(n_chars: int, max_cps: float, sample_rate: int) -> int:
    """按阅读速度上限计算的最短可读时长（采样点）。"""
    return math.ceil(n_chars / max_cps * sample_rate)


@dataclass
class Window:
    start: int
    end: int
    deficit: int = 0  # 仍欠缺的最短可读时长（采样点），>0 表示待取舍

    @property
    def length(self) -> int:
        return self.end - self.start


# detected_segments 与 project.segments 同序对齐。扩展只使用本句窗口结束
# 之后、下一句窗口开始之前的空隙（最后一句可用尾部静音），因此窗口两两
# 不重叠且顺序稳定；空隙不足时记录 deficit（待取舍），绝不静默截短文本。
def derive_windows(project, detected_segments: list, total_samples: int) -> dict:
    """为每句台词推导显示窗口，返回 {line_id: Window}。"""
    order = project.timeline_order()
    detected_by_seg = {seg.id: detected_segments[i]
                       for i, seg in enumerate(project.segments)}
    windows = {}
    for idx, line in enumerate(order):
        seg_start, seg_end = detected_by_seg[line.segment_id]
        if line.window is not None:
            start, end = line.window
        else:
            start, end = seg_start, seg_end
        need = min_readable_samples(len(line.text), line.max_cps, project.sample_rate)
        short = need - (end - start)
        deficit = 0
        if short > 0:
            if idx + 1 < len(order):
                nxt = order[idx + 1]
                boundary = detected_by_seg[nxt.segment_id][0]
                if nxt.window is not None:
                    boundary = nxt.window[0]
            else:
                boundary = total_samples
            gap = max(0, boundary - end)
            take = min(short, gap)
            end += take
            deficit = short - take
        windows[line.id] = Window(start=start, end=end, deficit=deficit)
    return windows
