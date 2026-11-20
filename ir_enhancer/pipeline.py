"""单声道处理管线与整段离线处理入口。"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .config import EnhanceConfig
from .gating import apply_gate
from .metrics import channel_band_metrics
from .noise import estimate_noise
from .onset_check import enforce_and_verify_onset
from .spectral import spectral_suppress


@dataclass
class ChannelResult:
    channel: int
    enhanced: np.ndarray
    onset: int
    noise_rms_in: float
    snr_in_db: float
    residual_ratio_mean: float
    residual_ratio_max: float
    band_metrics: list


def process_channel(
    x: np.ndarray,
    noise_segment: np.ndarray,
    onset: int,
    noise_rms: float,
    snr_in_db: float,
    pretrigger: tuple,
    config: EnhanceConfig,
    channel: int,
    cancel_check=None,
) -> ChannelResult:
    """谱域抑制 -> 时域门限 -> 直达声保护校验 -> 频带指标。"""
    spec = spectral_suppress(x, noise_segment, config, channel, cancel_check)
    residual_mean = float(spec.residual_ratio.mean())
    residual_rms = noise_rms * float(np.sqrt(max(residual_mean, 1e-30)))
    gated = apply_gate(spec.enhanced, residual_rms, onset, config, channel)
    if cancel_check is not None:
        cancel_check()
    enhanced = enforce_and_verify_onset(x, gated, onset, noise_rms, config, channel)
    bands = channel_band_metrics(x, enhanced, onset, pretrigger, config)
    return ChannelResult(
        channel=channel,
        enhanced=enhanced,
        onset=int(onset),
        noise_rms_in=float(noise_rms),
        snr_in_db=float(snr_in_db),
        residual_ratio_mean=float(spec.residual_ratio.mean()),
        residual_ratio_max=float(spec.residual_ratio.max()),
        band_metrics=bands,
    )


def process_all(
    data: np.ndarray,
    config: EnhanceConfig,
    cancel_check=None,
    channel_filter=None,
) -> tuple:
    """整段离线处理全部（或指定）声道，返回 (逐声道结果, 噪声估计, 预触发区间)。"""
    estimates, interval, onsets = estimate_noise(
        data, config.noise, config.metric.signal_window
    )
    p_start, p_end = interval
    results = []
    for ch in range(data.shape[1]):
        if channel_filter is not None and ch not in channel_filter:
            results.append(None)
            continue
        if cancel_check is not None:
            cancel_check()
        est = estimates[ch]
        results.append(
            process_channel(
                data[:, ch],
                data[p_start:p_end, ch],
                int(onsets[ch]),
                est.rms,
                est.snr_db,
                interval,
                config,
                ch,
                cancel_check,
            )
        )
    return results, estimates, interval
