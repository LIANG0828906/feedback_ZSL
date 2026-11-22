"""语音包络检测(阶段 1)。

链路: 逐帧能量 -> 活跃帧游程 -> 帧内亚帧边界估计 -> 帧网格量化(吸附到 hop 网格)。

边界量化规则: 估计边界吸附到最近的 hop 整数倍, 量化误差 <= hop // 2(采样点),
且当帧移按整数倍收紧(hop' 整除 hop)时, 量化残差单调不增 ——
因为 hop 网格是 hop' 网格的子集, 最近网格点距离不会变大。
所有边界以统一整型时间单位(采样点)表达。
"""

from __future__ import annotations

import numpy as np

from .errors import para_error


def frame_energies(envelope: np.ndarray, frame_len: int, hop: int) -> np.ndarray:
    """逐帧能量(均方)。只取完整帧, 帧 k 覆盖 [k*hop, k*hop+frame_len)。"""
    if frame_len <= 0:
        raise para_error("frame_len 必须为正整数", field="frame_len", value=frame_len)
    if hop <= 0:
        raise para_error("hop 必须为正整数", field="hop", value=hop)
    if hop > frame_len:
        raise para_error("hop 不得大于 frame_len", field="hop", value=hop)
    n = len(envelope)
    if n < frame_len:
        return np.zeros(0, dtype=np.float64)
    n_frames = (n - frame_len) // hop + 1
    starts = np.arange(n_frames, dtype=np.int64) * hop
    # (n_frames, frame_len) 视图, 逐帧均方
    frames = np.lib.stride_tricks.as_strided(
        envelope,
        shape=(n_frames, frame_len),
        strides=(envelope.strides[0] * hop, envelope.strides[0]),
    )
    frames = frames.astype(np.float64)
    return np.einsum("ij,ij->i", frames, frames) / frame_len


def _snap_to_grid(t: int, hop: int) -> int:
    """吸附到最近 hop 整数倍(半值向上), 量化误差 <= hop//2。"""
    return ((t + hop // 2) // hop) * hop if hop % 2 == 0 else int(round(t / hop)) * hop


def detect_segments(envelope: np.ndarray, frame_len: int, hop: int) -> list[tuple[int, int]]:
    """检测语音段, 返回 [(start, end)] 采样点列表(已量化到 hop 网格)。

    亚帧估计: 边界帧能量 e = overlap * A^2 / frame_len, 其中 A^2 取游程内最大帧能量
    (对应完全落入段内的帧), 故 overlap = e * frame_len / A^2, 可精确还原段边界,
    再吸附到 hop 网格引入 <= hop//2 的量化误差。
    """
    energies = frame_energies(envelope, frame_len, hop)
    if len(energies) == 0:
        return []
    active = energies > 0.0
    segments: list[tuple[int, int]] = []
    k = 0
    n_frames = len(energies)
    while k < n_frames:
        if not active[k]:
            k += 1
            continue
        lo = k
        while k + 1 < n_frames and active[k + 1]:
            k += 1
        hi = k
        k += 1
        a2 = float(energies[lo:hi + 1].max())
        overlap_lo = int(round(float(energies[lo]) * frame_len / a2))
        overlap_hi = int(round(float(energies[hi]) * frame_len / a2))
        start_est = lo * hop + frame_len - overlap_lo   # 首活跃帧的有效部分为其后缀
        end_est = hi * hop + overlap_hi                 # 末活跃帧的有效部分为其前缀
        start = _snap_to_grid(start_est, hop)
        end = _snap_to_grid(end_est, hop)
        start = max(0, min(start, len(envelope)))
        end = max(start, min(end, len(envelope)))
        segments.append((int(start), int(end)))
    return segments


def boundary_quant_bound(hop: int) -> int:
    """边界量化误差上界(采样点), 仅由帧移推导。"""
    return hop // 2
