"""Deterministic local multichannel WAV and analytic-truth generator."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from .config import SchedulerConfig
from .metering import K_HIGHPASS_A, K_HIGHPASS_B, K_SHELF_A, K_SHELF_B
from .wav import (
    ChannelLayout,
    MASK_LAYOUTS,
    write_wav,
)


SAMPLE_RATE = 48_000


@dataclass(frozen=True)
class ProgramSegment:
    seconds: float
    amplitudes: tuple[float, ...]


@dataclass(frozen=True)
class ProgramSpec:
    asset_id: str
    filename: str
    channels: int
    bits_per_sample: int
    mask: int
    frequencies: tuple[float, ...]
    segments: tuple[ProgramSegment, ...]

    @property
    def labels(self) -> tuple[str, ...]:
        return MASK_LAYOUTS[self.mask].labels


PROGRAMS: tuple[ProgramSpec, ...] = (
    ProgramSpec(
        asset_id="program_a_5_1",
        filename="program_a_5_1_24bit.wav",
        channels=6,
        bits_per_sample=24,
        mask=0x003F,
        frequencies=(1000.0, 1000.0, 1000.0, 80.0, 1200.0, 1200.0),
        segments=(
            ProgramSegment(1.2, (0.070, 0.070, 0.065, 0.020, 0.045, 0.045)),
            ProgramSegment(1.2, (0.085, 0.082, 0.075, 0.022, 0.052, 0.050)),
            ProgramSegment(1.0, (0.062, 0.064, 0.058, 0.018, 0.041, 0.043)),
            ProgramSegment(0.8, (0.078, 0.075, 0.069, 0.021, 0.048, 0.047)),
        ),
    ),
    ProgramSpec(
        asset_id="program_b_stereo",
        filename="program_b_stereo_16bit.wav",
        channels=2,
        bits_per_sample=16,
        mask=0x0003,
        frequencies=(1000.0, 1100.0),
        segments=(
            ProgramSegment(1.1, (0.095, 0.090)),
            ProgramSegment(1.1, (0.105, 0.101)),
            ProgramSegment(1.0, (0.082, 0.088)),
        ),
    ),
    ProgramSpec(
        asset_id="program_c_mono",
        filename="program_c_mono_16bit.wav",
        channels=1,
        bits_per_sample=16,
        mask=0x0004,
        frequencies=(1000.0,),
        segments=(
            ProgramSegment(1.0, (0.115,)),
            ProgramSegment(0.8, (0.102,)),
            ProgramSegment(0.8, (0.095,)),
        ),
    ),
)


def _phase(seed: int, asset_id: str, channel: int) -> float:
    digest = hashlib.sha256(f"{seed}:{asset_id}:{channel}".encode("ascii")).digest()
    integer = int.from_bytes(digest[:8], "big")
    return (integer / 2**64) * 2.0 * math.pi


def _biquad_magnitude(
    b: Sequence[float],
    a: Sequence[float],
    frequency: float,
    sample_rate: int,
) -> float:
    z = np.exp(-2j * math.pi * frequency / sample_rate)
    numerator = b[0] + b[1] * z + b[2] * z * z
    denominator = 1.0 + a[1] * z + a[2] * z * z
    return float(abs(numerator / denominator))


def _k_weight_magnitude(frequency: float) -> float:
    return _biquad_magnitude(
        K_SHELF_B,
        K_SHELF_A,
        frequency,
        SAMPLE_RATE,
    ) * _biquad_magnitude(
        K_HIGHPASS_B,
        K_HIGHPASS_A,
        frequency,
        SAMPLE_RATE,
    )


def _render_program(spec: ProgramSpec, seed: int) -> np.ndarray:
    total_frames = int(round(sum(segment.seconds for segment in spec.segments) * SAMPLE_RATE))
    output = np.zeros((total_frames, spec.channels), dtype=np.float64)
    cursor = 0
    phases = [_phase(seed, spec.asset_id, channel) for channel in range(spec.channels)]
    for segment in spec.segments:
        frames = int(round(segment.seconds * SAMPLE_RATE))
        end = min(total_frames, cursor + frames)
        local_frames = end - cursor
        if local_frames <= 0:
            continue
        time = np.arange(cursor, end, dtype=np.float64) / SAMPLE_RATE
        for channel in range(spec.channels):
            amplitude = float(segment.amplitudes[channel])
            frequency = float(spec.frequencies[channel])
            output[cursor:end, channel] = amplitude * np.sin(
                2.0 * math.pi * frequency * time + phases[channel]
            )
        cursor = end
    if cursor < total_frames:
        output = output[:cursor]
    return output


def _analytic_truth(spec: ProgramSpec, signal_frames: int) -> dict[str, Any]:
    layout = ChannelLayout(
        MASK_LAYOUTS[spec.mask].name,
        spec.mask,
        spec.labels,
    )
    weights = layout.weights
    total_seconds = sum(segment.seconds for segment in spec.segments)
    weighted_energy = 0.0
    true_peak = 0.0
    for segment in spec.segments:
        for channel, amplitude in enumerate(segment.amplitudes):
            frequency = spec.frequencies[channel]
            energy = (
                weights[channel]
                * (amplitude * amplitude / 2.0)
                * (_k_weight_magnitude(frequency) ** 2)
            )
            weighted_energy += energy * (segment.seconds / total_seconds)
            true_peak = max(true_peak, amplitude)
    declared_lufs = -0.691 + 10.0 * math.log10(max(weighted_energy, 1e-30))
    declared_true_peak = (
        20.0 * math.log10(true_peak) if true_peak > 0 else -120.0
    )
    return {
        "declared_lufs": float(declared_lufs),
        "declared_true_peak_dbtp": float(declared_true_peak),
        "declared_ungated_weighted_energy": float(weighted_energy),
        "frames": int(signal_frames),
    }


def _write_json_atomic(path: Path, payload: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    text = json.dumps(
        payload,
        sort_keys=True,
        indent=2,
        ensure_ascii=False,
        allow_nan=False,
    )
    with open(temporary, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
        handle.write("\n")
        handle.flush()
    temporary.replace(path)


def generate_samples(
    sample_dir: str | Path,
    *,
    seed: int = 20261121,
    config_path: str | Path | None = None,
) -> dict[str, Any]:
    """Generate WAVs, analytic truth, and a complete workbench config."""
    sample_dir = Path(sample_dir)
    sample_dir.mkdir(parents=True, exist_ok=True)
    generated: list[dict[str, Any]] = []
    for spec in PROGRAMS:
        samples = _render_program(spec, seed)
        wav_path = sample_dir / spec.filename
        info = write_wav(
            wav_path,
            samples,
            sample_rate=SAMPLE_RATE,
            bits_per_sample=spec.bits_per_sample,
            channel_mask=spec.mask,
        )
        truth = _analytic_truth(spec, info.num_frames)
        truth.update(
            {
                "asset_id": spec.asset_id,
                "path": spec.filename,
                "sample_rate": SAMPLE_RATE,
                "channels": spec.channels,
                "bits_per_sample": spec.bits_per_sample,
                "channel_mask": spec.mask,
                "layout": info.layout,
                "labels": list(spec.labels),
                "frequencies_hz": list(spec.frequencies),
                "segments": [
                    {
                        "seconds": segment.seconds,
                        "amplitudes": list(segment.amplitudes),
                    }
                    for segment in spec.segments
                ],
            }
        )
        generated.append(truth)

    truth_payload = {
        "generator": "deterministic-multichannel-v1",
        "seed": seed,
        "sample_rate": SAMPLE_RATE,
        "assets": generated,
    }
    _write_json_atomic(sample_dir / "truth.json", truth_payload)

    scheduler = SchedulerConfig(
        parallelism=3,
        memory_budget_bytes=24 * 1024 * 1024,
        per_task_memory_budget_bytes=9 * 1024 * 1024,
        oversample_batch_samples=512,
        internal_buffer_samples=65536,
        degrade_sequence=(
            "increase_oversample_batch",
            "shrink_internal_buffer",
        ),
    )
    config = {
        "meter": {
            "version": 3,
            "sample_rate": 48000,
            "block_samples": 4800,
            "short_term_samples": 144000,
            "oversample_factor": 4,
            "target_lufs": -23.0,
            "true_peak_limit_dbtp": -1.0,
            "absolute_gate_lufs": -70.0,
            "relative_gate_lu": -10.0,
            "channel_weights": None,
            "true_peak_include_lfe": True,
            "lufs_tolerance": 0.000001,
            "true_peak_tolerance": 0.000001,
            "scheduler": scheduler.to_dict(),
        },
        "assets": [
            {
                "id": "program_a_5_1",
                "path": (sample_dir / "program_a_5_1_24bit.wav").as_posix(),
            },
            {
                "id": "program_b_stereo",
                "path": (sample_dir / "program_b_stereo_16bit.wav").as_posix(),
            },
            {
                "id": "program_c_mono",
                "path": (sample_dir / "program_c_mono_16bit.wav").as_posix(),
            },
        ],
        "artifact_dir": "artifacts/run",
        "trace_path": "artifacts/run/trace.jsonl",
        "snapshot_dir": "artifacts/run/snapshots",
        "sample_dir": sample_dir.as_posix(),
        "seed": seed,
    }
    config_target = Path(config_path) if config_path else sample_dir / "config.json"
    _write_json_atomic(config_target, config)
    return {
        "seed": seed,
        "assets": generated,
        "truth_path": (sample_dir / "truth.json").as_posix(),
        "config_path": config_target.as_posix(),
    }
