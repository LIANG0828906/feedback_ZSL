"""Deterministic local sample-workspace generator."""

from __future__ import annotations

import math
import shutil
from pathlib import Path
from typing import Any

import numpy as np

from .config import AnalysisConfig
from .dsp import BandSpec, shift_by_fractional_samples, synthesize_band_response
from .errors import E_CONFIG_INVALID, E_GEOMETRY_INVALID, VenueError
from .utils import atomic_write_json, canonical_json
from .wavio import LAYOUT_NAMES, write_wav_channels

GENERATOR_VERSION = 1
CHANNELS = ("FL", "FR", "FC", "LFE", "SL", "SR")


def _default_geometry() -> dict[str, Any]:
    speakers = {
        "FL": [-3.6, 3.2, 3.0],
        "FR": [3.6, 3.2, 3.0],
        "FC": [0.0, 4.0, 3.1],
        "LFE": [0.0, 2.4, 0.7],
        "SL": [-5.3, 0.8, 2.2],
        "SR": [5.3, 0.8, 2.2],
    }
    pickups = {
        "FL": [-3.48, 3.1, 3.08],
        "FR": [3.48, 3.1, 3.08],
        "FC": [0.08, 3.92, 3.18],
        "LFE": [0.12, 2.3, 0.78],
        "SL": [-5.18, 0.9, 2.27],
        "SR": [5.18, 0.9, 2.27],
    }
    return {
        "coordinate_system": "cartesian_m",
        "speed_of_sound_m_s": 343.0,
        "speakers": {key: {"position": value} for key, value in speakers.items()},
        "pickups": {key: {"position": value} for key, value in pickups.items()},
    }


def _default_config() -> dict[str, Any]:
    return {
        "schema_version": 1,
        "estimator_version": 1,
        "sample_rate": 48_000,
        "bands": [
            {"name": "sub", "low_hz": 40.0, "high_hz": 120.0},
            {"name": "low", "low_hz": 120.0, "high_hz": 350.0},
            {"name": "mid", "low_hz": 350.0, "high_hz": 1_200.0},
            {"name": "high", "low_hz": 1_200.0, "high_hz": 5_000.0},
            {"name": "ultra", "low_hz": 5_000.0, "high_hz": 14_000.0},
        ],
        "filter": {"transition_hz": 35.0},
        "target_suppression_db": 18.0,
        "max_cancel_gain": 4.0,
        "max_primary_suppression_db": 0.5,
        "primary_leakage": 0.005,
        "polarity": {
            "inversion_tolerance_deg": 30.0,
            "jump_threshold_deg": 75.0,
            "wrap_threshold_deg": 240.0,
            "low_band_max_hz": 500.0,
            "group_delay_jump_ms": 1.5,
        },
    }


def _reference_signal(
    sample_rate: int,
    frame_count: int,
    seed: int,
    channel_index: int,
) -> np.ndarray:
    rng = np.random.default_rng(seed + 1009 * (channel_index + 1))
    noise = rng.standard_normal(frame_count)
    spectrum = np.fft.rfft(noise)
    frequencies = np.fft.rfftfreq(frame_count, d=1.0 / sample_rate)
    mask = np.zeros_like(frequencies)
    mask[(frequencies >= 32.0) & (frequencies <= 15_500.0)] = 1.0
    fade = 300.0
    rise = (frequencies > 32.0) & (frequencies < 32.0 + fade)
    mask[rise] *= 0.5 - 0.5 * np.cos(
        np.pi * (frequencies[rise] - 32.0) / fade
    )
    fall = (frequencies >= 15_500.0 - fade) & (frequencies < 15_500.0)
    mask[fall] *= 0.5 + 0.5 * np.cos(
        np.pi * (frequencies[fall] - (15_500.0 - fade)) / fade
    )
    signal = np.fft.irfft(spectrum * mask, n=frame_count)
    signal /= max(float(np.max(np.abs(signal))), 1e-12)
    signal *= 0.46
    edge = min(int(0.02 * sample_rate), frame_count // 4)
    if edge > 1:
        envelope = np.ones(frame_count)
        ramp = 0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, edge))
        envelope[:edge] = ramp
        envelope[-edge:] = ramp[::-1]
        signal *= envelope
    return signal


def _band_gains(pair_index: int) -> list[float]:
    base = [0.16, 0.095, 0.052, 0.026, 0.014]
    factor = 0.82 + 0.037 * (pair_index % 11)
    return [value * factor for value in base]


def _phase_profile(pair_index: int, scenario: str) -> list[float]:
    pair_offset = math.radians((pair_index % 5) * 0.7 - 1.4)
    base = [
        0.01 + pair_offset,
        0.02 + pair_offset,
        -0.01 + pair_offset,
        0.015 + pair_offset,
        -0.012 + pair_offset,
    ]
    if scenario == "true_inversion":
        return [
            math.pi - 0.05,
            -math.pi + 0.03,
            math.pi - 0.02,
            -math.pi + 0.04,
            math.pi - 0.03,
        ]
    if scenario == "boundary_wrap":
        return [0.10, 0.75, 1.88, math.radians(173.0), math.radians(-171.0)]
    if scenario == "partial_phase_flip":
        return [0.10, 0.18, math.pi - 0.08, 0.24, 0.16]
    if scenario == "low_group_delay":
        return [0.0, math.radians(105.0), 0.08, 0.12, 0.07]
    if scenario == "orthogonal":
        return [math.radians(82.0), math.radians(86.0), math.radians(90.0),
                math.radians(94.0), math.radians(98.0)]
    return base


def _scenario_for_pair(source: str, pickup: str) -> str:
    mapping = {
        ("FL", "FR"): "true_inversion",
        ("FL", "FC"): "boundary_wrap",
        ("FR", "FL"): "partial_phase_flip",
        ("LFE", "SL"): "low_group_delay",
        ("SL", "SR"): "orthogonal",
    }
    return mapping.get((source, pickup), "normal")


def _distance(
    source: str,
    pickup: str,
    geometry: dict[str, Any],
) -> tuple[float, float, int]:
    speaker = np.asarray(geometry["speakers"][source]["position"], dtype=float)
    listener = np.asarray(geometry["pickups"][pickup]["position"], dtype=float)
    distance = float(np.linalg.norm(speaker - listener))
    speed = float(geometry["speed_of_sound_m_s"])
    if distance < 0 or speed <= 0:
        raise VenueError(
            E_GEOMETRY_INVALID,
            "invalid generated geometry",
            {"source": source, "pickup": pickup},
        )
    seconds = distance / speed
    return distance, seconds, int(round(seconds * 48_000))


def generate_sample_workspace(
    output: str | Path,
    *,
    seed: int = 20_261_122,
    duration_s: float = 0.5,
    sample_rate: int = 48_000,
    overwrite: bool = False,
) -> dict[str, Any]:
    """Generate a complete deterministic workspace and return ground truth."""

    if sample_rate != 48_000:
        raise VenueError(
            E_CONFIG_INVALID,
            "sample generator target format is fixed at 48000 Hz",
            {"sample_rate": sample_rate},
        )
    if duration_s <= 0.05:
        raise VenueError(E_CONFIG_INVALID, "duration_s must be greater than 0.05")
    root = Path(output)
    if root.exists() and any(root.iterdir()):
        if not overwrite:
            raise FileExistsError(f"output directory is not empty: {root}")
        shutil.rmtree(root)
    root.mkdir(parents=True, exist_ok=True)
    pairs_dir = root / "pairs"
    references_dir = root / "references"
    pairs_dir.mkdir()
    references_dir.mkdir()

    geometry = _default_geometry()
    config_data = _default_config()
    config_data["sample_rate"] = sample_rate
    frame_count = int(round(duration_s * sample_rate))
    references = {
        channel: _reference_signal(sample_rate, frame_count, seed, index)
        for index, channel in enumerate(CHANNELS)
    }
    for channel in CHANNELS:
        write_wav_channels(
            references_dir / f"test_{channel}.wav",
            references[channel],
            sample_rate,
        )

    config = AnalysisConfig(
        bands=tuple(
            BandSpec(index, item["name"], item["low_hz"], item["high_hz"])
            for index, item in enumerate(config_data["bands"])
        ),
        sample_rate=sample_rate,
        transition_hz=float(config_data["filter"]["transition_hz"]),
        target_suppression_db=float(config_data["target_suppression_db"]),
        max_cancel_gain=float(config_data["max_cancel_gain"]),
        max_primary_suppression_db=float(
            config_data["max_primary_suppression_db"]
        ),
        primary_leakage=float(config_data["primary_leakage"]),
    )
    pair_names = [
        (source, pickup)
        for source in CHANNELS
        for pickup in CHANNELS
        if source != pickup
    ]
    pair_names.sort()
    pair_entries: list[dict[str, Any]] = []
    truths: list[dict[str, Any]] = []
    layout_names = LAYOUT_NAMES["5.1"]
    for pair_index, (source, pickup) in enumerate(pair_names):
        distance, delay_seconds, delay_samples = _distance(source, pickup, geometry)
        scenario = _scenario_for_pair(source, pickup)
        gains = _band_gains(pair_index)
        phases = _phase_profile(pair_index, scenario)
        capture = synthesize_band_response(
            references[source],
            sample_rate,
            list(config.bands),
            gains,
            phases,
            config.transition_hz,
        )
        capture = shift_by_fractional_samples(capture, delay_samples)
        rng = np.random.default_rng(seed + 7919 * (pair_index + 1))
        capture += rng.uniform(-0.5, 0.5, size=capture.size) / 32768.0
        capture += rng.normal(0.0, 0.000018, size=capture.size)
        pair_id = f"{source}>{pickup}"
        use_multichannel = pair_index % 2 == 0
        if use_multichannel:
            layout = "5.1"
            channels = np.zeros((frame_count, len(layout_names)), dtype=np.float64)
            channels[:, layout_names.index(source)] = references[source]
            channels[:, layout_names.index(pickup)] = capture
            wav_path = pairs_dir / f"{source}__to__{pickup}.wav"
            write_wav_channels(wav_path, channels, sample_rate)
            entry = {
                "id": pair_id,
                "source": source,
                "pickup": pickup,
                "wav": str(wav_path.relative_to(root)),
                "layout": layout,
                "source_channel_index": layout_names.index(source),
                "pickup_channel_index": layout_names.index(pickup),
                "reference": None,
                "reference_channel_index": 0,
                "distance_m": None,
            }
        else:
            layout = "mono"
            wav_path = pairs_dir / f"{source}__to__{pickup}.wav"
            write_wav_channels(wav_path, capture, sample_rate)
            entry = {
                "id": pair_id,
                "source": source,
                "pickup": pickup,
                "wav": str(wav_path.relative_to(root)),
                "layout": layout,
                "source_channel_index": None,
                "pickup_channel_index": 0,
                "reference": f"references/test_{source}.wav",
                "reference_channel_index": 0,
                "distance_m": None,
            }
        if scenario == "orthogonal":
            entry["primary_leakage"] = 0.12
        pair_entries.append(entry)
        truths.append(
            {
                "pair": pair_id,
                "source": source,
                "pickup": pickup,
                "scenario": scenario,
                "layout": layout,
                "distance_m": distance,
                "delay_seconds": delay_seconds,
                "delay_samples": delay_samples,
                "band_gains": {
                    band.name: value
                    for band, value in zip(config.bands, gains, strict=True)
                },
                "band_phases_deg": {
                    band.name: math.degrees(value)
                    for band, value in zip(config.bands, phases, strict=True)
                },
            }
        )

    manifest = {
        "schema_version": 1,
        "sample_rate": sample_rate,
        "config_file": "crossover.json",
        "geometry_file": "geometry.json",
        "generator": {
            "name": "venue_crosstalk.samplegen",
            "version": GENERATOR_VERSION,
            "seed": seed,
            "duration_s": duration_s,
            "frame_count": frame_count,
        },
        "pairs": pair_entries,
    }
    ground_truth = {
        "schema_version": 1,
        "seed": seed,
        "sample_rate": sample_rate,
        "duration_s": duration_s,
        "frame_count": frame_count,
        "pairs": truths,
    }
    atomic_write_json(root / "manifest.json", manifest)
    atomic_write_json(root / "geometry.json", geometry)
    atomic_write_json(root / "crossover.json", config_data)
    atomic_write_json(root / "ground_truth.json", ground_truth)
    return ground_truth


def generator_manifest_bytes(
    *,
    seed: int = 20_261_122,
    duration_s: float = 0.5,
) -> bytes:
    """Small helper for tests that need canonical generator metadata."""

    return canonical_json(
        {
            "seed": seed,
            "duration_s": duration_s,
            "generator_version": GENERATOR_VERSION,
        }
    ).encode("utf-8")
