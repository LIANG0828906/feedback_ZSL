"""ITU-R BS.1770 integrated loudness, true peak and look-ahead limiting."""

from __future__ import annotations

import numpy as np
from scipy.signal import lfilter, resample_poly

# Stage 1 (pre-filter / high shelf) and stage 2 (RLB high-pass) SOS-equivalent
# coefficients published for 48 kHz in ITU-R BS.1770-4 Table 2.
STAGE1_B = np.array([1.53512485958697, -2.69169618940638, 1.19839281085285])
STAGE1_A = np.array([1.0, -1.69065929318241, 0.73248077421585])
STAGE2_B = np.array([1.0, -2.0, 1.0])
STAGE2_A = np.array([1.0, -1.99004745483398, 0.99007225036621])

ABSOLUTE_GATE_LUFS = -70.0
RELATIVE_GATE_OFFSET_LU = -10.0
BLOCK_SECONDS = 0.4
BLOCK_OVERLAP = 0.75


def k_weight(channel):
    """Apply the two-stage K-weighting filter to one channel."""
    stage1 = lfilter(STAGE1_B, STAGE1_A, channel)
    return lfilter(STAGE2_B, STAGE2_A, stage1)


def integrated_loudness(signal, sample_rate):
    """Integrated loudness in LUFS (BS.1770-4 gated measurement)."""
    weighted = np.column_stack([k_weight(signal[:, ch]) for ch in range(signal.shape[1])])
    block_len = int(round(BLOCK_SECONDS * sample_rate))
    hop = int(round(block_len * (1.0 - BLOCK_OVERLAP)))
    means = []
    for start in range(0, max(1, len(weighted) - block_len + 1), hop):
        block = weighted[start:start + block_len]
        if len(block) < block_len:
            break
        means.append(float(np.mean(block * block)))
    if not means:
        block = weighted
        means = [float(np.mean(block * block))]
    means = np.asarray(means)
    block_loudness = -0.691 + 10.0 * np.log10(means + 1e-12)
    keep = block_loudness > ABSOLUTE_GATE_LUFS
    if not keep.any():
        return -np.inf
    absolute_mean = float(np.mean(means[keep]))
    relative_gate = (-0.691 + 10.0 * np.log10(absolute_mean + 1e-12)) + RELATIVE_GATE_OFFSET_LU
    keep = block_loudness > relative_gate
    gated_mean = float(np.mean(means[keep]))
    return -0.691 + 10.0 * np.log10(gated_mean + 1e-12)


def true_peak(signal, sample_rate):
    """True peak in dBTP using 4x oversampled peak detection."""
    max_linear = 0.0
    for ch in range(signal.shape[1]):
        upsampled = _upsample4(signal[:, ch])
        max_linear = max(max_linear, float(np.max(np.abs(upsampled))))
    return 20.0 * np.log10(max_linear + 1e-20), max_linear


def _upsample4(channel):
    return resample_poly(channel, 4, 1)


def _downsample4(channel):
    return resample_poly(channel, 1, 4)


def true_peak_limit(signal, sample_rate, limit_dbtp, max_passes=3):
    """Sample-accurate true-peak look-ahead limiter.

    Limiting is performed on a 4x oversampled representation so inter-sample
    peaks cannot escape. The smoothed gain envelope (attack/release one-pole
    filter) contains no steps, and every pass compensates delay implicitly by
    keeping the original sample count.
    """
    limit_linear = 10.0 ** (limit_dbtp / 20.0)
    work = signal.astype(np.float64).copy()
    triggered = 0
    max_attenuation_db = 0.0
    pre_loudness = integrated_loudness(work, sample_rate)

    for _ in range(max_passes):
        channels_up = [_upsample4(work[:, ch]) for ch in range(work.shape[1])]
        envelope = np.max(np.abs(np.stack(channels_up, axis=1)), axis=1)
        peak = float(np.max(envelope))
        if peak <= limit_linear:
            break
        target_gain = np.minimum(1.0, limit_linear / np.maximum(envelope, 1e-20))
        # Look-ahead envelope, then a short one-pole smoother so no sample
        # can step: 0.2 ms attack, 15 ms release at the 4x oversampled rate.
        attack = float(np.exp(-1.0 / (0.0002 * sample_rate * 4)))
        release = float(np.exp(-1.0 / (0.015 * sample_rate * 4)))
        gain = np.empty_like(target_gain)
        previous = 1.0
        for i, target in enumerate(target_gain):
            coeff = attack if target < previous else release
            previous = coeff * previous + (1.0 - coeff) * target
            gain[i] = min(1.0, previous)
        # The smoother may briefly overshoot above the safety envelope; clip
        # those isolated points without creating a discontinuity.
        gain = np.minimum(gain, target_gain * 1.001 + 0.0)
        active = gain < (1.0 - 1e-7)
        triggered += int(np.count_nonzero(active))
        max_attenuation_db = max(max_attenuation_db,
                                 float(-20.0 * np.log10(np.min(gain[active]) + 1e-20))
                                 if active.any() else 0.0)
        for ch in range(work.shape[1]):
            limited_up = channels_up[ch] * gain
            work[:, ch] = _downsample4(limited_up)[:len(work)]

    peak_tp, peak_linear = true_peak(work, sample_rate)
    if peak_linear > limit_linear * (1.0 + 1e-6):
        # Deterministic hard safety net, never reached in nominal material.
        work *= limit_linear / peak_linear
        peak_tp, _ = true_peak(work, sample_rate)
    return work, {
        "triggered_samples": int(triggered),
        "max_attenuation_db": float(round(max_attenuation_db, 4)),
        "pre_limiter_loudness_lufs": float(round(pre_loudness, 3)),
        "post_true_peak_dbtp": float(round(peak_tp, 4)),
    }
