"""语音包络检测：逐帧能量 + 阈值 + 近邻合并。"""
from __future__ import annotations

import numpy as np

from .errors import E_PARA, PipelineError


def _check_frame_params(n_samples, frame_len, hop):
    if frame_len <= 0:
        raise PipelineError(E_PARA, "帧长必须为正整数", field="frame_len")
    if hop <= 0 or hop > frame_len:
        raise PipelineError(E_PARA, "帧移必须满足 0 < hop <= frame_len", field="hop")
    if n_samples < frame_len:
        raise PipelineError(E_PARA, "帧长超过音频长度", field="frame_len")


def frame_energies(samples: np.ndarray, frame_len: int, hop: int) -> np.ndarray:
    """按帧长与帧移计算逐帧能量。"""
    _check_frame_params(len(samples), frame_len, hop)
    n_frames = 1 + (len(samples) - frame_len) // hop
    starts = np.arange(n_frames) * hop
    idx = starts[:, None] + np.arange(frame_len)[None, :]
    frames = samples[idx]
    return np.einsum("ij,ij->i", frames, frames)


def detect_segments(samples: np.ndarray, frame_len: int, hop: int,
                    thresh_ratio: float = 0.15, merge_gap_frames: int = 6,
                    min_len_frames: int = 2) -> list:
    """检测语音段，返回 [(start_sample, end_sample), ...]，按时间升序且确定。"""
    if not 0 < thresh_ratio < 1:
        raise PipelineError(E_PARA, "能量阈值比例须在 (0, 1) 内", field="thresh_ratio")
    energies = frame_energies(samples, frame_len, hop)
    peak = float(energies.max()) if energies.size else 0.0
    if peak <= 0:
        return []
    active = energies > peak * thresh_ratio

    runs = []
    i = 0
    n = len(active)
    while i < n:
        if active[i]:
            j = i
            while j + 1 < n and active[j + 1]:
                j += 1
            runs.append([i, j])
            i = j + 1
        else:
            i += 1
    merged = []
    for run in runs:
        if merged and run[0] - merged[-1][1] - 1 <= merge_gap_frames:
            merged[-1][1] = run[1]
        else:
            merged.append(run)
    segments = []
    for f0, f1 in merged:
        if f1 - f0 + 1 < min_len_frames:
            continue
        start = f0 * hop
        end = min(len(samples), f1 * hop + frame_len)
        segments.append((int(start), int(end)))
    return segments
