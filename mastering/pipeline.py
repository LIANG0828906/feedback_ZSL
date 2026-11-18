from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from . import __version__
from .automation import apply_automation_smoothing
from .errors import DetectionFailed, HardError, Issue, WavFormatError
from .limiter import limit_true_peak, true_peak_dbtp
from .loudness import integrated_loudness
from .manifest import ProjectSpec, TrackSpec, load_manifest
from .resample import (
    apply_integer_shift,
    fit_length,
    map_channels,
    resample_to_rate,
    source_to_output_sample,
    warp_to_output_timeline,
)
from .sync import detect_sync_pulse_start, integer_alignment
from .wavio import (
    encode_wav,
    inspect_wav,
    quantize_samples,
    read_wav,
    write_bytes_atomic,
    write_json_atomic,
)


@dataclass(frozen=True)
class ProcessingResult:
    status: str
    report: dict[str, Any]
    master_path: Path
    report_path: Path

    @property
    def ok(self) -> bool:
        return self.status == "ok"


def _db(value: float) -> float:
    if value <= 0:
        return float("-inf")
    return 20.0 * float(math.log10(value))


def _finite_or_none(value: float) -> float | None:
    return float(value) if math.isfinite(value) else None


def _base_report(project: ProjectSpec | None = None) -> dict[str, Any]:
    result: dict[str, Any] = {
        "engine": {"name": "deterministic-offline-mastering", "version": __version__},
        "hard_errors": [],
        "quality_warnings": [],
        "status": "error",
        "tracks": [],
    }
    if project is not None:
        result["project"] = {
            "name": project.name,
            "seed": project.seed,
            "output_seconds": project.output_seconds,
        }
    return result


def _write_failure_report(
    report_path: Path,
    project: ProjectSpec | None,
    hard_errors: list[Issue],
    warnings: list[Issue] | None = None,
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    report = _base_report(project)
    report["hard_errors"] = [issue.as_dict() for issue in hard_errors]
    report["quality_warnings"] = [issue.as_dict() for issue in (warnings or [])]
    report["master_written"] = False
    if extra:
        report.update(extra)
    write_json_atomic(report_path, report)
    return report


def _inspect_track_headers(project: ProjectSpec) -> list[Issue]:
    issues: list[Issue] = []
    for track in project.tracks:
        if not track.path.is_file():
            issues.append(
                Issue(
                    "TRACK_FILE_NOT_FOUND",
                    f"track file does not exist: {track.file}",
                    track.name,
                    {"file": track.file},
                )
            )
            continue
        try:
            info = inspect_wav(track.path)
        except WavFormatError as exc:
            issues.append(
                Issue(
                    "TRACK_READ_ERROR",
                    f"cannot parse WAV header: {exc}",
                    track.name,
                    {"file": track.file},
                )
            )
            continue
        if info.sample_rate != track.sample_rate:
            issues.append(
                Issue(
                    "SAMPLE_RATE_MISMATCH",
                    f"declared {track.sample_rate} Hz but WAV header is {info.sample_rate} Hz",
                    track.name,
                    {"declared": track.sample_rate, "actual": info.sample_rate},
                )
            )
        if info.bits_per_sample != track.bits_per_sample:
            issues.append(
                Issue(
                    "BIT_DEPTH_MISMATCH",
                    f"declared {track.bits_per_sample}-bit but WAV header is {info.bits_per_sample}-bit",
                    track.name,
                    {"declared": track.bits_per_sample, "actual": info.bits_per_sample},
                )
            )
        if info.channels != track.channels:
            issues.append(
                Issue(
                    "CHANNEL_COUNT_MISMATCH",
                    f"declared {track.channels} channel(s) but WAV header has {info.channels}",
                    track.name,
                    {"declared": track.channels, "actual": info.channels},
                )
            )
        for anchor in track.drift_anchors:
            if anchor.source_sample >= info.frames:
                issues.append(
                    Issue(
                        "INVALID_DRIFT_ANCHOR",
                        "drift anchor source_sample lies beyond the source WAV data",
                        track.name,
                        {
                            "source_sample": anchor.source_sample,
                            "source_frames": info.frames,
                        },
                    )
                )
    return issues


def _process_track(
    track: TrackSpec,
    project: ProjectSpec,
    warnings: list[Issue],
) -> tuple[np.ndarray, dict[str, Any]]:
    source, info = read_wav(track.path)
    if not np.all(np.isfinite(source)):
        raise HardError(
            [
                Issue(
                    "TRACK_NONFINITE_SAMPLE",
                    "track contains NaN or infinite floating-point samples",
                    track.name,
                )
            ]
        )

    detection_failed = False
    detected_source: float | None = None
    shift = 0
    residual: float | None = None
    try:
        expected_source = (
            project.reference_pulse_output_sample
            * track.sample_rate
            / project.output.sample_rate
        )
        detected_source, _ = detect_sync_pulse_start(
            source,
            track.sample_rate,
            expected_sample=expected_source,
        )
    except DetectionFailed as exc:
        detection_failed = True
        warnings.append(
            Issue(
                "DETECTION_FAILED",
                f"sync pulse detection failed; track kept with zero integer shift: {exc}",
                track.name,
            )
        )

    resampled = resample_to_rate(source, track.sample_rate, project.output.sample_rate)
    if track.drift_anchors:
        aligned = warp_to_output_timeline(
            resampled,
            track.drift_anchors,
            track.sample_rate,
            project.output.sample_rate,
            project.output_frames,
        )
    else:
        aligned = fit_length(resampled, project.output_frames)

    detected_output: float | None = None
    if detected_source is not None:
        predicted_output = source_to_output_sample(
            detected_source,
            track.drift_anchors,
            track.sample_rate,
            project.output.sample_rate,
        )
        try:
            detected_output, _ = detect_sync_pulse_start(
                aligned,
                project.output.sample_rate,
                expected_sample=project.reference_pulse_output_sample,
            )
        except DetectionFailed:
            detected_output = predicted_output
            warnings.append(
                Issue(
                    "DETECTION_FAILED",
                    "post-resample sync onset check failed; using source-domain prediction",
                    track.name,
                )
            )
        shift, residual = integer_alignment(
            detected_output, project.reference_pulse_output_sample
        )
        aligned = apply_integer_shift(aligned, shift, project.output_frames)
    else:
        aligned = fit_length(aligned, project.output_frames)

    seconds = np.arange(project.output_frames, dtype=np.float64) / float(
        project.output.sample_rate
    )
    gain_db, smoothing_segments = apply_automation_smoothing(
        seconds, track.automation, project.automation_config
    )
    gain = np.power(10.0, gain_db / 20.0)
    aligned *= gain[:, None]

    peak = float(np.max(np.abs(aligned))) if aligned.size else 0.0
    track_report: dict[str, Any] = {
        "detected_pulse_source_sample": (
            None if detected_source is None else float(detected_source)
        ),
        "detected_pulse_output_sample": (
            None if detected_output is None else float(detected_output)
        ),
        "drift_anchor_count": len(track.drift_anchors),
        "integer_shift_output_samples": int(shift),
        "peak_after_automation": peak,
        "pulse_residual_output_samples": residual,
        "smoothed_segment_count": len(smoothing_segments),
        "smoothing_segments": [segment.as_dict() for segment in smoothing_segments],
        "source_bits_per_sample": info.bits_per_sample,
        "source_channels": info.channels,
        "source_frames": info.frames,
        "source_rate": track.sample_rate,
        "sync_detection": "failed" if detection_failed else "ok",
        "total_smoothed_seconds": float(
            sum(segment.duration_seconds for segment in smoothing_segments)
        ),
        "track": track.name,
    }
    return aligned, track_report


def _finalize_quantized_peak(
    samples: np.ndarray,
    bits_per_sample: int,
    ceiling_dbtp: float,
) -> tuple[np.ndarray, float]:
    working = np.asarray(samples, dtype=np.float64)
    represented = quantize_samples(working, bits_per_sample)
    for _ in range(4):
        measured = true_peak_dbtp(represented)
        if measured <= ceiling_dbtp:
            return represented, measured
        target = float(10.0 ** ((ceiling_dbtp - 0.005) / 20.0))
        current = float(10.0 ** (measured / 20.0))
        if current <= 0:
            break
        working *= min(1.0, target / current)
        represented = quantize_samples(working, bits_per_sample)
    return represented, true_peak_dbtp(represented)


def _success_report(
    project: ProjectSpec,
    master_path: Path,
    track_reports: list[dict[str, Any]],
    mix_peak_linear: float,
    pre_limiter_tp: float,
    loudness: dict[str, Any],
    limiter_result,
    final_true_peak: float,
    warnings: list[Issue],
) -> dict[str, Any]:
    report = _base_report(project)
    report.update(
        {
            "final_integrated_loudness_lufs": loudness.get(
                "final_integrated_loudness_lufs"
            ),
            "hard_errors": [],
            "limiter": limiter_result.report(
                project.output_frames, project.output_frames, project.output.true_peak_limit_dbtp
            ),
            "loudness": loudness,
            "master_written": True,
            "mix": {
                "frames": project.output_frames,
                "pre_limiter_sample_peak_dbfs": _finite_or_none(_db(mix_peak_linear)),
                "pre_limiter_true_peak_dbtp": _finite_or_none(pre_limiter_tp),
            },
            "output": {
                "bits_per_sample": project.output.bits_per_sample,
                "channels": project.output.channels,
                "frames": project.output_frames,
                "path": str(master_path),
                "sample_rate": project.output.sample_rate,
                "true_peak_dbtp": _finite_or_none(final_true_peak),
            },
            "quality_warnings": [issue.as_dict() for issue in warnings],
            "status": "ok",
            "tracks": track_reports,
        }
    )
    return report


def process_project(
    manifest_path: str | Path,
    master_path: str | Path,
    report_path: str | Path | None = None,
) -> ProcessingResult:
    master = Path(master_path)
    report_file = Path(report_path) if report_path is not None else master.with_name("report.json")
    warnings: list[Issue] = []

    try:
        project = load_manifest(manifest_path)
    except HardError as exc:
        report = _write_failure_report(report_file, None, exc.issues, warnings)
        return ProcessingResult("error", report, master, report_file)

    header_errors = _inspect_track_headers(project)
    if header_errors:
        report = _write_failure_report(report_file, project, header_errors, warnings)
        return ProcessingResult("error", report, master, report_file)

    mix = np.zeros(
        (project.output_frames, project.output.channels), dtype=np.float64
    )
    track_reports: list[dict[str, Any]] = []
    try:
        for track in project.tracks:
            processed, track_report = _process_track(track, project, warnings)
            mapped = map_channels(processed, project.output.channels)
            mix += mapped
            track_reports.append(track_report)
    except HardError as exc:
        report = _write_failure_report(report_file, project, exc.issues, warnings)
        return ProcessingResult("error", report, master, report_file)
    except (OSError, WavFormatError, ValueError) as exc:
        issue = Issue("PROCESSING_ERROR", str(exc))
        report = _write_failure_report(report_file, project, [issue], warnings)
        return ProcessingResult("error", report, master, report_file)

    pre_gain_loudness = integrated_loudness(mix, project.output.sample_rate)
    applied_gain_db = 0.0
    post_gain_loudness = pre_gain_loudness
    deviation = 0.0
    target = project.output.target_loudness_lufs

    if target is not None:
        if not math.isfinite(pre_gain_loudness):
            issue = Issue(
                "LOUDNESS_UNMEASURABLE",
                "integrated loudness is unmeasurable for the mixed program",
                details={"target_lufs": target},
            )
            loudness = {
                "applied_gain_db": None,
                "deviation_lu": None,
                "measured_post_gain_lufs": None,
                "measured_pre_gain_lufs": None,
                "target_lufs": target,
                "tolerance_lu": project.output.loudness_tolerance_lu,
            }
            report = _write_failure_report(
                report_file, project, [issue], warnings, {"loudness": loudness}
            )
            return ProcessingResult("error", report, master, report_file)

        applied_gain_db = float(target - pre_gain_loudness)
        mix *= float(10.0 ** (applied_gain_db / 20.0))
        post_gain_loudness = integrated_loudness(mix, project.output.sample_rate)
        deviation = (
            float(post_gain_loudness - target)
            if math.isfinite(post_gain_loudness)
            else float("inf")
        )
        if not math.isfinite(deviation) or abs(deviation) > project.output.loudness_tolerance_lu:
            issue = Issue(
                "LOUDNESS_OUT_OF_TOLERANCE",
                "loudness normalization did not converge inside the configured tolerance",
                details={
                    "measured_lufs": _finite_or_none(post_gain_loudness),
                    "target_lufs": target,
                    "tolerance_lu": project.output.loudness_tolerance_lu,
                    "deviation_lu": _finite_or_none(deviation),
                },
            )
            loudness = {
                "applied_gain_db": applied_gain_db,
                "deviation_lu": _finite_or_none(deviation),
                "measured_post_gain_lufs": _finite_or_none(post_gain_loudness),
                "measured_pre_gain_lufs": _finite_or_none(pre_gain_loudness),
                "target_lufs": target,
                "tolerance_lu": project.output.loudness_tolerance_lu,
            }
            report = _write_failure_report(
                report_file, project, [issue], warnings, {"loudness": loudness}
            )
            return ProcessingResult("error", report, master, report_file)

    loudness_report = {
        "applied_gain_db": applied_gain_db,
        "deviation_lu": _finite_or_none(deviation),
        "measured_post_gain_lufs": _finite_or_none(post_gain_loudness),
        "measured_pre_gain_lufs": _finite_or_none(pre_gain_loudness),
        "target_lufs": target,
        "tolerance_lu": project.output.loudness_tolerance_lu,
    }

    mix_peak = float(np.max(np.abs(mix))) if mix.size else 0.0
    limited = limit_true_peak(
        mix,
        project.output.sample_rate,
        project.output.true_peak_limit_dbtp,
    )
    pre_limiter_tp = limited.input_true_peak_dbtp
    quantized, final_tp = _finalize_quantized_peak(
        limited.samples,
        project.output.bits_per_sample,
        project.output.true_peak_limit_dbtp,
    )
    if math.isnan(final_tp) or (
        math.isfinite(final_tp) and final_tp > project.output.true_peak_limit_dbtp
    ):
        issue = Issue(
            "TRUE_PEAK_LIMIT_EXCEEDED",
            "final quantized output exceeds the configured true-peak ceiling",
            details={
                "ceiling_dbtp": project.output.true_peak_limit_dbtp,
                "measured_dbtp": _finite_or_none(final_tp),
            },
        )
        report = _write_failure_report(report_file, project, [issue], warnings)
        return ProcessingResult("error", report, master, report_file)

    payload, represented = encode_wav(
        quantized,
        project.output.sample_rate,
        project.output.bits_per_sample,
    )

    final_loudness = integrated_loudness(represented, project.output.sample_rate)
    loudness_report["final_integrated_loudness_lufs"] = _finite_or_none(final_loudness)

    try:
        write_bytes_atomic(master, payload)
    except OSError as exc:
        issue = Issue("OUTPUT_WRITE_FAILED", f"failed to write master WAV: {exc}")
        report = _write_failure_report(report_file, project, [issue], warnings)
        return ProcessingResult("error", report, master, report_file)

    report = _success_report(
        project,
        master,
        track_reports,
        mix_peak,
        pre_limiter_tp,
        loudness_report,
        limited,
        final_tp,
        warnings,
    )
    try:
        write_json_atomic(report_file, report)
    except OSError:
        try:
            master.unlink()
        except FileNotFoundError:
            pass
        raise
    return ProcessingResult("ok", report, master, report_file)
