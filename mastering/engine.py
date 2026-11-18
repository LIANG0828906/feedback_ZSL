from __future__ import annotations

import dataclasses
import json
import math
import os
import tempfile
from pathlib import Path
from typing import Any

import numpy as np

from .automation import AutomationPlan
from .errors import Issue, MasteringError
from .limiter import limit_true_peak, true_peak_dbtp
from .loudness import integrated_loudness, loudness_gain_for_target
from .project import TrackSpec, load_project
from .resample import apply_integer_shift, render_track
from .sync import SyncDetection, detect_sync_pulse, plan_sync_alignment
from .wav import (
    WavData,
    quantized_float_view,
    read_wav,
    sha256_file,
    write_wav_atomic,
)


def _json_safe(value: Any) -> Any:
    if isinstance(value, dict):
        return {str(key): _json_safe(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_json_safe(item) for item in value]
    if isinstance(value, (np.floating, float)):
        result = float(value)
        if not math.isfinite(result):
            return None
        return result
    if isinstance(value, (np.integer,)):
        return int(value)
    if isinstance(value, (np.bool_,)):
        return bool(value)
    return value


def _write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp_name: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            dir=path.parent,
            prefix=f".{path.name}.",
            suffix=".partial",
            delete=False,
        ) as handle:
            temp_name = handle.name
            json.dump(
                _json_safe(payload),
                handle,
                ensure_ascii=False,
                indent=2,
                sort_keys=True,
                allow_nan=False,
            )
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
        temp_name = None
    finally:
        if temp_name is not None:
            try:
                os.unlink(temp_name)
            except FileNotFoundError:
                pass


def _verify_loaded_wav(track: TrackSpec, wav: WavData) -> None:
    header = wav.header
    if (
        header.sample_rate != track.header.sample_rate
        or header.channels != track.header.channels
        or header.bits_per_sample != track.header.bits_per_sample
        or header.format_code != track.header.format_code
        or header.frame_count != track.header.frame_count
        or header.data_bytes != track.header.data_bytes
    ):
        raise MasteringError(
            "SOURCE_CHANGED_DURING_MASTERING",
            "track header changed after project validation",
            [
                Issue(
                    "SOURCE_CHANGED_DURING_MASTERING",
                    "file changed after validation",
                    track.track_id,
                )
            ],
        )


def _apply_automation(
    audio: np.ndarray,
    plan: AutomationPlan,
    sample_rate: int,
) -> None:
    frames = audio.shape[0]
    block = 262144
    for start in range(0, frames, block):
        stop = min(frames, start + block)
        times = np.arange(start, stop, dtype=np.float64) / sample_rate
        gains = plan.linear_gain_at(times)
        audio[start:stop] *= gains[:, None]


def _anchor_mapping_report(
    track: TrackSpec,
    output_sample_rate: int,
    sync_shift_samples: int,
) -> dict[str, Any]:
    anchor_times = np.asarray(
        [anchor.target_seconds for anchor in track.drift_map.anchors],
        dtype=np.float64,
    )
    target_frames = np.rint(anchor_times * output_sample_rate).astype(np.int64)
    anchor_source = np.asarray(
        [anchor.source_sample for anchor in track.drift_map.anchors], dtype=np.float64
    )
    mapped_frames = np.rint(
        track.drift_map.source_to_target(anchor_source) * output_sample_rate
    ).astype(np.int64)
    post_sync_frames = mapped_frames + int(sync_shift_samples)
    errors_output_samples = np.abs(post_sync_frames - target_frames).astype(np.float64)
    return {
        "applied_sync_shift_samples": int(sync_shift_samples),
        "max_mapping_error_output_samples": round(
            float(np.max(errors_output_samples)), 12
        ),
        "within_two_output_samples": bool(np.max(errors_output_samples) <= 2.0),
        "anchors": [
            {
                **anchor.as_dict(),
                "mapped_output_frame_before_sync": int(mapped_frame),
                "output_frame_after_sync": int(post_sync_frame),
                "target_output_frame": int(target_frame),
                "mapping_error_output_samples": round(float(error), 12),
            }
            for anchor, mapped_frame, post_sync_frame, target_frame, error in zip(
                track.drift_map.anchors,
                mapped_frames,
                post_sync_frames,
                target_frames,
                errors_output_samples,
            )
        ],
    }


def _actual_output_sync_residual(
    audio: np.ndarray,
    track: TrackSpec,
    detection: SyncDetection,
    output_sample_rate: int,
) -> SyncDetection:
    if not detection.detected or detection.target_output_sample is None:
        return detection
    target = detection.target_output_sample
    search_radius = max(1, int(round(0.05 * output_sample_rate)))
    search_start = max(0, target - search_radius)
    search_end = min(audio.shape[0], target + search_radius + 1)
    onset, warning, _ = detect_sync_pulse(
        audio,
        output_sample_rate,
        {
            "search_start_sample": search_start,
            "search_end_sample": search_end,
            "threshold_ratio": track.sync_spec.get("threshold_ratio", 0.20),
            "min_peak": track.sync_spec.get("min_peak", 1e-5),
        },
        track.track_id,
    )
    if onset is None:
        return dataclasses.replace(
            detection,
            warning=warning,
            aligned=False,
            alignment_residual_samples=None,
        )
    residual = abs(onset - target)
    return dataclasses.replace(
        detection,
        alignment_residual_samples=float(residual),
        aligned=residual <= 1,
        warning=None,
    )


def _measure_loudness_or_none(
    samples: np.ndarray, sample_rate: int
) -> dict[str, Any] | None:
    try:
        return integrated_loudness(samples, sample_rate).as_dict()
    except ValueError as exc:
        return {"unavailable_reason": str(exc)}


def master_project(
    manifest_path: str | os.PathLike[str],
    automation_path: str | os.PathLike[str],
    config_path: str | os.PathLike[str],
    output_path: str | os.PathLike[str],
    report_path: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    """Validate, process, render and write a complete master and processing report."""

    project = load_project(manifest_path, automation_path, config_path)
    config = project.config
    frames = project.output_frames
    mix = np.zeros((frames, config.channels), dtype=np.float64)
    warnings: list[Issue] = []
    track_reports: list[dict[str, Any]] = []

    for track in project.tracks:
        wav = read_wav(track.resolved_path)
        _verify_loaded_wav(track, wav)
        detected_source, detection_warning, detection_metrics = detect_sync_pulse(
            wav.samples,
            wav.sample_rate,
            track.sync_spec,
            track.track_id,
        )
        if detection_warning is not None:
            warnings.append(detection_warning)
        sync_detection = plan_sync_alignment(
            track.track_id,
            detected_source,
            track.drift_map,
            config.sample_rate,
            track.target_sync_seconds,
            detection_warning,
        )
        rendered = render_track(
            wav.samples,
            wav.sample_rate,
            config.sample_rate,
            frames,
            track.drift_map,
            config.channels,
            pan=track.pan,
        )
        if sync_detection.detected:
            rendered = apply_integer_shift(
                rendered, sync_detection.applied_offset_samples
            )
            sync_detection = _actual_output_sync_residual(
                rendered, track, sync_detection, config.sample_rate
            )
        plan = project.automation[track.track_id]
        _apply_automation(rendered, plan, config.sample_rate)
        mix += rendered
        track_reports.append(
            {
                "id": track.track_id,
                "name": track.name,
                "file": track.relative_file,
                "declared": {
                    "sample_rate": track.declared_sample_rate,
                    "bit_depth": track.declared_bit_depth,
                    "channels": track.declared_channels,
                },
                "actual_header": track.header.as_dict(),
                "output_frames": frames,
                "pan": track.pan,
                "drift": {
                    **track.drift_map.summary(),
                    "verification": _anchor_mapping_report(
                        track,
                        config.sample_rate,
                        sync_detection.applied_offset_samples,
                    ),
                },
                "sync": {
                    **sync_detection.as_dict(),
                    "detection_metrics": detection_metrics,
                    "target_time_seconds": track.target_sync_seconds,
                    "search_window": track.sync_spec,
                },
                "automation": plan.summary(),
            }
        )

    pre_limiter_loudness = _measure_loudness_or_none(mix, config.sample_rate)
    loudness_report: dict[str, Any] = {
        "before_calibration": pre_limiter_loudness,
        "target": None,
        "calibration": None,
    }
    if config.target_lufs is not None:
        mix, calibration = loudness_gain_for_target(
            mix, config.sample_rate, config.target_lufs
        )
        loudness_report["target"] = round(config.target_lufs, 9)
        loudness_report["calibration"] = calibration
        loudness_report["after_calibration_before_limiter"] = _measure_loudness_or_none(
            mix, config.sample_rate
        )

    limiter_result = limit_true_peak(
        mix,
        config.sample_rate,
        config.true_peak_dbtp,
        lookahead_ms=config.limiter_lookahead_ms,
        release_ms=config.limiter_release_ms,
        max_iterations=config.limiter_max_iterations,
        safety_margin_db=config.limiter_safety_margin_db,
        oversample=config.true_peak_oversample,
    )
    limited = limiter_result.samples
    decoded_view = quantized_float_view(
        limited, config.wave_format, config.bits_per_sample
    )
    decoded_dbtp, decoded_peak = true_peak_dbtp(
        decoded_view, config.true_peak_oversample
    )
    target_ceiling = 10.0 ** (config.true_peak_dbtp / 20.0)
    if decoded_peak > target_ceiling:
        # Quantization can move a sample onto the next integer step. Reserve a
        # tiny final margin and re-quantize before the file is committed.
        trim = (target_ceiling / decoded_peak) * 10.0 ** (-0.01 / 20.0)
        limited *= trim
        decoded_view = quantized_float_view(
            limited, config.wave_format, config.bits_per_sample
        )
        decoded_dbtp, decoded_peak = true_peak_dbtp(
            decoded_view, config.true_peak_oversample
        )
        limiter_result.report["quantization_trim_db"] = round(
            20.0 * math.log10(trim), 9
        )
    else:
        limiter_result.report["quantization_trim_db"] = 0.0
    if decoded_peak > target_ceiling + 1e-12:
        raise MasteringError(
            "TRUE_PEAK_CEILING_VIOLATION",
            "quantized master exceeds the configured true-peak ceiling",
            [
                Issue(
                    "TRUE_PEAK_CEILING_VIOLATION",
                    "final true peak exceeds ceiling",
                    details={
                        "ceiling_dbtp": config.true_peak_dbtp,
                        "actual_dbtp": decoded_dbtp,
                    },
                )
            ],
        )
    limiter_result.report["output_true_peak_dbtp"] = round(decoded_dbtp, 9)
    limiter_result.report["ceiling_respected"] = True

    final_loudness = _measure_loudness_or_none(decoded_view, config.sample_rate)
    final_wav = write_wav_atomic(
        output_path,
        limited,
        config.sample_rate,
        config.wave_format,
        config.bits_per_sample,
    )
    final_wav["path"] = str(Path(output_path).resolve())

    report: dict[str, Any] = {
        "schema_version": 1,
        "success": True,
        "project": {
            "name": project.project_name,
            "manifest": str(project.manifest_path),
            "automation": str(project.automation_path),
            "output_config": str(project.config_path),
            "duration_seconds": project.duration_seconds,
            "output_frames": frames,
            "sample_rate": config.sample_rate,
            "channels": config.channels,
            "format": config.output_format_name,
        },
        "tracks": track_reports,
        "warnings": [warning.as_dict() for warning in warnings],
        "mix": {
            "pre_limiter_true_peak_dbtp": limiter_result.report[
                "input_true_peak_dbtp"
            ],
            "pre_limiter_loudness": pre_limiter_loudness,
        },
        "loudness": {
            **loudness_report,
            "final_master": final_loudness,
        },
        "limiter": limiter_result.report,
        "wav": final_wav,
        "verification": {
            "output_frame_count_preserved": bool(
                final_wav["frame_count"] == frames
            ),
            "wav_header_matches_report": bool(
                final_wav["sample_rate"] == config.sample_rate
                and final_wav["channels"] == config.channels
                and final_wav["bits_per_sample"] == config.bits_per_sample
                and final_wav["frame_count"] == frames
                and final_wav["data_bytes"]
                == frames
                * config.channels
                * (config.bits_per_sample // 8)
            ),
            "true_peak_within_ceiling": True,
            "artifact_sha256": final_wav["sha256"],
        },
    }
    if report_path is not None:
        _write_json_atomic(Path(report_path), report)
    return _json_safe(report)

