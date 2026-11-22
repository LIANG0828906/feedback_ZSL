"""核心计算流水线: 包络检测 -> 窗口推导 -> 字符排程 -> 误差报告。

该流水线为纯函数, 生成器(产出真值)与引擎(实时/恢复/全量三条路径)共用同一实现,
从根本上保证三条路径结果一致。
"""

from __future__ import annotations

import numpy as np

from . import errprop, schedule
from .envelope import detect_segments
from .model import Project
from .windows import derive_windows


def compute_detection(project: Project, envelope: np.ndarray) -> list:
    """阶段 1: 检测语音段边界(采样点, hop 网格量化)。"""
    return detect_segments(envelope, project.frame_len, project.hop)


def compute_windows(project: Project, detected: list) -> list:
    """阶段 2: 推导逐句显示窗口。"""
    return derive_windows(project, detected)


def compute_sentence_schedule(project: Project, sub_id: int, window: tuple) -> tuple:
    """阶段 3: 单句逐字符高亮排程, 返回各字符高亮起始时刻。"""
    sub = project.subtitles[sub_id]
    weights = schedule.effective_weights(
        project.assist.char_weights[sub_id], project.assist.highlight_mode
    )
    schedule.validate_weights(weights, len(sub.text), sub_id)
    return schedule.schedule_chars(window, weights)


def sentence_error_entry(
    project: Project, sub_id: int, detected: list, window: tuple, char_starts: tuple
) -> dict:
    """阶段 4: 单句误差统计(不做超界判定)。"""
    sub = project.subtitles[sub_id]
    weights = schedule.effective_weights(
        project.assist.char_weights[sub_id], project.assist.highlight_mode
    )
    return errprop.sentence_error_entry(
        project, sub_id, detected[sub.segment], window, weights, char_starts
    )


def finalize_report(project: Project, sentence_entries: list, windows: list) -> dict:
    """汇总整工程误差报告, 并做全部上界判定(可能抛 E_ERR)。"""
    bounds = errprop.cumulative_bounds(project)
    for entry in sentence_entries:
        errprop.check_sentence_bounds(entry, bounds)
    total_window = sum(w1 - w0 for w0, w1 in windows)
    total_actual = schedule.total_read_samples(total_window, project.assist.speed)
    errprop.check_total_bound(total_actual, project.truth.total_read_samples, bounds)
    return {
        "project_id": project.project_id,
        "time_unit": "samples",
        "bounds": bounds,
        "sentences": sentence_entries,
        "total_read": {
            "actual": total_actual,
            "truth": project.truth.total_read_samples,
            "deviation": abs(total_actual - project.truth.total_read_samples),
            "bound": bounds["total"],
        },
    }
