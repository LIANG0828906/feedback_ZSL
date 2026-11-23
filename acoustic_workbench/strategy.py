"""Explainable resolution trade-off policy and mode comparison."""

from __future__ import annotations

from dataclasses import replace
from typing import Any

from .codes import E_TOL
from .config import AnalysisConfig, frame_count, validate_analysis_config
from .errors import error
from .models import ResolutionDecision, Sample


def resolve_analysis_config(
    sample: Sample,
    requested: AnalysisConfig,
) -> tuple[AnalysisConfig, ResolutionDecision]:
    validate_analysis_config(requested)
    duration = sample.duration_s
    mode = requested.mode
    rationale: list[str] = []

    if mode == "auto":
        if duration <= 1.0:
            effective_mode = "calibration"
            rationale.append("short IR (<=1.0 s): retain high-resolution calibration")
        else:
            effective_mode = "preview"
            rationale.append("long IR (>1.0 s): auto-compress for bounded preview cost")
    else:
        effective_mode = mode
        rationale.append(f"explicit {mode} mode requested")

    frame_length = requested.frame_length
    hop_length = requested.hop_length
    estimation_window = requested.estimation_window

    if effective_mode == "preview":
        compressed_window = max(64, int(round(estimation_window * 0.5)))
        estimation_window = min(frame_length, compressed_window)
        minimum_preview_hop = max(hop_length, frame_length // 2)
        hop_length = min(frame_length, minimum_preview_hop)
        rationale.append(
            "preview halves the estimation window and raises the hop to at least half a frame"
        )
    else:
        if duration <= 1.0 and requested.auto_tradeoff:
            high_resolution_window = min(frame_length, max(estimation_window, 4096))
            if high_resolution_window != estimation_window:
                estimation_window = high_resolution_window
                rationale.append(
                    "short calibration raised estimation_window for high temporal resolution"
                )
            high_resolution_hop = min(hop_length, max(64, frame_length // 8))
            if high_resolution_hop != hop_length:
                hop_length = high_resolution_hop
                rationale.append("short calibration increased overlap for dense frame coverage")
        else:
            rationale.append("calibration retains requested estimation window and overlap")

    overlap_ratio = 0.0 if frame_length == 0 else 1.0 - hop_length / frame_length
    effective = replace(
        requested,
        frame_length=frame_length,
        hop_length=hop_length,
        estimation_window=estimation_window,
        mode=effective_mode,
    )
    decision = ResolutionDecision(
        requested_mode=requested.mode,
        effective_mode=effective_mode,
        frame_length=frame_length,
        hop_length=hop_length,
        estimation_window=estimation_window,
        overlap_ratio=float(overlap_ratio),
        rationale=rationale,
        ir_duration_s=duration,
    )
    return effective, decision


def compare_preview_calibration(
    sample: Sample,
    requested: AnalysisConfig,
    *,
    analyzer: Any,
) -> dict[str, Any]:
    base = replace(requested, mode="calibration")
    calibration_config, calibration_decision = resolve_analysis_config(sample, base)
    preview_config, preview_decision = resolve_analysis_config(
        sample, replace(requested, mode="preview")
    )
    calibration_config = replace(calibration_config, mode="calibration")
    preview_config = replace(preview_config, mode="preview")
    calibration_frames = analyzer.analyze(sample, calibration_config)
    preview_frames = analyzer.analyze(sample, preview_config)

    calibration_by_start = {
        int(frame.frame_start): (index, frame)
        for index, frame in enumerate(calibration_frames)
    }
    calibration_starts = sorted(calibration_by_start)
    comparisons: list[dict[str, Any]] = []
    tolerance = requested.tolerance
    first_bad: dict[str, Any] | None = None
    max_delay_delta = 0.0
    max_energy_delta = 0.0
    max_drr_delta = 0.0

    for preview_index, preview_frame in enumerate(preview_frames):
        calibration_item = calibration_by_start.get(preview_frame.frame_start)
        if calibration_item is None:
            nearest_start = min(
                calibration_starts,
                key=lambda start: abs(start - preview_frame.frame_start),
            )
            calibration_item = calibration_by_start[nearest_start]
        calibration_index, calibration_frame = calibration_item
        delay_delta = max(
            (
                abs(float(a) - float(b))
                for a, b in zip(
                    preview_frame.delay_samples, calibration_frame.delay_samples, strict=True
                )
            ),
            default=0.0,
        )
        energy_delta = max(
            (
                _relative_delta(float(a), float(b))
                for left, right in (
                    (preview_frame.direct_energy, calibration_frame.direct_energy),
                    (preview_frame.early_energy, calibration_frame.early_energy),
                    (preview_frame.reverberant_energy, calibration_frame.reverberant_energy),
                )
                for a, b in zip(left, right, strict=True)
            ),
            default=0.0,
        )
        drr_delta = max(
            (
                abs(float(a) - float(b))
                for a, b in zip(preview_frame.drr_db, calibration_frame.drr_db, strict=True)
            ),
            default=0.0,
        )
        max_delay_delta = max(max_delay_delta, delay_delta)
        max_energy_delta = max(max_energy_delta, energy_delta)
        max_drr_delta = max(max_drr_delta, drr_delta)
        item = {
            "frame_index": preview_index,
            "calibration_frame_index": calibration_index,
            "frame_start": preview_frame.frame_start,
            "calibration_frame_start": calibration_frame.frame_start,
            "calibration_start_delta_samples": (
                calibration_frame.frame_start - preview_frame.frame_start
            ),
            "max_delay_delta_samples": delay_delta,
            "max_energy_relative_delta": energy_delta,
            "max_drr_delta_db": drr_delta,
        }
        comparisons.append(item)
        exceeded = (
            delay_delta > tolerance.delay_samples
            or energy_delta > tolerance.energy_relative
            or drr_delta > tolerance.drr_db
        )
        if exceeded and first_bad is None:
            first_bad = item
            break

    result = {
        "ok": first_bad is None,
        "tolerance": tolerance.to_dict(),
        "calibration_decision": calibration_decision.to_dict(),
        "preview_decision": preview_decision.to_dict(),
        "frame_count_calibration": len(calibration_frames),
        "frame_count_preview": len(preview_frames),
        "max_delay_delta_samples": max_delay_delta,
        "max_energy_relative_delta": max_energy_delta,
        "max_drr_delta_db": max_drr_delta,
        "comparisons": comparisons,
        "first_exceeded": first_bad,
    }
    if first_bad is not None:
        exc = error(
            E_TOL,
            "preview and calibration estimates exceed the declared tolerance",
            stage="strategy.mode_compare",
            frame_index=int(first_bad["frame_index"]),
            context={
                "preview_frame_index": first_bad["frame_index"],
                "calibration_frame_index": first_bad["calibration_frame_index"],
                "frame_start": first_bad["frame_start"],
                "delay_delta_samples": first_bad["max_delay_delta_samples"],
                "energy_relative_delta": first_bad["max_energy_relative_delta"],
                "drr_delta_db": first_bad["max_drr_delta_db"],
                "tolerance": tolerance.to_dict(),
            },
        )
        exc.add_stage("strategy.result", context={"comparison_count": len(comparisons)})
        raise exc
    return result


def _relative_delta(left: float, right: float) -> float:
    denominator = max(abs(left), abs(right), 1e-15)
    return abs(left - right) / denominator


def work_estimate(sample: Sample, config: AnalysisConfig) -> dict[str, Any]:
    count = frame_count(sample.ir.shape[1], config.frame_length, config.hop_length)
    work_units = count * config.frame_length * config.estimation_window
    return {
        "frame_count": count,
        "work_units": int(work_units),
        "work_units_millions": float(work_units / 1_000_000.0),
        "overlap_ratio": 1.0 - config.hop_length / config.frame_length,
        "estimation_window": config.estimation_window,
        "frame_length": config.frame_length,
        "hop_length": config.hop_length,
    }
