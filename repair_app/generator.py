from __future__ import annotations

from pathlib import Path

import numpy as np

from .audio import WavInfo, write_wav_atomic
from .errors import AppError
from .models import validate_sample_rate


def generate_samples(
    *,
    sample_rate: int = 48_000,
    channels: int = 2,
    seconds: float = 4.0,
    seed: int = 20_261_120,
    dc_offset: float = 0.025,
) -> np.ndarray:
    """Generate a deterministic speech/music-like 16-bit test signal.

    The implementation uses a separate PCG64 stream per channel and a fixed
    sequence of NumPy operations. No wall-clock time or unseeded RNG is used.
    """

    validate_sample_rate(sample_rate)
    if channels not in (1, 2):
        raise AppError("E_INVALID_ARGUMENT", "样本声道数必须是 1 或 2")
    if not 0.1 <= seconds <= 600.0:
        raise AppError("E_INVALID_ARGUMENT", "样本时长必须在 0.1..600 秒")
    if isinstance(seed, bool) or not isinstance(seed, int) or seed < 0:
        raise AppError("E_INVALID_ARGUMENT", "随机种子必须是非负整数")

    frames = max(1, int(round(seconds * sample_rate)))
    time = np.arange(frames, dtype=np.float64) / float(sample_rate)
    channel_payloads: list[np.ndarray] = []

    for channel in range(channels):
        rng = np.random.Generator(np.random.PCG64(seed + channel * 1_000_003))
        f1 = 180.0 + channel * 23.0 + float(rng.integers(0, 17))
        f2 = 430.0 + channel * 31.0 + float(rng.integers(0, 29))
        f3 = 910.0 + channel * 47.0 + float(rng.integers(0, 41))
        vibrato = 1.0 + 0.035 * np.sin(2.0 * np.pi * 5.3 * time + channel)

        signal = (
            0.18 * np.sin(2.0 * np.pi * f1 * time * vibrato)
            + 0.11 * np.sin(2.0 * np.pi * f2 * time + 0.3 * channel)
            + 0.065 * np.sin(2.0 * np.pi * f3 * time + 0.7)
        )

        pulse_positions = np.arange(0, frames, max(1, sample_rate // 3))
        for offset in pulse_positions:
            pulse_length = min(max(1, sample_rate // 90), frames - offset)
            pulse_time = np.arange(pulse_length, dtype=np.float64) / sample_rate
            pulse = 0.12 * np.sin(2.0 * np.pi * 2_600.0 * pulse_time) * np.exp(
                -pulse_time * 45.0
            )
            signal[offset : offset + pulse_length] += pulse

        speech_gate = np.ones_like(time)
        speech_gate[(time > 1.15) & (time < 1.35)] = 0.015
        speech_gate[(time > 2.45) & (time < 2.75)] = 0.025
        low_noise = rng.standard_normal(frames) * 0.009
        crackle = np.zeros(frames, dtype=np.float64)
        crackle_positions = rng.integers(0, frames, size=max(4, frames // 18_000))
        crackle[crackle_positions] = rng.uniform(-0.22, 0.22, size=crackle_positions.size)
        signal = signal * speech_gate + low_noise + crackle
        signal = signal * (0.86 + 0.14 * np.sin(2.0 * np.pi * 0.17 * time + channel))
        signal += dc_offset * (1.0 if channel == 0 else -0.8)
        channel_payloads.append(signal)

    stacked = np.stack(channel_payloads, axis=1)
    peak = float(np.max(np.abs(stacked))) if stacked.size else 0.0
    if peak > 0.92:
        stacked = stacked * (0.92 / peak)
    quantized = np.clip(np.rint(stacked * 32767.0), -32768, 32767).astype(np.int16)
    return quantized


def generate_sample_file(
    path: str | Path,
    *,
    sample_rate: int = 48_000,
    channels: int = 2,
    seconds: float = 4.0,
    seed: int = 20_261_120,
) -> WavInfo:
    samples = generate_samples(
        sample_rate=sample_rate, channels=channels, seconds=seconds, seed=seed
    )
    write_wav_atomic(path, samples, sample_rate, expected_channels=channels)
    return WavInfo(sample_rate=sample_rate, channels=channels, frames=samples.shape[0])

