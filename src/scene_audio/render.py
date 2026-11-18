from __future__ import annotations

import math
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .audio_io import read_wav, sha256_file, write_wav_pcm24
from .camera import CameraPath
from .detection import detect_onset
from .errors import BusinessError
from .jsonio import write_json_atomic
from .mastering import master_audio
from .resample import (
    expected_source_sample,
    resample_track,
    source_sample_to_target_time,
)
from .reverb import ReverbSchedule, StereoReverb
from .scene import EventSpec, Scene, TrackSpec, load_scene
from .snapshot import (
    SNAPSHOT_SCHEMA_VERSION,
    discard_snapshot,
    load_snapshot,
    save_snapshot,
)
from .spatial import occluder_layers, render_spatial_event


@dataclass
class PreparedEvent:
    track: TrackSpec
    spec: EventSpec
    start_sample: int
    mono: np.ndarray
    stereo: np.ndarray
    ild_db: np.ndarray
    itd_us: np.ndarray
    azimuth_rad: np.ndarray
    elevation_rad: np.ndarray
    detection: dict[str, Any]
    quality_warning: dict[str, Any] | None = None

    @property
    def end_sample(self) -> int:
        return self.start_sample + int(self.mono.shape[0])

    @property
    def energy(self) -> float:
        return float(np.sum(self.stereo * self.stereo, dtype=np.float64))


@dataclass
class RenderResult:
    exit_code: int
    interrupted: bool
    report_path: Path | None
    master_path: Path | None
    report: dict[str, Any] | None = None
    error: dict[str, Any] | None = None


def _db_to_linear(value: float) -> float:
    return float(10.0 ** (value / 20.0))


def _extract_shifted_event(
    full_track: np.ndarray,
    source_start: int,
    length: int,
    total_samples: int,
) -> np.ndarray:
    mono_source = (
        np.mean(full_track, axis=1)
        if full_track.ndim == 2
        else np.asarray(full_track, dtype=np.float64)
    )
    result = np.zeros(length, dtype=np.float64)
    source_begin = max(0, source_start)
    source_end = min(total_samples, source_start + length)
    if source_end > source_begin:
        count = min(source_end - source_begin, length)
        result[:count] = mono_source[source_begin : source_begin + count]
    return result


def _prepare_events(
    scene: Scene,
    tracks_resampled: dict[str, np.ndarray],
) -> tuple[list[PreparedEvent], list[dict[str, Any]], list[dict[str, Any]]]:
    prepared: list[PreparedEvent] = []
    alignment: list[dict[str, Any]] = []
    warnings: list[dict[str, Any]] = []
    output = scene.output
    camera = CameraPath(scene.camera_path, scene.camera_up)

    for track in scene.tracks:
        if track.samples is None:
            raise BusinessError(
                "E_TRACK_DECODE",
                "track samples were not loaded",
                track=track.name,
            )
        full_output = tracks_resampled[track.name]
        source_mono = np.mean(track.samples, axis=1)
        for event in sorted(track.events, key=lambda item: (item.start_time, item.id)):
            expected_source = expected_source_sample(
                track.sync_anchors,
                event.start_time,
                track.declared_sample_rate,
            )
            detection = detect_onset(
                source_mono,
                track.declared_sample_rate,
                expected_source,
                event.detection_threshold,
                event.detection_search_ms,
            )
            desired_start = int(round(event.start_time * output.sample_rate))
            if detection.success and detection.onset_sample is not None:
                detected_target = source_sample_to_target_time(
                    track.sync_anchors,
                    float(detection.onset_sample),
                    track.declared_sample_rate,
                )
                detected_output = int(round(detected_target * output.sample_rate))
                output_offset = desired_start - detected_output
                extraction_start = detected_output
                status = "OK"
                warning = None
            else:
                detected_target = None
                detected_output = desired_start
                output_offset = 0
                extraction_start = desired_start
                status = "DETECTION_FAILED"
                warning = {
                    "code": "DETECTION_FAILED",
                    "message": "event onset could not be detected; declared time used",
                    "track": track.name,
                    "event_id": event.id,
                }
                warnings.append(warning)

            event_length = max(1, int(round(event.duration * output.sample_rate)))
            mono = _extract_shifted_event(
                full_output,
                extraction_start,
                event_length,
                output.total_samples,
            )
            gain = _db_to_linear(track.gain_db + event.gain_db)
            mono = mono * gain
            times = (
                np.arange(event_length, dtype=np.float64) + desired_start
            ) / float(output.sample_rate)
            spatial = render_spatial_event(
                mono,
                times,
                event.source_position,
                camera,
                scene.spatial_limits,
                output.sample_rate,
            )
            prepared.append(
                PreparedEvent(
                    track=track,
                    spec=event,
                    start_sample=desired_start,
                    mono=mono,
                    stereo=spatial.stereo,
                    ild_db=spatial.ild_db,
                    itd_us=spatial.itd_us,
                    azimuth_rad=spatial.azimuth_rad,
                    elevation_rad=spatial.elevation_rad,
                    detection={
                        "expected_source_sample": expected_source,
                        "detected_source_sample": (
                            int(detection.onset_sample)
                            if detection.onset_sample is not None
                            else None
                        ),
                        "detected_target_time": detected_target,
                        "search_start_sample": int(detection.search_start),
                        "search_stop_sample": int(detection.search_stop),
                        "peak": float(detection.peak),
                        "threshold": float(detection.threshold),
                    },
                    quality_warning=warning,
                )
            )
            placement_error = abs(
                desired_start / float(output.sample_rate) - event.start_time
            ) * output.sample_rate
            alignment.append(
                {
                    "track": track.name,
                    "event_id": event.id,
                    "declared_start_time": event.start_time,
                    "duration": event.duration,
                    "expected_source_sample": expected_source,
                    "detected_source_sample": (
                        int(detection.onset_sample)
                        if detection.onset_sample is not None
                        else None
                    ),
                    "detected_target_time": detected_target,
                    "source_offset_samples": (
                        float(detection.onset_sample - expected_source)
                        if detection.onset_sample is not None
                        else None
                    ),
                    "source_offset_integer_samples": (
                        int(round(detection.onset_sample - expected_source))
                        if detection.onset_sample is not None
                        else None
                    ),
                    "detected_output_sample": detected_output,
                    "placed_output_sample": desired_start,
                    "output_offset_samples": int(output_offset),
                    "placement_error_samples": float(placement_error),
                    "status": status,
                }
            )
    return prepared, alignment, warnings


def _active_events(prepared: list[PreparedEvent], sample: int) -> list[PreparedEvent]:
    return [
        event
        for event in prepared
        if event.start_sample <= sample < event.end_sample
    ]


def _spatial_snapshot_summary(
    prepared: list[PreparedEvent],
    camera: CameraPath,
    sample: int,
    sample_rate: int,
    total_samples: int,
) -> dict[str, Any]:
    clipped = min(max(sample, 0), total_samples)
    time = clipped / float(sample_rate)
    frame = camera.frame_at(time)
    active = _active_events(prepared, sample)
    ild_values: list[float] = []
    itd_values: list[float] = []
    for event in active:
        index = sample - event.start_sample
        if 0 <= index < event.ild_db.shape[0]:
            ild_values.append(float(event.ild_db[index]))
            itd_values.append(float(event.itd_us[index]))
    return {
        "sample": clipped,
        "time": time,
        "camera_position": [float(value) for value in frame.position],
        "camera_forward": [float(value) for value in frame.forward],
        "active_event_ids": [event.spec.id for event in active],
        "active_event_count": len(active),
        "ild_db": float(np.mean(ild_values)) if ild_values else 0.0,
        "itd_us": float(np.mean(itd_values)) if itd_values else 0.0,
    }


def _snapshot_metadata(
    scene: Scene,
    sample: int,
    completed_blocks: int,
    total_blocks: int,
    recovery_count: int,
    reverb_schedule: ReverbSchedule,
    prepared: list[PreparedEvent],
    camera: CameraPath,
) -> dict[str, Any]:
    time = min(sample / float(scene.output.sample_rate), scene.duration)
    reverb_state = reverb_schedule.state_at(time)
    return {
        "schema_version": SNAPSHOT_SCHEMA_VERSION,
        "scene_digest": scene.scene_digest,
        "total_samples": scene.output.total_samples,
        "sample_rate": scene.output.sample_rate,
        "block_frames": scene.output.block_frames,
        "completed_blocks": int(completed_blocks),
        "total_blocks": int(total_blocks),
        "recovery_count": int(recovery_count),
        "current_reverb": {
            "time": time,
            "region_index": reverb_state.region_index,
            "rt60": reverb_state.rt60,
            "wet_dry": reverb_state.wet_dry,
        },
        "spatial_summary": _spatial_snapshot_summary(
            prepared,
            camera,
            sample,
            scene.output.sample_rate,
            scene.output.total_samples,
        ),
    }


def _resume_mismatch(
    snapshot_path: Path, message: str, details: dict[str, Any]
) -> BusinessError:
    discard_snapshot(snapshot_path)
    return BusinessError(
        "E_RESUME_MISMATCH",
        message,
        details=details,
        exit_code=2,
    )


def _load_resume_state(
    snapshot_path: Path,
    scene: Scene,
    schedule: ReverbSchedule,
    total_blocks: int,
) -> tuple[np.ndarray, StereoReverb, int, int, dict[str, Any]]:
    try:
        metadata, arrays = load_snapshot(snapshot_path)
    except Exception as exc:
        raise _resume_mismatch(
            snapshot_path,
            "snapshot is unreadable or corrupt",
            {"reason": str(exc)},
        ) from exc
    if int(metadata.get("schema_version", -1)) != SNAPSHOT_SCHEMA_VERSION:
        raise _resume_mismatch(
            snapshot_path,
            "snapshot schema version differs",
            {"schema_version": metadata.get("schema_version")},
        )
    if metadata.get("scene_digest") != scene.scene_digest:
        raise _resume_mismatch(
            snapshot_path,
            "scene digest differs from snapshot",
            {"snapshot_digest": metadata.get("scene_digest")},
        )
    if int(metadata.get("total_samples", -1)) != scene.output.total_samples:
        raise _resume_mismatch(
            snapshot_path,
            "snapshot total sample count differs",
            {"snapshot_total_samples": metadata.get("total_samples")},
        )
    if int(metadata.get("block_frames", -1)) != scene.output.block_frames:
        raise _resume_mismatch(
            snapshot_path,
            "snapshot block size differs",
            {"snapshot_block_frames": metadata.get("block_frames")},
        )
    completed_blocks = int(metadata.get("completed_blocks", -1))
    if completed_blocks < 0 or completed_blocks > total_blocks:
        raise _resume_mismatch(
            snapshot_path,
            "snapshot completed block count is invalid",
            {"completed_blocks": completed_blocks, "total_blocks": total_blocks},
        )
    next_sample = completed_blocks * scene.output.block_frames
    time = min(
        next_sample / float(scene.output.sample_rate),
        scene.duration,
    )
    expected_reverb = schedule.state_at(time)
    current_reverb = metadata.get("current_reverb")
    if not isinstance(current_reverb, dict):
        raise _resume_mismatch(
            snapshot_path,
            "snapshot reverb state is missing",
            {"completed_blocks": completed_blocks},
        )
    try:
        matches = (
            int(current_reverb.get("region_index")) == expected_reverb.region_index
            and abs(
                float(current_reverb.get("rt60")) - expected_reverb.rt60
            )
            <= 1e-12
            and abs(
                float(current_reverb.get("wet_dry")) - expected_reverb.wet_dry
            )
            <= 1e-12
        )
    except (TypeError, ValueError):
        matches = False
    if not matches:
        raise _resume_mismatch(
            snapshot_path,
            "snapshot reverb interval state differs from current scene",
            {
                "completed_blocks": completed_blocks,
                "expected": {
                    "region_index": expected_reverb.region_index,
                    "rt60": expected_reverb.rt60,
                    "wet_dry": expected_reverb.wet_dry,
                },
                "snapshot": current_reverb,
            },
        )
    master = arrays.get("master")
    expected_shape = (scene.output.total_samples, 2)
    if (
        master is None
        or master.shape != expected_shape
        or not np.isfinite(master).all()
    ):
        raise _resume_mismatch(
            snapshot_path,
            "snapshot master accumulator is invalid",
            {
                "expected_shape": list(expected_shape),
                "actual_shape": list(master.shape) if master is not None else None,
            },
        )
    try:
        reverb = StereoReverb.from_exported(scene.output.sample_rate, arrays)
    except Exception as exc:
        raise _resume_mismatch(
            snapshot_path,
            "snapshot reverb DSP state is invalid",
            {"reason": str(exc)},
        ) from exc
    recovery_count = int(metadata.get("recovery_count", 0)) + 1
    return (
        np.asarray(master, dtype=np.float64).copy(),
        reverb,
        completed_blocks,
        recovery_count,
        metadata,
    )


def _write_master_atomic(path: Path, samples: np.ndarray, sample_rate: int) -> None:
    temporary = path.with_name(path.name + ".tmp")
    try:
        write_wav_pcm24(temporary, samples, sample_rate)
        with temporary.open("rb+") as handle:
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        try:
            directory = os.open(str(path.parent), os.O_RDONLY)
        except OSError:
            directory = None
        if directory is not None:
            try:
                os.fsync(directory)
            except OSError:
                pass
            finally:
                os.close(directory)
    finally:
        if temporary.exists():
            try:
                temporary.unlink()
            except OSError:
                pass


def _interrupted_report(
    scene: Scene,
    snapshot_metadata: dict[str, Any],
) -> dict[str, Any]:
    output = scene.output
    completed = int(snapshot_metadata["completed_blocks"])
    total_blocks = int(snapshot_metadata["total_blocks"])
    return {
        "schema_version": 1,
        "status": "interrupted",
        "report_kind": "recovery",
        "scene_digest": scene.scene_digest,
        "total_samples": output.total_samples,
        "sample_rate": output.sample_rate,
        "block_frames": output.block_frames,
        "completed_blocks": completed,
        "total_blocks": total_blocks,
        "next_sample": completed * output.block_frames,
        "recovery_count": int(snapshot_metadata["recovery_count"]),
        "current_reverb": snapshot_metadata["current_reverb"],
        "spatial_summary": snapshot_metadata["spatial_summary"],
    }


def _build_frame_report(
    scene: Scene,
    prepared: list[PreparedEvent],
    schedule: ReverbSchedule,
    camera: CameraPath,
) -> list[dict[str, Any]]:
    output = scene.output
    sample_positions = list(
        range(0, output.total_samples, output.report_frame_stride)
    )
    if not sample_positions or sample_positions[-1] != output.total_samples - 1:
        sample_positions.append(output.total_samples - 1)
    frames: list[dict[str, Any]] = []
    ordered_events = sorted(prepared, key=lambda item: item.spec.id)
    for sample in sample_positions:
        time = sample / float(output.sample_rate)
        camera_frame = camera.frame_at(time)
        rt60, wet_dry, _ = schedule.values_at(
            np.asarray([time], dtype=np.float64)
        )
        event_entries: list[dict[str, Any]] = []
        layer_count = 0
        for event in ordered_events:
            active = event.start_sample <= sample < event.end_sample
            index = sample - event.start_sample
            layers = occluder_layers(
                camera_frame.position,
                event.spec.source_position,
                scene.occluders,
            )
            if active:
                layer_count += 1
            event_entries.append(
                {
                    "event_id": event.spec.id,
                    "track": event.track.name,
                    "active": bool(active),
                    "occlusion_layers": int(layers),
                    "rt60": float(rt60[0]),
                    "wet_dry": float(wet_dry[0]),
                    "ild_db": (
                        float(event.ild_db[index]) if active else None
                    ),
                    "itd_us": (
                        float(event.itd_us[index]) if active else None
                    ),
                }
            )
        frames.append(
            {
                "sample": int(sample),
                "time": time,
                "layer_count": int(layer_count),
                "camera_position": [
                    float(value) for value in camera_frame.position
                ],
                "camera_forward": [
                    float(value) for value in camera_frame.forward
                ],
                "events": event_entries,
            }
        )
    return frames


def _spatial_summary(
    scene: Scene, prepared: list[PreparedEvent]
) -> dict[str, Any]:
    sample_rate = scene.output.sample_rate
    max_ild_rate = 0.0
    max_itd_rate = 0.0
    max_ild_step = 0.0
    max_itd_step = 0.0
    for event in prepared:
        if event.ild_db.size > 1:
            ild_steps = np.abs(np.diff(event.ild_db))
            itd_steps = np.abs(np.diff(event.itd_us))
            max_ild_step = max(max_ild_step, float(np.max(ild_steps)))
            max_itd_step = max(max_itd_step, float(np.max(itd_steps)))
            max_ild_rate = max(max_ild_rate, float(np.max(ild_steps)) * sample_rate)
            max_itd_rate = max(max_itd_rate, float(np.max(itd_steps)) * sample_rate)
    limits = scene.spatial_limits
    return {
        "max_ild_rate_db_per_s": max_ild_rate,
        "max_itd_rate_us_per_s": max_itd_rate,
        "max_ild_step_db": max_ild_step,
        "max_itd_step_us": max_itd_step,
        "ild_rate_limit_db_per_s": limits.max_ild_rate_db_per_s,
        "itd_rate_limit_us_per_s": limits.max_itd_rate_us_per_s,
        "ild_rate_compliant": bool(
            max_ild_rate <= limits.max_ild_rate_db_per_s + 1e-6
        ),
        "itd_rate_compliant": bool(
            max_itd_rate <= limits.max_itd_rate_us_per_s + 1e-6
        ),
    }


def _reverb_summary(
    scene: Scene, schedule: ReverbSchedule
) -> dict[str, Any]:
    times = np.arange(scene.output.total_samples, dtype=np.float64) / float(
        scene.output.sample_rate
    )
    rt60, wet_dry, _ = schedule.values_at(times)
    if rt60.size > 1:
        max_rt_rate = float(np.max(np.abs(np.diff(rt60)))) * scene.output.sample_rate
        max_wet_rate = (
            float(np.max(np.abs(np.diff(wet_dry)))) * scene.output.sample_rate
        )
    else:
        max_rt_rate = 0.0
        max_wet_rate = 0.0
    return {
        "blend_width_s": scene.reverb_blend_width,
        "min_rt60_s": float(np.min(rt60)),
        "max_rt60_s": float(np.max(rt60)),
        "min_wet_dry": float(np.min(wet_dry)),
        "max_wet_dry": float(np.max(wet_dry)),
        "max_rt60_rate_s_per_s": max_rt_rate,
        "max_wet_dry_rate_per_s": max_wet_rate,
    }


def _diff_report(
    rendered_path: Path,
    reference_path: Path,
    block_frames: int,
) -> dict[str, Any]:
    rendered_bytes = rendered_path.read_bytes()
    reference_bytes = reference_path.read_bytes()
    rendered_sha = sha256_file(rendered_path)
    reference_sha = sha256_file(reference_path)
    differences: list[dict[str, Any]] = []
    sample_mismatch_count = 0
    try:
        rendered, rendered_rate, rendered_channels = read_wav(rendered_path)
        reference, reference_rate, reference_channels = read_wav(reference_path)
        if (
            rendered_rate != reference_rate
            or rendered_channels != reference_channels
            or rendered.shape != reference.shape
        ):
            differences.append(
                {
                    "kind": "format",
                    "rendered_sample_rate": rendered_rate,
                    "reference_sample_rate": reference_rate,
                    "rendered_channels": rendered_channels,
                    "reference_channels": reference_channels,
                    "rendered_shape": list(rendered.shape),
                    "reference_shape": list(reference.shape),
                }
            )
        else:
            mismatched = np.argwhere(rendered != reference)
            sample_mismatch_count = int(mismatched.shape[0])
            for sample, channel in mismatched[:100]:
                differences.append(
                    {
                        "kind": "sample",
                        "block_index": int(sample) // int(block_frames),
                        "sample": int(sample),
                        "channel": int(channel),
                        "rendered": float(rendered[sample, channel]),
                        "reference": float(reference[sample, channel]),
                    }
                )
    except Exception as exc:
        differences.append({"kind": "decode_error", "message": str(exc)})
    return {
        "schema_version": 1,
        "status": "mismatch",
        "code": "E_RESUME_OUTPUT_MISMATCH",
        "byte_mismatch": rendered_bytes != reference_bytes,
        "rendered_sha256": rendered_sha,
        "reference_sha256": reference_sha,
        "sample_mismatch_count": sample_mismatch_count,
        "first_differences": differences,
    }


def _build_final_report(
    scene: Scene,
    prepared: list[PreparedEvent],
    alignment: list[dict[str, Any]],
    warnings: list[dict[str, Any]],
    schedule: ReverbSchedule,
    camera: CameraPath,
    master_result: Any,
    master_sha256: str,
    recovery_count: int,
) -> dict[str, Any]:
    output = scene.output
    total_energy = sum(event.energy for event in prepared)
    contributions = []
    for event in prepared:
        ratio = event.energy / total_energy if total_energy > 0 else 0.0
        contributions.append(
            {
                "event_id": event.spec.id,
                "track": event.track.name,
                "type": event.spec.type,
                "energy": event.energy,
                "ratio": ratio,
            }
        )
    contributions.sort(key=lambda item: item["event_id"])
    loudness_delta = (
        master_result.integrated_loudness_lufs
        - output.target_loudness_lufs
    )
    return {
        "schema_version": 1,
        "status": "complete",
        "report_kind": "final",
        "scene_name": scene.name,
        "scene_digest": scene.scene_digest,
        "recovery_count": int(recovery_count),
        "total_samples": int(output.total_samples),
        "output": {
            "sample_rate": output.sample_rate,
            "channels": output.channels,
            "duration": output.duration,
            "block_frames": output.block_frames,
            "report_frame_stride": output.report_frame_stride,
        },
        "validation": {
            "hard_errors": [],
            "quality_warnings": warnings,
        },
        "tracks": [
            {
                "name": track.name,
                "source_sample_rate": track.declared_sample_rate,
                "source_channels": track.declared_channels,
                "output_sample_rate": output.sample_rate,
                "output_samples": output.total_samples,
                "sync_anchor_count": len(track.sync_anchors),
            }
            for track in scene.tracks
        ],
        "alignment": alignment,
        "master": {
            "path": "master.wav",
            "sha256": master_sha256,
            "integrated_loudness_lufs": master_result.integrated_loudness_lufs,
            "target_loudness_lufs": output.target_loudness_lufs,
            "loudness_delta_lu": loudness_delta,
            "loudness_within_tolerance": bool(abs(loudness_delta) <= 0.5),
            "true_peak_dbtp": master_result.true_peak_dbtp,
            "true_peak_limit_dbtp": output.true_peak_limit_dbtp,
            "true_peak_compliant": bool(
                master_result.true_peak_dbtp <= output.true_peak_limit_dbtp + 1e-9
            ),
            "suppressed_samples": master_result.suppressed_samples,
            "suppressed_frames": master_result.suppressed_frames,
            "limiter_iterations": master_result.iterations,
            "event_contributions": contributions,
        },
        "spatial_summary": _spatial_summary(scene, prepared),
        "reverb_summary": _reverb_summary(scene, schedule),
        "frames": _build_frame_report(scene, prepared, schedule, camera),
        "hashes": {
            "scene_digest": scene.scene_digest,
            "master_sha256": master_sha256,
        },
    }


def render_scene(
    scene_path: str | Path,
    out_dir: str | Path,
    *,
    snapshot_path: str | Path | None = None,
    fresh: bool = False,
    verify_reference: str | Path | None = None,
    interrupt_requested: Callable[[], bool] | None = None,
) -> RenderResult:
    scene = load_scene(scene_path, load_audio=True)
    output = scene.output
    out = Path(out_dir).resolve()
    try:
        out.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        raise BusinessError(
            "E_OUTPUT_IO",
            f"cannot create output directory: {exc}",
            details={"directory": out.name},
        ) from exc
    snapshot_file = (
        Path(snapshot_path).resolve()
        if snapshot_path is not None
        else out / "render_snapshot.npz"
    )
    if fresh:
        discard_snapshot(snapshot_file)

    tracks_resampled: dict[str, np.ndarray] = {}
    for track in scene.tracks:
        if track.samples is None:
            raise BusinessError(
                "E_TRACK_DECODE", "track samples are missing", track=track.name
            )
        tracks_resampled[track.name] = resample_track(
            track.samples,
            track.declared_sample_rate,
            output.sample_rate,
            output.total_samples,
            track.sync_anchors,
        )
    prepared, alignment, warnings = _prepare_events(scene, tracks_resampled)
    schedule = ReverbSchedule(scene.reverb_regions, scene.reverb_blend_width)
    camera = CameraPath(scene.camera_path, scene.camera_up)

    total_blocks = math.ceil(output.total_samples / output.block_frames)
    if snapshot_file.exists():
        (
            master,
            reverb,
            completed_blocks,
            recovery_count,
            resume_metadata,
        ) = _load_resume_state(
            snapshot_file, scene, schedule, total_blocks
        )
    else:
        master = np.zeros(
            (output.total_samples, output.channels), dtype=np.float64
        )
        reverb = StereoReverb(output.sample_rate)
        completed_blocks = 0
        recovery_count = 0
        resume_metadata = None

    interrupted = False
    while completed_blocks < total_blocks:
        if interrupt_requested is not None and interrupt_requested():
            interrupted = True
            break
        start = completed_blocks * output.block_frames
        stop = min(output.total_samples, start + output.block_frames)
        dry = np.zeros((stop - start, 2), dtype=np.float64)
        for event in prepared:
            overlap_start = max(start, event.start_sample)
            overlap_stop = min(stop, event.end_sample)
            if overlap_stop <= overlap_start:
                continue
            source_start = overlap_start - event.start_sample
            source_stop = overlap_stop - event.start_sample
            dry[
                overlap_start - start : overlap_stop - start
            ] += event.stereo[source_start:source_stop]
        times = (
            np.arange(start, stop, dtype=np.float64) / float(output.sample_rate)
        )
        rt60, wet_dry, _ = schedule.values_at(times)
        wet_signal, _ = reverb.process(dry, rt60, wet_dry)
        wet_gain = wet_dry[:, None]
        dry_gain = 1.0 - wet_gain
        master[start:stop] = dry * dry_gain + wet_signal * wet_gain
        completed_blocks += 1
        next_sample = min(
            completed_blocks * output.block_frames, output.total_samples
        )
        metadata = _snapshot_metadata(
            scene,
            next_sample,
            completed_blocks,
            total_blocks,
            recovery_count,
            schedule,
            prepared,
            camera,
        )
        arrays = {
            "master": master,
            **reverb.export_state(),
        }
        try:
            save_snapshot(snapshot_file, metadata, arrays)
        except OSError as exc:
            raise BusinessError(
                "E_SNAPSHOT_IO",
                f"cannot commit snapshot: {exc}",
                details={"completed_blocks": completed_blocks},
            ) from exc
        if interrupt_requested is not None and interrupt_requested():
            interrupted = True
            break

    if interrupted:
        next_sample = min(
            completed_blocks * output.block_frames, output.total_samples
        )
        metadata = _snapshot_metadata(
            scene,
            next_sample,
            completed_blocks,
            total_blocks,
            recovery_count,
            schedule,
            prepared,
            camera,
        )
        save_snapshot(
            snapshot_file,
            metadata,
            {"master": master, **reverb.export_state()},
        )
        report = _interrupted_report(scene, metadata)
        report_path = out / "recovery_report.json"
        write_json_atomic(report_path, report)
        return RenderResult(
            exit_code=0,
            interrupted=True,
            report_path=report_path,
            master_path=None,
            report=report,
        )

    if interrupt_requested is not None and interrupt_requested():
        metadata = _snapshot_metadata(
            scene,
            output.total_samples,
            completed_blocks,
            total_blocks,
            recovery_count,
            schedule,
            prepared,
            camera,
        )
        save_snapshot(
            snapshot_file,
            metadata,
            {"master": master, **reverb.export_state()},
        )

    master_result = master_audio(
        master,
        output.sample_rate,
        output.target_loudness_lufs,
        output.true_peak_limit_dbtp,
    )
    if not math.isfinite(master_result.integrated_loudness_lufs):
        raise BusinessError(
            "E_MASTER_LOUDNESS",
            "integrated loudness could not be measured",
            exit_code=2,
        )
    loudness_delta = (
        master_result.integrated_loudness_lufs
        - output.target_loudness_lufs
    )
    if abs(loudness_delta) > 0.5:
        raise BusinessError(
            "E_MASTER_LOUDNESS",
            "master loudness is outside the +/-0.5 LU tolerance",
            details={
                "integrated_loudness_lufs": master_result.integrated_loudness_lufs,
                "target_loudness_lufs": output.target_loudness_lufs,
                "delta_lu": loudness_delta,
            },
            exit_code=2,
        )
    ceiling_linear = 10.0 ** (output.true_peak_limit_dbtp / 20.0)
    if (
        master_result.true_peak_dbtp
        > output.true_peak_limit_dbtp + 1e-9
        or float(np.max(np.abs(master_result.samples))) > ceiling_linear + 1e-12
    ):
        raise BusinessError(
            "E_MASTER_TRUE_PEAK",
            "master exceeds the declared true-peak ceiling",
            details={
                "true_peak_dbtp": master_result.true_peak_dbtp,
                "limit_dbtp": output.true_peak_limit_dbtp,
            },
            exit_code=2,
        )
    master_path = out / "master.wav"
    _write_master_atomic(master_path, master_result.samples, output.sample_rate)
    master_sha = sha256_file(master_path)
    report = _build_final_report(
        scene,
        prepared,
        alignment,
        warnings,
        schedule,
        camera,
        master_result,
        master_sha,
        recovery_count,
    )
    report_path = out / "report.json"
    write_json_atomic(report_path, report)

    exit_code = 0
    error = None
    if verify_reference is not None:
        reference = Path(verify_reference).resolve()
        if not reference.is_file():
            raise BusinessError(
                "E_REFERENCE_IO",
                "reference master does not exist",
                details={"reference": reference.name},
            )
        if sha256_file(reference) != master_sha:
            difference = _diff_report(
                master_path, reference, output.block_frames
            )
            write_json_atomic(out / "difference_report.json", difference)
            exit_code = 3
            error = {
                "ok": False,
                "code": "E_RESUME_OUTPUT_MISMATCH",
                "message": "resumed master differs from reference",
                "difference_report": "difference_report.json",
            }
    discard_snapshot(snapshot_file)
    return RenderResult(
        exit_code=exit_code,
        interrupted=False,
        report_path=report_path,
        master_path=master_path,
        report=report,
        error=error,
    )
