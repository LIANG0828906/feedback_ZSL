"""谱域噪声抑制模块：分帧加窗、确定性 FFT、按频带增益、重叠相加重建。"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .config import EnhanceConfig


@dataclass
class SpectralResult:
    enhanced: np.ndarray
    residual_ratio: np.ndarray
    band_gain: dict
    noise_psd: np.ndarray


def hann_window(n: int) -> np.ndarray:
    k = np.arange(n, dtype=np.float64)
    return 0.5 - 0.5 * np.cos(2.0 * np.pi * k / n)


def frame_signal(x: np.ndarray, frame: int, hop: int):
    """两端各补 frame//2 零后分帧，返回 (帧矩阵 [n_frames, frame], 原长)。"""
    pad = frame // 2
    xp = np.concatenate([np.zeros(pad), x, np.zeros(pad)])
    n_frames = 1 + (xp.shape[0] - frame) // hop
    idx = np.arange(frame)[None, :] + hop * np.arange(n_frames)[:, None]
    return xp[idx], x.shape[0]


def band_bin_map(frame_size: int, sample_rate: int, bands) -> list:
    """每个频带对应的 FFT bin 下标（仅正频率）。"""
    freqs = np.fft.rfftfreq(frame_size, d=1.0 / sample_rate)
    mapping = []
    for name, f_lo, f_hi in bands:
        bins = np.nonzero((freqs >= f_lo) & (freqs < f_hi))[0]
        mapping.append((name, bins))
    return mapping


def spectral_suppress(
    x: np.ndarray,
    noise_segment: np.ndarray,
    config: EnhanceConfig,
    channel: int,
    cancel_check=None,
) -> SpectralResult:
    """对单声道执行谱域噪声抑制（wiener 或 spectral_sub 增益）。"""
    sp = config.spectral_for(channel)
    frame, hop = sp.frame_size, sp.hop
    win = hann_window(frame)
    frames, n_orig = frame_signal(x, frame, hop)
    n_frames = frames.shape[0]
    spec = np.fft.rfft(frames * win[None, :], axis=1)

    noise_frames, _ = frame_signal(noise_segment, frame, hop)
    noise_spec = np.fft.rfft(noise_frames * win[None, :], axis=1)
    noise_power_bins = np.mean(np.abs(noise_spec) ** 2, axis=0)

    mapping = band_bin_map(frame, config.sample_rate, config.bands)
    gains = np.ones((n_frames, spec.shape[1]), dtype=np.float64)
    band_gain: dict = {}
    noise_psd = np.zeros(len(mapping), dtype=np.float64)
    residual = np.zeros(n_frames, dtype=np.float64)
    input_noise_energy = 0.0

    for bi, (name, bins) in enumerate(mapping):
        if bins.size == 0:
            band_gain[name] = np.ones(n_frames)
            continue
        spb = config.spectral_for(channel, name)
        nb = float(np.sum(noise_power_bins[bins]))
        noise_psd[bi] = nb
        sig_band = np.sum(np.abs(spec[:, bins]) ** 2, axis=1)
        if spb.method == "spectral_sub":
            g = np.sqrt(
                np.maximum(0.0, 1.0 - spb.over_subtract * nb / np.maximum(sig_band, 1e-30))
            )
        else:
            g = 1.0 - spb.over_subtract * nb / np.maximum(sig_band, 1e-30)
        g = np.clip(g, spb.gain_floor, 1.0)
        gains[:, bins] = g[:, None]
        band_gain[name] = g
        residual += (g ** 2) * nb
        input_noise_energy += nb
        if cancel_check is not None:
            cancel_check()

    out_spec = spec * gains
    out_frames = np.fft.irfft(out_spec, n=frame, axis=1) * win[None, :]

    total = (n_frames - 1) * hop + frame
    out = np.zeros(total, dtype=np.float64)
    wsum = np.zeros(total, dtype=np.float64)
    for t in range(n_frames):
        s = t * hop
        out[s : s + frame] += out_frames[t]
        wsum[s : s + frame] += win
    nz = wsum > 1e-12
    out[nz] /= wsum[nz]
    pad = frame // 2
    enhanced = out[pad : pad + n_orig]

    ratio = residual / max(input_noise_energy, 1e-30)
    return SpectralResult(
        enhanced=enhanced,
        residual_ratio=ratio,
        band_gain=band_gain,
        noise_psd=noise_psd,
    )
