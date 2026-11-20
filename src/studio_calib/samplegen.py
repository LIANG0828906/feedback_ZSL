from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .hashing import file_sha256
from .wavio import write_pcm16_wav


DEFAULT_ARRIVALS = {
    "FL": 233,
    "FR": 241,
    "FC": 229,
    "LFE": 247,
    "SL": 237,
    "SR": 243,
}

CONTROL_FREQUENCIES = np.asarray(
    [20.0, 63.0, 125.0, 250.0, 500.0, 1000.0, 2000.0, 4000.0, 8000.0, 16000.0, 20000.0],
    dtype=np.float64,
)

GAIN_CURVES_DB: dict[str, list[float]] = {
    "FL": [0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0],
    "FR": [1.2, 0.8, 0.4, 0.2, -0.1, -0.3, 0.1, 0.5, 0.8, 0.4, 0.0],
    "FC": [-0.5, -0.2, 0.1, 0.4, 0.3, 0.1, -0.2, -0.6, -0.7, -0.4, 0.0],
    "LFE": [8.0, 7.0, 5.0, 3.0, 1.0, -3.0, -8.0, -12.0, -15.0, -18.0, -20.0],
    "SL": [1.0, 2.0, 1.5, 0.8, 0.2, -0.4, -0.9, -0.5, 0.3, 0.8, 0.5],
    "SR": [-1.5, -2.2, -1.6, -0.5, 0.3, 0.8, 1.2, 0.6, -0.2, -0.9, -0.6],
}

PHASE_CURVES_DEG: dict[str, list[float]] = {
    "FL": [0.0] * len(CONTROL_FREQUENCIES),
    "FR": [4.0, 5.0, 2.0, 0.0, -3.0, 0.0, 3.0, 2.0, 0.0, -2.0, 0.0],
    "FC": [-2.0, 1.0, 4.0, 2.0, 0.0, -1.0, 0.0, 2.0, 1.0, 0.0, 0.0],
    "LFE": [0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0],
    "SL": [8.0, 20.0, 45.0, 80.0, 40.0, 10.0, -5.0, 0.0, 8.0, 5.0, 0.0],
    "SR": [140.0, 175.0, 155.0, 90.0, 30.0, -5.0, 0.0, 8.0, 12.0, 3.0, 0.0],
}


def _curve_values(
    values: list[float], frequencies_hz: np.ndarray
) -> np.ndarray:
    safe = np.maximum(frequencies_hz, 1e-9)
    return np.interp(
        np.log2(safe), np.log2(CONTROL_FREQUENCIES), np.asarray(values)
    )


def synth_channel(
    channel: str,
    sample_rate: int,
    length: int,
    arrival_sample: int,
    rng: np.random.Generator,
) -> np.ndarray:
    frequencies = np.fft.rfftfreq(length, d=1.0 / sample_rate)
    gain_db = _curve_values(GAIN_CURVES_DB[channel], frequencies)
    phase_deg = _curve_values(PHASE_CURVES_DEG[channel], frequencies)
    magnitude = np.power(10.0, gain_db / 20.0)
    phase = -np.deg2rad(phase_deg)
    transfer = magnitude * np.exp(1j * phase)
    impulse = np.roll(np.fft.irfft(transfer, n=length), arrival_sample)
    impulse[:arrival_sample] = 0.0
    impulse[arrival_sample] += 0.035
    impulse *= 0.20

    noise_rms = (1.0 / 32768.0) * 0.35
    impulse += rng.normal(0.0, noise_rms, size=length)

    tail_start = min(length - 1, arrival_sample + 1)
    tail_time = np.arange(length - tail_start, dtype=np.float64)
    tail = rng.normal(0.0, 1.0, size=length - tail_start)
    tail *= noise_rms * 1.8 * np.exp(-tail_time / (0.45 * sample_rate))
    impulse[tail_start:] += tail
    return impulse


def _quantized(values: np.ndarray) -> np.ndarray:
    return np.clip(np.rint(values * 32768.0), -32768, 32767) / 32768.0


def _manifest(
    seed: int,
    sample_rate: int,
    length: int,
    files: dict[str, dict[str, Any]],
    root: Path,
) -> dict[str, Any]:
    channels = list(DEFAULT_ARRIVALS)
    expected_offsets = {
        channel: DEFAULT_ARRIVALS[channel] - DEFAULT_ARRIVALS["FL"]
        for channel in channels
    }
    return {
        "schema": "studio-calibration-sample-v1",
        "seed": seed,
        "sample_rate": sample_rate,
        "length_samples": length,
        "quantization_step": 1.0 / 32768.0,
        "channel_layout_51": channels,
        "truth": {
            "arrival_samples": DEFAULT_ARRIVALS,
            "reference_channel": "FL",
            "expected_offsets_samples": expected_offsets,
            "gain_control_frequencies_hz": CONTROL_FREQUENCIES.tolist(),
            "gain_curves_db": GAIN_CURVES_DB,
            "phase_curves_deg": PHASE_CURVES_DEG,
        },
        "files": {
            name: {
                **metadata,
                "sha256": file_sha256(str(root / metadata["filename"])),
            }
            for name, metadata in files.items()
        },
    }


def generate_dataset(
    output_dir: str | Path,
    seed: int = 20261120,
    sample_rate: int = 48000,
    length: int = 24000,
) -> dict[str, Any]:
    root = Path(output_dir)
    root.mkdir(parents=True, exist_ok=True)
    rng = np.random.Generator(np.random.PCG64(seed))
    channels = list(DEFAULT_ARRIVALS)
    matrix = np.column_stack(
        [
            _quantized(
                synth_channel(
                    channel,
                    sample_rate,
                    length,
                    DEFAULT_ARRIVALS[channel],
                    rng,
                )
            )
            for channel in channels
        ]
    )
    mono = matrix[:, channels.index("FC")]
    surround_path = root / "surround51.wav"
    mono_path = root / "mono.wav"
    write_pcm16_wav(str(surround_path), matrix, sample_rate)
    write_pcm16_wav(str(mono_path), mono, sample_rate)

    files = {
        "surround51": {
            "filename": surround_path.name,
            "channels": channels,
            "layout": "5.1",
        },
        "mono": {
            "filename": mono_path.name,
            "channels": ["M"],
            "layout": "mono",
            "source_channel": "FC",
        },
    }
    manifest = _manifest(seed, sample_rate, length, files, root)
    baseline = {
        "schema": "studio-calibration-baseline-v1",
        "seed": seed,
        "measurement_sha256": manifest["files"]["surround51"]["sha256"],
        "reference_channel": "FL",
        "arrival_samples": DEFAULT_ARRIVALS,
        "offset_samples": manifest["truth"]["expected_offsets_samples"],
        "gain_control_frequencies_hz": CONTROL_FREQUENCIES.tolist(),
        "gain_curves_db": GAIN_CURVES_DB,
        "phase_curves_deg": PHASE_CURVES_DEG,
        "expected_diagnostic_checks": [
            "detected arrivals match truth within 1 sample",
            "low-frequency gain jumps have a named propagation source",
            "phase mismatches include the channel and band label",
            "filter coefficients and JSONL traces are byte-reproducible",
        ],
    }
    for name, payload in (("manifest.json", manifest), ("baseline.json", baseline)):
        with open(root / name, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, sort_keys=True, indent=2, ensure_ascii=True)
            handle.write("\n")
    return manifest

