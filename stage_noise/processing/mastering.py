"""Stereo downmix, ITU-R BS.1770 loudness, true-peak limiting."""

from __future__ import annotations

import numpy as np

from ..manifest import Manifest


def downmix(processed_mics: dict, manifest: Manifest,
            total_samples: int) -> np.ndarray:
    stereo = np.zeros((total_samples, 2), dtype=np.float64)
    for mic in manifest.mics:
        sig = processed_mics[mic.name]
        stereo[:len(sig), 0] += mic.downmix[0] * sig
        stereo[:len(sig), 1] += mic.downmix[1] * sig
    return stereo


def _k_weighting_response(n_fft: int, sr: int) -> np.ndarray:
    """Combined BS.1770-4 pre-filter + RLB frequency response (zero phase)."""
    freqs = np.fft.rfftfreq(n_fft, d=1.0 / sr)
    omega = 2 * np.pi * freqs

    def shelf(f0: float, gain_db: float, q: float) -> np.ndarray:
        amp = 10.0 ** (gain_db / 20.0)
        a = np.sqrt(amp)
        # RBJ high-shelf coefficients
        w0 = 2 * np.pi * f0 / sr
        cos_w0 = np.cos(w0)
        sin_w0 = np.sin(w0)
        al = sin_w0 / (2.0 * q)
        b0 = a * ((a + 1.0) + (a - 1.0) * cos_w0 + 2.0 * np.sqrt(a) * al)
        b1 = -2.0 * a * ((a - 1.0) + (a + 1.0) * cos_w0)
        b2 = a * ((a + 1.0) + (a - 1.0) * cos_w0 - 2.0 * np.sqrt(a) * al)
        a0 = (a + 1.0) - (a - 1.0) * cos_w0 + 2.0 * np.sqrt(a) * al
        a1 = 2.0 * ((a - 1.0) - (a + 1.0) * cos_w0)
        a2 = (a + 1.0) - (a - 1.0) * cos_w0 - 2.0 * np.sqrt(a) * al
        z = np.exp(-1j * omega)
        return (b0 + b1 * z + b2 * z ** 2) / (a0 + a1 * z + a2 * z ** 2)

    def highpass(f0: float, q: float) -> np.ndarray:
        w0 = 2 * np.pi * f0 / sr
        cos_w0 = np.cos(w0)
        sin_w0 = np.sin(w0)
        al = sin_w0 / (2.0 * q)
        b0 = (1.0 + cos_w0) / 2.0
        b1 = -(1.0 + cos_w0)
        b2 = (1.0 + cos_w0) / 2.0
        a0 = 1.0 + al
        a1 = -2.0 * cos_w0
        a2 = 1.0 - al
        z = np.exp(-1j * omega)
        return (b0 + b1 * z + b2 * z ** 2) / (a0 + a1 * z + a2 * z ** 2)

    response = (shelf(1681.763, 4.0, 0.70717535390606)
                * highpass(38.13547, 0.50032704553116))
    response[0] = 0.0
    return response


def k_weight(signal: np.ndarray, sr: int) -> np.ndarray:
    n_fft = int(2 ** np.ceil(np.log2(len(signal) + 4096)))
    response = _k_weighting_response(n_fft, sr)
    spectrum = np.fft.rfft(signal, n_fft, axis=0)
    filtered = np.fft.irfft(spectrum * response[:, None], n_fft, axis=0)
    return filtered[:len(signal)]


def integrated_loudness(stereo: np.ndarray, sr: int) -> float:
    weighted = k_weight(stereo, sr)
    block = max(1, int(round(0.4 * sr)))
    hop = max(1, int(round(0.1 * sr)))
    n_blocks = 1 + max(0, (len(weighted) - block) // hop)
    levels = np.empty(n_blocks)
    for i in range(n_blocks):
        chunk = weighted[i * hop:i * hop + block]
        levels[i] = np.mean(chunk ** 2) + 1e-20
    gated = levels[levels > 10 ** ((-70.0 + 0.691) / 10.0)]
    if gated.size == 0:
        gated = levels
    absolute = 10 ** (-70.0 / 10.0)
    for _ in range(16):
        threshold = -10.0 * np.log10(np.mean(gated) / absolute) - 10.0
        limit = 10 ** ((threshold + 0.691) / 10.0)
        new_gated = levels[levels > limit]
        if new_gated.size == 0 or len(new_gated) == len(gated):
            gated = new_gated if new_gated.size else gated
            break
        gated = new_gated
    return float(-0.691 + 10.0 * np.log10(np.mean(gated)))


def true_peak_dbtp(stereo: np.ndarray, sr: int, factor: int = 4) -> float:
    n = len(stereo)
    n_fft = int(2 ** np.ceil(np.log2(n * factor + factor)))
    spectrum = np.fft.rfft(stereo, n_fft, axis=0)
    up = np.fft.irfft(spectrum * factor, n_fft * factor, axis=0)
    return float(20.0 * np.log10(max(np.max(np.abs(up)), 1e-12)))


def _sliding_min(values: np.ndarray, radius: int) -> np.ndarray:
    """Van Herk/Gil-Werman sliding minimum with O(n) complexity.

    The result at index i is ``min(values[i-radius : i+radius+1])``; edge
    windows shrink to the available samples.
    """
    n = len(values)
    width = 2 * radius + 1
    forward = np.empty(n)
    backward = np.empty(n)
    for block_start in range(0, n, width):
        block_end = min(block_start + width, n)
        running = np.inf
        for i in range(block_start, block_end):
            running = min(running, values[i])
            forward[i] = running
        running = np.inf
        for i in range(block_end - 1, block_start - 1, -1):
            running = min(running, values[i])
            backward[i] = running
    result = np.empty(n)
    for i in range(n):
        left = backward[max(0, i - radius)]
        right = forward[min(n - 1, i + radius)]
        result[i] = min(left, right)
    return result


def lookahead_limiter(stereo: np.ndarray, sr: int,
                      ceiling_linear: float, lookahead_ms: float = 5.0,
                      release_ms: float = 40.0):
    """Look-ahead peak limiter; delay is compensated so length is unchanged."""
    lookahead = max(1, int(round(lookahead_ms / 1000.0 * sr)))
    release_coef = float(np.exp(-1.0 / max(1.0,
                                            release_ms / 1000.0 * sr)))
    peak_env = np.max(np.abs(stereo), axis=1)
    target = ceiling_linear / np.maximum(peak_env, 1e-20)
    raw_gain = np.minimum(1.0, target)
    envelope = _sliding_min(raw_gain, lookahead)
    gain = np.ones(len(stereo))
    smoothed = 1.0
    for i in range(len(stereo)):
        desired = envelope[i]
        smoothed = desired if desired < smoothed else \
            release_coef * smoothed + (1.0 - release_coef) * desired
        gain[i] = smoothed
    delayed = np.zeros_like(stereo)
    delayed[:-lookahead] = stereo[lookahead:]
    limited = delayed * gain[:, None]
    return limited, gain, lookahead


def master(stereo_in: np.ndarray, manifest: Manifest) -> dict:
    sr = manifest.sample_rate
    stereo = stereo_in.copy()
    current_loudness = integrated_loudness(stereo, sr)
    gain_db = manifest.target_loudness_lufs - current_loudness
    gain_linear = 10.0 ** (gain_db / 20.0)
    stereo *= gain_linear
    pre_limiter_tp = true_peak_dbtp(stereo, sr)

    # Start below the target because oversampled true peak exceeds sample
    # peak; iterate the ceiling until the measured true peak is compliant.
    margin_db = 0.5
    limited = stereo
    gain_curve = np.ones(len(stereo))
    lookahead = max(1, int(round(0.005 * sr)))
    for _ in range(6):
        ceiling = 10.0 ** (
            (manifest.true_peak_limit_dbtp - margin_db) / 20.0)
        limited, gain_curve, lookahead = lookahead_limiter(
            stereo, sr, ceiling)
        if true_peak_dbtp(limited, sr) <= manifest.true_peak_limit_dbtp:
            break
        margin_db += 0.5
    limited_samples = int(np.count_nonzero(gain_curve < 0.999999))
    max_atten_db = float(-20.0 * np.log10(max(np.min(gain_curve), 1e-12)))
    final_loudness = integrated_loudness(limited, sr)
    final_tp = true_peak_dbtp(limited, sr)
    if final_tp > manifest.true_peak_limit_dbtp:
        safety = 10.0 ** ((manifest.true_peak_limit_dbtp - final_tp
                           - 0.05) / 20.0)
        limited *= safety
        gain_curve *= safety
        final_loudness = integrated_loudness(limited, sr)
        final_tp = true_peak_dbtp(limited, sr)
    return {
        "master": limited,
        "input_loudness_lufs": round(current_loudness, 3),
        "normalization_gain_db": round(gain_db, 3),
        "pre_limiter_true_peak_dbtp": round(pre_limiter_tp, 3),
        "limiter_lookahead_samples": lookahead,
        "limited_sample_count": limited_samples,
        "limiter_max_attenuation_db": round(max_atten_db, 3),
        "final_integrated_loudness_lufs": round(final_loudness, 3),
        "final_true_peak_dbtp": round(final_tp, 3),
    }
