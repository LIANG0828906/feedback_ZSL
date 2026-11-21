"""Drift-source diagnosis separating clock frequency offset from event jumps."""

from __future__ import annotations

from typing import Iterable

import numpy as np

from .errors import E_DRIFT_UNKNOWN, Issue
from .models import DiagnosisConfig, DriftDiagnosis, DriftFit, SegmentDelay


def _robust_scale(values: np.ndarray) -> float:
    if values.size == 0:
        return 0.0
    center = float(np.median(values))
    return 1.4826 * float(np.median(np.abs(values - center)))


def diagnose_channel(
    segments: Iterable[SegmentDelay],
    fit: DriftFit,
    config: DiagnosisConfig,
) -> DriftDiagnosis:
    ordered = sorted(segments, key=lambda item: item.segment_index)
    delays = np.array([segment.delay_samples for segment in ordered])
    segment_indices = [segment.segment_index for segment in ordered]
    if ordered:
        reference_samples = np.array(
            [
                0.5 * (segment.start_sample + segment.end_sample - 1)
                for segment in ordered
            ],
            dtype=np.float64,
        )
        predicted = fit.initial_offset_samples + fit.slope * reference_samples
        total = float(np.sum((delays - np.mean(delays)) ** 2))
        squared_error = float(np.sum((delays - predicted) ** 2))
        r_squared = 1.0 - squared_error / total if total > 1.0e-15 else 1.0
    else:
        r_squared = 0.0
    if delays.size >= 2:
        differences = np.diff(delays)
        difference_center = float(np.median(differences))
        difference_scale = _robust_scale(differences)
        jump_threshold = max(
            config.jump_threshold_samples,
            5.0 * difference_scale,
        )
        deviations = np.abs(differences - difference_center)
        jump_positions = np.flatnonzero(deviations >= jump_threshold)
        jump_segments = tuple(
            segment_indices[int(position) + 1] for position in jump_positions
        )
        jump_magnitude = (
            float(np.max(deviations[jump_positions]))
            if jump_positions.size
            else 0.0
        )
    else:
        jump_segments = ()
        jump_magnitude = 0.0
        jump_threshold = config.jump_threshold_samples
        difference_scale = 0.0

    slope_sigma_ppm = fit.slope_std * 1.0e6
    significance = (
        abs(fit.drift_ppm) / max(slope_sigma_ppm, 1.0e-9)
        if slope_sigma_ppm > 0
        else (10.0 if abs(fit.drift_ppm) >= config.min_drift_ppm else 0.0)
    )
    has_clock_drift = (
        abs(fit.drift_ppm) >= config.min_drift_ppm
        and significance >= 2.0
        and (
            r_squared >= 0.75
            or fit.residual_rms <= config.residual_threshold_samples
        )
    )
    has_event_jump = bool(jump_segments)
    clock_confidence = float(
        np.clip(
            0.45 * min(significance / 5.0, 1.0)
            + 0.35 * max(0.0, min(r_squared, 1.0))
            + 0.20
            * (
                1.0
                if fit.residual_rms <= config.residual_threshold_samples
                else 0.4
            ),
            0.0,
            1.0,
        )
    )
    jump_confidence = float(
        np.clip(
            1.0
            - np.exp(
                -max(jump_magnitude - config.jump_threshold_samples, 0.0)
                / max(4.0 * difference_scale, 2.0)
            ),
            0.0,
            1.0,
        )
    )
    evidence: list[int] = []
    if has_clock_drift and segment_indices:
        evidence.extend(
            [
                segment_indices[0],
                segment_indices[len(segment_indices) // 2],
                segment_indices[-1],
            ]
        )
    for jump_segment in jump_segments:
        position = segment_indices.index(jump_segment)
        evidence.extend(segment_indices[max(0, position - 1) : position + 1])
    evidence_segments = tuple(sorted(set(evidence)))

    if (
        ordered
        and np.all(np.abs(delays) <= 1.0e-9)
        and abs(fit.drift_ppm) <= 1.0e-9
        and fit.residual_max <= 1.0e-9
    ):
        conclusion = "reference_channel"
        confidence = 1.0
        evidence_segments = tuple(segment_indices)
        message = "this channel is the declared zero-delay reference"
    elif len(ordered) < 3:
        conclusion = "unknown"
        confidence = 0.25
        message = (
            "fewer than three valid segments; a linear clock model and event "
            "jumps cannot be separated reliably"
        )
    elif has_clock_drift and has_event_jump:
        conclusion = "mixed_clock_drift_and_event_jump"
        confidence = max(clock_confidence, jump_confidence)
        message = (
            "a persistent linear trend coexists with one or more abrupt delay "
            "steps"
        )
    elif has_event_jump and not has_clock_drift:
        conclusion = "event_jump"
        confidence = jump_confidence
        message = (
            "the delay sequence contains abrupt persistent steps without a "
            "significant clock-frequency trend"
        )
    elif has_clock_drift:
        conclusion = "clock_frequency_offset"
        confidence = clock_confidence
        message = (
            "delay changes continuously and is explained by a first-order "
            "clock-frequency offset"
        )
    else:
        conclusion = "unknown"
        confidence = max(0.2, 1.0 - clock_confidence)
        message = (
            "neither a significant clock-frequency trend nor a well-supported "
            "event jump was found"
        )
    candidates = ()
    if conclusion == "unknown":
        candidates = (
            "clock_frequency_offset_below_threshold",
            "event_time_jump_without_stable_neighbors",
            "marker_detection_error_or_low_correlation",
        )
    if fit.exceeded_threshold and conclusion == "clock_frequency_offset":
        message += (
            f"; residual threshold exceeded at segment {fit.max_residual_segment}"
        )
    return DriftDiagnosis(
        channel=fit.channel,
        conclusion=conclusion,
        confidence=float(confidence),
        evidence_segments=evidence_segments,
        ci_low_ppm=fit.drift_ppm_ci_low,
        ci_high_ppm=fit.drift_ppm_ci_high,
        jump_segments=jump_segments,
        candidates=candidates,
        message=message,
    )


def diagnose_all_channels(
    segments: Iterable[SegmentDelay],
    fits: Iterable[DriftFit],
    config: DiagnosisConfig,
) -> tuple[tuple[DriftDiagnosis, ...], tuple[Issue, ...]]:
    grouped: dict[int, list[SegmentDelay]] = {}
    for segment in segments:
        grouped.setdefault(segment.channel, []).append(segment)
    diagnoses: list[DriftDiagnosis] = []
    issues: list[Issue] = []
    for fit in sorted(fits, key=lambda item: item.channel):
        diagnosis = diagnose_channel(grouped.get(fit.channel, []), fit, config)
        diagnoses.append(diagnosis)
        if diagnosis.conclusion == "unknown":
            issues.append(
                Issue(
                    code=E_DRIFT_UNKNOWN,
                    message=diagnosis.message,
                    channel=fit.channel,
                    details={
                        "candidates": list(diagnosis.candidates),
                        "evidence_segments": list(diagnosis.evidence_segments),
                        "confidence_interval_ppm": [
                            diagnosis.ci_low_ppm,
                            diagnosis.ci_high_ppm,
                        ],
                    },
                )
            )
    return tuple(diagnoses), tuple(issues)

