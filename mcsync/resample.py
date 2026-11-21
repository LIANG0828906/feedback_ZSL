"""Fractional-delay resampling, synchronized concatenation and residuals."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable

import numpy as np

from .errors import (
    E_MODE_INVALID,
    Issue,
    SyncError,
)
from .markers import MarkerDetectionSet, detect_all_markers
from .models import (
    ChannelResidual,
    DriftFit,
    MarkerConfig,
    ResidualSummary,
)
from .utils import sha256_array
from .wavio import AudioData


VALID_MODES = ("linear", "windowed_sinc")


def validate_interpolation_mode(mode: str) -> str:
    if mode not in VALID_MODES:
        raise SyncError(
            E_MODE_INVALID,
            f"unsupported interpolation mode: {mode}",
            fields=["compensation.mode"],
            details={"allowed": list(VALID_MODES)},
        )
    return mode


def compensated_length(
    frame_count: int,
    fits: Iterable[DriftFit],
) -> int:
    candidates = [int(frame_count)]
    for fit in fits:
        scale = 1.0 + float(fit.slope)
        if scale <= 1.0e-9:
            candidates.append(0)
            continue
        available = int(
            np.floor((frame_count - 1.0 - fit.initial_offset_samples) / scale)
        ) + 1
        candidates.append(max(0, min(frame_count, available)))
    return max(0, min(candidates)) if candidates else 0


def source_positions(
    fit: DriftFit,
    output_start: int,
    output_end: int,
) -> np.ndarray:
    indices = np.arange(output_start, output_end, dtype=np.float64)
    return fit.initial_offset_samples + (1.0 + fit.slope) * indices


def _linear_range(
    samples: np.ndarray,
    positions: np.ndarray,
) -> np.ndarray:
    source_index = np.arange(samples.size, dtype=np.float64)
    return np.interp(
        positions,
        source_index,
        samples,
        left=0.0,
        right=0.0,
    )


def _windowed_sinc_range(
    samples: np.ndarray,
    positions: np.ndarray,
    half_taps: int,
    block_size: int = 65_536,
) -> np.ndarray:
    output = np.empty(positions.size, dtype=np.float64)
    tap_offsets = np.arange(-half_taps + 1, half_taps + 1, dtype=np.int64)
    padded = np.pad(samples, (half_taps, half_taps), mode="edge")
    for start in range(0, positions.size, block_size):
        end = min(positions.size, start + block_size)
        block_positions = positions[start:end]
        base = np.floor(block_positions).astype(np.int64)
        fractional = block_positions - base
        distances = fractional[:, None] - tap_offsets[None, :]
        indices = base[:, None] + tap_offsets[None, :] + half_taps
        indices = np.clip(indices, 0, padded.size - 1)
        weights = np.sinc(distances)
        window = 0.5 + 0.5 * np.cos(np.pi * distances / half_taps)
        window = np.where(np.abs(distances) < half_taps, window, 0.0)
        weights *= window
        normalization = np.sum(weights, axis=1)
        normalization = np.where(
            np.abs(normalization) < 1.0e-12, 1.0, normalization
        )
        values = padded[indices]
        output[start:end] = np.sum(values * weights, axis=1) / normalization
    return output


def resample_channel_range(
    samples: np.ndarray,
    fit: DriftFit,
    *,
    mode: str,
    output_start: int,
    output_end: int,
    sinc_half_taps: int = 16,
) -> np.ndarray:
    validate_interpolation_mode(mode)
    if output_end <= output_start:
        return np.zeros(0, dtype=np.float64)
    positions = source_positions(fit, output_start, output_end)
    if mode == "linear":
        return _linear_range(samples, positions)
    return _windowed_sinc_range(samples, positions, sinc_half_taps)


def compensate_channel(
    samples: np.ndarray,
    fit: DriftFit,
    *,
    mode: str,
    output_length: int,
    sinc_half_taps: int = 16,
) -> np.ndarray:
    return resample_channel_range(
        samples,
        fit,
        mode=mode,
        output_start=0,
        output_end=output_length,
        sinc_half_taps=sinc_half_taps,
    )


def concatenate_channel_chunks(
    chunks: Iterable[tuple[int, int, np.ndarray]],
    output_length: int,
) -> np.ndarray:
    output = np.zeros(output_length, dtype=np.float64)
    for start, end, values in chunks:
        start = max(0, int(start))
        end = min(output_length, int(end))
        if end <= start:
            continue
        source = np.asarray(values, dtype=np.float64)
        width = min(end - start, source.size)
        output[start : start + width] = source[:width]
    return output


def build_residual_summary(
    compensated: AudioData,
    *,
    reference_channel: int,
    marker_config: MarkerConfig,
    tolerance_samples: float,
    channels: Iterable[int] | None = None,
) -> tuple[ResidualSummary, MarkerDetectionSet, tuple[Issue, ...]]:
    channel_list = tuple(
        range(compensated.channel_count)
        if channels is None
        else sorted(set(channels))
    )
    detection_set = detect_all_markers(
        compensated,
        marker_config,
        reference_channel=reference_channel,
        channels=channel_list,
    )
    reference_map = {
        marker.marker_index: marker.arrival_sample
        for marker in detection_set.markers_for(reference_channel)
    }
    channel_residuals: list[ChannelResidual] = []
    all_errors: list[float] = []
    for channel in channel_list:
        channel_map = {
            marker.marker_index: marker.arrival_sample
            for marker in detection_set.markers_for(channel)
        }
        common = sorted(set(reference_map).intersection(channel_map))
        if channel == reference_channel:
            errors = np.zeros(len(common), dtype=np.float64)
        else:
            errors = np.array(
                [
                    channel_map[index] - reference_map[index]
                    for index in common
                ],
                dtype=np.float64,
            )
        all_errors.extend(float(value) for value in errors)
        rms = (
            float(np.sqrt(np.mean(errors**2)))
            if errors.size
            else float("inf")
        )
        maximum = (
            float(np.max(np.abs(errors)))
            if errors.size
            else float("inf")
        )
        channel_residuals.append(
            ChannelResidual(
                channel=channel,
                marker_count=int(errors.size),
                residual_rms_samples=rms,
                residual_max_samples=maximum,
                max_abs_delay_samples=maximum,
                within_tolerance=maximum <= tolerance_samples,
            )
        )
    finite_applicable = [
        item.max_abs_delay_samples
        for item in channel_residuals
        if item.channel != reference_channel
    ]
    worst_channel: int | None = None
    maximum_overall = 0.0
    if finite_applicable:
        worst = max(
            (
                item
                for item in channel_residuals
                if item.channel != reference_channel
            ),
            key=lambda item: item.max_abs_delay_samples,
        )
        worst_channel = worst.channel
        maximum_overall = worst.max_abs_delay_samples
    rms_overall = (
        float(
            np.sqrt(
                np.mean(
                    [
                        item.residual_rms_samples**2
                        for item in channel_residuals
                        if item.channel != reference_channel
                    ]
                )
            )
        )
        if finite_applicable
        else 0.0
    )
    summary = ResidualSummary(
        tolerance_samples=float(tolerance_samples),
        worst_channel=worst_channel,
        max_abs_delay_samples=float(maximum_overall),
        rms_samples=rms_overall,
        channels=tuple(channel_residuals),
    )
    return summary, detection_set, detection_set.issues


def compensate_audio(
    audio: AudioData,
    fits: Iterable[DriftFit],
    *,
    mode: str,
    reference_channel: int,
    marker_config: MarkerConfig,
    tolerance_samples: float,
    sinc_half_taps: int = 16,
    channels: Iterable[int] | None = None,
) -> tuple[AudioData, ResidualSummary, MarkerDetectionSet, tuple[Issue, ...]]:
    validate_interpolation_mode(mode)
    fit_by_channel = {fit.channel: fit for fit in fits}
    channel_list = tuple(
        range(audio.channel_count)
        if channels is None
        else sorted(set(channels))
    )
    selected_fits = [
        fit_by_channel[channel]
        for channel in channel_list
        if channel in fit_by_channel
    ]
    output_length = compensated_length(audio.frame_count, selected_fits)
    matrix = np.zeros((len(channel_list), output_length), dtype=np.float64)
    for row, channel in enumerate(channel_list):
        fit = fit_by_channel.get(channel)
        if fit is None:
            fit = DriftFit(
                channel=channel,
                initial_offset_samples=0.0,
                drift_ppm=0.0,
                slope=0.0,
                residual_rms=0.0,
                residual_max=0.0,
                residual_threshold_samples=0.0,
                max_residual_segment=None,
                residual_by_segment=(),
                predicted_by_segment=(),
                exceeded_threshold=False,
            )
        matrix[row] = compensate_channel(
            audio.channel(channel),
            fit,
            mode=mode,
            output_length=output_length,
            sinc_half_taps=sinc_half_taps,
        )
    compensated = AudioData(
        samples=matrix,
        sample_rate=audio.sample_rate,
        channel_names=tuple(audio.channel_names[channel] for channel in channel_list),
    )
    summary, detection_set, issues = build_residual_summary(
        compensated,
        reference_channel=reference_channel,
        marker_config=marker_config,
        tolerance_samples=tolerance_samples,
        channels=channel_list,
    )
    return compensated, summary, detection_set, issues


def compare_interpolation_modes(
    audio: AudioData,
    fits: Iterable[DriftFit],
    *,
    reference_channel: int,
    marker_config: MarkerConfig,
    tolerance_samples: float,
    sinc_half_taps: int = 16,
    channels: Iterable[int] | None = None,
) -> dict[str, object]:
    fit_list = tuple(fits)
    results: dict[str, dict[str, object]] = {}
    issues_by_mode: dict[str, list[dict[str, object]]] = {}
    for mode in VALID_MODES:
        compensated, summary, _, issues = compensate_audio(
            audio,
            fit_list,
            mode=mode,
            reference_channel=reference_channel,
            marker_config=marker_config,
            tolerance_samples=tolerance_samples,
            sinc_half_taps=sinc_half_taps,
            channels=channels,
        )
        results[mode] = {
            "residual_summary": summary.to_dict(),
            "audio_sha256": compensated.content_hash(),
        }
        issues_by_mode[mode] = [issue.to_dict() for issue in issues]
    linear = results["linear"]["residual_summary"]
    sinc = results["windowed_sinc"]["residual_summary"]
    assert isinstance(linear, dict)
    assert isinstance(sinc, dict)
    linear_max = float(linear["max_abs_delay_samples"])
    sinc_max = float(sinc["max_abs_delay_samples"])
    best_mode = (
        "linear"
        if linear_max < sinc_max
        else "windowed_sinc"
        if sinc_max < linear_max
        else "tie"
    )
    return {
        "schema_version": "interpolation-comparison-v1",
        "tolerance_samples": tolerance_samples,
        "modes": results,
        "issues_by_mode": issues_by_mode,
        "comparison": {
            "max_abs_delay_difference_samples": abs(linear_max - sinc_max),
            "rms_difference_samples": abs(
                float(linear["rms_samples"]) - float(sinc["rms_samples"])
            ),
            "best_mode": best_mode,
            "worst_channel_by_mode": {
                "linear": linear["worst_channel"],
                "windowed_sinc": sinc["worst_channel"],
            },
        },
    }


def compensating_chunk_hash(
    samples: np.ndarray,
) -> str:
    return sha256_array(samples)

