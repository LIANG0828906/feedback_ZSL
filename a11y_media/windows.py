"""台词窗口推导(阶段 2)。

把每句台词映射为所属语音段内的显示窗口:
- 单句段: 窗口 = 检测到的段边界;
- 多句段: 按句长(字符数)以整型有理运算比例切分, 余数按最大余数法确定性分配;
- 显式窗口: 原样采用(加载期已校验不越出声明段)。
推导结果保证: 不越出所属语音段、同段内窗口首尾相接互不重叠。
"""

from __future__ import annotations

from .errors import schema_error
from .model import Project


def allocate(total: int, shares: list[int]) -> list[int]:
    """整型有理分配: 按 shares 比例切分 total, 最大余数法分配余数(同余数取下标小者)。

    保证 sum(result) == total 且每项 >= 0。字符排程的时长归一化复用同一规则。
    """
    n = len(shares)
    if n == 0:
        return []
    denom = sum(shares)
    if denom <= 0:
        raise schema_error("分配份额之和必须为正", shares=shares)
    base = [(total * s) // denom for s in shares]
    rem = total - sum(base)
    # 最大余数法: 按 (fractional_part, -index) 降序, 确定性地分发剩余单位
    order = sorted(range(n), key=lambda i: (-(total * shares[i] - base[i] * denom), i))
    for i in order[:rem]:
        base[i] += 1
    return base


def derive_windows(
    project: Project, detected: list[tuple[int, int]]
) -> list[tuple[int, int]]:
    """逐句推导显示窗口 [start, end), 返回按句号索引的列表。"""
    if len(detected) != len(project.segments):
        raise schema_error(
            "检测到的语音段数与清单声明不一致",
            detected=len(detected),
            declared=len(project.segments),
        )
    by_segment: dict[int, list[int]] = {}
    for sub in project.subtitles:
        by_segment.setdefault(sub.segment, []).append(sub.sub_id)

    windows: list[tuple[int, int] | None] = [None] * len(project.subtitles)
    for seg_id, sub_ids in by_segment.items():
        det_start, det_end = detected[seg_id]
        explicit = [i for i in sub_ids if project.subtitles[i].window is not None]
        if explicit and len(explicit) != len(sub_ids):
            raise schema_error(
                "同一语音段内显式窗口与隐式窗口不得混用",
                segment=seg_id,
                subs=sub_ids,
            )
        if explicit:
            cursor = det_start
            for i in sub_ids:
                w0, w1 = project.subtitles[i].window  # type: ignore[misc]
                if w0 < det_start or w1 > det_end:
                    raise schema_error(
                        "显式窗口越出检测段边界", sub=i, window=(w0, w1), segment=seg_id
                    )
                if w0 < cursor:
                    raise schema_error("显式窗口彼此重叠", sub=i, segment=seg_id)
                cursor = w1
                windows[i] = (w0, w1)
        else:
            shares = [max(1, len(project.subtitles[i].text)) for i in sub_ids]
            lengths = allocate(det_end - det_start, shares)
            cursor = det_start
            for i, ln in zip(sub_ids, lengths):
                windows[i] = (cursor, cursor + ln)
                cursor += ln
    return [w for w in windows if w is not None]  # type: ignore[misc]
