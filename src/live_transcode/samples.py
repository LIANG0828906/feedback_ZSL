from __future__ import annotations

import struct
from pathlib import Path
from typing import Any

import numpy as np

from .resample import quantize_pcm

_PCM_SUBFORMAT = bytes.fromhex("0100000000001000800000aa00389b71")


def _wav_bytes(samples: np.ndarray, sample_rate: int, bits: int, channel_mask: int) -> bytes:
    channels = samples.shape[1]
    pcm, _ = quantize_pcm(samples, bits)
    block_align = channels * (bits // 8)
    fmt = struct.pack(
        "<HHIIHHHHI",
        0xFFFE,
        channels,
        sample_rate,
        sample_rate * block_align,
        block_align,
        bits,
        22,
        bits,
        channel_mask,
    ) + _PCM_SUBFORMAT
    data_chunk = b"data" + struct.pack("<I", len(pcm)) + pcm
    if len(pcm) & 1:
        data_chunk += b"\x00"
    fmt_chunk = b"fmt " + struct.pack("<I", len(fmt)) + fmt
    body = b"WAVE" + fmt_chunk + data_chunk
    return b"RIFF" + struct.pack("<I", len(body)) + body


def synthesize_material(
    *,
    channels: int,
    seed: int,
    duration: float = 2.4,
    sample_rate: int = 48000,
    bits: int = 16,
) -> np.ndarray:
    if channels not in (1, 6):
        raise ValueError("channels must be 1 or 6")
    rng = np.random.default_rng(seed)
    frames = int(round(duration * sample_rate))
    t = np.arange(frames, dtype=np.float64) / sample_rate
    normalized = t / max(duration, 1e-9)
    signal = np.zeros((frames, channels), dtype=np.float64)
    for channel in range(channels):
        f0 = 120.0 + 37.0 * channel
        f1 = 2300.0 - 131.0 * channel
        span = max(duration - 0.65, 0.1)
        local = np.clip((t - 0.35) / span, 0.0, 1.0)
        phase = 2.0 * np.pi * (f0 * t + (f1 - f0) * t * local / 2.0)
        channel_signal = np.sin(phase + channel * 0.31)
        channel_signal += 0.18 * np.sin(2.0 * np.pi * (53.0 + channel * 9.0) * t)
        rng_noise = rng.standard_normal(frames)
        channel_signal += 0.006 * rng_noise
        signal[:, channel] = channel_signal

    envelope = np.zeros(frames, dtype=np.float64)
    active_windows = ((0.35, 1.10), (1.30, 2.16))
    ramp = 0.012
    for start, stop in active_windows:
        left = int(start * sample_rate)
        right = int(stop * sample_rate)
        if left >= frames or right <= 0:
            continue
        left = max(0, left)
        right = min(frames, right)
        envelope[left:right] = 1.0
        ramp_samples = int(ramp * sample_rate)
        for offset in range(ramp_samples):
            gain = offset / max(ramp_samples - 1, 1)
            if left + offset < frames:
                envelope[left + offset] *= gain
            if 0 <= right - offset - 1 < frames:
                envelope[right - offset - 1] *= gain
    signal *= envelope[:, None]
    peak = float(np.max(np.abs(signal)))
    if peak > 0:
        signal *= 0.76 / peak
    return signal


def generate_fixtures(
    output_dir: str | Path,
    *,
    seed: int = 19093,
    duration: float = 2.4,
) -> list[dict[str, Any]]:
    target = Path(output_dir)
    target.mkdir(parents=True, exist_ok=True)
    fixtures: list[dict[str, Any]] = []
    for layout, channels, mask in (("mono", 1, 0x4), ("surround51", 6, 0x3F)):
        for bits in (16, 24):
            material_id = f"{layout}_{bits}bit_seed{seed}"
            samples = synthesize_material(
                channels=channels,
                seed=seed + channels * 100 + bits,
                duration=duration,
                bits=bits,
            )
            path = target / f"{material_id}.wav"
            payload = _wav_bytes(samples, 48000, bits, mask)
            temp = path.with_name(f".{path.name}.tmp")
            temp.write_bytes(payload)
            temp.replace(path)
            fixtures.append(
                {
                    "material_id": material_id,
                    "path": str(path),
                    "channels": channels,
                    "bits_per_sample": bits,
                    "layout": layout,
                    "channel_mask": f"0x{mask:08x}",
                    "bytes": len(payload),
                    "seed": seed,
                    "silence_windows_seconds": [[0.0, 0.35], [1.10, 1.30], [2.16, duration]],
                }
            )
    return fixtures

