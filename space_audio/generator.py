from __future__ import annotations

import json
import math
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .config import AnalysisConfig, WorkbenchConfig
from .errors import para_error
from .layout import get_layout
from .model import (
    ChannelTruthSummary,
    DirectEvent,
    FrameTruth,
    ReflectionEvent,
    TailEvent,
    frame_truth_for_sample,
)


@dataclass(frozen=True)
class GeneratedSample:
    manifest_path: Path
    sample_id: str
    ir_path: Path
    dry_path: Path


@dataclass(frozen=True)
class GeneratedDataset:
    manifest_path: Path
    sample_ids: tuple[str, ...]


def _gaussian_write(
    target: np.ndarray,
    center: float,
    amplitude: float,
    sigma: float,
) -> None:
    low = max(0, int(math.floor(center - 8.0 * sigma)))
    high = min(target.size, int(math.ceil(center + 8.0 * sigma)) + 1)
    if high <= low:
        return
    indices = np.arange(low, high, dtype=np.float64)
    target[low:high] += amplitude * np.exp(-0.5 * ((indices - center) / sigma) ** 2)


def _tail_values(
    count: int,
    sample_rate: int,
    amplitude: float,
    tau_s: float,
    frequency_1_hz: float,
    frequency_2_hz: float,
    phase_rad: float,
) -> np.ndarray:
    if count <= 0:
        return np.empty(0, dtype=np.float64)
    t = np.arange(count, dtype=np.float64) / float(sample_rate)
    envelope = amplitude * np.exp(-t / tau_s)
    return envelope * (
        0.72 * np.sin(2.0 * np.pi * frequency_1_hz * t + phase_rad)
        + 0.28 * np.sin(2.0 * np.pi * frequency_2_hz * t + 0.7 * phase_rad)
    )


def _channel_delay_offsets(channel_count: int) -> tuple[float, ...]:
    base = (0.0, 1.75, 3.25, 0.75, 2.50, 4.00, 1.25, 3.00)
    if channel_count <= len(base):
        return base[:channel_count]
    return tuple(base[index % len(base)] + 0.25 * (index // len(base)) for index in range(channel_count))


def generate_demo(
    output_dir: str | Path,
    *,
    seed: int = 20261123,
    sample_id: str = "studio_quad",
    sample_rate: int = 48_000,
    layout_name: str = "quad",
    frame_len: int = 2048,
    hop: int = 1024,
    window_len: int = 768,
    frame_count: int = 12,
    dry_duration_s: float = 0.35,
    channel_count: int | None = None,
) -> GeneratedSample:
    """Create a deterministic multichannel IR and dry clip.

    Truth is computed from the construction equations (Gaussian direct events,
    discrete reflection impulses and closed-form decaying-tail samples), never
    by measuring the estimator or rendered output.
    """

    config = WorkbenchConfig(
        analysis=AnalysisConfig(
            frame_len=frame_len,
            hop=hop,
            window_len=window_len,
        )
    )
    config.validate()
    if not 1 <= frame_count <= 4096:
        raise para_error("frame_count must be in [1, 4096]", "frame_count")
    if not 0.12 <= dry_duration_s <= 120.0:
        raise para_error(
            "dry_duration_s must be in [0.12, 120] seconds",
            "dry_duration_s",
        )

    try:
        layout = get_layout(layout_name)
    except Exception:
        raise
    if channel_count is None:
        channel_count = layout.count
    if not 1 <= channel_count <= 64:
        raise para_error("channel_count must be in [1, 64]", "channel_count")
    if channel_count != layout.count:
        raise para_error(
            "channel_count does not match the declared layout",
            "channel_count",
        )

    rng = np.random.default_rng(seed)
    analysis = config.analysis
    total_samples = (frame_count - 1) * hop + frame_len
    ir = np.zeros((channel_count, total_samples), dtype=np.float64)
    offsets = _channel_delay_offsets(channel_count)
    direct_events: list[list[DirectEvent]] = [[] for _ in range(channel_count)]
    reflection_events: list[list[ReflectionEvent]] = [[] for _ in range(channel_count)]
    tail_events: list[list[TailEvent]] = [[] for _ in range(channel_count)]

    reflection_offsets = (22, 41, 67, 103, 149)
    reflection_scales = (0.31, 0.22, 0.16, 0.105, 0.070)

    for frame_index in range(frame_count):
        start = frame_index * hop
        event_gain = 0.78 * (
            0.86 + 0.14 * math.sin(0.71 * frame_index + 0.11 * seed)
        )
        for channel_index in range(channel_count):
            phase_jitter = 0.28 * math.sin(
                0.37 * frame_index + 0.91 * channel_index + 0.03 * seed
            )
            direct_center = start + 28.0 + offsets[channel_index] + phase_jitter
            direct_sigma = 1.32
            direct = DirectEvent(
                position_samples=direct_center,
                amplitude=event_gain,
                sigma_samples=direct_sigma,
                source_frame=frame_index,
            )
            direct_events[channel_index].append(direct)
            _gaussian_write(ir[channel_index], direct_center, event_gain, direct_sigma)

            for reflection_index, (delay, scale) in enumerate(
                zip(reflection_offsets, reflection_scales)
            ):
                offset_jitter = (
                    (frame_index * 3 + channel_index * 5 + reflection_index * 7) % 9
                ) - 4
                position = int(round(direct_center + delay + offset_jitter))
                if position <= int(round(direct_center)) + analysis.direct_half_width:
                    position = (
                        int(round(direct_center))
                        + analysis.direct_half_width
                        + 2
                        + reflection_index
                    )
                sign = -1.0 if (reflection_index + channel_index) % 2 else 1.0
                amplitude = (
                    event_gain
                    * scale
                    * (
                        0.86
                        + 0.14
                        * math.cos(
                            0.19 * frame_index
                            + 0.43 * channel_index
                            + 0.61 * reflection_index
                        )
                    )
                    * sign
                )
                reflection = ReflectionEvent(position, float(amplitude))
                reflection_events[channel_index].append(reflection)
                if 0 <= position < total_samples:
                    ir[channel_index, position] += amplitude

            tail_start = start + window_len
            tail_end = start + hop
            if tail_end > total_samples:
                tail_end = total_samples
            if tail_end > tail_start:
                tail_amp = 0.052 * (
                    0.92 + 0.08 * math.sin(0.29 * frame_index + 0.8 * channel_index)
                )
                tau = 0.0075 + 0.0008 * ((frame_index + channel_index) % 4)
                f1 = 1450.0 + 23.0 * ((frame_index * 5 + channel_index * 11) % 17)
                f2 = 2910.0 + 31.0 * ((frame_index * 7 + channel_index * 3) % 13)
                phase = 0.37 * ((frame_index + 2 * channel_index + seed) % 29)
                tail = TailEvent(
                    start_samples=tail_start,
                    end_samples=tail_end,
                    amplitude=float(tail_amp),
                    tau_s=float(tau),
                    frequency_1_hz=float(f1),
                    frequency_2_hz=float(f2),
                    phase_rad=float(phase),
                )
                tail_events[channel_index].append(tail)
                ir[channel_index, tail_start:tail_end] += _tail_values(
                    tail_end - tail_start,
                    sample_rate,
                    tail_amp,
                    tau,
                    f1,
                    f2,
                    phase,
                )

    if np.max(np.abs(ir)) >= analysis.clip_ceiling:
        raise RuntimeError("generator gain exceeded configured clip ceiling")

    dry_channels = min(2, channel_count)
    dry_len = max(1, int(round(dry_duration_s * sample_rate)))
    dry_t = np.arange(dry_len, dtype=np.float64) / float(sample_rate)
    dry = np.zeros((dry_channels, dry_len), dtype=np.float64)
    for channel_index in range(dry_channels):
        freq = 220.0 + 55.0 * channel_index
        carrier = 0.22 * np.sin(2.0 * np.pi * freq * dry_t + 0.13 * channel_index)
        pulse = np.zeros_like(dry_t)
        pulse_indices = (0.04 + 0.021 * np.arange(9)) * sample_rate
        for pulse_index, index in enumerate(pulse_indices):
            integer = int(round(index))
            if integer < dry_len:
                pulse[integer] += 0.65 * math.exp(-0.14 * pulse_index)
        noise = rng.standard_normal(dry_len) * 0.012
        dry[channel_index] = carrier + pulse + noise

    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    ir_path = output_dir / f"{sample_id}_ir.npz"
    dry_path = output_dir / f"{sample_id}_dry.npy"
    np.savez_compressed(ir_path, ir=ir)
    np.save(dry_path, dry)

    channels_payload: list[dict[str, Any]] = []
    construction_payload: dict[str, Any] = {}
    for channel_index, channel_spec in enumerate(layout.channels):
        construction_payload[channel_spec.id] = {
            "direct_events": [
                {
                    "position_samples": event.position_samples,
                    "amplitude": event.amplitude,
                    "sigma_samples": event.sigma_samples,
                    "source_frame": event.source_frame,
                }
                for event in direct_events[channel_index]
            ],
            "reflections": [
                {
                    "position_samples": event.position_samples,
                    "amplitude": event.amplitude,
                }
                for event in reflection_events[channel_index]
            ],
            "tails": [
                {
                    "start_samples": event.start_samples,
                    "end_samples": event.end_samples,
                    "amplitude": event.amplitude,
                    "tau_s": event.tau_s,
                    "frequency_1_hz": event.frequency_1_hz,
                    "frequency_2_hz": event.frequency_2_hz,
                    "phase_rad": event.phase_rad,
                }
                for event in tail_events[channel_index]
            ],
        }
        channels_payload.append(
            {
                "channel": channel_spec.id,
                "direct_delay_samples": 0.0,
                "direct_delay_s": 0.0,
                "early_reflection_energy": 0.0,
                "reverb_energy": 0.0,
                "drr": 0.0,
                "tdoa_samples": 0.0,
                "tdoa_s": 0.0,
                "frames": [],
            }
        )

    manifest: dict[str, Any] = {
        "schema_version": 1,
        "seed": seed,
        "analysis_profile": analysis.to_dict(),
        "samples": [
            {
                "sample_id": sample_id,
                "sample_rate": sample_rate,
                "channel_layout": layout.to_dict(),
                "ir_file": ir_path.name,
                "dry_file": dry_path.name,
                "construction": construction_payload,
                "truth": {"channels": channels_payload},
            }
        ],
    }

    manifest_path = output_dir / f"{sample_id}_manifest.json"
    manifest_path.write_text(
        json.dumps(manifest, indent=2, ensure_ascii=False, allow_nan=False) + "\n",
        encoding="utf-8",
    )

    from .loader import load_sample

    sample = load_sample(manifest_path, sample_id=sample_id)
    summary_rows: list[ChannelTruthSummary] = []
    per_frame_truth: list[list[FrameTruth]] = [
        [] for _ in range(channel_count)
    ]
    for frame_index in range(sample.frame_count(analysis)):
        frame_truth = frame_truth_for_sample(sample, frame_index, analysis)
        for channel_index, truth in enumerate(frame_truth):
            per_frame_truth[channel_index].append(truth)

    for channel_index, channel_data in enumerate(channels_payload):
        truths = per_frame_truth[channel_index]
        channel_data["direct_delay_samples"] = float(
            np.mean([truth.direct_delay_samples for truth in truths])
        )
        channel_data["direct_delay_s"] = (
            channel_data["direct_delay_samples"] / sample_rate
        )
        channel_data["early_reflection_energy"] = float(
            np.sum([truth.early_reflection_energy for truth in truths])
        )
        channel_data["reverb_energy"] = float(
            np.sum([truth.reverb_energy for truth in truths])
        )
        channel_data["drr"] = float(
            np.mean([truth.drr for truth in truths])
        )
        channel_data["tdoa_samples"] = float(
            np.mean([truth.tdoa_samples for truth in truths])
        )
        channel_data["tdoa_s"] = float(
            np.mean([truth.tdoa_s for truth in truths])
        )
        channel_data["frames"] = [truth.to_dict() for truth in truths]
        summary_rows.append(
            ChannelTruthSummary(
                channel_id=channel_data["channel"],
                direct_delay_samples=channel_data["direct_delay_samples"],
                direct_delay_s=channel_data["direct_delay_s"],
                early_reflection_energy=channel_data["early_reflection_energy"],
                reverb_energy=channel_data["reverb_energy"],
                drr=channel_data["drr"],
                tdoa_samples=channel_data["tdoa_samples"],
                tdoa_s=channel_data["tdoa_s"],
                frames=tuple(truths),
            )
        )

    manifest_path.write_text(
        json.dumps(manifest, indent=2, ensure_ascii=False, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    return GeneratedSample(
        manifest_path=manifest_path,
        sample_id=sample_id,
        ir_path=ir_path,
        dry_path=dry_path,
    )


def generate_dataset(
    output_dir: str | Path,
    *,
    seed: int = 20261123,
    count: int = 3,
    layouts: tuple[str, ...] = ("stereo", "quad", "5.1"),
    frame_len: int = 2048,
    hop: int = 1024,
    window_len: int = 768,
    frame_count: int = 8,
    dry_duration_s: float = 0.35,
) -> GeneratedDataset:
    """Generate several deterministic samples and one selectable manifest."""

    if count < 1:
        raise para_error("dataset count must be at least one", "count")
    if not layouts:
        raise para_error("layouts must not be empty", "layouts")
    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    combined_samples: list[dict[str, Any]] = []
    sample_ids: list[str] = []
    for index in range(count):
        layout_name = layouts[index % len(layouts)]
        sample_id = f"space_{index:02d}_{layout_name.replace('.', '_')}"
        sample_dir = output_dir / f"sample_{index:02d}"
        if sample_dir.exists():
            shutil.rmtree(sample_dir)
        generated = generate_demo(
            sample_dir,
            seed=seed + index * 97,
            sample_id=sample_id,
            layout_name=layout_name,
            frame_len=frame_len,
            hop=hop,
            window_len=window_len,
            frame_count=frame_count,
            dry_duration_s=dry_duration_s,
        )
        payload = json.loads(generated.manifest_path.read_text(encoding="utf-8"))
        sample_payload = payload["samples"][0]
        sample_payload["ir_file"] = (
            Path(sample_dir.name) / sample_payload["ir_file"]
        ).as_posix()
        sample_payload["dry_file"] = (
            Path(sample_dir.name) / sample_payload["dry_file"]
        ).as_posix()
        combined_samples.append(sample_payload)
        sample_ids.append(sample_id)

    manifest_path = output_dir / "dataset_manifest.json"
    manifest = {
        "schema_version": 1,
        "seed": seed,
        "analysis_profile": AnalysisConfig(
            frame_len=frame_len,
            hop=hop,
            window_len=window_len,
        ).to_dict(),
        "samples": combined_samples,
    }
    manifest_path.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    return GeneratedDataset(
        manifest_path=manifest_path,
        sample_ids=tuple(sample_ids),
    )
