"""门限积分响度测量（ITU-R BS.1770-4）。

流程：K 加权 -> 分帧 -> 逐帧均方能量（立体声 +1.5 dB 通道权重）
-> 绝对门限（-70 LKFS）-> 相对门限（均值 -10 LU）-> 积分响度 L_KG。
"""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from ..core.config import FrameConfig, SUPPORTED_SAMPLE_RATES
from ..core.errors import ParameterError
from .kweighting import apply_k_weighting, true_peak_db

#: BS.1770 立体声权重（左/右各 +1.5 dB -> 线性系数 10^(1.5/10)）。
STEREO_WEIGHT = 10.0 ** (1.5 / 10.0)


@dataclass(frozen=True)
class LoudnessResult:
    """单段响度测量结果。"""

    integrated_lufs: float
    true_peak_db: float
    frame_loudness: np.ndarray          # 逐帧响度 LKFS（未门限）
    gate_active: np.ndarray             # 逐帧是否通过双门限
    frame_count: int
    gated_frame_count: int


def frame_energy(x: np.ndarray, frame_length: int, hop: int) -> np.ndarray:
    """对 (N,) 或 (N, C) 信号分帧并返回逐帧均方能量。

    尾帧不足 frame_length 时丢弃（BS.1770 采用固定 400ms 帧）。
    """
    if x.ndim == 1:
        n = x.shape[0]
        starts = np.arange(0, n - frame_length + 1, hop)
        if starts.size == 0:
            return np.empty(0, dtype=np.float64)
        frames = np.stack([x[s:s + frame_length] for s in starts], axis=0)
        return np.mean(frames.astype(np.float64) ** 2, axis=1)
    n = x.shape[0]
    starts = np.arange(0, n - frame_length + 1, hop)
    if starts.size == 0:
        return np.empty(0, dtype=np.float64)
    energies = np.zeros(starts.size, dtype=np.float64)
    for ch in range(x.shape[1]):
        frames = np.stack([x[s:s + frame_length, ch] for s in starts],
                          axis=0)
        energies += np.mean(frames.astype(np.float64) ** 2, axis=1)
    return energies


def _validate_signal(x: np.ndarray, fs: int, frame_cfg: FrameConfig) -> None:
    if fs not in SUPPORTED_SAMPLE_RATES:
        raise ParameterError(
            f"不支持的采样率 {fs}，允许 {SUPPORTED_SAMPLE_RATES}",
            field="sample_rate")
    if x.ndim not in (1, 2) or (x.ndim == 2 and x.shape[1] != 2):
        raise ParameterError("仅支持单声道 (N,) 或立体声 (N, 2) 输入",
                             field="channels")
    if not np.isfinite(x).all():
        raise ParameterError("信号包含 NaN/Inf", field="samples")
    frame_cfg.validate(fs)


def measure_loudness(x: np.ndarray, fs: int,
                     frame_cfg: FrameConfig | None = None) -> LoudnessResult:
    """计算整段积分响度、真峰值与逐帧响度。"""
    cfg = frame_cfg or FrameConfig()
    _validate_signal(x, fs, cfg)

    frame_length = int(round(cfg.frame_seconds * fs))
    hop = int(round(cfg.hop_seconds * fs))
    if hop < 1 or frame_length < 1 or hop > frame_length:
        raise ParameterError(
            f"帧参数越界: frame_length={frame_length}, hop={hop}",
            field="frame_seconds")

    weighted = apply_k_weighting(x, fs)
    energies = frame_energy(weighted, frame_length, hop)
    if x.ndim == 2:
        # frame_energy 对两通道取均值，BS.1770 立体声每通道 +1.5 dB，
        # 均值口径下直接乘线性权重 10^(1.5/10) 即可。
        energies = energies * STEREO_WEIGHT

    with np.errstate(divide="ignore"):
        frame_lk = -0.691 + 10.0 * np.log10(
            np.maximum(energies, 1e-20))
    frame_lk[energies <= 0] = -np.inf

    # 绝对门限
    abs_active = frame_lk > cfg.absolute_gate
    gated = abs_active.copy()
    if np.any(abs_active):
        # 相对门限：绝对门限内帧的非加权平均响度 - 10 LU
        mean_lk = -0.691 + 10.0 * np.log10(
            np.mean(energies[abs_active]))
        relative_gate = mean_lk + cfg.relative_gate_offset
        # BS.1770：相对门限若高于绝对门限则采用相对门限
        effective_gate = max(cfg.absolute_gate, relative_gate)
        gated = frame_lk > effective_gate

    if np.any(gated):
        integrated = -0.691 + 10.0 * np.log10(
            np.mean(energies[gated]))
    else:
        # 全部帧低于门限：按规范积分响度无定义，记为 -inf
        integrated = -np.inf

    tp = true_peak_db(x, cfg.true_peak_oversample)
    return LoudnessResult(
        integrated_lufs=float(integrated),
        true_peak_db=float(tp),
        frame_loudness=frame_lk,
        gate_active=gated,
        frame_count=int(energies.size),
        gated_frame_count=int(np.count_nonzero(gated)),
    )
