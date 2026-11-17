"""Stable alignment/recovery report construction."""

from __future__ import annotations

from typing import Any

import numpy as np

from .audio import AudioTrack
from .drift import DriftCurve
from .jsonio import display_name
from .project import LoadedProject
from .render import RenderOutcome
from .signal import Detection


def _interval_summary(hits: np.ndarray) -> tuple[list[int], dict[str, Any]]:
    if hits.size < 2:
        intervals: list[int] = []
        return intervals, {
            "count": 0,
            "min_samples": None,
            "max_samples": None,
            "mean_samples": None,
        }
    intervals_array = np.diff(hits)
    intervals = [int(value) for value in intervals_array]
    return intervals, {
        "count": int(intervals_array.size),
        "min_samples": int(np.min(intervals_array)),
        "max_samples": int(np.max(intervals_array)),
        "mean_samples": round(float(np.mean(intervals_array)), 6),
    }


def _base_curve(bars: list[int]) -> DriftCurve:
    if not bars:
        ticks = [0]
    elif len(bars) == 1:
        ticks = [int(bars[0])]
    else:
        ticks = [int(bars[0]), int(bars[-1])]
    return DriftCurve(
        ticks=ticks,
        offsets=[0 for _ in ticks],
        bars=list(ticks),
        base_hits=list(ticks),
        aux_hits=list(ticks),
        correlations=[1.0 for _ in ticks],
    )


def _curve_report(curve: DriftCurve) -> dict[str, Any]:
    return {
        "ticks_samples": [int(value) for value in curve.ticks],
        "offsets_samples": [int(value) for value in curve.offsets],
        "bar_starts_samples": [int(value) for value in curve.bars],
        "reference_hits_samples": [int(value) for value in curve.base_hits],
        "source_hits_samples": [int(value) for value in curve.aux_hits],
        "local_correlations": [
            round(float(value), 6) for value in curve.correlations
        ],
    }


def build_report(
    *,
    project: LoadedProject,
    project_digest: str,
    reference: AudioTrack,
    tracks: dict[str, AudioTrack],
    detections: dict[str, Detection],
    curves: dict[str, DriftCurve],
    phase_results: dict[str, dict[str, Any]],
    effective_gains: dict[str, float],
    reference_gain: float,
    outcome: RenderOutcome,
    warnings: list[dict[str, Any]],
) -> dict[str, Any]:
    complete = outcome.status == "complete"
    quality_flags: list[dict[str, Any]] = []
    camera_reports: list[dict[str, Any]] = []

    for camera in sorted(tracks, key=lambda name: (name != project.base_camera, name)):
        track = tracks[camera]
        detection = detections[camera]
        intervals, summary = _interval_summary(detection.hits)
        is_base = camera == project.base_camera
        curve = _base_curve(project.bar_starts) if is_base else curves[camera]
        phase = phase_results.get(
            camera,
            {
                "status": "not_applicable",
                "threshold": project.phase.threshold,
                "flagged": False,
                "windows": [],
            },
        )
        if phase.get("flagged"):
            flagged_windows = [
                window for window in phase.get("windows", []) if window.get("flagged")
            ]
            quality_flags.append(
                {
                    "code": "W_PHASE_CORRELATION",
                    "severity": "quality",
                    "camera": camera,
                    "threshold": project.phase.threshold,
                    "attenuation_gain": project.phase.attenuation_gain,
                    "windows": flagged_windows,
                }
            )
        camera_reports.append(
            {
                "camera": camera,
                "role": "base" if is_base else "auxiliary",
                "source_file": track.path.relative_to(project.root).as_posix()
                if _is_relative_to(track.path, project.root)
                else track.path.name,
                "source_sample_count": int(track.frame_count),
                "source_channels": int(track.channels),
                "source_sha256": track.sha256,
                "declared_gain": project.tracks[camera].gain,
                "effective_gain": float(
                    reference_gain if is_base else effective_gains[camera]
                ),
                "detected_hit_count": int(detection.hits.size),
                "hits_samples": [int(value) for value in detection.hits],
                "hit_intervals_samples": intervals,
                "hit_interval_summary": summary,
                "drift_curve": _curve_report(curve),
                "phase_check": {
                    "status": phase.get("status", "not_applicable"),
                    "threshold": project.phase.threshold,
                    "flagged": bool(phase.get("flagged", False)),
                    "windows": phase.get("windows", []),
                },
            }
        )

    report: dict[str, Any] = {
        "report_kind": "alignment" if complete else "recovery",
        "schema_version": 1,
        "algorithm_version": "drum-align-v1",
        "status": outcome.status,
        "project_digest": project_digest,
        "sample_rate": int(reference.sample_rate),
        "base_camera": project.base_camera,
        "master": (
            {
                "file": display_name(project.master_path, project.root),
                "sample_rate": int(reference.sample_rate),
                "channels": int(reference.channels),
                "sample_count": int(reference.frame_count),
                "sha256": outcome.master_sha256,
            }
            if complete
            else None
        ),
        "bar_starts_samples": [int(value) for value in project.bar_starts],
        "curve_tick_rule": (
            "evenly selected score bars; integer-sample piecewise linear "
            "interpolation; endpoint offsets held constant"
        ),
        "cameras": camera_reports,
        "quality_flags": quality_flags,
        "errors": [],
        "warnings": warnings,
        "recovery_events": outcome.recovery_events,
        "resume_count": int(outcome.resume_count),
        "render": {
            "block_samples": int(project.block_samples),
            "completed_blocks": int(outcome.completed_blocks),
            "total_blocks": int(outcome.total_blocks),
        },
    }
    return report


def _is_relative_to(path, root) -> bool:
    try:
        path.resolve().relative_to(root.resolve())
        return True
    except (OSError, ValueError):
        return False
