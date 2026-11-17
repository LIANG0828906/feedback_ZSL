"""End-to-end alignment pipeline."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from .audio import AudioTrack, read_wav
from .drift import DriftCurve, estimate_curve
from .errors import BusinessError
from .jsonio import atomic_write_json, sha256_bytes, canonical_json_bytes
from .project import LoadedProject, complete_time_based_bars, load_project
from .render import RenderOutcome, render_master
from .report import build_report
from .resample import phase_windows
from .signal import Detection, detect_kicks


@dataclass
class AlignmentResult:
    status: str
    exit_code: int
    report: dict[str, Any]
    report_path: Path
    master_path: Path
    project_digest: str


WarningSink = Callable[[dict[str, Any]], None]


def align_project(
    project_path: str | Path,
    *,
    master_path: str | Path | None = None,
    report_path: str | Path | None = None,
    snapshot_path: str | Path | None = None,
    oneshot: bool = False,
    interrupt_after_blocks: int | None = None,
    warning_sink: WarningSink | None = None,
) -> AlignmentResult:
    project = load_project(Path(project_path))
    if master_path is not None:
        project.master_path = Path(master_path).resolve()
    if report_path is not None:
        project.report_path = Path(report_path).resolve()
    if snapshot_path is not None:
        project.snapshot_path = Path(snapshot_path).resolve()

    loaded: dict[str, AudioTrack] = {}
    for camera in sorted(project.tracks):
        track = read_wav(
            project.tracks[camera].path,
            camera,
            project.sample_rate,
        )
        loaded[camera] = track

    if project.sample_rate is None:
        project.sample_rate = next(iter(loaded.values())).sample_rate
    complete_time_based_bars(project, project.sample_rate)

    reference = loaded[project.base_camera]
    for camera, track in loaded.items():
        if track.sample_rate != reference.sample_rate:
            raise BusinessError(
                "E_SAMPLE_RATE",
                f"sample rate {track.sample_rate} differs from base camera "
                f"{reference.sample_rate}",
                camera=camera,
            )
        if camera != project.base_camera and track.frame_count < reference.frame_count:
            raise BusinessError(
                "E_TRACK_SHORT",
                f"has {track.frame_count} frames but base camera requires at least "
                f"{reference.frame_count}",
                camera=camera,
            )
    for bar in project.bar_starts:
        if bar >= reference.frame_count:
            raise BusinessError(
                "E_BAR_STARTS_RANGE",
                f"bar start {bar} lies outside reference length "
                f"{reference.frame_count}",
            )

    detections: dict[str, Detection] = {}
    for camera in sorted(loaded):
        detection = detect_kicks(
            loaded[camera].mono(),
            reference.sample_rate,
            min_required=len(project.bar_starts),
        )
        detections[camera] = detection
        if detection.hits.size < len(project.bar_starts):
            raise BusinessError(
                "E_DETECTION_INSUFFICIENT",
                f"detected {detection.hits.size} kicks but score requires at least "
                f"{len(project.bar_starts)}",
                camera=camera,
            )

    aux_tracks = {
        camera: track
        for camera, track in loaded.items()
        if camera != project.base_camera
    }
    curves: dict[str, DriftCurve] = {}
    for camera in sorted(aux_tracks):
        curves[camera] = estimate_curve(
            bars=project.bar_starts,
            base=detections[project.base_camera],
            aux=detections[camera],
            base_raw=reference.mono(),
            aux_raw=aux_tracks[camera].mono(),
            sample_rate=reference.sample_rate,
            curve_points=project.curve_points,
            max_offset_seconds=project.max_offset_seconds,
        )

    phase_results: dict[str, dict[str, Any]] = {
        project.base_camera: {
            "status": "not_applicable",
            "threshold": project.phase.threshold,
            "flagged": False,
            "windows": [],
        }
    }
    effective_gains: dict[str, float] = {}
    for camera in sorted(aux_tracks):
        flagged, windows = phase_windows(
            bar_starts=project.bar_starts,
            reference_mono=reference.mono(),
            aux_samples=aux_tracks[camera].samples,
            curve=curves[camera],
            window_samples=project.phase.window_samples,
            pre_roll_samples=project.phase.pre_roll_samples,
            threshold=project.phase.threshold,
        )
        phase_results[camera] = {
            "status": "flagged" if flagged else "pass",
            "threshold": project.phase.threshold,
            "flagged": flagged,
            "windows": windows,
        }
        effective_gains[camera] = project.tracks[camera].gain * (
            project.phase.attenuation_gain if flagged else 1.0
        )

    wav_hashes = {camera: track.sha256 for camera, track in loaded.items()}
    wav_metadata = {
        camera: {
            "sample_rate": int(track.sample_rate),
            "channels": int(track.channels),
            "frame_count": int(track.frame_count),
            "bits_per_sample": 16,
        }
        for camera, track in loaded.items()
    }
    digest_payload = project.digest_payload(
        wav_hashes=wav_hashes,
        wav_metadata=wav_metadata,
    )
    project_digest = sha256_bytes(canonical_json_bytes(digest_payload))

    reference_gain = project.tracks[project.base_camera].gain
    outcome = render_master(
        project=project,
        project_digest=project_digest,
        reference=reference,
        aux_tracks=aux_tracks,
        curves=curves,
        effective_gains=effective_gains,
        reference_gain=reference_gain,
        oneshot=oneshot,
        interrupt_after_blocks=interrupt_after_blocks,
    )

    warnings: list[dict[str, Any]] = []
    if warning_sink is not None:
        for event in outcome.recovery_events:
            warning_sink(event)

    report = build_report(
        project=project,
        project_digest=project_digest,
        reference=reference,
        tracks=loaded,
        detections=detections,
        curves=curves,
        phase_results=phase_results,
        effective_gains=effective_gains,
        reference_gain=reference_gain,
        outcome=outcome,
        warnings=warnings,
    )
    atomic_write_json(project.report_path, report)
    return AlignmentResult(
        status=outcome.status,
        exit_code=0,
        report=report,
        report_path=project.report_path,
        master_path=project.master_path,
        project_digest=project_digest,
    )
