"""Deterministic local sample generation.

The generated NPZ is deliberately self-contained: arrays plus a JSON metadata
blob containing sample rate, channel layout, analytic truth and injected fault
markers. No network or external service is involved.
"""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .canonical import canonical_json
from .errors import ErrorCode, WorkbenchError
from .models import ALGORITHM_VERSION, ChannelLayout


@dataclass(frozen=True)
class SampleBundle:
    directory: Path
    normal: Path
    missing_channel: Path
    time_reversed: Path
    gain_overflow: Path
    bad_frame: Path

    def as_dict(self) -> dict[str, str]:
        return {
            "normal": str(self.normal),
            "missing_channel": str(self.missing_channel),
            "time_reversed": str(self.time_reversed),
            "gain_overflow": str(self.gain_overflow),
            "bad_frame": str(self.bad_frame),
        }


def _atomic_write_npz(path: Path, arrays: dict[str, np.ndarray]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    handle = tempfile.NamedTemporaryFile(
        prefix=f".{path.name}.",
        suffix=".tmp",
        dir=path.parent,
        delete=False,
    )
    temp_path = Path(handle.name)
    try:
        with handle:
            np.savez_compressed(handle, **arrays)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
    except Exception:
        try:
            temp_path.unlink(missing_ok=True)
        except OSError:
            pass
        raise


def _synthesize_ir(
    *,
    sample_rate: int,
    length: int,
    channels: tuple[str, ...],
    seed: int,
) -> tuple[np.ndarray, dict[str, Any]]:
    rng = np.random.default_rng(seed)
    channel_count = len(channels)
    ir = np.zeros((channel_count, length), dtype=np.float64)
    direct_delays = np.array([3, 6, 10, 13, 16, 19], dtype=np.int64)[:channel_count]
    direct_amplitudes = np.array(
        [0.50, 0.45, 0.40, 0.36, 0.42, 0.40],
        dtype=np.float64,
    )[:channel_count]
    reflection_base = np.array([73, 149, 257, 389, 557], dtype=np.int64)
    reflection_gain_base = np.array([0.30, 0.20, 0.13, 0.08, 0.05], dtype=np.float64)
    reflection_delays: list[list[int]] = []
    reflection_gains: list[list[float]] = []
    energy_decay_coefficients: list[float] = []

    for channel_index in range(channel_count):
        delay = int(direct_delays[channel_index])
        amplitude = float(direct_amplitudes[channel_index])
        ir[channel_index, delay] += amplitude

        delays_for_channel: list[int] = []
        gains_for_channel: list[float] = []
        for reflection_index, base_delay in enumerate(reflection_base):
            delay_offset = 2 * channel_index + 9 * reflection_index
            delay_sample = delay + int(base_delay) + delay_offset
            if delay_sample >= length:
                continue
            polarity = -1.0 if reflection_index % 2 else 1.0
            gain = polarity * float(reflection_gain_base[reflection_index]) * (
                0.80 + 0.06 * channel_index
            )
            ir[channel_index, delay_sample] += gain
            delays_for_channel.append(delay_sample)
            gains_for_channel.append(gain)

        decay_samples = 650.0 + 85.0 * channel_index
        energy_decay_coefficient = 2.0 / decay_samples
        energy_decay_coefficients.append(float(energy_decay_coefficient))
        tail_start = min(length - 1, delay + 35 + 5 * channel_index)
        tail_positions = np.arange(tail_start, length, dtype=np.float64)
        envelope = np.exp(
            -0.5 * energy_decay_coefficient * (tail_positions - tail_start)
        )
        tail_noise = rng.standard_normal(tail_positions.size)
        tail = 0.018 * envelope * tail_noise
        ir[channel_index, tail_start:] += tail

        truth_frequency = 180.0 + 31.0 * channel_index
        hold = np.sin(
            2.0
            * np.pi
            * truth_frequency
            * np.arange(length, dtype=np.float64)
            / float(sample_rate)
        )
        ir[channel_index] += 0.000015 * hold * np.exp(
            -np.arange(length, dtype=np.float64) / float(length)
        )

        reflection_delays.append(delays_for_channel)
        reflection_gains.append(gains_for_channel)

    peak = float(np.max(np.abs(ir)))
    if peak > 0.75:
        ir *= 0.75 / peak

    truth = {
        "direct_delay_samples": [int(value) for value in direct_delays],
        "direct_delays_seconds": [
            float(value) / float(sample_rate) for value in direct_delays
        ],
        "direct_amplitudes": [float(value) for value in direct_amplitudes],
        "energy_decay_coeff": [float(value) for value in energy_decay_coefficients],
        "energy_decay_tau_samples": [
            float(1.0 / value) for value in energy_decay_coefficients
        ],
        "reflection_delays_samples": reflection_delays,
        "reflection_arrival_times_seconds": [
            [float(value) / float(sample_rate) for value in delays]
            for delays in reflection_delays
        ],
        "reflection_gains": reflection_gains,
        "truth_version": "analytic-v1",
        "generator_algorithm_version": ALGORITHM_VERSION,
    }
    return ir, truth


def _synthesize_dry(sample_rate: int, length: int, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed + 91_711)
    time = np.arange(length, dtype=np.float64) / float(sample_rate)
    envelope = np.minimum(1.0, time / 0.03) * np.minimum(
        1.0,
        np.maximum(0.0, (time[-1] - time) / 0.08),
    )
    signal = (
        0.075 * np.sin(2.0 * np.pi * 196.0 * time)
        + 0.048 * np.sin(2.0 * np.pi * 293.66 * time + 0.4)
        + 0.034 * np.sin(2.0 * np.pi * 440.0 * time + 1.1)
        + 0.013 * np.sin(2.0 * np.pi * 1_011.0 * time + 0.2)
    )
    chirp_phase = 2.0 * np.pi * (120.0 * time + 0.5 * 35.0 * time * time)
    signal += 0.025 * np.sin(chirp_phase)
    signal += 0.003 * rng.standard_normal(length)
    signal *= envelope
    peak = float(np.max(np.abs(signal)))
    if peak > 0:
        signal *= 0.16 / peak
    return signal.astype(np.float64)


def _metadata(
    *,
    sample_rate: int,
    layout: ChannelLayout,
    truth: dict[str, Any],
    sample_id: str,
    bad_frames: tuple[int, ...] = (),
    missing_channels: tuple[str, ...] = (),
) -> dict[str, Any]:
    return {
        "format": "spatial-sample-v1",
        "sample_id": sample_id,
        "sample_rate": sample_rate,
        "channel_layout": layout.to_dict(),
        "truth": truth,
        "bad_frames": list(bad_frames),
        "missing_channels": list(missing_channels),
        "generated_with": ALGORITHM_VERSION,
    }


def _write_sample(
    path: Path,
    *,
    ir: np.ndarray,
    dry: np.ndarray,
    metadata: dict[str, Any],
) -> None:
    _atomic_write_npz(
        path,
        {
            "ir": np.asarray(ir, dtype=np.float64),
            "dry": np.asarray(dry, dtype=np.float64),
            "metadata": np.array(canonical_json(metadata)),
        },
    )


def generate_sample_bundle(
    output_dir: str | Path,
    *,
    seed: int = 42_042,
    sample_rate: int = 16_000,
    length: int = 32_768,
    layout_name: str = "quad",
    overwrite: bool = True,
) -> SampleBundle:
    if sample_rate < 8_000:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "sample_rate must be >= 8000",
            stage="sample-generation",
        )
    if length < 2_048:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "sample length must be >= 2048",
            stage="sample-generation",
        )
    layout = ChannelLayout.standard(layout_name)
    if len(layout.channels) > 6:
        # The analytic generator has six independent spatial templates.
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "built-in sample generator supports up to six channels",
            stage="sample-generation",
        )

    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    ir, truth = _synthesize_ir(
        sample_rate=sample_rate,
        length=length,
        channels=layout.channels,
        seed=seed,
    )
    dry = _synthesize_dry(sample_rate, length, seed)
    bundle = SampleBundle(
        directory=output,
        normal=output / "spatial_normal.npz",
        missing_channel=output / "fault_missing_channel.npz",
        time_reversed=output / "fault_time_reversed.npz",
        gain_overflow=output / "fault_gain_overflow.npz",
        bad_frame=output / "fault_bad_frame.npz",
    )
    paths = {
        bundle.normal: _metadata(
            sample_rate=sample_rate,
            layout=layout,
            truth=truth,
            sample_id="normal",
        ),
        bundle.missing_channel: _metadata(
            sample_rate=sample_rate,
            layout=layout,
            truth=truth,
            sample_id="fault-missing-channel",
            missing_channels=(layout.channels[-1],),
        ),
        bundle.time_reversed: _metadata(
            sample_rate=sample_rate,
            layout=layout,
            truth=truth,
            sample_id="fault-time-reversed",
        ),
        bundle.gain_overflow: _metadata(
            sample_rate=sample_rate,
            layout=layout,
            truth=truth,
            sample_id="fault-gain-overflow",
        ),
        bundle.bad_frame: _metadata(
            sample_rate=sample_rate,
            layout=layout,
            truth=truth,
            sample_id="fault-bad-frame",
            bad_frames=(3,),
        ),
    }
    ir_for_path: dict[Path, np.ndarray] = {
        bundle.normal: ir,
        bundle.missing_channel: ir,
        bundle.time_reversed: np.ascontiguousarray(ir[:, ::-1]),
        bundle.gain_overflow: np.asarray(ir * 32.0, dtype=np.float64),
        bundle.bad_frame: ir,
    }
    for path, metadata in paths.items():
        if path.exists() and not overwrite:
            continue
        _write_sample(
            path,
            ir=ir_for_path[path],
            dry=dry,
            metadata=metadata,
        )
    return bundle


def write_corrupt_sample(path: str | Path) -> Path:
    """Create a deterministic undecodable sample for E_DECODE tests."""

    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_bytes(b"not-a-valid-npz\x00\x01\x02")
    return destination


def load_metadata_for_debug(path: str | Path) -> dict[str, Any]:
    """Read only the metadata blob; intended for diagnostics."""

    with np.load(Path(path), allow_pickle=False) as data:
        if "metadata" not in data:
            raise WorkbenchError(
                ErrorCode.E_META,
                "metadata field is missing",
                stage="load",
            )
        try:
            return json.loads(str(data["metadata"].item()))
        except (ValueError, TypeError) as exc:
            raise WorkbenchError(
                ErrorCode.E_META,
                "metadata field is not valid JSON",
                stage="load",
            ) from exc

