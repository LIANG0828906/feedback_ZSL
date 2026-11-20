"""噪声底估计与直达声（onset）检测。"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .config import NoiseParams
from .errors import NoiseEstInvalidError

ONSET_K = 6.0  # 到达检测阈值 = ONSET_K * 噪声 RMS


@dataclass(frozen=True)
class NoiseEstimate:
    channel: int
    interval: tuple  # [start, end)
    rms: float
    power: float
    snr_db: float  # 输入信噪比（信号区能量 / 噪声底能量）


def coarse_onset(data: np.ndarray) -> int:
    """基于全声道最大包络的粗到达检测，用于划定预触发区间。"""
    env = np.max(np.abs(data), axis=1)
    peak = float(env.max())
    if peak <= 0.0:
        return data.shape[0]
    idx = np.nonzero(env >= 0.1 * peak)[0]
    return int(idx[0]) if idx.size else data.shape[0]


def pretrigger_interval(n_samples: int, onset_hint: int, params: NoiseParams) -> tuple:
    end = max(0, onset_hint - params.guard)
    if params.pretrigger_len is not None:
        start = max(0, end - params.pretrigger_len)
    else:
        start = 0
    return start, end


def detect_onset(channel_data: np.ndarray, noise_rms: float) -> int:
    """确定性到达检测：首个 |x| >= ONSET_K * noise_rms 的样本。"""
    thr = max(ONSET_K * noise_rms, 1.0 / 32768.0)
    idx = np.nonzero(np.abs(channel_data) >= thr)[0]
    return int(idx[0]) if idx.size else channel_data.shape[0]


def estimate_noise(data: np.ndarray, params: NoiseParams, signal_window: int):
    """按声道在预触发区间估计噪声底。

    返回 (逐声道估计, 预触发区间 (start,end), 各声道到达样本)。
    区间样本不足或全为零时抛出 E_NOISE_EST_INVALID，附带声道号与区间。
    """
    n_samples, n_channels = data.shape
    hint = coarse_onset(data)
    interval = pretrigger_interval(n_samples, hint, params)
    start, end = interval
    if end - start < params.min_pretrigger:
        raise NoiseEstInvalidError(
            "预触发区间样本不足",
            channel=-1,
            interval=[start, end],
            required=params.min_pretrigger,
        )
    estimates: list = []
    onsets = np.zeros(n_channels, dtype=np.int64)
    for ch in range(n_channels):
        seg = data[start:end, ch]
        power = float(np.mean(seg * seg))
        if power <= 0.0:
            raise NoiseEstInvalidError(
                "预触发区间全为零，无法估计噪声底",
                channel=ch,
                interval=[start, end],
            )
        rms = float(np.sqrt(power))
        onset = detect_onset(data[:, ch], rms)
        onsets[ch] = onset
        sig_end = min(n_samples, onset + signal_window)
        if sig_end > onset:
            sig_power = float(np.mean(data[onset:sig_end, ch] ** 2))
        else:
            sig_power = 0.0
        snr_db = 10.0 * np.log10(max(sig_power, 1e-30) / power)
        estimates.append(
            NoiseEstimate(
                channel=ch,
                interval=(start, end),
                rms=rms,
                power=power,
                snr_db=float(snr_db),
            )
        )
    return estimates, interval, onsets
