"""前瞻限幅器（真峰上限保护）。

设计要点：

* 在 ``lookahead`` 窗口内预先看到未来峰值，提前衰减，避免阶跃削波
* 增益加深/恢复均使用一阶时间常数平滑，不允许阶跃
* 不做硬裁剪（clipping），仅施加受控增益；输出长度/通道数与输入严格一致
"""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np
from scipy.ndimage import maximum_filter1d

from ..core.config import LimiterConfig, SUPPORTED_SAMPLE_RATES
from ..core.errors import ParameterError
from .kweighting import true_peak_db

EPS = 1e-12


@dataclass(frozen=True)
class LimiterResult:
    samples: np.ndarray
    gain_db: np.ndarray
    max_gain_correction_db: float


def _alpha(tau_seconds: float, fs: int) -> float:
    return float(np.exp(-1.0 / (tau_seconds * fs)))


def limit(x: np.ndarray, fs: int, cfg: LimiterConfig) -> LimiterResult:
    if fs not in SUPPORTED_SAMPLE_RATES:
        raise ParameterError(f"不支持的采样率 {fs}", field="sample_rate")
    cfg.validate(fs)
    if x.ndim not in (1, 2) or (x.ndim == 2 and x.shape[1] != 2):
        raise ParameterError("仅支持单声道或立体声输入", field="channels")

    mono = x.ndim == 1
    signal_2d = x[:, np.newaxis] if mono else x
    n = signal_2d.shape[0]
    lookahead = max(1, int(round(cfg.lookahead_seconds * fs)))
    ceiling_linear = 10.0 ** (cfg.ceiling_db / 20.0)

    level = np.max(np.abs(signal_2d.astype(np.float64)), axis=1)
    # 前瞻：lookahead 窗口内的最大电平（右对齐最大值滤波）
    # 对末尾补 -inf 后做窗口最大值，等价于逐点 max(level[i:i+lookahead])
    padded = np.concatenate([level, np.full(lookahead - 1, -np.inf)])
    future_peak = maximum_filter1d(
        padded, size=lookahead, origin=-(lookahead // 2), mode="nearest"
    )[:n]

    # 需要的最小线性增益（<=1）
    required_gain = np.ones(n, dtype=np.float64)
    exceed = future_peak > ceiling_linear
    required_gain[exceed] = ceiling_linear / np.maximum(
        future_peak[exceed], EPS)
    required_db = 20.0 * np.log10(np.maximum(required_gain, EPS))

    # 一阶平滑：加深（启动）快、恢复（释放）慢，均不阶跃
    attack_a = _alpha(max(cfg.lookahead_seconds, 1.0 / fs), fs)
    release_a = _alpha(cfg.release_seconds, fs)
    gain_db = np.zeros(n, dtype=np.float64)
    prev = 0.0
    for i in range(n):
        target = required_db[i]  # 负值或 0
        if target < prev:        # 需要更深衰减
            prev = attack_a * prev + (1.0 - attack_a) * target
        else:                    # 恢复
            prev = release_a * prev + (1.0 - release_a) * target
        gain_db[i] = prev

    # 前瞻增益对齐：当前时刻施加 lookahead 之后的增益，保证峰值到达前已衰减。
    # 序列尾部（无未来信息）保持最后一个增益，长度不变。
    aligned = np.empty(n, dtype=np.float64)
    aligned[:n - lookahead] = gain_db[lookahead:]
    aligned[n - lookahead:] = gain_db[-1]

    gain_linear = 10.0 ** (aligned / 20.0)
    out = signal_2d * gain_linear[:, np.newaxis]

    # 数值安全：若平滑导致极轻微超限，再施加一次受控（非裁剪）整体修正
    post_tp = true_peak_db(out, 4)
    extra_db = 0.0
    if np.isfinite(post_tp) and post_tp > cfg.ceiling_db:
        extra_db = cfg.ceiling_db - post_tp - 1e-6
        out *= 10.0 ** (extra_db / 20.0)
        aligned += extra_db

    correction = float(-np.min(aligned))
    return LimiterResult(
        samples=out[:, 0].copy() if mono else out,
        gain_db=aligned,
        max_gain_correction_db=correction,
    )
