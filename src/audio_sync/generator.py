from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .markers import marker_half_support, marker_waveform
from .manifest import MarkerSpec


@dataclass(frozen=True)
class SyntheticSessionConfig:
    session_id: str = "session-clean-001"
    sample_rate: int = 48_000
    duration_seconds: float = 8.0
    channel_count: int = 4
    truth_segment_length: int = 64_000
    marker_spacing_seconds: float = 0.55
    first_marker_seconds: float = 0.35
    guard_samples: int = 96
    seed: int = 20261123
    background_level: float = 0.115
    noise_std: float = 2.0e-5


_CHANNEL_PROFILES: tuple[tuple[float, tuple[float, ...]], ...] = (
    (11.75, (23.0, -7.5, 14.0, -11.0, 6.5, -3.0)),
    (15.25, (-5.0, 18.0, -12.0, 9.0, -15.0, 5.5)),
    (19.50, (12.0, 4.0, -18.0, 16.0, -5.0, 9.0)),
)


def _json_dump(path: Path, payload: dict[str, Any]) -> None:
    text = json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=True) + "\n"
    path.write_text(text, encoding="utf-8")


def _pwl_delay(
    positions: np.ndarray | float,
    knots: np.ndarray,
    initial_offset: float,
    rates_ppm: tuple[float, ...] | np.ndarray,
) -> np.ndarray:
    t = np.asarray(positions, dtype=np.float64)
    delay = np.full_like(t, initial_offset, dtype=np.float64)
    rates = np.asarray(rates_ppm, dtype=np.float64) * 1.0e-6
    for index, rate in enumerate(rates):
        left = float(knots[index])
        right = float(knots[index + 1])
        contribution = np.clip(t, left, right) - left
        delay += float(rate) * contribution
    return delay


def _make_markers(config: SyntheticSessionConfig, session_samples: int) -> list[MarkerSpec]:
    count = int(
        math.floor(
            (config.duration_seconds - config.first_marker_seconds - 0.25)
            / config.marker_spacing_seconds
        )
    ) + 1
    markers: list[MarkerSpec] = []
    for index in range(count):
        seconds = config.first_marker_seconds + index * config.marker_spacing_seconds
        ref_sample = int(round(seconds * config.sample_rate))
        if ref_sample <= 256 or ref_sample >= session_samples - 256:
            continue
        kind = "pulse" if index % 2 == 0 else "chirp"
        if kind == "pulse":
            template: dict[str, Any] = {
                "sample_rate": config.sample_rate,
                "carrier_hz": 7_000.0,
                "sigma_samples": 10.5,
                "support_samples": 48.0,
            }
        else:
            template = {
                "sample_rate": config.sample_rate,
                "start_hz": 2_400.0,
                "end_hz": 10_500.0,
                "length_samples": 120.0,
            }
        markers.append(
            MarkerSpec(
                id=f"m{index:02d}",
                kind=kind,
                ref_sample=ref_sample,
                time_seconds=ref_sample / config.sample_rate,
                template=template,
            )
        )
    return markers


def _background(t: np.ndarray, config: SyntheticSessionConfig, phases: np.ndarray) -> np.ndarray:
    frequencies = (181.0, 293.0, 431.0, 677.0, 1013.0, 1451.0, 2111.0)
    weights = (1.0, 0.72, 0.58, 0.42, 0.31, 0.23, 0.16)
    signal = np.zeros_like(t, dtype=np.float64)
    for frequency, weight, phase in zip(frequencies, weights, phases):
        signal += weight * np.sin(2.0 * np.pi * frequency * t / config.sample_rate + phase)
    signal /= float(np.sum(weights))
    modulation = 1.0 + 0.22 * np.sin(2.0 * np.pi * 0.37 * t / config.sample_rate + phases[0])
    return config.background_level * modulation * signal


def _source_values(
    t: np.ndarray,
    markers: list[MarkerSpec],
    config: SyntheticSessionConfig,
    phases: np.ndarray,
) -> np.ndarray:
    signal = _background(t, config, phases)
    for marker in markers:
        half = marker_half_support(marker)
        mask = np.abs(t - marker.ref_sample) <= half + 2.0
        if not np.any(mask):
            continue
        waveform = marker_waveform(marker, t[mask] - marker.ref_sample)
        amplitude = 0.72 if marker.kind == "pulse" else 0.54
        signal[mask] += amplitude * waveform
    return signal


def _truth_marker_entries(
    marker_specs: list[MarkerSpec],
    delay_at_markers: np.ndarray,
    rates_ppm: tuple[float, ...],
    knots: np.ndarray,
    initial_offset: float,
    guard_before: int = 0,
) -> list[dict[str, Any]]:
    entries: list[dict[str, Any]] = []
    for marker, offset in zip(marker_specs, delay_at_markers):
        segment = int(np.searchsorted(knots[1:], marker.ref_sample, side="right"))
        segment = min(segment, len(rates_ppm) - 1)
        region = knots[segment] <= marker.ref_sample <= knots[segment + 1]
        rate = float(rates_ppm[segment]) if region else 0.0
        cumulative_drift = float(offset) - float(initial_offset)
        cumulative_rate_ppm = (
            cumulative_drift / float(marker.ref_sample) * 1.0e6 if marker.ref_sample > 0 else rate
        )
        entries.append(
            {
                "id": marker.id,
                "kind": marker.kind,
                "ref_sample": marker.ref_sample,
                "arrival_sample": float(marker.ref_sample) + float(offset),
                "file_arrival_sample": float(marker.ref_sample + guard_before) + float(offset),
                "offset_samples": float(offset),
                "cumulative_drift_samples": cumulative_drift,
                "cumulative_drift_rate_ppm": cumulative_rate_ppm,
                "rate_ppm": rate,
            }
        )
    return entries


def generate_session(
    output_dir: str | Path,
    *,
    config: SyntheticSessionConfig | None = None,
    overwrite: bool = True,
) -> dict[str, Any]:
    config = config or SyntheticSessionConfig()
    if config.sample_rate < 8_000:
        raise ValueError("sample_rate is too low for the deterministic marker set")
    if config.channel_count != 4:
        raise ValueError("this sample generator defines exactly one reference and three capture channels")

    output_dir = Path(output_dir)
    channel_dir = output_dir / "channels"
    if output_dir.exists() and any(output_dir.iterdir()) and not overwrite:
        raise FileExistsError(f"output directory is not empty: {output_dir}")
    channel_dir.mkdir(parents=True, exist_ok=True)

    session_samples = int(round(config.duration_seconds * config.sample_rate))
    markers = _make_markers(config, session_samples)
    segment_count = int(math.ceil(session_samples / config.truth_segment_length))
    knots = np.array(
        [min(index * config.truth_segment_length, session_samples) for index in range(segment_count + 1)],
        dtype=np.int64,
    )
    rng = np.random.default_rng(config.seed)
    phases = rng.uniform(0.0, 2.0 * np.pi, size=7)

    reference_id = "ref"
    capture_ids = ["capture_a", "capture_b", "capture_c"]
    channel_ids = [reference_id, *capture_ids]
    arrays: dict[str, np.ndarray] = {}
    truth_channels: dict[str, Any] = {}

    all_marker_samples = np.array([marker.ref_sample for marker in markers], dtype=np.float64)
    source_signature = hashlib.sha256(
        json.dumps(
            {
                "seed": config.seed,
                "sample_rate": config.sample_rate,
                "duration_seconds": config.duration_seconds,
                "phases": phases.tolist(),
                "markers": [marker.id for marker in markers],
            },
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    ).hexdigest()

    reference_positions = np.arange(session_samples, dtype=np.float64)
    reference_signal = _source_values(reference_positions, markers, config, phases)
    reference_noise = rng.normal(0.0, config.noise_std, size=session_samples)
    arrays[reference_id] = np.asarray(reference_signal + reference_noise, dtype=np.float32)
    truth_channels[reference_id] = {
        "initial_offset_samples": 0.0,
        "rates_ppm": [0.0] * segment_count,
        "knots_ref_samples": knots.tolist(),
        "markers": [
            {
                "id": marker.id,
                "kind": marker.kind,
                "ref_sample": marker.ref_sample,
                "arrival_sample": float(marker.ref_sample),
                "file_arrival_sample": float(marker.ref_sample),
                "offset_samples": 0.0,
                "cumulative_drift_samples": 0.0,
                "cumulative_drift_rate_ppm": 0.0,
                "rate_ppm": 0.0,
            }
            for marker in markers
        ],
    }

    for channel_index, channel_id in enumerate(capture_ids):
        base_profile = _CHANNEL_PROFILES[channel_index]
        initial_offset = base_profile[0] + 0.125 * (config.seed % 7)
        base_rates = base_profile[1]
        if len(base_rates) != segment_count:
            rates = tuple(
                float(base_rates[index % len(base_rates)]) * (1.0 if index % 2 == 0 else 0.85)
                for index in range(segment_count)
            )
        else:
            rates = base_rates
        local_time = np.arange(
            -config.guard_samples,
            session_samples + config.guard_samples,
            dtype=np.float64,
        )
        delay = _pwl_delay(local_time, knots, initial_offset, rates)
        source_time = local_time - delay
        sampled = _source_values(source_time, markers, config, phases)
        channel_rng = np.random.default_rng(config.seed + 1009 * (channel_index + 1))
        sampled += channel_rng.normal(0.0, config.noise_std, size=sampled.shape[0])
        arrays[channel_id] = np.asarray(sampled, dtype=np.float32)

        marker_offsets = _pwl_delay(all_marker_samples, knots, initial_offset, rates)
        truth_channels[channel_id] = {
            "initial_offset_samples": initial_offset,
            "rates_ppm": [float(rate) for rate in rates],
            "knots_ref_samples": knots.tolist(),
            "delay_curve_parameters": {
                "type": "piecewise_linear",
                "initial_phase_samples": initial_offset,
                "rates_ppm": [float(rate) for rate in rates],
            },
            "markers": _truth_marker_entries(
                markers,
                marker_offsets,
                tuple(rates),
                knots,
                initial_offset,
                config.guard_samples,
            ),
        }

    channel_specs: list[dict[str, Any]] = []
    for channel_id in channel_ids:
        relative = f"channels/{channel_id}.npy"
        path = output_dir / relative
        data = np.ascontiguousarray(arrays[channel_id], dtype=np.float32)
        np.save(path, data)
        channel_specs.append(
            {
                "id": channel_id,
                "file": relative,
                "samples": int(data.shape[0]),
                "sample_rate": config.sample_rate,
                "dtype": "float32",
                "content_sha256": hashlib.sha256(data.tobytes()).hexdigest(),
                "guard_before": 0 if channel_id == reference_id else config.guard_samples,
                "guard_after": 0 if channel_id == reference_id else config.guard_samples,
            }
        )

    truth = {
        "schema_version": 1,
        "session_id": config.session_id,
        "reference_channel": reference_id,
        "sample_rate": config.sample_rate,
        "session_samples": session_samples,
        "source_fingerprint": source_signature,
        "channels": truth_channels,
        "notice": "Analytic truth is for observation and acceptance only; estimators must not read it.",
    }
    truth_path = output_dir / "truth.json"
    _json_dump(truth_path, truth)
    truth_hash = hashlib.sha256(truth_path.read_bytes()).hexdigest()

    manifest = {
        "schema_version": 1,
        "session_id": config.session_id,
        "nominal_sample_rate": config.sample_rate,
        "channel_count": config.channel_count,
        "reference_channel": reference_id,
        "session_samples": session_samples,
        "duration_seconds": config.duration_seconds,
        "channels": channel_specs,
        "markers": [
            {
                "id": marker.id,
                "kind": marker.kind,
                "ref_sample": marker.ref_sample,
                "time_seconds": marker.ref_sample / config.sample_rate,
                "template": marker.template,
            }
            for marker in markers
        ],
        "truth": {
            "file": "truth.json",
            "sha256": truth_hash,
        },
        "layout": {
            "name": "quad",
            "channels": channel_ids,
            "speakers": ["FL", "FR", "SL", "SR"],
        },
        "generator": {
            "name": "deterministic-multichannel-v1",
            "seed": config.seed,
            "guard_samples": config.guard_samples,
            "truth_segment_length": config.truth_segment_length,
        },
    }
    _json_dump(output_dir / "manifest.json", manifest)
    return {
        "session_dir": str(output_dir),
        "manifest": str(output_dir / "manifest.json"),
        "truth": str(truth_path),
        "channels": len(channel_specs),
        "markers": len(markers),
        "session_samples": session_samples,
        "truth_sha256": truth_hash,
    }


def generate_demo_suite(
    root: str | Path,
    *,
    seeds: Iterable[int] = (20261123, 20261124, 20261125),
) -> list[dict[str, Any]]:
    root = Path(root)
    results = []
    for index, seed in enumerate(seeds):
        config = SyntheticSessionConfig(
            session_id=f"session-{index + 1:02d}-seed{seed}",
            seed=seed,
            duration_seconds=8.0 + 0.5 * index,
            truth_segment_length=64_000,
        )
        results.append(generate_session(root / f"session_{index + 1:02d}", config=config))
    return results

