"""Frame-wise spatial acoustic analysis.

The IR is divided into overlapping analysis frames. Direct arrivals are
detected once per channel over the whole IR, while early-reflection energy,
local arrival, decay and direction are measured per frame. Analytic truth is
used only by the verification report, never to fill missing metadata or to
drive estimation.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Iterable

import numpy as np
from scipy.signal import find_peaks

from .errors import ErrorCode, WorkbenchError
from .models import (
    AcousticSample,
    AnalysisConfig,
    ChannelMap,
    FrameParameters,
)


@dataclass(frozen=True)
class FrameSchedule:
    frame_index: int
    dry_start: int
    frame_length: int
    hop_length: int
    source_analysis_index: int
    ir_start: int


@dataclass(frozen=True)
class AnalysisContext:
    sample_hash: str
    sample_rate: int
    channels: tuple[str, ...]
    azimuths_deg: tuple[float, ...]
    global_direct_delays: tuple[int, ...]
    global_direct_energies: tuple[float, ...]
    mapping: ChannelMap
    config: AnalysisConfig

    @property
    def channel_count(self) -> int:
        return len(self.channels)


def _db_ratio(numerator: float, denominator: float) -> float:
    epsilon = 1.0e-30
    return float(10.0 * math.log10(max(numerator, epsilon) / max(denominator, epsilon)))


def _energy_decay(frame: np.ndarray, start: int) -> float:
    remaining = frame.shape[0] - start
    if remaining <= 8:
        return 0.0
    block = max(8, frame.shape[0] // 48)
    count = remaining // block
    if count < 3:
        return 0.0
    energies = np.empty(count, dtype=np.float64)
    for index in range(count):
        begin = start + index * block
        end = begin + block
        energies[index] = float(np.sum(np.square(frame[begin:end])))
    peak_energy = float(np.max(energies))
    if peak_energy <= 1.0e-30:
        return 0.0
    significant = np.flatnonzero(energies >= peak_energy * 1.0e-3)
    if significant.size:
        energies = energies[: int(significant[-1]) + 1]
        count = energies.size
    positive = energies > 1.0e-30
    if np.count_nonzero(positive) < 3:
        return 0.0
    positions = np.arange(count, dtype=np.float64)[positive]
    logs = np.log(energies[positive])
    slope, _ = np.polyfit(positions, logs, 1)
    coefficient = max(0.0, float(-slope / float(block)))
    return min(coefficient, 1.0)


def build_frame_schedule(sample: AcousticSample, config: AnalysisConfig) -> list[FrameSchedule]:
    dry_length = int(sample.dry.shape[0])
    ir_length = int(sample.ir.shape[1])
    render_count = max(1, int(math.ceil(dry_length / float(config.hop_length))))
    analysis_count = max(1, int(math.ceil(ir_length / float(config.hop_length))))
    schedules: list[FrameSchedule] = []
    for frame_index in range(render_count):
        dry_start = frame_index * config.hop_length
        if dry_start >= dry_length:
            break
        if render_count <= 1 or analysis_count <= 1:
            source_index = 0
        else:
            source_index = int(
                round(frame_index * (analysis_count - 1) / float(render_count - 1))
            )
        source_index = min(max(source_index, 0), analysis_count - 1)
        schedules.append(
            FrameSchedule(
                frame_index=frame_index,
                dry_start=dry_start,
                frame_length=config.frame_length,
                hop_length=config.hop_length,
                source_analysis_index=source_index,
                ir_start=source_index * config.hop_length,
            )
        )
    return schedules


def validate_input(sample: AcousticSample, config: AnalysisConfig) -> None:
    if sample.missing_channels:
        raise WorkbenchError(
            ErrorCode.E_CHAN,
            "input sample is missing one or more channels",
            stage="input-validation",
            details={"missing_channels": list(sample.missing_channels)},
        )
    if sample.ir.ndim != 2 or sample.ir.shape[0] != len(sample.layout.channels):
        raise WorkbenchError(
            ErrorCode.E_CHAN,
            "input channel count does not match the channel layout",
            stage="input-validation",
            details={
                "ir_channels": int(sample.ir.shape[0]) if sample.ir.ndim == 2 else None,
                "layout_channels": list(sample.layout.channels),
            },
        )
    if sample.ir.size == 0 or sample.dry.size == 0:
        raise WorkbenchError(
            ErrorCode.E_DECODE,
            "input sample contains an empty signal",
            stage="input-validation",
        )
    if not np.all(np.isfinite(sample.ir)) or not np.all(np.isfinite(sample.dry)):
        raise WorkbenchError(
            ErrorCode.E_DECODE,
            "input sample contains NaN or infinite samples",
            stage="input-validation",
        )
    if float(np.max(np.abs(sample.ir))) > config.input_peak_limit:
        raise WorkbenchError(
            ErrorCode.E_CLIP,
            "input IR peak exceeds the configured input headroom",
            stage="input-validation",
            details={
                "peak": float(np.max(np.abs(sample.ir))),
                "limit": config.input_peak_limit,
            },
        )
    maximum_delay = int(
        math.floor(config.max_direct_delay_seconds * sample.sample_rate)
    )
    # A direct-arrival detector sees the global peak. If that peak lives late
    # in the IR, the sample has been time-reversed or otherwise order-corrupted.
    late_channels = [
        channel_index
        for channel_index in range(sample.ir.shape[0])
        if int(np.argmax(np.abs(sample.ir[channel_index]))) > maximum_delay
    ]
    if late_channels:
        raise WorkbenchError(
            ErrorCode.E_ORDER,
            "direct arrival is later than the configured time-order bound",
            stage="input-validation",
            details={
                "channels": late_channels,
                "maximum_delay_samples": maximum_delay,
                "detected_delays": [
                    int(np.argmax(np.abs(sample.ir[index])))
                    for index in late_channels
                ],
            },
        )
    if not np.isfinite(sample.sample_rate) or sample.sample_rate <= 0:
        raise WorkbenchError(
            ErrorCode.E_META,
            "sample rate metadata is invalid",
            stage="input-validation",
        )


def build_analysis_context(
    sample: AcousticSample,
    config: AnalysisConfig,
) -> AnalysisContext:
    validate_input(sample, config)
    mapping = ChannelMap.from_config(
        sample.layout,
        config.output_layout,
        config.channel_weights,
    )
    global_delays: list[int] = []
    global_energies: list[float] = []
    for channel_index in range(sample.ir.shape[0]):
        delay = int(np.argmax(np.abs(sample.ir[channel_index])))
        amplitude = float(sample.ir[channel_index, delay])
        global_delays.append(delay)
        global_energies.append(amplitude * amplitude)
    return AnalysisContext(
        sample_hash=sample.content_hash,
        sample_rate=sample.sample_rate,
        channels=sample.layout.channels,
        azimuths_deg=sample.layout.azimuths_deg,
        global_direct_delays=tuple(global_delays),
        global_direct_energies=tuple(global_energies),
        mapping=mapping,
        config=config,
    )


def estimate_frame(
    sample: AcousticSample,
    schedule: FrameSchedule,
    context: AnalysisContext,
) -> FrameParameters:
    frame_index = schedule.frame_index
    if frame_index in sample.bad_frames:
        raise WorkbenchError(
            ErrorCode.E_FRAME,
            "injected bad frame",
            stage="analysis",
            frame_index=frame_index,
        )
    config = context.config
    channel_count = context.channel_count
    if context.config.frame_length * channel_count * 8 * 8 > config.max_buffer_bytes:
        raise WorkbenchError(
            ErrorCode.E_RESOURCE,
            "estimated worker buffer exceeds max_buffer_bytes",
            stage="analysis",
            frame_index=frame_index,
            details={
                "required_bytes_estimate": context.config.frame_length
                * channel_count
                * 8
                * 8,
                "max_buffer_bytes": config.max_buffer_bytes,
            },
        )

    ir_start = schedule.ir_start
    ir_end = min(ir_start + schedule.frame_length, sample.ir.shape[1])
    source = sample.ir[:, ir_start:ir_end]
    if source.shape[1] < schedule.frame_length:
        frame = np.zeros((channel_count, schedule.frame_length), dtype=np.float64)
        frame[:, : source.shape[1]] = source
    else:
        frame = np.array(source, dtype=np.float64, copy=True)

    absolute = np.abs(frame)
    local_arrivals = np.argmax(absolute, axis=1).astype(np.int64)
    local_peak_amplitudes = frame[np.arange(channel_count), local_arrivals]
    local_peak_energy = np.square(local_peak_amplitudes)
    total_energy = np.sum(np.square(frame), axis=1)
    threshold = config.energy_threshold * max(float(np.max(total_energy)), 1.0e-30)
    active = total_energy >= threshold
    total_energy_effective = np.where(active, total_energy, 0.0)
    direct_energy = np.zeros(channel_count, dtype=np.float64)
    for channel_index in range(channel_count):
        if ir_start <= context.global_direct_delays[channel_index] < ir_end:
            local_delay = context.global_direct_delays[channel_index] - ir_start
            direct_energy[channel_index] = float(
                frame[channel_index, local_delay] ** 2
            )
        if not active[channel_index]:
            direct_energy[channel_index] = 0.0

    early_window = max(1, int(round(config.early_window_seconds * sample.sample_rate)))
    early_energy = np.zeros(channel_count, dtype=np.float64)
    decay_coefficients = np.zeros(channel_count, dtype=np.float64)
    reflection_delays: list[list[int]] = []
    reflection_gains: list[list[float]] = []
    reflection_amplitudes: list[list[float]] = []
    for channel_index in range(channel_count):
        arrival = int(local_arrivals[channel_index])
        early_start = min(schedule.frame_length, arrival + 1)
        early_end = min(schedule.frame_length, early_start + early_window)
        if active[channel_index] and early_end > early_start:
            early_energy[channel_index] = float(
                np.sum(np.square(frame[channel_index, early_start:early_end]))
            )
        decay_coefficients[channel_index] = _energy_decay(
            frame[channel_index],
            early_start,
        )

        peaks, properties = find_peaks(
            absolute[channel_index],
            distance=2,
        )
        candidate_delays: list[int] = []
        candidate_amplitudes: list[float] = []
        for peak in peaks:
            peak = int(peak)
            if peak <= arrival or peak >= early_end:
                continue
            amplitude = float(frame[channel_index, peak])
            if abs(amplitude) <= 0.0:
                continue
            candidate_delays.append(peak)
            candidate_amplitudes.append(amplitude)
        order = sorted(
            range(len(candidate_amplitudes)),
            key=lambda index: abs(candidate_amplitudes[index]),
            reverse=True,
        )[: config.max_reflections]
        delays_for_channel = [candidate_delays[index] for index in order]
        amplitudes_for_channel = [float(candidate_amplitudes[index]) for index in order]
        peak_reference = max(abs(float(local_peak_amplitudes[channel_index])), 1.0e-30)
        gains_for_channel = [
            float(amplitude / peak_reference) for amplitude in amplitudes_for_channel
        ]
        reflection_delays.append(delays_for_channel)
        reflection_amplitudes.append(amplitudes_for_channel)
        reflection_gains.append(gains_for_channel)

    reference = 0
    delay_differences = [
        int(context.global_direct_delays[index] - context.global_direct_delays[reference])
        for index in range(channel_count)
    ]
    delay_differences_seconds = [
        float(value) / float(sample.sample_rate) for value in delay_differences
    ]
    energy_ratio = [
        float(
            total_energy_effective[index]
            / max(total_energy_effective[reference], 1.0e-30)
        )
        for index in range(channel_count)
    ]
    direct_ratio = [
        float(direct_energy[index] / max(direct_energy[reference], 1.0e-30))
        for index in range(channel_count)
    ]
    early_ratio = [
        float(early_energy[index] / max(early_energy[reference], 1.0e-30))
        for index in range(channel_count)
    ]
    ild_db = [
        _db_ratio(total_energy_effective[index], total_energy_effective[reference])
        for index in range(channel_count)
    ]

    energy_sum = float(np.sum(total_energy_effective))
    if energy_sum > 1.0e-30:
        weighted = np.asarray(total_energy_effective, dtype=np.float64) / energy_sum
        angles = np.deg2rad(np.asarray(context.azimuths_deg, dtype=np.float64))
        direction_x = float(np.sum(weighted * np.cos(angles)))
        direction_y = float(np.sum(weighted * np.sin(angles)))
        direction_confidence = float(
            min(1.0, math.hypot(direction_x, direction_y))
        )
        direction_azimuth = float(math.degrees(math.atan2(direction_y, direction_x)))
    else:
        direction_x = 0.0
        direction_y = 0.0
        direction_confidence = 0.0
        direction_azimuth = 0.0

    return FrameParameters(
        frame_index=frame_index,
        source_analysis_index=schedule.source_analysis_index,
        dry_start=schedule.dry_start,
        ir_start=ir_start,
        frame_length=schedule.frame_length,
        hop_length=schedule.hop_length,
        sample_rate=sample.sample_rate,
        direct_delays_samples=list(context.global_direct_delays),
        direct_delays_seconds=[
            float(value) / float(sample.sample_rate)
            for value in context.global_direct_delays
        ],
        local_arrival_delays_samples=[int(value) for value in local_arrivals],
        local_arrival_energy=[float(value) for value in local_peak_energy],
        delay_differences_samples=delay_differences,
        delay_differences_seconds=delay_differences_seconds,
        direct_energy=[float(value) for value in direct_energy],
        early_reflection_energy=[float(value) for value in early_energy],
        early_reflection_energy_ratio=early_ratio,
        total_energy=[float(value) for value in total_energy_effective],
        energy_ratio=energy_ratio,
        energy_decay_coefficients=[float(value) for value in decay_coefficients],
        ild_db=ild_db,
        reflection_delays_samples=reflection_delays,
        reflection_gains=reflection_gains,
        reflection_amplitudes=reflection_amplitudes,
        direction_azimuth_deg=direction_azimuth,
        direction_x=direction_x,
        direction_y=direction_y,
        direction_confidence=direction_confidence,
        output_channels=context.mapping.output_channels,
        channel_map=context.mapping.to_dict(),
    )


def analyze_frames(
    sample: AcousticSample,
    config: AnalysisConfig,
    *,
    schedules: Iterable[FrameSchedule] | None = None,
) -> tuple[AnalysisContext, list[FrameParameters]]:
    context = build_analysis_context(sample, config)
    frame_schedules = list(schedules or build_frame_schedule(sample, config))
    parameters = [
        estimate_frame(sample, schedule, context)
        for schedule in frame_schedules
    ]
    return context, parameters


def truth_report(
    sample: AcousticSample,
    context: AnalysisContext,
) -> dict[str, Any]:
    truth_delays = [int(value) for value in sample.truth.get("direct_delay_samples", [])]
    truth_decay = [
        float(value) for value in sample.truth.get("energy_decay_coeff", [])
    ]
    direct_errors = [
        int(estimated - truth)
        for estimated, truth in zip(context.global_direct_delays, truth_delays)
    ]
    estimated_decay: list[float] = []
    for channel_index in range(sample.ir.shape[0]):
        estimated_decay.append(
            _energy_decay(sample.ir[channel_index], int(context.global_direct_delays[channel_index]) + 1)
        )
    decay_errors = [
        float(estimated - truth)
        for estimated, truth in zip(estimated_decay, truth_decay)
    ]
    return {
        "channels": list(sample.layout.channels),
        "truth_direct_delays_samples": truth_delays,
        "estimated_direct_delays_samples": list(context.global_direct_delays),
        "direct_delay_errors_samples": direct_errors,
        "max_abs_direct_delay_error_samples": (
            int(max((abs(value) for value in direct_errors), default=0))
        ),
        "truth_energy_decay_coeff": truth_decay,
        "estimated_energy_decay_coeff": estimated_decay,
        "energy_decay_absolute_errors": [abs(value) for value in decay_errors],
        "max_abs_energy_decay_error": max(
            (abs(value) for value in decay_errors),
            default=0.0,
        ),
    }


def analyze_sample(
    sample: AcousticSample,
    config: AnalysisConfig,
) -> tuple[AnalysisContext, list[FrameParameters], dict[str, Any]]:
    """Analyze all frames and return a truth comparison report."""

    context, parameters = analyze_frames(sample, config)
    return context, parameters, truth_report(sample, context)

