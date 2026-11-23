"""动态范围压缩器。

* 电平检测：逐样本全通道峰值包络，转 dBFS
* 静态曲线：阈值以上按 ratio 压缩
* 平滑：启动/释放一阶低通，系数由时间常数
  ``alpha = exp(-1/(fs*tau))`` 决定，增益永远不会阶跃
"""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from ..core.config import CompressorConfig, SUPPORTED_SAMPLE_RATES
from ..core.errors import ParameterError

EPS = 1e-12


@dataclass(frozen=True)
class CompressorResult:
    samples: np.ndarray          # 压缩后音频，shape 与输入一致
    gain_db: np.ndarray          # 逐样本增益（dB，含 makeup）
    gain_reduction_db: np.ndarray  # 逐样本压缩量（正数 dB）


def _time_constant_alpha(tau_seconds: float, fs: int) -> float:
    return float(np.exp(-1.0 / (tau_seconds * fs)))


def compress(x: np.ndarray, fs: int, cfg: CompressorConfig) -> CompressorResult:
    if fs not in SUPPORTED_SAMPLE_RATES:
        raise ParameterError(f"不支持的采样率 {fs}", field="sample_rate")
    cfg.validate()
    if x.ndim not in (1, 2) or (x.ndim == 2 and x.shape[1] != 2):
        raise ParameterError("仅支持单声道或立体声输入", field="channels")

    mono = x.ndim == 1
    signal_2d = x[:, np.newaxis] if mono else x
    n = signal_2d.shape[0]

    level = np.max(np.abs(signal_2d.astype(np.float64)), axis=1)
    level_db = 20.0 * np.log10(np.maximum(level, EPS))

    # 静态压缩曲线 -> 目标增益（负值 dB）
    over = np.maximum(level_db - cfg.threshold_db, 0.0)
    target_reduction = over * (1.0 - 1.0 / cfg.ratio)

    attack_a = _time_constant_alpha(cfg.attack_seconds, fs)
    release_a = _time_constant_alpha(cfg.release_seconds, fs)

    # 平滑压缩量：压缩加深（启动）与恢复（释放）使用不同系数
    reduction = np.zeros(n, dtype=np.float64)
    prev = 0.0
    for i in range(n):
        target = target_reduction[i]
        if target > prev:  # 需要更深压缩 -> 启动
            prev = attack_a * prev + (1.0 - attack_a) * target
        else:              # 恢复 -> 释放
            prev = release_a * prev + (1.0 - release_a) * target
        reduction[i] = prev

    gain_db = cfg.makeup_gain_db - reduction
    gain_linear = 10.0 ** (gain_db / 20.0)
    out = signal_2d * gain_linear[:, np.newaxis]

    return CompressorResult(
        samples=out[:, 0].copy() if mono else out,
        gain_db=gain_db,
        gain_reduction_db=reduction,
    )
