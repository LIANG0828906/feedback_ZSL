from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

import numpy as np

from .manifest import SCENE_NAMES, TRACK_NAMES


def _event_times(
    seed: int,
    duration_seconds: float,
    count: int,
    *,
    jitter_fraction: float = 0.0,
    channel_index: int = 0,
) -> list[float]:
    count = max(0, int(count))
    if count == 0:
        return []
    margin = min(0.12, max(duration_seconds * 0.02, 0.002))
    usable_start = margin
    usable_end = duration_seconds - margin
    if usable_end <= usable_start:
        usable_start = duration_seconds * 0.1
        usable_end = duration_seconds * 0.9
    spacing = (usable_end - usable_start) / count
    rng = np.random.Generator(np.random.PCG64(seed * 1_000_003 + channel_index * 97))
    values: list[float] = []
    for index in range(count):
        center = usable_start + (index + 0.5) * spacing
        jitter = rng.uniform(-1.0, 1.0) * spacing * jitter_fraction
        value = min(usable_end, max(usable_start, center + jitter))
        if values and value <= values[-1] + 1e-6:
            value = values[-1] + 1e-5
        values.append(round(value, 6))
    while values and values[-1] > duration_seconds:
        values[-1] = round(duration_seconds - 1e-5, 6)
    return values


def _track(
    *,
    name: str,
    scene_name: str,
    path: str,
    weights: tuple[float, float],
    mix_weight: float,
    times: list[float],
    synthesis: dict[str, Any],
    pan_depth: float,
    pan_cycles: float,
    pan_phase: float,
) -> dict[str, Any]:
    return {
        "name": name,
        "scene_name": scene_name,
        "path": path,
        "channels": 1,
        "sample_rate": 48_000,
        "bit_depth": 32,
        "surround_weights": [weights[0], weights[1]],
        "mix_weight": mix_weight,
        "transient_reference_times": times,
        "pan_motion": {
            "depth_radians": pan_depth,
            "cycles": pan_cycles,
            "phase": pan_phase,
        },
        "synthesis": synthesis,
    }


def build_demo_manifest(
    seed: int = 249,
    duration_seconds: float = 600.0,
    *,
    block_frames: int = 48_000,
) -> dict[str, Any]:
    duration = float(duration_seconds)
    if duration <= 0.0:
        raise ValueError("duration must be positive")

    drops = _event_times(seed, duration, int(duration * 2.5), jitter_fraction=0.38, channel_index=1)
    insects = _event_times(seed, duration, int(duration * 2.2), jitter_fraction=0.025, channel_index=2)
    birds = _event_times(seed, duration, max(2, int(duration * 0.12)), jitter_fraction=0.30, channel_index=3)
    thunder = _event_times(seed, duration, max(1, int(duration / 55.0)), jitter_fraction=0.20, channel_index=4)
    footsteps = _event_times(
        seed, duration, max(1, int(duration * 0.22)), jitter_fraction=0.28, channel_index=5
    )
    masking_windows = [
        [round(max(0.0, seconds - 0.06), 6), round(min(duration, seconds + 0.28), 6)]
        for seconds in birds
    ]

    tracks = [
        _track(
            name="rain_drops",
            scene_name="雨滴",
            path="stems/01_rain_drops.wav",
            weights=(0.82, 0.56),
            mix_weight=0.19,
            times=drops,
            synthesis={
                "kind": "rain_drops",
                "seed_offset": 101,
                "peak": 0.82,
                "base_gain": 0.52,
                "tone_hz": 3900.0,
            },
            pan_depth=0.16,
            pan_cycles=1.0,
            pan_phase=0.37,
        ),
        _track(
            name="rain_curtain",
            scene_name="雨幕",
            path="stems/02_rain_curtain.wav",
            weights=(0.78, 0.68),
            mix_weight=0.25,
            times=[],
            synthesis={
                "kind": "rain_curtain",
                "seed_offset": 202,
                "peak": 0.78,
                "low_hz": 45.0,
                "high_hz": 15_500.0,
            },
            pan_depth=0.10,
            pan_cycles=0.31,
            pan_phase=1.10,
        ),
        _track(
            name="stream",
            scene_name="溪流",
            path="stems/03_stream.wav",
            weights=(0.58, 0.84),
            mix_weight=0.18,
            times=[],
            synthesis={
                "kind": "stream",
                "seed_offset": 303,
                "peak": 0.72,
                "low_hz": 420.0,
                "high_hz": 1750.0,
                "modulation_hz": 0.071,
                "modulation_depth": 0.23,
            },
            pan_depth=0.22,
            pan_cycles=0.43,
            pan_phase=2.12,
        ),
        _track(
            name="insects",
            scene_name="虫鸣",
            path="stems/04_insects.wav",
            weights=(0.89, 0.48),
            mix_weight=0.16,
            times=insects,
            synthesis={
                "kind": "insects",
                "seed_offset": 404,
                "peak": 0.76,
                "base_gain": 0.29,
                "carrier_hz": 4350.0,
            },
            pan_depth=0.12,
            pan_cycles=0.57,
            pan_phase=0.71,
        ),
        _track(
            name="birds",
            scene_name="鸟鸣",
            path="stems/05_birds.wav",
            weights=(0.42, 0.91),
            mix_weight=0.31,
            times=birds,
            synthesis={
                "kind": "birds",
                "seed_offset": 505,
                "peak": 0.88,
                "base_gain": 0.48,
                "base_hz": 2450.0,
            },
            pan_depth=0.13,
            pan_cycles=0.71,
            pan_phase=2.66,
        ),
        _track(
            name="thunder",
            scene_name="雷声",
            path="stems/06_thunder.wav",
            weights=(0.71, 0.69),
            mix_weight=0.17,
            times=thunder,
            synthesis={
                "kind": "thunder",
                "seed_offset": 606,
                "peak": 0.93,
                "base_gain": 0.58,
                "rumble_hz": 43.0,
            },
            pan_depth=0.08,
            pan_cycles=0.19,
            pan_phase=1.83,
        ),
        _track(
            name="footsteps",
            scene_name="脚步",
            path="stems/07_footsteps.wav",
            weights=(0.64, 0.77),
            mix_weight=0.18,
            times=footsteps,
            synthesis={
                "kind": "footsteps",
                "seed_offset": 707,
                "peak": 0.85,
                "base_gain": 0.40,
                "pair_seconds": 0.16,
            },
            pan_depth=0.20,
            pan_cycles=0.89,
            pan_phase=0.29,
        ),
    ]

    assert [track["scene_name"] for track in tracks] == list(SCENE_NAMES)
    assert [track["name"] for track in tracks] == list(TRACK_NAMES)

    return {
        "schema_version": 1,
        "name": "rainforest_loop_master",
        "seed": int(seed),
        "sample_rate": 48_000,
        "duration_seconds": duration,
        "output_bit_depth": 32,
        "loop": {
            "crossfade_seconds": min(2.0, max(0.08, duration * 0.02)),
            "max_seam_sample_delta": 0.08,
            "seam_energy_length_seconds": 0.02,
            "endpoint_seal_seconds": 0.02,
        },
        "panning": {
            "model": "equal_power_slow_lfo",
            "stability_threshold_db": 0.01,
        },
        "masking": {
            "reference_scene": "雨幕",
            "target_scenes": ["鸟鸣"],
            "window_before_seconds": 0.06,
            "window_after_seconds": 0.28,
            "attack_seconds": 0.01,
            "release_seconds": 0.03,
            "residual_energy_ratio": 0.15,
            "required_snr_improvement_db": 6.0,
            "detection_min_transient_dbfs": -55.0,
            "windows": masking_windows,
        },
        "mono": {
            "fold_weights": [0.5, 0.5],
            "mid_band_hz": [300.0, 5000.0],
            "energy_ratio_tolerance_db": 1.0,
        },
        "loudness": {
            "target_lufs": -18.0,
            "max_abs_deviation_lu": 0.5,
            "true_peak_limit_dbtp": -1.0,
        },
        "rendering": {
            "block_frames": int(block_frames),
            "snapshot_every_blocks": 1,
        },
        "tracks": tracks,
    }


def write_demo_manifest(path: str | Path, manifest: dict[str, Any]) -> Path:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    temp = output.with_suffix(output.suffix + ".tmp")
    temp.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=False) + "\n",
        encoding="utf-8",
    )
    temp.replace(output)
    return output


def demo_manifest(
    output_dir: str | Path,
    *,
    seed: int = 249,
    duration_seconds: float = 600.0,
    block_frames: int = 48_000,
) -> Path:
    output = Path(output_dir).resolve()
    output.mkdir(parents=True, exist_ok=True)
    (output / "stems").mkdir(exist_ok=True)
    manifest = build_demo_manifest(seed=seed, duration_seconds=duration_seconds, block_frames=block_frames)
    return write_demo_manifest(output / "manifest.json", manifest)

