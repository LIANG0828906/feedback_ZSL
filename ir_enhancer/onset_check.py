"""直达声到达位置与幅度基准的不变性校验。"""

from __future__ import annotations

import numpy as np

from .config import EnhanceConfig
from .errors import OnsetGainError, OnsetShiftError
from .gating import protection_window
from .noise import detect_onset


def enforce_and_verify_onset(
    original: np.ndarray,
    enhanced: np.ndarray,
    onset_in: int,
    noise_rms_in: float,
    config: EnhanceConfig,
    channel: int,
) -> np.ndarray:
    """保护性恢复并校验不变性；失败时中止且不返回增强结果。

    重检测使用与输入相同的噪声 RMS 基准，保证检测器口径一致。
    """
    pp = config.protect_for(channel)
    n = original.shape[0]
    w_start, w_end = protection_window(onset_in, n, pp.pre, pp.post)

    out = enhanced.copy()
    out[w_start:w_end] = original[w_start:w_end]

    onset_out = detect_onset(out, noise_rms_in)
    if onset_out != onset_in:
        raise OnsetShiftError(
            "增强后直达声到达样本发生偏移",
            channel=channel,
            onset_in=int(onset_in),
            onset_out=int(onset_out),
        )

    peak_in = float(np.max(np.abs(original[w_start:w_end])))
    peak_out = float(np.max(np.abs(out[w_start:w_end])))
    if peak_in > 0.0:
        dev = abs(peak_out - peak_in) / peak_in
        if dev > pp.amp_tol:
            raise OnsetGainError(
                "保护窗口内峰值幅度偏差超过声明容差",
                channel=channel,
                peak_in=peak_in,
                peak_out=peak_out,
                deviation=dev,
                tolerance=pp.amp_tol,
            )
    return out
