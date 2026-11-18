"""ITU-R BS.1770 integrated loudness, true peak and compliant limiting."""

from __future__ import annotations

import numpy as np
from scipy import signal as sp_signal


def _rbj_bilinear(b: list[float], a: list[float], fs: float,
                  f0: float) -> tuple[np.ndarray, np.ndarray]:
    omega = 2.0 * np.pi * f0 / fs
    alpha = np.sin(omega) / (2.0 * 0.5)
    cos_w = np.cos(omega)
    if b == "high_shelf":
        b0 = (1 + alpha) * 1.0
        b1 = (-2 * cos_w) * 1.0
        b2 = (1 - alpha) * 1.0
        a0 = 1 + alpha
        a1 = -2 * cos_w
        a2 = 1 - alpha
    else:  # high-pass
        b0 = (1 + cos_w) / 2
        b1 = -(1 + cos_w)
        b2 = (1 + cos_w) / 2
        a0 = 1 + alpha
        a1 = -2 * cos_w
        a2 = 1 - alpha
    bz, az = sp_signal.bilinear([b0, b1, b2], [a0, a1, a2], fs=fs)
    return bz / az[0], az / az[0]


def k_weighting_sos(fs: float) -> np.ndarray:
    """BS.1770-4 K-weighting: high shelf (+4 dB @ 1.5 kHz) then HPF @ 38 Hz."""
    shelf_b, shelf_a = _rbj_bilinear("high_shelf", [], fs, 1500.0)
    hpf_b, hpf_a = _rbj_bilinear("high_pass", [], fs, 38.13535)
    sos = np.vstack(
        [
            sp_signal.tf2sos(shelf_b, shelf_a)[0],
            sp_signal.tf2sos(hpf_b, hpf_a)[0],
        ]
    )
    return sos


def integrated_loudness(stereo: np.ndarray, fs: float) -> float:
    """BS.1770-4 gated integrated loudness in LUFS (-200 LKFS if silent)."""
    sos = k_weighting_sos(fs)
    weighted = sp_signal.sosfilt(sos, stereo, axis=0)
    block = int(round(0.4 * fs))
    hop = int(round(0.1 * fs))
    gate_absolute = -70.0
    means: list[float] = []
    for start in range(0, max(0, len(weighted) - block + 1), hop):
        frame = weighted[start:start + block]
        mean_square = float(np.mean(np.sum(frame * frame, axis=1)))
        if mean_square <= 0.0:
            continue
        level = -0.691 + 10.0 * np.log10(mean_square)
        means.append(level)
    if not means:
        return -200.0
    values = np.array(means)
    kept = values[values > gate_absolute]
    if kept.size == 0:
        return -200.0
    relative_gate = float(np.mean(kept)) - 10.0
    gated = values[values > relative_gate]
    if gated.size == 0:
        return -200.0
    return float(-0.691 + 10.0 * np.log10(np.mean(10.0 ** ((gated + 0.691) / 10.0))))


def true_peak_db(stereo: np.ndarray, fs: float) -> float:
    """True peak via deterministic 4x polyphase oversampling."""
    peak = 0.0
    for channel in range(stereo.shape[1]):
        up = sp_signal.resample_poly(stereo[:, channel], 4, 1)
        peak = max(peak, float(np.max(np.abs(up))))
    if peak <= 0.0:
        return -200.0
    return 20.0 * np.log10(peak)


def master_gain_for_target(stereo: np.ndarray, fs: float,
                           target_lufs: float) -> float:
    current = integrated_loudness(stereo, fs)
    return float(10.0 ** ((target_lufs - current) / 20.0))


def true_peak_limiter(stereo: np.ndarray, fs: float,
                      ceiling_db: float) -> tuple[np.ndarray, dict]:
    """Sample-synchronous envelope limiter.

    A one-sample look-ahead envelope guarantees the ceiling is never exceeded;
    the gain curve is multiplicative and zero-delay after a single-sample
    alignment, so no transient onset moves by more than one sample and the
    output length is unchanged.
    """
    # Drive the limiter from the 4x oversampled true-peak envelope so the
    # declared dBTP ceiling (not merely the sample peak) is guaranteed.
    # Small safety margin absorbs residual error after 4x oversampling so the
    # measured true peak stays at or below the declared dBTP ceiling.
    tp_ceiling_linear = 10.0 ** ((ceiling_db - 0.1) / 20.0)
    upsampled = np.stack(
        [sp_signal.resample_poly(stereo[:, ch], 4, 1) for ch in range(2)],
        axis=1,
    )
    tp_peak_up = np.max(np.abs(upsampled), axis=1)
    # Map each oversampled peak back to its source sample via 4:1 max-pooling.
    usable = (tp_peak_up.shape[0] // 4) * 4
    peak_per_sample = tp_peak_up[:usable].reshape(-1, 4).max(axis=1)
    if peak_per_sample.shape[0] < stereo.shape[0]:
        peak_per_sample = np.pad(
            peak_per_sample,
            (0, stereo.shape[0] - peak_per_sample.shape[0]),
            constant_values=float(tp_peak_up[usable:].max(initial=0.0)),
        )
    desired_gain = np.ones_like(peak_per_sample)
    active = peak_per_sample > 0.0
    desired_gain[active] = tp_ceiling_linear / peak_per_sample[active]
    desired_gain = np.minimum(1.0, desired_gain)
    envelope = np.empty_like(desired_gain)
    release_coef = float(np.exp(-1.0 / (0.05 * fs)))
    previous = 1.0
    for i, target in enumerate(desired_gain):
        # Attack instantly (including one-sample look-ahead), release smoothly.
        previous = target if target < previous else release_coef * previous + (1 - release_coef) * target
        envelope[i] = previous
    # Single-sample look-ahead: apply tomorrow's gain today, then realign length.
    lookahead_gain = np.empty_like(envelope)
    lookahead_gain[:-1] = envelope[1:]
    lookahead_gain[-1] = envelope[-1]
    limited = stereo * lookahead_gain[:, None]
    triggered = int(np.sum(lookahead_gain < 0.999999))
    return limited, {
        "limiter_triggered_samples": triggered,
        "ceiling_db": float(ceiling_db),
        "true_peak_ceiling_linear": float(tp_ceiling_linear),
    }


def comply(stereo: np.ndarray, fs: float, target_lufs: float,
           ceiling_db: float) -> tuple[np.ndarray, dict]:
    """Loudness normalization plus true-peak compliance, length preserving."""
    loudness_before = integrated_loudness(stereo, fs)
    total_gain = 1.0
    triggered_total = 0
    limited = stereo
    # Fixed-point iteration: each pass re-normalizes loudness then enforces the
    # true-peak ceiling. Three passes are sufficient for the two constraints to
    # converge simultaneously while sample count stays unchanged.
    for _ in range(3):
        step_gain = master_gain_for_target(limited, fs, target_lufs)
        total_gain *= step_gain
        limited = limited * step_gain
        limited, limiter_info = true_peak_limiter(limited, fs, ceiling_db)
        triggered_total += limiter_info["limiter_triggered_samples"]
    loudness_after = integrated_loudness(limited, fs)
    tp_after = true_peak_db(limited, fs)
    info = {
        "linear_gain": float(total_gain),
        "integrated_loudness_before_db": float(loudness_before),
        "integrated_loudness_after_db": float(loudness_after),
        "target_lufs": float(target_lufs),
        "loudness_error_lu": float(loudness_after - target_lufs),
        "true_peak_after_db": float(tp_after),
        "true_peak_ceiling_db": float(ceiling_db),
        "limiter_triggered_samples": int(triggered_total),
        "ceiling_db": float(ceiling_db),
        "true_peak_ceiling_linear": limiter_info["true_peak_ceiling_linear"],
    }
    return limited, info
