"""Deterministic local sample generator.

Generates synthetic 5.1 WAV samples at several bit depths with known
true-peak references, plus baseline metering values. Everything is derived
from a fixed seed, so identical seeds and operation sequences produce
byte-identical files, reports and logs.
"""

from __future__ import annotations

import struct
from pathlib import Path

import numpy as np

from .wavfile import KSDATAFORMAT_SUBTYPE_PCM, MASK_51_BACK

RATE = 48000
CHANNELS = 6


def write_wav(path: str | Path, samples: np.ndarray, bits: int,
              channel_mask: int = MASK_51_BACK) -> None:
    """Write a 5.1 WAVEFORMATEXTENSIBLE PCM file (deterministic bytes)."""
    n, ch = samples.shape
    assert ch == CHANNELS
    block_align = ch * (bits // 8)
    byte_rate = RATE * block_align
    if bits == 16:
        pcm = np.clip(np.round(samples * 32768.0), -32768, 32767).astype("<i2")
        data = pcm.tobytes()
    elif bits == 24:
        vals = np.clip(np.round(samples * 8388608.0), -8388608, 8388607).astype(np.int32)
        vals_u = vals & 0xFFFFFF
        b = np.empty((n, ch, 3), dtype=np.uint8)
        b[:, :, 0] = vals_u & 0xFF
        b[:, :, 1] = (vals_u >> 8) & 0xFF
        b[:, :, 2] = (vals_u >> 16) & 0xFF
        data = b.tobytes()
    else:
        raise ValueError("bits must be 16 or 24")

    fmt = struct.pack(
        "<HHIIHHHHI16s",
        0xFFFE, ch, RATE, byte_rate, block_align, bits,
        22, bits, channel_mask, KSDATAFORMAT_SUBTYPE_PCM,
    )
    riff_size = 4 + (8 + len(fmt)) + (8 + len(data))
    with open(path, "wb") as fh:
        fh.write(b"RIFF")
        fh.write(struct.pack("<I", riff_size))
        fh.write(b"WAVE")
        fh.write(b"fmt ")
        fh.write(struct.pack("<I", len(fmt)))
        fh.write(fmt)
        fh.write(b"data")
        fh.write(struct.pack("<I", len(data)))
        fh.write(data)


def _sine(freq: float, amp: float, n: int, phase: float = 0.0) -> np.ndarray:
    t = np.arange(n, dtype=np.float64) / RATE
    return amp * np.sin(2.0 * np.pi * freq * t + phase)


def generate_all(directory: str | Path, seed: int = 20261122) -> list[dict]:
    """Generate the deterministic sample set; returns asset descriptors."""
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(seed)
    assets = []

    # 1) 16-bit: steady 1 kHz tone, -20 dBFS on all 6 channels, 2.0 s.
    n = 2 * RATE
    tone = _sine(1000.0, 10 ** (-20.0 / 20.0), n)
    samples = np.tile(tone[:, None], (1, CHANNELS))
    path = directory / "tone16.wav"
    write_wav(path, samples, 16)
    assets.append({"asset_id": "tone16", "path": str(path), "bits": 16})

    # 2) 24-bit: per-channel frequency spread, 1.5 s.
    n = RATE * 3 // 2
    cols = [_sine(440.0 * (i + 1), 10 ** (-18.0 / 20.0), n, phase=0.1 * i)
            for i in range(CHANNELS)]
    samples = np.stack(cols, axis=1)
    path = directory / "tone24.wav"
    write_wav(path, samples, 24)
    assets.append({"asset_id": "tone24", "path": str(path), "bits": 24})

    # 3) 16-bit true-peak reference: windowed 6 kHz burst whose carrier and
    #    envelope both peak exactly halfway between two sample instants, so
    #    the inter-sample true peak is exactly the amplitude 0.5
    #    (-6.02 dBTP) while sample peaks stay ~0.7 dB lower.
    n = RATE // 10  # 100 ms
    t_c = (n // 2 + 0.5) / RATE  # halfway between two samples
    up_t = np.arange(n * 4, dtype=np.float64) / (RATE * 4)
    env = np.cos(np.pi * (up_t - t_c) / (n / RATE)) ** 2
    env[np.abs(up_t - t_c) > (n / RATE) / 2] = 0.0
    up = 0.5 * env * np.cos(2.0 * np.pi * 6000.0 * (up_t - t_c))
    base = up[::4]
    samples = np.tile(base[:, None], (1, CHANNELS))
    path = directory / "tp_ref.wav"
    write_wav(path, samples, 16)
    assets.append({"asset_id": "tp_ref", "path": str(path), "bits": 16,
                   "true_peak_ref_dbtp": 20.0 * np.log10(0.5)})

    # 4) 24-bit gated content: loud/quiet alternating sections + noise so
    #    that absolute and relative gates actually filter blocks.
    n = 2 * RATE
    sig = np.zeros(n)
    loud = _sine(800.0, 10 ** (-12.0 / 20.0), RATE // 2)
    quiet_amp = 10 ** (-75.0 / 20.0)
    quiet = quiet_amp * rng.standard_normal(RATE // 2)
    seg = np.concatenate([loud, quiet, loud, quiet])
    samples = np.tile(seg[:, None], (1, CHANNELS))
    path = directory / "gated24.wav"
    write_wav(path, samples, 24)
    assets.append({"asset_id": "gated24", "path": str(path), "bits": 24})

    return assets
