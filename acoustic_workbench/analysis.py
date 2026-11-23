"""Deterministic per-channel, per-frame acoustic estimation."""

from __future__ import annotations

import math
from typing import Any, Callable, Iterable

import numpy as np

from .codes import E_FRAME, E_META, E_PARA
from .config import ALGORITHM_VERSION, AnalysisConfig, frame_count, validate_analysis_config
from .errors import error
from .io import validate_sample
from .models import FrameEstimate, Sample


def _safe_energy_db(numerator: float, denominator: float) -> float:
    return float(10.0 * math.log10(max(numerator, 1e-18) / max(denominator, 1e-18)))


def _parabolic_offset(signal_values: np.ndarray, index: int) -> float:
    if index <= 0 or index >= signal_values.size - 1:
        return 0.0
    left = float(signal_values[index - 1])
    center = float(signal_values[index])
    right = float(signal_values[index + 1])
    denominator = left - 2.0 * center + right
    if abs(denominator) < 1e-20:
        return 0.0
    offset = 0.5 * (left - right) / denominator
    if not math.isfinite(offset):
        return 0.0
    return float(np.clip(offset, -0.5, 0.5))


class FrameAnalyzer:
    def __init__(self, algorithm_version: str = ALGORITHM_VERSION) -> None:
        self.algorithm_version = algorithm_version

    def frame_starts(self, sample: Sample, config: AnalysisConfig) -> list[int]:
        count = frame_count(sample.ir.shape[1], config.frame_length, config.hop_length)
        return [index * config.hop_length for index in range(count)]

    def analyze(
        self,
        sample: Sample,
        config: AnalysisConfig,
        *,
        progress: Callable[[int, int, FrameEstimate | None], None] | None = None,
        frame_indices: Iterable[int] | None = None,
    ) -> list[FrameEstimate]:
        validate_analysis_config(config)
        validate_sample(sample)
        total = frame_count(sample.ir.shape[1], config.frame_length, config.hop_length)
        indices = range(total) if frame_indices is None else frame_indices
        results: list[FrameEstimate] = []
        for index in indices:
            if index < 0 or index >= total:
                raise error(
                    E_PARA,
                    f"frame index {index} is outside [0, {total})",
                    stage="analysis.frame_range",
                    field="frame_index",
                    frame_index=index,
                )
            estimate = self.estimate_frame(sample, config, index)
            results.append(estimate)
            if progress:
                progress(index, total, estimate)
        return results

    def estimate_frame(
        self,
        sample: Sample,
        config: AnalysisConfig,
        frame_index: int,
        *,
        resolution: dict[str, Any] | None = None,
    ) -> FrameEstimate:
        validate_analysis_config(config)
        validate_sample(sample)
        if sample.sample_rate is None or sample.layout is None:
            raise error(
                E_META,
                "sample metadata is incomplete",
                stage="analysis.frame.metadata",
                frame_index=frame_index,
            )
        if frame_index in sample.injection.bad_frames:
            raise error(
                E_FRAME,
                "injected bad frame cannot be decoded",
                stage="analysis.frame.decode",
                frame_index=frame_index,
                channel=sample.layout.channel_ids[0] if sample.layout.channels else None,
                context={"bad_frames": list(sample.injection.bad_frames)},
            )

        sample_rate = sample.sample_rate
        frame_length = config.frame_length
        frame_start = frame_index * config.hop_length
        frame_end = frame_start + frame_length
        available_end = min(frame_end, sample.ir.shape[1])
        segment = np.zeros((sample.ir.shape[0], frame_length), dtype=np.float64)
        if frame_start < sample.ir.shape[1]:
            segment[:, : available_end - frame_start] = sample.ir[
                :, frame_start:available_end
            ]

        channel_ids = sample.layout.channel_ids
        direct_delays: list[float] = []
        delay_in_frame: list[float] = []
        direct_energies: list[float] = []
        early_energies: list[float] = []
        reverberant_energies: list[float] = []
        drr_values: list[float] = []

        direct_half_width = max(1, int(round(0.00025 * sample_rate)))
        early_start_offset = max(2, int(round(0.0015 * sample_rate)))
        early_window = max(
            early_start_offset + 2,
            min(frame_length, int(round(config.early_window_ms * sample_rate / 1000.0))),
        )

        for channel_index in range(segment.shape[0]):
            channel_signal = segment[channel_index]
            squared = channel_signal * channel_signal
            absolute = np.abs(channel_signal)
            # Peak search spans the frame so late-tail frames remain stable when
            # preview mode compresses its refinement support. The estimation
            # window is then used to bound the local parabolic refinement.
            if absolute.size == 0:
                peak_index = 0
            else:
                peak_index = int(np.argmax(absolute))
            refine_radius = max(1, config.estimation_window // 2)
            refine_start = max(0, peak_index - refine_radius)
            refine_end = min(absolute.size, peak_index + refine_radius + 1)
            if refine_end - refine_start > 1:
                peak_index = refine_start + int(np.argmax(absolute[refine_start:refine_end]))
            peak_index = min(peak_index, channel_signal.size - 1)
            fractional = _parabolic_offset(squared, peak_index)
            local_delay = float(peak_index) + fractional
            direct_delays.append(float(frame_start) + local_delay)
            delay_in_frame.append(local_delay)

            direct_start = max(0, peak_index - direct_half_width)
            direct_end = min(frame_length, peak_index + direct_half_width + 1)
            direct_energy = float(np.sum(squared[direct_start:direct_end]))

            early_start = min(frame_length, peak_index + early_start_offset)
            early_end = min(frame_length, peak_index + early_window)
            early_energy = float(np.sum(squared[early_start:early_end]))

            total_energy = float(np.sum(squared))
            reverberant_energy = max(0.0, total_energy - direct_energy)
            direct_energies.append(direct_energy)
            early_energies.append(early_energy)
            reverberant_energies.append(reverberant_energy)
            drr_values.append(_safe_energy_db(direct_energy, reverberant_energy))

        reference_delay = direct_delays[0] if direct_delays else 0.0
        itd_reference = tuple(delay - reference_delay for delay in direct_delays)
        direction_deg, width_deg, confidence = _direction_clue(
            sample, direct_energies, direct_delays
        )
        resolution_record = realization_record(config, resolution)
        return FrameEstimate(
            frame_index=frame_index,
            frame_start=frame_start,
            frame_end=frame_end,
            channel_ids=channel_ids,
            delay_samples=tuple(direct_delays),
            delay_in_frame=tuple(delay_in_frame),
            itd_ref_samples=itd_reference,
            direct_energy=tuple(direct_energies),
            early_energy=tuple(early_energies),
            reverberant_energy=tuple(reverberant_energies),
            drr_db=tuple(drr_values),
            direction_deg=direction_deg,
            direction_width_deg=width_deg,
            confidence=confidence,
            algorithm_version=self.algorithm_version,
            resolution=resolution_record,
        )


def realization_record(
    config: AnalysisConfig,
    resolution: dict[str, Any] | None,
) -> dict[str, Any]:
    record = {
        "mode": config.mode,
        "frame_length": config.frame_length,
        "hop_length": config.hop_length,
        "estimation_window": config.estimation_window,
        "early_window_ms": config.early_window_ms,
        "overlap_ratio": 1.0 - config.hop_length / config.frame_length,
        "algorithm_version": ALGORITHM_VERSION,
    }
    if resolution:
        record["decision"] = dict(resolution)
    return record


def _direction_clue(
    sample: Sample,
    direct_energies: list[float],
    direct_delays: list[float],
) -> tuple[float, float, float]:
    if sample.layout is None or not sample.layout.channels:
        return 0.0, 0.0, 0.0
    x_sum = 0.0
    y_sum = 0.0
    weight_sum = 0.0
    delays = np.asarray(direct_delays, dtype=np.float64)
    delays = delays - np.min(delays) if delays.size else delays
    delay_spread = float(np.max(delays)) if delays.size else 1.0
    for index, channel in enumerate(sample.layout.channels):
        energy = max(0.0, direct_energies[index])
        weight = math.sqrt(energy)
        if delay_spread > 0.0 and index < delays.size:
            weight *= 1.0 + 0.15 * (1.0 - float(delays[index]) / delay_spread)
        angle = math.radians(channel.azimuth_deg)
        x_sum += weight * math.cos(angle)
        y_sum += weight * math.sin(angle)
        weight_sum += weight
    if weight_sum <= 1e-20:
        return 0.0, 180.0, 0.0
    resultant = math.hypot(x_sum, y_sum) / weight_sum
    direction = math.degrees(math.atan2(y_sum, x_sum))
    confidence = float(np.clip(resultant, 0.0, 1.0))
    width = float(math.degrees(math.sqrt(max(0.0, -2.0 * math.log(max(resultant, 1e-12))))))
    return float(direction), width, confidence
