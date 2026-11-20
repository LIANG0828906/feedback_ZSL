"""降噪效果的量化验证：逐声道逐频带信噪比改善与残余噪声能量。"""

from __future__ import annotations

import numpy as np

from .config import EnhanceConfig


def bandpass(x: np.ndarray, f_lo: float, f_hi: float, rate: int) -> np.ndarray:
    """确定性 FFT 理想带通（带外 bin 置零）。"""
    spec = np.fft.rfft(x)
    freqs = np.fft.rfftfreq(x.shape[0], d=1.0 / rate)
    spec[(freqs < f_lo) | (freqs >= f_hi)] = 0.0
    return np.fft.irfft(spec, n=x.shape[0])


def _energy(seg: np.ndarray) -> float:
    return float(np.sum(seg * seg))


def channel_band_metrics(
    x_in: np.ndarray,
    x_out: np.ndarray,
    onset: int,
    pretrigger: tuple,
    config: EnhanceConfig,
) -> list:
    """计算单声道全部频带的改善摘要，劣化频带附带标注与可能原因。

    噪声与信号能量分别在各自区间内做带通，避免砖墙滤波的
    sinc 振铃把直达声能量泄漏进噪声估计。
    """
    n = x_in.shape[0]
    p_start, p_end = pretrigger
    s_start = onset
    s_end = min(n, onset + config.metric.signal_window)
    rows = []
    for name, f_lo, f_hi in config.bands:
        noise_in = _energy(bandpass(x_in[p_start:p_end], f_lo, f_hi, config.sample_rate))
        noise_out = _energy(bandpass(x_out[p_start:p_end], f_lo, f_hi, config.sample_rate))
        sig_in = _energy(bandpass(x_in[s_start:s_end], f_lo, f_hi, config.sample_rate))
        sig_out = _energy(bandpass(x_out[s_start:s_end], f_lo, f_hi, config.sample_rate))
        eps = 1e-30
        snr_in = 10.0 * np.log10(max(sig_in, eps) / max(noise_in, eps))
        snr_out = 10.0 * np.log10(max(sig_out, eps) / max(noise_out, eps))
        improvement = snr_out - snr_in
        degraded = bool(improvement < 0.0 or noise_out > noise_in)
        reason = ""
        if degraded:
            causes = []
            if noise_out > noise_in:
                causes.append("残余噪声能量超过输入（增益下限过高或门限衰减不足）")
            if improvement < 0.0:
                causes.append("信噪比改善为负（过减因子过大损伤信号或频带内信号占比过低）")
            reason = "；".join(causes)
        rows.append(
            {
                "band": name,
                "f_lo": f_lo,
                "f_hi": f_hi,
                "snr_in_db": float(snr_in),
                "snr_out_db": float(snr_out),
                "snr_improvement_db": float(improvement),
                "noise_energy_in": noise_in,
                "noise_energy_out": noise_out,
                "residual_ratio": float(noise_out / max(noise_in, eps)),
                "degraded": degraded,
                "reason": reason,
            }
        )
    return rows
