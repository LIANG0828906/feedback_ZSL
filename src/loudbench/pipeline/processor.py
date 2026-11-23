"""单段确定性 DSP 处理（测量 -> 压缩 -> 限幅）。

纯函数实现，不持有锁/线程状态，并行与串行路径调用同一份代码，
从根本上保证两条路径逐样本一致。
"""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from ..audio.io import content_hash
from ..core.config import PipelineConfig
from ..core.errors import ParameterError
from ..dsp.compressor import compress
from ..dsp.limiter import limit
from ..dsp.loudness import LoudnessResult, measure_loudness


@dataclass
class ProcessedSegment:
    index: int
    fs: int
    input_hash: str
    pre_norm_samples: np.ndarray      # 压缩+限幅后、归一前
    input_loudness: LoudnessResult
    limited_tp_db: float
    compressor_gain_db: np.ndarray
    limiter_gain_db: np.ndarray

    @property
    def total_dsp_gain_curve(self) -> np.ndarray:
        return self.compressor_gain_db + self.limiter_gain_db


def process_segment_dsp(index: int, x: np.ndarray, fs: int,
                        config: PipelineConfig) -> ProcessedSegment:
    """对单段执行测量、压缩、限幅（不含全局归一）。"""
    config.validate(fs)
    if x.ndim not in (1, 2) or (x.ndim == 2 and x.shape[1] != 2):
        raise ParameterError("仅支持单声道或立体声输入", field="channels")

    budget = config.scheduler.per_worker_buffer_samples
    if x.shape[0] > budget:
        raise ParameterError(
            f"段 {index} 样本数 {x.shape[0]} 超过每线程缓冲上限 {budget}",
            field="per_worker_buffer_samples")

    loudness = measure_loudness(x, fs, config.frame)
    comp = compress(x, fs, config.compressor)
    lim = limit(comp.samples, fs, config.limiter)

    # 限幅后真峰不得超过上限（含数值保护后复核）
    from ..dsp.kweighting import true_peak_db
    post_tp = true_peak_db(lim.samples, config.frame.true_peak_oversample)
    if np.isfinite(post_tp) and post_tp > config.limiter.ceiling_db + 1e-6:
        raise ParameterError(
            f"限幅后真峰 {post_tp:.3f} dBTP 超过上限 "
            f"{config.limiter.ceiling_db} dBTP",
            field="limiter.ceiling_db")

    return ProcessedSegment(
        index=index,
        fs=fs,
        input_hash=content_hash(x),
        pre_norm_samples=lim.samples,
        input_loudness=loudness,
        limited_tp_db=post_tp,
        compressor_gain_db=comp.gain_db,
        limiter_gain_db=lim.gain_db,
    )
