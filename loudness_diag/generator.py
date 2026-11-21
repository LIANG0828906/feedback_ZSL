"""Deterministic local 5.1 PCM sample generation."""

from __future__ import annotations

import math
import struct
from pathlib import Path
from typing import Any

import numpy as np

from .dsp import oversampled_peak_envelope
from .jsonutil import atomic_write_json, canonical_bytes, sha256_bytes
from .wav import CHANNEL_LAYOUTS

DEFAULT_SAMPLE_RATE = 48000
DEFAULT_CHANNEL_MASK = 0x003F
_PCM_GUID = bytes.fromhex("0100000000001000800000aa00389b71")


def _chunk(chunk_id: bytes, payload: bytes) -> bytes:
    result = chunk_id + struct.pack("<I", len(payload)) + payload
    if len(payload) & 1:
        result += b"\0"
    return result


def write_pcm51_wav(
    path: str | Path,
    samples: np.ndarray,
    *,
    bit_depth: int,
    sample_rate: int = DEFAULT_SAMPLE_RATE,
    channel_mask: int = DEFAULT_CHANNEL_MASK,
) -> None:
    """Write deterministic WAVE_FORMAT_EXTENSIBLE 5.1 PCM."""

    values = np.asarray(samples, dtype=np.float64)
    if values.ndim != 2 or values.shape[1] != 6:
        raise ValueError("expected a (frames, 6) sample matrix")
    if bit_depth not in (16, 24):
        raise ValueError("only 16-bit and 24-bit PCM are supported")
    if sample_rate != 48000:
        raise ValueError("sample generator is fixed at 48000 Hz")
    if channel_mask not in CHANNEL_LAYOUTS:
        raise ValueError("unsupported 5.1 channel mask")

    scale = 1 << (bit_depth - 1)
    minimum = -scale
    maximum = scale - 1
    integers = np.rint(np.clip(values, -1.0, 0.999999999) * scale)
    integers = np.clip(integers, minimum, maximum).astype(np.int64)

    if bit_depth == 16:
        data = integers.astype("<i2").tobytes()
    else:
        unsigned = integers & 0xFFFFFF
        packed = np.empty((integers.shape[0], integers.shape[1], 3), dtype=np.uint8)
        packed[:, :, 0] = unsigned & 0xFF
        packed[:, :, 1] = (unsigned >> 8) & 0xFF
        packed[:, :, 2] = (unsigned >> 16) & 0xFF
        data = packed.tobytes()

    channels = 6
    block_align = channels * (bit_depth // 8)
    byte_rate = sample_rate * block_align
    fmt_payload = (
        struct.pack(
            "<HHIIHH",
            0xFFFE,
            channels,
            sample_rate,
            byte_rate,
            block_align,
            bit_depth,
        )
        + struct.pack("<HHI", 22, bit_depth, channel_mask)
        + _PCM_GUID
    )
    body = _chunk(b"fmt ", fmt_payload) + _chunk(b"data", data)
    riff = b"RIFF" + struct.pack("<I", len(body) + 4) + b"WAVE" + body
    Path(path).write_bytes(riff)


def _synthesize(seed: int, seconds: float = 4.0) -> tuple[np.ndarray, list[dict[str, int]]]:
    if seconds <= 0:
        raise ValueError("duration must be positive")
    rng = np.random.default_rng(seed)
    sample_rate = DEFAULT_SAMPLE_RATE
    count = round(seconds * sample_rate)
    time = np.arange(count, dtype=np.float64) / sample_rate

    vibrato = 0.012 * np.sin(2.0 * math.pi * 5.1 * time)
    fundamental = 196.0 * (1.0 + vibrato)
    phase = 2.0 * math.pi * np.cumsum(fundamental) / sample_rate
    singing = (
        0.34 * np.sin(phase)
        + 0.13 * np.sin(2.0 * phase + 0.17)
        + 0.07 * np.sin(3.0 * phase + 0.43)
        + 0.035 * np.sin(5.0 * phase + 0.91)
    )

    sweep_start = 180.0
    sweep_end = 4200.0
    sweep_phase = 2.0 * math.pi * (
        sweep_start * time
        + (sweep_end - sweep_start)
        * time
        * time
        / (2.0 * max(seconds, 1e-9))
    )
    sweep = 0.12 * np.sin(sweep_phase)

    low_start = round(1.2 * sample_rate)
    low_end = round(2.4 * sample_rate)
    low_second_start = round(3.3 * sample_rate)
    low_second_end = round(3.8 * sample_rate)

    channels = np.zeros((count, 6), dtype=np.float64)
    channel_sources = [
        singing,
        0.97 * singing + 0.02 * sweep,
        0.91 * singing,
        0.25 * np.sin(2.0 * math.pi * 48.0 * time),
        0.67 * singing + 0.18 * sweep,
        0.70 * singing + 0.16 * np.roll(sweep, 97),
    ]
    random_phase = rng.uniform(-0.04, 0.04, size=6)
    for channel, source in enumerate(channel_sources):
        channel_gain = 0.82 * (1.0 + random_phase[channel])
        channels[:, channel] = channel_gain * source

    low_gain = 2.0e-5
    low_signal = (
        np.sin(2.0 * math.pi * 997.0 * time)
        + 0.3 * np.sin(2.0 * math.pi * 1499.0 * time + 0.2)
    ) * low_gain
    channels[low_start:low_end, :] = low_signal[low_start:low_end, None]
    channels[low_second_start:low_second_end, :] = low_signal[
        low_second_start:low_second_end, None
    ]

    channels = np.nan_to_num(channels, nan=0.0, posinf=0.0, neginf=0.0)
    peak = float(np.max(np.abs(channels)))
    if peak > 0.89:
        channels *= 0.89 / peak
    regions = []
    for start, end, reason in (
        (low_start, low_end, "low_level_1"),
        (low_second_start, low_second_end, "low_level_2"),
    ):
        clipped_start = min(start, count)
        clipped_end = min(end, count)
        if clipped_start < clipped_end:
            regions.append(
                {"start": clipped_start, "end": clipped_end, "reason": reason}
            )
    return channels, regions


def generate_test_set(
    output_dir: str | Path,
    *,
    seed: int = 20261122,
    seconds: float = 4.0,
) -> dict[str, Any]:
    output_path = Path(output_dir)
    output_path.mkdir(parents=True, exist_ok=True)
    samples, low_regions = _synthesize(seed, seconds)
    manifest: dict[str, Any] = {
        "schema_version": 1,
        "generator": "loudness_diag.generator.v1",
        "seed": seed,
        "sample_rate": DEFAULT_SAMPLE_RATE,
        "seconds": seconds,
        "channel_mask": DEFAULT_CHANNEL_MASK,
        "channel_names": list(CHANNEL_LAYOUTS[DEFAULT_CHANNEL_MASK]),
        "low_level_regions": low_regions,
        "assets": [],
    }
    for bit_depth in (16, 24):
        scale = 1 << (bit_depth - 1)
        quantized = np.clip(
            np.rint(np.clip(samples, -1.0, 0.999999999) * scale),
            -scale,
            scale - 1,
        )
        normalized = quantized.astype(np.float64) / scale
        true_peak = 0.0
        for channel in range(6):
            envelope = oversampled_peak_envelope(normalized[:, channel], factor=4)
            true_peak = max(true_peak, float(np.max(np.abs(envelope))))
        filename = f"program_51_{bit_depth}bit.wav"
        target = output_path / filename
        write_pcm51_wav(target, samples, bit_depth=bit_depth)
        file_bytes = target.read_bytes()
        manifest["assets"].append(
            {
                "asset_id": f"program_51_{bit_depth}bit",
                "file": filename,
                "bit_depth": bit_depth,
                "sample_count": int(normalized.shape[0]),
                "sample_peak": float(np.max(np.abs(normalized))),
                "true_peak_reference": true_peak,
                "true_peak_reference_dbtp": (
                    20.0 * math.log10(true_peak) if true_peak > 0 else None
                ),
                "sha256": sha256_bytes(file_bytes),
            }
        )
    atomic_write_json(output_path / "manifest.json", manifest)
    return manifest


def manifest_bytes(manifest: dict[str, Any]) -> bytes:
    return canonical_bytes(manifest)

