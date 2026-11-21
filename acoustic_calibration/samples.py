"""Deterministic local impulse-response sample generator."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .wavio import CHANNEL_LAYOUTS, write_wav_16


@dataclass(frozen=True)
class SampleInfo:
    path: Path
    layout: str
    sample_rate: int
    arrivals: tuple[int, ...]
    amplitudes: tuple[float, ...]
    seed: int

    def to_dict(self) -> dict[str, object]:
        return {
            "path": str(self.path),
            "layout": self.layout,
            "sample_rate": self.sample_rate,
            "arrivals": list(self.arrivals),
            "amplitudes": list(self.amplitudes),
            "seed": self.seed,
        }


def _make_channel(
    rng: np.random.Generator,
    sample_rate: int,
    sample_count: int,
    arrival: int,
    amplitude: float,
    channel_index: int,
    is_lfe: bool,
) -> np.ndarray:
    output = np.zeros(sample_count, dtype=np.float64)
    time = np.arange(sample_count, dtype=np.float64) / sample_rate
    decay = np.exp(-(time - arrival / sample_rate) / (0.11 + 0.015 * channel_index))
    decay[:arrival] = 0.0
    tail = rng.normal(0.0, 0.012, sample_count) * decay
    if is_lfe:
        lfe_phase = 0.4 + channel_index
        tail += 0.06 * np.sin(2.0 * np.pi * 52.0 * time + lfe_phase) * decay
    else:
        for frequency, relative_level in ((220.0, 0.018), (630.0, 0.012), (1400.0, 0.008)):
            phase = (channel_index + 1) * 0.37
            tail += relative_level * np.sin(2.0 * np.pi * frequency * time + phase) * decay

    output[arrival] += amplitude
    if arrival + 17 < sample_count:
        output[arrival + 17] += amplitude * 0.21
    if arrival + 53 < sample_count:
        output[arrival + 53] += amplitude * 0.11
    output += tail
    output += rng.normal(0.0, 0.00022, sample_count)
    return np.clip(output, -0.999, 0.999)


def generate_sample_wav(
    path: str | Path,
    layout: str = "5.1",
    sample_rate: int = 48000,
    duration_s: float = 0.75,
    seed: int = 19093,
    write_metadata: bool = True,
) -> SampleInfo:
    if layout not in ("mono", "5.1"):
        raise ValueError("layout must be 'mono' or '5.1'")
    if sample_rate != 48000:
        raise ValueError("sample_rate must be 48000")
    if not 0.1 <= duration_s <= 10.0:
        raise ValueError("duration_s must be in [0.1, 10]")
    channel_names = CHANNEL_LAYOUTS[1 if layout == "mono" else 6]
    rng = np.random.default_rng(seed)
    sample_count = int(round(sample_rate * duration_s))
    base_arrivals = (312, 367, 401, 448, 329, 389)
    base_amplitudes = (0.72, 0.61, 0.66, 0.43, 0.56, 0.52)
    arrivals = tuple(base_arrivals[index] for index in range(len(channel_names)))
    amplitudes = tuple(base_amplitudes[index] for index in range(len(channel_names)))
    channels = np.vstack(
        [
            _make_channel(
                rng,
                sample_rate,
                sample_count,
                arrivals[index],
                amplitudes[index],
                index,
                channel_names[index] == "LFE",
            )
            for index in range(len(channel_names))
        ]
    )
    output_path = write_wav_16(path, channels, sample_rate)
    info = SampleInfo(
        path=output_path,
        layout=layout,
        sample_rate=sample_rate,
        arrivals=arrivals,
        amplitudes=amplitudes,
        seed=seed,
    )
    if write_metadata:
        metadata_path = output_path.with_suffix(output_path.suffix + ".json")
        metadata_path.write_text(json.dumps(info.to_dict(), ensure_ascii=False, indent=2), encoding="utf-8")
    return info
