"""Deterministic local sample generator.

The generator intentionally avoids wall-clock time and unseeded randomness so
that repeated runs produce byte-identical manifests and sample arrays.
"""

from __future__ import annotations

import json
import math
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
from scipy import signal

from .models import ChannelInfo, ChannelLayout, canonical_json, stable_seed

GENERATOR_VERSION = "1.0.0"
DEFAULT_SEED = 20261123
SAMPLE_RATE = 48_000


@dataclass(frozen=True)
class SuiteSpec:
    name: str
    count: int
    min_duration_s: float
    max_duration_s: float
    description: str


SUITES = (
    SuiteSpec("short_single", 1, 0.42, 0.48, "Single short IR for low-latency checks"),
    SuiteSpec("medium_batch", 6, 1.05, 1.55, "Small batch of medium-length IRs"),
    SuiteSpec("long_many", 24, 2.10, 2.90, "Dozens of long IRs for scale/trade-off tests"),
)

LAYOUTS = {
    "quad": ChannelLayout(
        name="quad",
        channels=(
            ChannelInfo("FL", "Front Left", -30.0),
            ChannelInfo("FR", "Front Right", 30.0),
            ChannelInfo("SL", "Surround Left", -110.0),
            ChannelInfo("SR", "Surround Right", 110.0),
        ),
    ),
    "stereo": ChannelLayout(
        name="stereo",
        channels=(
            ChannelInfo("L", "Left", -30.0),
            ChannelInfo("R", "Right", 30.0),
        ),
    ),
}


def _rounded_duration(rng: np.random.Generator, spec: SuiteSpec) -> float:
    return float(rng.uniform(spec.min_duration_s, spec.max_duration_s))


def _make_dry_signal(rng: np.random.Generator, sample_rate: int, duration_s: float) -> np.ndarray:
    length = max(64, int(round(sample_rate * duration_s)))
    time = np.arange(length, dtype=np.float64) / sample_rate
    signal_out = np.zeros(length, dtype=np.float64)
    partial_count = int(rng.integers(3, 7))
    for _ in range(partial_count):
        frequency = float(rng.uniform(90.0, 2400.0))
        phase = float(rng.uniform(0.0, 2.0 * math.pi))
        amplitude = float(rng.uniform(0.04, 0.22))
        signal_out += amplitude * np.sin(2.0 * math.pi * frequency * time + phase)
    attack = np.minimum(1.0, time / max(1e-4, float(rng.uniform(0.004, 0.025))))
    release_start = duration_s * float(rng.uniform(0.55, 0.78))
    release = np.ones_like(time)
    release_mask = time > release_start
    if np.any(release_mask):
        release[release_mask] = np.linspace(
            1.0, 0.0, int(np.sum(release_mask)), dtype=np.float64
        )
    noise = rng.normal(0.0, 0.012, size=length)
    envelope = attack * release
    signal_out = signal_out * envelope + noise
    peak = float(np.max(np.abs(signal_out)))
    if peak > 0.0:
        signal_out *= 0.72 / peak
    return signal_out.astype(np.float32)


def _direct_delay_for_channel(
    common_delay: int,
    channel: ChannelInfo,
    source_bearing_deg: float,
    sample_rate: int,
) -> int:
    angular_distance = abs(((channel.azimuth_deg - source_bearing_deg + 180.0) % 360.0) - 180.0)
    max_itd = int(round(0.00072 * sample_rate))
    delay = common_delay + int(round(max_itd * (angular_distance / 180.0)))
    return max(1, delay)


def _make_ir(
    rng: np.random.Generator,
    sample_rate: int,
    duration_s: float,
    layout: ChannelLayout,
) -> tuple[np.ndarray, dict[str, Any]]:
    length = max(128, int(round(sample_rate * duration_s)))
    channel_count = len(layout.channels)
    ir = np.zeros((channel_count, length), dtype=np.float64)
    source_bearing = float(rng.uniform(-25.0, 25.0))
    common_delay = int(rng.integers(24, 180))
    decay_high = max(0.12, min(0.75, duration_s * 0.38))
    decay_tau = float(rng.uniform(decay_high * 0.55, decay_high))
    time = np.arange(length, dtype=np.float64) / sample_rate
    truth_channels: list[dict[str, Any]] = []

    for channel_index, channel in enumerate(layout.channels):
        direct_delay = _direct_delay_for_channel(
            common_delay, channel, source_bearing, sample_rate
        )
        angular_distance = abs(
            ((channel.azimuth_deg - source_bearing + 180.0) % 360.0) - 180.0
        )
        direct_amp = 0.36 + 0.55 * (1.0 - angular_distance / 180.0)
        direct_amp *= float(rng.uniform(0.9, 1.08))
        ir[channel_index, direct_delay] += direct_amp

        reflection_data: list[dict[str, Any]] = []
        tap_positions: list[int] = []
        tap_gains: list[float] = []
        reflection_count = int(rng.integers(4, 8))
        for reflection_index in range(reflection_count):
            spacing = 14.0 / max(1, reflection_count)
            offset_ms = float(
                np.clip(
                    4.0 + reflection_index * spacing + rng.uniform(-0.55, 0.55),
                    4.0,
                    19.0,
                )
            )
            offset = int(round(offset_ms * sample_rate / 1000.0))
            position = direct_delay + offset
            if position >= length:
                continue
            gain = direct_amp * float(rng.uniform(0.035, 0.22))
            gain *= -1.0 if rng.random() < 0.35 else 1.0
            ir[channel_index, position] += gain
            tap_positions.append(position)
            tap_gains.append(gain)
            reflection_data.append(
                {
                    "delay_samples": int(position),
                    "relative_ms": float(offset / sample_rate * 1000.0),
                    "gain": float(gain),
                }
            )

        tail_start = direct_delay + int(round(float(rng.uniform(0.021, 0.032)) * sample_rate))
        tail_end = length
        tail_noise = rng.normal(0.0, 1.0, size=max(0, tail_end - tail_start))
        tail_scale = 0.0
        if tail_noise.size:
            cutoff = float(rng.uniform(0.03, 0.22))
            b, a = signal.butter(2, cutoff, btype="lowpass")
            tail_noise = signal.lfilter(b, a, tail_noise)
            tail_time = time[tail_start:tail_end] - time[tail_start]
            envelope = np.exp(-tail_time / decay_tau)
            tail_unit = tail_noise * envelope
            tail_unit_energy = float(np.sum(tail_unit * tail_unit))
            target_drr_db = float(rng.uniform(10.0, 20.0))
            direct_raw_energy = direct_amp * direct_amp
            early_tap_energy = float(
                np.sum(np.asarray(tap_gains, dtype=np.float64) ** 2)
            )
            desired_reverb = direct_raw_energy * (10.0 ** (-target_drr_db / 10.0))
            if early_tap_energy > desired_reverb * 0.65 and tap_gains:
                reflection_scale = math.sqrt(
                    max(0.0, desired_reverb * 0.65) / early_tap_energy
                )
                for reflection_index, gain in enumerate(tap_gains):
                    tap_gains[reflection_index] = gain * reflection_scale
                for reflection in reflection_data:
                    reflection["gain"] *= reflection_scale
                for position, gain in zip(tap_positions, tap_gains, strict=True):
                    ir[channel_index, position] += gain - (
                        gain / reflection_scale if reflection_scale > 0.0 else 0.0
                    )
                early_tap_energy = float(
                    np.sum(np.asarray(tap_gains, dtype=np.float64) ** 2)
                )
            target_tail_energy = max(0.0, desired_reverb - early_tap_energy)
            if tail_unit_energy > 0.0:
                tail_scale = math.sqrt(target_tail_energy / tail_unit_energy)
            ir[channel_index, tail_start:tail_end] += tail_unit * tail_scale

        direct_region = max(0, direct_delay - 1), min(length, direct_delay + 2)
        direct_energy = float(np.sum(ir[channel_index, direct_region[0] : direct_region[1]] ** 2))
        early_start = min(length, direct_delay + max(2, int(round(0.0015 * sample_rate))))
        early_end = min(
            length,
            direct_delay + max(4, int(round(0.020 * sample_rate))),
        )
        early_window_energy = float(np.sum(ir[channel_index, early_start:early_end] ** 2))
        early_tap_energy = float(np.sum(np.asarray(tap_gains, dtype=np.float64) ** 2))
        total_energy = float(np.sum(ir[channel_index] ** 2))
        reverberant_energy = max(0.0, total_energy - direct_energy)
        drr_db = 10.0 * math.log10(
            max(direct_energy, 1e-18) / max(reverberant_energy, 1e-18)
        )
        truth_channels.append(
            {
                "id": channel.id,
                "azimuth_deg": channel.azimuth_deg,
                "direct_delay_samples": int(direct_delay),
                "direct_delay_seconds": float(direct_delay / sample_rate),
                "direct_energy": direct_energy,
                "early_reflection_energy": early_tap_energy,
                "early_tap_energy": early_tap_energy,
                "early_window_energy": early_window_energy,
                "early_window_samples": [early_start, early_end],
                "reverberant_energy": reverberant_energy,
                "drr_db": float(drr_db),
                "early_reflections": reflection_data,
            }
        )

    peak = float(np.max(np.abs(ir)))
    if peak > 0.95:
        scale = 0.95 / peak
        ir *= scale
        for channel_truth in truth_channels:
            channel_truth["direct_energy"] *= scale * scale
            channel_truth["early_reflection_energy"] *= scale * scale
            channel_truth["early_tap_energy"] *= scale * scale
            channel_truth["early_window_energy"] *= scale * scale
            channel_truth["reverberant_energy"] *= scale * scale
            for reflection in channel_truth["early_reflections"]:
                reflection["gain"] *= scale
            channel_truth["drr_db"] = 10.0 * math.log10(
                max(channel_truth["direct_energy"], 1e-18)
                / max(channel_truth["reverberant_energy"], 1e-18)
            )

    truth = {
        "source_bearing_deg": source_bearing,
        "ir_length": length,
        "channels": truth_channels,
    }
    return ir.astype(np.float32), truth


def generate_samples(
    root: str | Path,
    *,
    seed: int = DEFAULT_SEED,
    force: bool = False,
) -> dict[str, Any]:
    root_path = Path(root)
    manifest_path = root_path / "manifest.json"
    if manifest_path.exists() and not force:
        return json.loads(manifest_path.read_text(encoding="utf-8"))
    if root_path.exists() and force:
        for child in root_path.iterdir():
            if child.is_file():
                child.unlink()
            elif child.is_dir():
                shutil.rmtree(child)
    root_path.mkdir(parents=True, exist_ok=True)
    (root_path / "ir").mkdir(exist_ok=True)
    (root_path / "dry").mkdir(exist_ok=True)

    samples: list[dict[str, Any]] = []
    suite_records: list[dict[str, Any]] = []
    for suite_index, suite in enumerate(SUITES):
        suite_records.append(
            {
                "name": suite.name,
                "count": suite.count,
                "min_duration_s": suite.min_duration_s,
                "max_duration_s": suite.max_duration_s,
                "description": suite.description,
            }
        )
        for sample_index in range(suite.count):
            rng = np.random.default_rng(stable_seed(seed, suite.name, sample_index))
            layout = LAYOUTS["quad"]
            ir_duration = _rounded_duration(rng, suite)
            dry_duration = float(rng.uniform(0.32, 0.72))
            ir, truth = _make_ir(rng, SAMPLE_RATE, ir_duration, layout)
            dry = _make_dry_signal(rng, SAMPLE_RATE, dry_duration)
            sample_id = f"{suite.name}_{sample_index:03d}"
            ir_relative = Path("ir") / f"{sample_id}.npy"
            dry_relative = Path("dry") / f"{sample_id}.npy"
            np.save(root_path / ir_relative, ir, allow_pickle=False)
            np.save(root_path / dry_relative, dry, allow_pickle=False)
            truth.update(
                {
                    "sample_rate": SAMPLE_RATE,
                    "ir_shape": list(ir.shape),
                    "dry_length": int(dry.shape[0]),
                    "dry_duration_s": float(dry.shape[0] / SAMPLE_RATE),
                }
            )
            samples.append(
                {
                    "id": sample_id,
                    "suite": suite.name,
                    "sample_rate": SAMPLE_RATE,
                    "layout": layout.to_dict(),
                    "ir_path": ir_relative.as_posix(),
                    "dry_path": dry_relative.as_posix(),
                    "truth": truth,
                }
            )

    injection_base = "medium_batch_000"
    injection_cases = [
        {
            "id": "inject_missing_channel",
            "sample_id": injection_base,
            "expected_code": "E_CHAN",
            "injection": {"missing_channel": "SL"},
            "description": "Remove one IR channel while metadata still declares quad",
        },
        {
            "id": "inject_order_inversion",
            "sample_id": injection_base,
            "expected_code": "E_ORDER",
            "injection": {"invert_timing_order": True},
            "description": "Declare a reversed channel timing order on the input",
        },
        {
            "id": "inject_gain_overflow",
            "sample_id": injection_base,
            "expected_code": "E_CLIP",
            "injection": {"gain_scale": 4.0},
            "description": "Scale one input above the digital full-scale boundary",
        },
        {
            "id": "inject_bad_frame",
            "sample_id": injection_base,
            "expected_code": "E_FRAME",
            "injection": {"bad_frames": [2]},
            "description": "Mark frame 2 as unreadable without poisoning other frames",
        },
    ]
    manifest = {
        "schema_version": 1,
        "generator_version": GENERATOR_VERSION,
        "seed": int(seed),
        "sample_rate": SAMPLE_RATE,
        "suites": suite_records,
        "layouts": {name: layout.to_dict() for name, layout in LAYOUTS.items()},
        "samples": samples,
        "injection_cases": injection_cases,
        "digest_note": "Canonical JSON plus raw .npy payloads; regeneration is deterministic.",
    }
    manifest_bytes = canonical_json(manifest).encode("utf-8")
    tmp_path = manifest_path.with_suffix(".json.tmp")
    tmp_path.write_bytes(manifest_bytes + b"\n")
    tmp_path.replace(manifest_path)
    return manifest


if __name__ == "__main__":
    generated = generate_samples(Path(__file__).resolve().parents[1] / "samples", force=True)
    print(
        f"generated {len(generated['samples'])} samples across "
        f"{len(generated['suites'])} suites"
    )
