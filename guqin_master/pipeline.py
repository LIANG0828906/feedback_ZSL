from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .alignment import (
    AlignmentResult,
    align_channels,
    apply_alignment_parameters,
)
from .checkpoint import (
    build_parameters,
    discard_progress,
    initialize_snapshot,
    parameters_sha256,
    read_partial_master,
    render_with_checkpoint,
    validate_existing_snapshot,
    verify_partial_blocks,
)
from .decoupling import (
    DecouplingResult,
    apply_decoupling_parameters,
    decouple_channels,
)
from .errors import BusinessError
from .manifest import ProjectSpec, load_and_validate_channels, load_manifest
from .mastering import (
    MasteringStats,
    StereoMixer,
    integrated_loudness,
    master_compliance,
    true_peak_db,
)
from .reporting import json_float, write_json_atomic
from .wavio import read_wav_mono, write_wav


def _preprocess(
    project: ProjectSpec,
    raw_channels: dict[str, np.ndarray],
) -> tuple[DecouplingResult, AlignmentResult, dict[str, Any]]:
    decoupled = decouple_channels(project, raw_channels)
    aligned = align_channels(project, decoupled.channels)
    parameters = build_parameters(
        decoupled.parameter_events,
        tuple(item.as_parameter() for item in aligned.alignments),
    )
    return decoupled, aligned, parameters


def _resume_apply(
    project: ProjectSpec,
    raw_channels: dict[str, np.ndarray],
    snapshot: dict[str, Any],
    state_directory: Path,
    recovery_path: Path,
) -> tuple[DecouplingResult, AlignmentResult]:
    parameters = snapshot.get("parameters")
    if not isinstance(parameters, dict):
        discard_progress(
            state_directory / "progress.json",
            state_directory / "master.partial.f64",
            recovery_path,
        )
        raise BusinessError(
            "E_RESUME_MISMATCH",
            "stored rendering parameters are missing",
            "project",
            {"progress_discarded": True},
        )
    try:
        decoupled_channels, decoupling_events = apply_decoupling_parameters(
            project,
            raw_channels,
            tuple(parameters["decoupling"]),
        )
        aligned_channels, alignments = apply_alignment_parameters(
            project,
            decoupled_channels,
            tuple(parameters["alignment"]),
        )
    except (KeyError, TypeError, ValueError) as exc:
        discard_progress(
            state_directory / "progress.json",
            state_directory / "master.partial.f64",
            recovery_path,
        )
        raise BusinessError(
            "E_RESUME_MISMATCH",
            "stored rendering parameters cannot be applied",
            "project",
            {"progress_discarded": True, "error": str(exc)},
        ) from exc
    reference = project.reference_channel.name
    decoupled = DecouplingResult(
        channels=decoupled_channels,
        events=decoupling_events,
        noise_reference=reference,
        noise_floor_rms=float(decoupling_events[0].noise_floor_rms)
        if decoupling_events
        else 0.0,
        modified_targets=tuple(
            sorted(
                {
                    event.target
                    for event in decoupling_events
                    if event.detected
                }
            )
        ),
        untouched_channels=tuple(
            sorted(
                {
                    channel.name
                    for channel in project.channels
                    if channel.name
                    not in {
                        event.target for event in decoupling_events if event.detected
                    }
                }
            )
        ),
        parameter_events=tuple(parameters["decoupling"]),
    )
    aligned = AlignmentResult(channels=aligned_channels, alignments=alignments)
    return decoupled, aligned


def _segment_statistics(
    project: ProjectSpec,
    master: np.ndarray,
) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    for segment in project.segments:
        start = int(round(segment.start_seconds * project.sample_rate))
        stop = min(
            project.total_samples,
            int(round(segment.end_seconds * project.sample_rate)),
        )
        loudness = integrated_loudness(master[start:stop], project.sample_rate)
        peak = true_peak_db(master[start:stop], project.sample_rate)
        result.append(
            {
                "name": segment.name,
                "start_sample": start,
                "end_sample": stop,
                "loudness_lufs": json_float(loudness),
                "true_peak_dbtp": json_float(peak),
            }
        )
    return result


def _read_back_master(
    project: ProjectSpec,
    master_path: Path,
) -> tuple[np.ndarray, dict[str, Any]]:
    samples, info = read_wav_mono(master_path, "master")
    if (
        info.sample_rate != project.sample_rate
        or info.bit_depth != project.bit_depth
        or info.channels != 2
        or info.frames != project.total_samples
    ):
        raise BusinessError(
            "E_IO",
            "rendered master WAV does not match the requested format",
            "master",
            {
                "sample_rate": info.sample_rate,
                "bit_depth": info.bit_depth,
                "channels": info.channels,
                "frames": info.frames,
            },
        )
    return np.asarray(samples), {
        "file_sha256": info.sha256,
        "sample_rate": info.sample_rate,
        "bit_depth": info.bit_depth,
        "channels": info.channels,
        "frames": info.frames,
    }


def _compliance_report(
    project: ProjectSpec,
    master: np.ndarray,
    mastering: MasteringStats,
    decoupled: DecouplingResult,
    aligned: AlignmentResult,
    contributions: dict[str, float],
    audio_metadata: dict[str, Any],
    parameters_digest: str,
) -> dict[str, Any]:
    loudness = integrated_loudness(master, project.sample_rate)
    peak = true_peak_db(master, project.sample_rate)
    loudness_delta = loudness - project.target_loudness_lufs
    loudness_ok = abs(loudness_delta) <= 0.5
    peak_ok = peak <= project.true_peak_limit_dbtp + 1e-6
    max_residual = max(
        (abs(value) for item in aligned.alignments for value in item.residual_samples),
        default=0,
    )
    decoupling_ok = all(
        event.detected
        and event.attenuation_db is not None
        and event.attenuation_db >= project.decoupling.minimum_attenuation_db
        and (
            event.unrelated_peak_loss_db is None
            or event.unrelated_peak_loss_db
            <= project.decoupling.max_unrelated_peak_loss_db + 1e-9
        )
        for event in decoupled.events
    )
    limiter_enabled = mastering.limiter.triggered_samples > 0
    return {
        "schema_version": 1,
        "business_code": "OK",
        "status": "COMPLETE",
        "project_id": project.project_id,
        "manifest_sha256": project.manifest_sha256,
        "parameters_sha256": parameters_digest,
        "audio": {
            "path": "master.wav",
            **audio_metadata,
            "samples": project.total_samples,
        },
        "loudness": {
            "standard": "ITU-R BS.1770",
            "integrated_lufs": json_float(loudness),
            "target_lufs": project.target_loudness_lufs,
            "delta_lu": json_float(loudness_delta),
            "tolerance_lu": 0.5,
            "compliant": loudness_ok,
        },
        "true_peak": {
            "oversampling_factor": 4,
            "max_dbtp": json_float(peak),
            "limit_dbtp": project.true_peak_limit_dbtp,
            "compliant": peak_ok,
        },
        "limiter": {
            "enabled": limiter_enabled,
            "triggered_samples": mastering.limiter.triggered_samples,
            "max_gain_reduction_db": json_float(
                mastering.limiter.max_gain_reduction_db
            ),
            "processing_delay_samples": mastering.limiter.processing_delay_samples,
            "sample_count_preserved": master.shape[0] == project.total_samples,
        },
        "decoupling": {
            "noise_reference": decoupled.noise_reference,
            "noise_floor_rms": json_float(decoupled.noise_floor_rms),
            "minimum_attenuation_db": project.decoupling.minimum_attenuation_db,
            "max_unrelated_peak_loss_db": project.decoupling.max_unrelated_peak_loss_db,
            "compliant": decoupling_ok,
            "modified_targets": list(decoupled.modified_targets),
            "untouched_channels": list(decoupled.untouched_channels),
            "events": [event.as_dict() for event in decoupled.events],
        },
        "alignment": {
            "reference_times_seconds": list(project.reference_times),
            "maximum_allowed_residual_samples": 1,
            "max_abs_residual_samples": max_residual,
            "compliant": max_residual <= 1,
            "channels": [item.as_dict() for item in aligned.alignments],
        },
        "segments": _segment_statistics(project, master),
        "contributions": {
            "normalized_energy_fraction": contributions,
        },
        "checks": {
            "all_compliant": bool(
                loudness_ok
                and peak_ok
                and decoupling_ok
                and max_residual <= 1
                and master.shape[0] == project.total_samples
            ),
            "sample_count_preserved": master.shape[0] == project.total_samples,
        },
    }


def run_project(
    manifest_path: Path,
    output_directory: Path,
    progress_reporter: Callable[[dict[str, Any]], None] | None = None,
) -> dict[str, Any]:
    project = load_manifest(manifest_path)
    raw_channels, input_metadata = load_and_validate_channels(project)
    output_directory.mkdir(parents=True, exist_ok=True)
    state_directory = output_directory / "state"
    state_directory.mkdir(parents=True, exist_ok=True)
    snapshot_path = state_directory / "progress.json"
    partial_path = state_directory / "master.partial.f64"
    recovery_path = state_directory / "recovery_report.json"

    fresh_decoupled, fresh_aligned, fresh_parameters = _preprocess(
        project, raw_channels
    )
    current_parameters_digest = parameters_sha256(fresh_parameters)
    validation = validate_existing_snapshot(
        project,
        input_metadata,
        current_parameters_digest,
        snapshot_path,
        partial_path,
        recovery_path,
    )
    if validation.snapshot is not None:
        snapshot = validation.snapshot
        verify_partial_blocks(
            snapshot,
            partial_path,
            project.block_size,
            project.total_samples,
        )
        decoupled, aligned = _resume_apply(
            project,
            raw_channels,
            snapshot,
            state_directory,
            recovery_path,
        )
    else:
        decoupled = fresh_decoupled
        aligned = fresh_aligned
        snapshot = initialize_snapshot(
            project,
            input_metadata,
            fresh_parameters,
            state_directory,
        )

    mixer = StereoMixer(project, aligned.channels)
    render_result = render_with_checkpoint(
        project,
        mixer,
        snapshot,
        state_directory,
        recovery_path,
        progress_reporter,
    )
    if render_result.status == "interrupted":
        return {
            "business_code": "OK",
            "status": "INTERRUPTED",
            "completed_blocks": render_result.completed_blocks,
            "total_blocks": render_result.total_blocks,
            "committed_samples": render_result.committed_samples,
            "recovery_report": recovery_path.relative_to(output_directory).as_posix(),
        }

    raw_master = read_partial_master(project, partial_path)
    mastered, stats = master_compliance(
        raw_master,
        project.sample_rate,
        project.target_loudness_lufs,
        project.true_peak_limit_dbtp,
    )
    master_path = output_directory / "master.wav"
    write_wav(master_path, mastered, project.sample_rate, project.bit_depth)
    round_trip_master, audio_metadata = _read_back_master(project, master_path)
    contributions = mixer.contributions(project.block_size)
    stats.contributions = contributions
    report = _compliance_report(
        project,
        round_trip_master,
        stats,
        decoupled,
        aligned,
        contributions,
        audio_metadata,
        current_parameters_digest,
    )
    report_path = output_directory / "master_report.json"
    write_json_atomic(report_path, report)
    if recovery_path.exists():
        recovery_path.unlink()
    return {
        "business_code": "OK",
        "status": "COMPLETE",
        "master": "master.wav",
        "report": "master_report.json",
        "master_sha256": audio_metadata["file_sha256"],
        "report_sha256": hashlib.sha256(
            (output_directory / "master_report.json").read_bytes()
        ).hexdigest(),
        "checks": report["checks"],
    }
