"""Resumable deterministic block renderer and report generation."""

from __future__ import annotations

import hashlib
import json
import math
import os
import time
from dataclasses import dataclass
from typing import Any, Callable

import numpy as np

from .audio import (
    atomic_write_json,
    db_from_amplitude,
    pcm_encode,
    read_wav,
    sha256_file,
    true_peak_dbtp,
    wav_header,
    write_wav,
)
from .dsp import (
    Calibration,
    TransitionInterval,
    calibrate_master,
    compensate_first_violin,
    contribution_energy_shares,
    detect_transition_intervals,
    integrated_loudness_lufs,
)
from .errors import BusinessError
from .manifest import (
    INSTRUMENTS,
    INSTRUMENT_ALIASES,
    ManifestContext,
    canonical_json,
    load_manifest,
    manifest_digest,
)
from .synth import synthesize_tracks


STATE_SCHEMA_VERSION = 1
ALGORITHM_VERSION = "stringmaster-v1"
WAV_HEADER_BYTES = 44


@dataclass
class WorkingPlan:
    context: ManifestContext
    source_tracks: dict[str, np.ndarray]
    compensated_tracks: dict[str, np.ndarray]
    contributions: dict[str, np.ndarray]
    intervals: tuple[TransitionInterval, ...]
    intervals_by_instrument: dict[str, tuple[TransitionInterval, ...]]
    compensation: tuple[dict[str, Any], ...]
    voice_gain_db: dict[str, float]
    calibration: Calibration
    total_frames: int
    voice_energy_shares: dict[str, float]
    voice_peaks_dbfs: dict[str, float]
    fingerprint: str
    source_sha256: dict[str, str]


def _cleanup_progress(paths: list[str]) -> None:
    for path in paths:
        if not path:
            continue
        try:
            if os.path.exists(path):
                os.unlink(path)
        except OSError:
            pass


def _state_business_error(path: str, message: str) -> BusinessError:
    return BusinessError("E_STATE_CORRUPT", path, message)


def _load_state(path: str) -> dict[str, Any]:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            state = json.load(handle)
    except json.JSONDecodeError as exc:
        raise _state_business_error(
            path, f"invalid snapshot JSON at line {exc.lineno}, column {exc.colno}"
        ) from exc
    except OSError as exc:
        raise _state_business_error(path, str(exc)) from exc
    if not isinstance(state, dict):
        raise _state_business_error(path, "snapshot root must be an object")
    return state


def _source_hashes(context: ManifestContext) -> dict[str, str]:
    result: dict[str, str] = {}
    for track in context.tracks:
        digest = sha256_file(track.abs_path)
        if track.sha256 is not None and digest != track.sha256:
            raise BusinessError(
                "E_FILE_CHANGED",
                track.abs_path,
                "audio file changed after manifest validation",
            )
        result[track.instrument] = digest
    return result


def _load_source_tracks(context: ManifestContext) -> dict[str, np.ndarray]:
    result: dict[str, np.ndarray] = {}
    for track in context.tracks:
        data, info = read_wav(track.abs_path)
        if info.sample_rate != context.sample_rate:
            raise BusinessError(
                "E_SAMPLE_RATE_MISMATCH",
                track.abs_path,
                f"actual sample rate {info.sample_rate} != manifest {context.sample_rate}",
            )
        if info.bit_depth != context.bit_depth:
            raise BusinessError(
                "E_BIT_DEPTH_MISMATCH",
                track.abs_path,
                f"actual bit depth {info.bit_depth} != manifest {context.bit_depth}",
            )
        result[track.instrument] = data
    return result


def _track_to_contribution(
    data: np.ndarray,
    weights: tuple[float, float],
    gain: float,
) -> np.ndarray:
    frames = data.shape[0]
    output = np.zeros((frames, 2), dtype=np.float64)
    if data.shape[1] == 1:
        output[:, 0] = data[:, 0] * weights[0] * gain
        output[:, 1] = data[:, 0] * weights[1] * gain
    else:
        output[:, 0] = data[:, 0] * weights[0] * gain
        output[:, 1] = data[:, 1] * weights[1] * gain
    return output


def _limiter_envelope_digest(envelope: np.ndarray) -> str:
    return hashlib.sha256(
        np.asarray(envelope, dtype="<f8").tobytes(order="C")
    ).hexdigest()


def _plan_fingerprint(
    context: ManifestContext,
    intervals: tuple[TransitionInterval, ...],
    voice_gain_db: dict[str, float],
    calibration: Calibration,
    source_sha256: dict[str, str],
    total_frames: int,
) -> str:
    confirmed = {
        "algorithm": ALGORITHM_VERSION,
        "manifest_digest": context.digest,
        "transition_intervals": [interval.as_dict() for interval in intervals],
        "voice_gain_db": voice_gain_db,
        "common_gain_db": calibration.common_gain_db,
        "limiter_envelope_sha256": _limiter_envelope_digest(
            calibration.limiter_envelope
        ),
        "limiter_triggered_samples": calibration.limiter_triggered_samples,
        "source_sha256": source_sha256,
        "total_frames": total_frames,
    }
    return hashlib.sha256(canonical_json(confirmed)).hexdigest()


def build_working_plan(context: ManifestContext) -> WorkingPlan:
    source_tracks = _load_source_tracks(context)
    first_violin = source_tracks["第一小提琴"]
    detected = detect_transition_intervals(
        first_violin,
        context.sample_rate,
        context.band_low_hz,
        context.band_high_hz,
        instrument="第一小提琴",
    )
    compensation_result = compensate_first_violin(
        first_violin,
        context.sample_rate,
        context.band_low_hz,
        context.band_high_hz,
        detected,
    )
    for entry in compensation_result.entries:
        if float(entry["jump_reduction_db"]) < 8.0:
            raise BusinessError(
                "E_COMPENSATION",
                context.track("第一小提琴").abs_path,
                (
                    "band jump reduction is "
                    f"{float(entry['jump_reduction_db']):.3f} dB; at least 8 dB is required"
                ),
            )

    compensated_tracks = dict(source_tracks)
    compensated_tracks["第一小提琴"] = compensation_result.compensated
    total_frames = max(data.shape[0] for data in compensated_tracks.values())
    for instrument, data in compensated_tracks.items():
        if data.shape[0] < total_frames:
            padded = np.zeros((total_frames, data.shape[1]), dtype=np.float64)
            padded[: data.shape[0]] = data
            compensated_tracks[instrument] = padded

    raw_contributions: dict[str, np.ndarray] = {}
    for track in context.tracks:
        data = compensated_tracks[track.instrument]
        contribution = _track_to_contribution(data, track.downmix_weights, 1.0)
        energy = float(np.sum(contribution * contribution))
        if energy <= 1e-20:
            raise BusinessError(
                "E_SILENT_TRACK",
                track.abs_path,
                "track has no usable energy and cannot meet its target ratio",
            )
        gain = math.sqrt(track.target_energy_ratio / energy)
        raw_contributions[track.instrument] = contribution * gain

    voice_gain_db: dict[str, float] = {}
    for track in context.tracks:
        contribution = raw_contributions[track.instrument]
        unit = _track_to_contribution(
            compensated_tracks[track.instrument], track.downmix_weights, 1.0
        )
        unit_energy = float(np.sum(unit * unit))
        gain = math.sqrt(track.target_energy_ratio / unit_energy)
        voice_gain_db[track.instrument] = round(20.0 * math.log10(gain), 12)

    ordered_contributions = {
        instrument: raw_contributions[instrument] for instrument in INSTRUMENTS
    }
    base_master = np.sum(
        np.stack([ordered_contributions[instrument] for instrument in INSTRUMENTS]),
        axis=0,
    )
    calibration = calibrate_master(
        base_master,
        context.sample_rate,
        context.target_loudness_lufs,
        context.true_peak_limit_dbtp,
    )
    if abs(calibration.loudness_lufs - context.target_loudness_lufs) > 0.5:
        raise BusinessError(
            "E_LOUDNESS_TARGET",
            context.manifest_path,
            (
                f"calibrated loudness {calibration.loudness_lufs:.3f} LUFS is outside "
                f"target {context.target_loudness_lufs:.3f} +/- 0.5 LU"
            ),
        )
    if calibration.true_peak_dbtp > context.true_peak_limit_dbtp + 1e-6:
        raise BusinessError(
            "E_TRUE_PEAK_LIMIT",
            context.manifest_path,
            (
                f"calibrated true peak {calibration.true_peak_dbtp:.3f} dBTP exceeds "
                f"limit {context.true_peak_limit_dbtp:.3f} dBTP"
            ),
        )

    shares = contribution_energy_shares(
        ordered_contributions, calibration.attenuation
    )
    target_by_instrument = {
        track.instrument: track.target_energy_ratio for track in context.tracks
    }
    for instrument in INSTRUMENTS:
        if abs(shares[instrument] - target_by_instrument[instrument]) > 0.02:
            raise BusinessError(
                "E_ENERGY_RATIO",
                context.manifest_path,
                (
                    f"{instrument} final energy share {shares[instrument]:.6f} deviates "
                    f"from target {target_by_instrument[instrument]:.6f} by more than 0.02"
                ),
            )

    voice_peaks = {
        instrument: db_from_amplitude(
            float(
                np.max(
                    np.abs(
                        ordered_contributions[instrument]
                        * calibration.attenuation[:, None]
                    )
                )
            )
        )
        for instrument in INSTRUMENTS
    }
    source_sha256 = _source_hashes(context)
    fingerprint = _plan_fingerprint(
        context,
        detected,
        voice_gain_db,
        calibration,
        source_sha256,
        total_frames,
    )
    intervals_by_instrument = {instrument: () for instrument in INSTRUMENTS}
    intervals_by_instrument["第一小提琴"] = detected
    return WorkingPlan(
        context=context,
        source_tracks=source_tracks,
        compensated_tracks=compensated_tracks,
        contributions=ordered_contributions,
        intervals=detected,
        intervals_by_instrument=intervals_by_instrument,
        compensation=compensation_result.entries,
        voice_gain_db=voice_gain_db,
        calibration=calibration,
        total_frames=total_frames,
        voice_energy_shares=shares,
        voice_peaks_dbfs=voice_peaks,
        fingerprint=fingerprint,
        source_sha256=source_sha256,
    )


def _confirmed_snapshot(plan: WorkingPlan) -> dict[str, Any]:
    return {
        "transition_intervals": [interval.as_dict() for interval in plan.intervals],
        "compensation": list(plan.compensation),
        "voice_gain_db": plan.voice_gain_db,
        "common_gain_db": plan.calibration.common_gain_db,
        "limiter_envelope_sha256": _limiter_envelope_digest(
            plan.calibration.limiter_envelope
        ),
        "limiter_triggered_samples": plan.calibration.limiter_triggered_samples,
        "source_sha256": plan.source_sha256,
    }


def _make_state(
    plan: WorkingPlan,
    *,
    output_path: str,
    state_path: str,
    completed_blocks: int,
    total_blocks: int,
    block_size: int,
    data_sha256: str,
    snapshot_commits: int,
) -> dict[str, Any]:
    return {
        "schema_version": STATE_SCHEMA_VERSION,
        "algorithm": ALGORITHM_VERSION,
        "manifest_digest": plan.context.digest,
        "manifest": plan.context.raw,
        "manifest_path": plan.context.manifest_path,
        "master_path": os.path.abspath(output_path),
        "state_path": os.path.abspath(state_path),
        "sample_rate": plan.context.sample_rate,
        "bit_depth": plan.context.bit_depth,
        "channels": 2,
        "total_frames": plan.total_frames,
        "block_size": block_size,
        "total_blocks": total_blocks,
        "completed_blocks": completed_blocks,
        "snapshot_commits": snapshot_commits,
        "master_pcm_sha256": data_sha256,
        "plan_fingerprint": plan.fingerprint,
        "confirmed": _confirmed_snapshot(plan),
    }


def _write_header_and_truncate(
    handle: Any,
    *,
    sample_rate: int,
    bit_depth: int,
    frames: int,
) -> None:
    header = wav_header(sample_rate, 2, bit_depth, frames)
    handle.seek(0)
    handle.write(header)
    handle.flush()
    os.fsync(handle.fileno())


def _validate_state_against_plan(
    state: dict[str, Any],
    plan: WorkingPlan,
    *,
    state_path: str,
    output_path: str,
) -> None:
    if state.get("schema_version") != STATE_SCHEMA_VERSION:
        raise BusinessError(
            "E_RESUME_MISMATCH",
            state_path,
            "snapshot schema version does not match current renderer",
            {"discarded": True},
        )
    if state.get("manifest_digest") != plan.context.digest:
        raise BusinessError(
            "E_RESUME_MISMATCH",
            state_path,
            "manifest digest differs from the committed snapshot",
            {"discarded": True},
        )
    if state.get("plan_fingerprint") != plan.fingerprint:
        raise BusinessError(
            "E_RESUME_MISMATCH",
            state_path,
            "confirmed intervals, gains, calibration, or source audio differ from snapshot",
            {"discarded": True},
        )
    if os.path.abspath(str(state.get("master_path", ""))) != os.path.abspath(output_path):
        raise BusinessError(
            "E_RESUME_MISMATCH",
            state_path,
            "output path differs from the committed snapshot",
            {"discarded": True},
        )
    if int(state.get("total_frames", -1)) != plan.total_frames:
        raise BusinessError(
            "E_RESUME_MISMATCH",
            state_path,
            "total frame count differs from the committed snapshot",
            {"discarded": True},
        )


def _ensure_partial_master(
    state: dict[str, Any],
    *,
    plan: WorkingPlan,
    output_path: str,
    state_path: str,
) -> tuple[Any, hashlib._Hash, int]:
    committed = int(state.get("completed_blocks", 0))
    total_blocks = int(state.get("total_blocks", 0))
    block_size = int(state.get("block_size", plan.context.block_size))
    if committed < 0 or total_blocks < committed:
        raise _state_business_error(state_path, "invalid committed/total block counts")
    expected_total_blocks = (plan.total_frames + block_size - 1) // block_size
    if total_blocks != expected_total_blocks:
        raise _state_business_error(
            state_path,
            "snapshot total block count is inconsistent with frames and block size",
        )
    bytes_per_frame = 2 * (plan.context.bit_depth // 8)
    expected_data_bytes = plan.total_frames * bytes_per_frame
    committed_frames = min(plan.total_frames, committed * block_size)
    committed_bytes = committed_frames * bytes_per_frame
    if not os.path.isfile(output_path):
        raise _state_business_error(
            state_path, "snapshot exists but the partial master is missing"
        )
    file_size = os.path.getsize(output_path)
    if file_size < WAV_HEADER_BYTES + committed_bytes:
        raise _state_business_error(
            state_path,
            "partial master is shorter than the committed snapshot prefix",
        )
    handle = open(output_path, "r+b")
    _write_header_and_truncate(
        handle,
        sample_rate=plan.context.sample_rate,
        bit_depth=plan.context.bit_depth,
        frames=plan.total_frames,
    )
    handle.seek(WAV_HEADER_BYTES + committed_bytes)
    handle.truncate()
    handle.flush()
    os.fsync(handle.fileno())
    handle.seek(WAV_HEADER_BYTES)
    prefix = handle.read(committed_bytes)
    digest = hashlib.sha256(prefix)
    if digest.hexdigest() != state.get("master_pcm_sha256"):
        handle.close()
        raise _state_business_error(
            state_path,
            "committed PCM prefix hash does not match snapshot",
        )
    handle.seek(WAV_HEADER_BYTES + committed_bytes)
    if committed_bytes == expected_data_bytes:
        handle.seek(0)
        handle.write(
            wav_header(
                plan.context.sample_rate,
                2,
                plan.context.bit_depth,
                plan.total_frames,
            )
        )
        handle.flush()
        os.fsync(handle.fileno())
    return handle, digest, committed


def _write_master_summary(
    output_path: str,
    plan: WorkingPlan,
    *,
    block_size: int,
    completed_blocks: int,
    total_blocks: int,
) -> dict[str, Any]:
    master, info = read_wav(output_path)
    peak_values: list[float] = []
    for channel in range(master.shape[1]):
        peak_values.append(
            round(
                db_from_amplitude(float(np.max(np.abs(master[:, channel])))),
                9,
            )
        )
    loudness = integrated_loudness_lufs(master, plan.context.sample_rate)
    peak = true_peak_dbtp(master, plan.context.sample_rate)
    context = plan.context
    workload = {
        "algorithm": ALGORITHM_VERSION,
        "manifest_digest": context.digest,
        "source_sha256": plan.source_sha256,
        "blocks": {
            "size": int(block_size),
            "completed_blocks": int(completed_blocks),
            "total_blocks": int(total_blocks),
            "snapshot_commits": int(total_blocks),
        },
        "transition_intervals": [
            interval.as_dict() for interval in plan.intervals
        ],
        "transition_intervals_by_instrument": {
            instrument: [
                interval.as_dict()
                for interval in plan.intervals_by_instrument[instrument]
            ]
            for instrument in INSTRUMENTS
        },
        "compensation": list(plan.compensation),
        "voice_gain_db": plan.voice_gain_db,
        "target_energy_ratios": {
            track.instrument: track.target_energy_ratio for track in context.tracks
        },
        "measured_energy_ratios": {
            instrument: round(plan.voice_energy_shares[instrument], 12)
            for instrument in INSTRUMENTS
        },
        "max_energy_ratio_deviation": round(
            max(
                abs(
                    plan.voice_energy_shares[track.instrument]
                    - track.target_energy_ratio
                )
                for track in context.tracks
            ),
            12,
        ),
        "loudness": {
            "integrated_lufs": round(float(loudness), 9),
            "target_lufs": context.target_loudness_lufs,
            "deviation_lu": round(float(loudness - context.target_loudness_lufs), 9),
        },
        "true_peak": {
            "dbtp": round(float(peak), 9),
            "limit_dbtp": context.true_peak_limit_dbtp,
            "compliant": bool(peak <= context.true_peak_limit_dbtp + 1e-6),
        },
        "limiter": {
            "triggered_samples": int(plan.calibration.limiter_triggered_samples),
            "max_reduction_db": round(
                float(plan.calibration.max_limiter_reduction_db), 9
            ),
            "ceiling_dbtp": round(float(plan.calibration.ceiling_dbtp), 9),
        },
        "channel_peaks_dbfs": peak_values,
        "voice_peaks_dbfs": {
            instrument: round(plan.voice_peaks_dbfs[instrument], 9)
            for instrument in INSTRUMENTS
        },
        "master": {
            "sha256": sha256_file(output_path),
            "frames": int(info.frames),
            "channels": int(info.channels),
            "sample_rate": int(info.sample_rate),
            "bit_depth": int(info.bit_depth),
            "longest_source_frames": int(plan.total_frames),
        },
        "status": "complete",
    }
    return workload


def _write_recovery_report(
    path: str,
    *,
    plan: WorkingPlan,
    state_path: str,
    completed_blocks: int,
    total_blocks: int,
    data_sha256: str,
) -> dict[str, Any]:
    report = {
        "status": "interrupted",
        "manifest_digest": plan.context.digest,
        "completed_blocks": int(completed_blocks),
        "total_blocks": int(total_blocks),
        "finished_ratio": round(completed_blocks / total_blocks, 12)
        if total_blocks
        else 0.0,
        "snapshot_path": os.path.abspath(state_path),
        "master_pcm_sha256": data_sha256,
        "next_action": "rerun the same render command to resume from the committed snapshot",
    }
    atomic_write_json(path, report)
    return report


def _next_block_end(start: int, block_size: int, total_frames: int) -> int:
    return min(total_frames, start + block_size)


def render_project(
    manifest_path: str,
    *,
    output_path: str | None = None,
    state_path: str | None = None,
    report_path: str | None = None,
    recovery_report_path: str | None = None,
    block_size: int | None = None,
    interrupt_requested: Callable[[], bool] | None = None,
) -> dict[str, Any]:
    manifest_abs = os.path.abspath(manifest_path)
    manifest_dir = os.path.dirname(manifest_abs)
    output_path = os.path.abspath(
        output_path or os.path.join(manifest_dir, "render", "master.wav")
    )
    state_path = os.path.abspath(
        state_path or os.path.join(manifest_dir, "render", "state.json")
    )
    report_path = os.path.abspath(
        report_path or os.path.join(manifest_dir, "render", "report.json")
    )
    recovery_report_path = os.path.abspath(
        recovery_report_path
        or os.path.join(manifest_dir, "render", "recovery_report.json")
    )
    if os.path.isfile(state_path):
        preflight_state = _load_state(state_path)
        try:
            with open(manifest_abs, "r", encoding="utf-8") as handle:
                current_raw = json.load(handle)
        except (OSError, json.JSONDecodeError):
            current_raw = None
        if isinstance(current_raw, dict):
            current_digest = manifest_digest(current_raw)
            if preflight_state.get("manifest_digest") != current_digest:
                old_master = str(preflight_state.get("master_path", ""))
                _cleanup_progress(
                    [
                        state_path,
                        output_path,
                        old_master,
                        report_path,
                        recovery_report_path,
                    ]
                )
                raise BusinessError(
                    "E_RESUME_MISMATCH",
                    state_path,
                    "manifest digest differs from the committed snapshot; old progress discarded",
                    {"discarded": True},
                )
            committed_sources = preflight_state.get("confirmed", {}).get(
                "source_sha256"
            )
            tracks_raw = current_raw.get("tracks")
            if isinstance(committed_sources, dict) and isinstance(tracks_raw, list):
                current_hashes: dict[str, str] = {}
                all_present = True
                for track_raw in tracks_raw:
                    if not isinstance(track_raw, dict):
                        all_present = False
                        break
                    instrument_raw = track_raw.get("instrument")
                    instrument = (
                        INSTRUMENT_ALIASES.get(instrument_raw.strip())
                        if isinstance(instrument_raw, str)
                        else None
                    )
                    path_text = track_raw.get("path")
                    if instrument is None or not isinstance(path_text, str):
                        all_present = False
                        break
                    source_path = (
                        path_text
                        if os.path.isabs(path_text)
                        else os.path.join(manifest_dir, path_text)
                    )
                    source_path = os.path.abspath(source_path)
                    if not os.path.isfile(source_path):
                        all_present = False
                        break
                    current_hashes[instrument] = sha256_file(source_path)
                if (
                    all_present
                    and current_hashes.keys() == committed_sources.keys()
                    and current_hashes != committed_sources
                ):
                    old_master = str(preflight_state.get("master_path", ""))
                    _cleanup_progress(
                        [
                            state_path,
                            output_path,
                            old_master,
                            report_path,
                            recovery_report_path,
                        ]
                    )
                    raise BusinessError(
                        "E_RESUME_MISMATCH",
                        state_path,
                        "source audio hashes differ from the committed snapshot; old progress discarded",
                        {"discarded": True},
                    )

    context = load_manifest(manifest_path, require_files=True)
    os.makedirs(os.path.dirname(output_path), exist_ok=True)
    os.makedirs(os.path.dirname(state_path), exist_ok=True)
    os.makedirs(os.path.dirname(report_path), exist_ok=True)
    os.makedirs(os.path.dirname(recovery_report_path), exist_ok=True)

    old_state: dict[str, Any] | None = None
    if os.path.isfile(state_path):
        old_state = _load_state(state_path)
        if old_state.get("manifest_digest") != context.digest:
            old_master = str(old_state.get("master_path", ""))
            _cleanup_progress(
                [state_path, output_path, old_master, report_path, recovery_report_path]
            )
            raise BusinessError(
                "E_RESUME_MISMATCH",
                state_path,
                "manifest digest differs from the committed snapshot; old progress discarded",
                {"discarded": True},
            )
        committed_sources = old_state.get("confirmed", {}).get("source_sha256")
        current_sources = {
            track.instrument: sha256_file(track.abs_path)
            for track in context.tracks
        }
        if committed_sources != current_sources:
            old_master = str(old_state.get("master_path", ""))
            _cleanup_progress(
                [state_path, output_path, old_master, report_path, recovery_report_path]
            )
            raise BusinessError(
                "E_RESUME_MISMATCH",
                state_path,
                "source audio hashes differ from the committed snapshot; old progress discarded",
                {"discarded": True},
            )

    plan = build_working_plan(context)
    if old_state is not None:
        try:
            _validate_state_against_plan(
                old_state,
                plan,
                state_path=state_path,
                output_path=output_path,
            )
        except BusinessError as exc:
            if exc.code == "E_RESUME_MISMATCH":
                _cleanup_progress(
                    [state_path, output_path, report_path, recovery_report_path]
                )
            raise
        chosen_block_size = int(old_state["block_size"])
        handle, data_digest, completed = _ensure_partial_master(
            old_state,
            plan=plan,
            output_path=output_path,
            state_path=state_path,
        )
        total_blocks = int(old_state["total_blocks"])
    else:
        chosen_block_size = int(block_size or context.block_size)
        if chosen_block_size <= 0:
            raise BusinessError(
                "E_MANIFEST_SCHEMA",
                context.manifest_path,
                "block size must be a positive integer",
            )
        total_blocks = (
            plan.total_frames + chosen_block_size - 1
        ) // chosen_block_size
        handle = open(output_path, "w+b")
        _write_header_and_truncate(
            handle,
            sample_rate=context.sample_rate,
            bit_depth=context.bit_depth,
            frames=plan.total_frames,
        )
        handle.truncate(WAV_HEADER_BYTES)
        handle.seek(WAV_HEADER_BYTES)
        data_digest = hashlib.sha256()
        completed = 0
        initial_state = _make_state(
            plan,
            output_path=output_path,
            state_path=state_path,
            completed_blocks=0,
            total_blocks=total_blocks,
            block_size=chosen_block_size,
            data_sha256=data_digest.hexdigest(),
            snapshot_commits=0,
        )
        atomic_write_json(state_path, initial_state)

    bytes_per_frame = 2 * (context.bit_depth // 8)
    test_delay_ms = max(
        0.0, float(os.environ.get("STRINGMASTER_TEST_BLOCK_SLEEP_MS", "0"))
    )
    interrupted = bool(interrupt_requested and interrupt_requested())
    try:
        while completed < total_blocks and not interrupted:
            start = completed * chosen_block_size
            end = _next_block_end(start, chosen_block_size, plan.total_frames)
            block = np.zeros((end - start, 2), dtype=np.float64)
            for instrument in INSTRUMENTS:
                block += (
                    plan.contributions[instrument][start:end]
                    * plan.calibration.attenuation[start:end, None]
                )
            payload = pcm_encode(block, context.bit_depth)
            expected = (end - start) * bytes_per_frame
            if len(payload) != expected:
                raise BusinessError(
                    "E_RENDER_FAILED",
                    output_path,
                    f"encoded block has {len(payload)} bytes, expected {expected}",
                )
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
            data_digest.update(payload)
            completed += 1
            state = _make_state(
                plan,
                output_path=output_path,
                state_path=state_path,
                completed_blocks=completed,
                total_blocks=total_blocks,
                block_size=chosen_block_size,
                data_sha256=data_digest.hexdigest(),
                snapshot_commits=completed,
            )
            atomic_write_json(state_path, state)
            if test_delay_ms > 0.0:
                time.sleep(test_delay_ms / 1000.0)
            interrupted = bool(interrupt_requested and interrupt_requested())

        if interrupted and completed < total_blocks:
            committed_frames = min(plan.total_frames, completed * chosen_block_size)
            _write_header_and_truncate(
                handle,
                sample_rate=context.sample_rate,
                bit_depth=context.bit_depth,
                frames=committed_frames,
            )
            handle.truncate(WAV_HEADER_BYTES + committed_frames * bytes_per_frame)
            handle.flush()
            os.fsync(handle.fileno())
            handle.close()
            recovery = _write_recovery_report(
                recovery_report_path,
                plan=plan,
                state_path=state_path,
                completed_blocks=completed,
                total_blocks=total_blocks,
                data_sha256=data_digest.hexdigest(),
            )
            return recovery

        if completed != total_blocks:
            raise BusinessError(
                "E_RENDER_FAILED",
                output_path,
                f"render stopped at block {completed} of {total_blocks}",
            )
        _write_header_and_truncate(
            handle,
            sample_rate=context.sample_rate,
            bit_depth=context.bit_depth,
            frames=plan.total_frames,
        )
        handle.truncate(WAV_HEADER_BYTES + plan.total_frames * bytes_per_frame)
        handle.flush()
        os.fsync(handle.fileno())
        handle.close()
        final_state = _make_state(
            plan,
            output_path=output_path,
            state_path=state_path,
            completed_blocks=total_blocks,
            total_blocks=total_blocks,
            block_size=chosen_block_size,
            data_sha256=data_digest.hexdigest(),
            snapshot_commits=total_blocks,
        )
        atomic_write_json(state_path, final_state)
        report = _write_master_summary(
            output_path,
            plan,
            block_size=chosen_block_size,
            completed_blocks=completed,
            total_blocks=total_blocks,
        )
        if abs(report["loudness"]["deviation_lu"]) > 0.5:
            raise BusinessError(
                "E_LOUDNESS_TARGET",
                output_path,
                (
                    f"rendered loudness deviation {report['loudness']['deviation_lu']:.3f} LU "
                    "exceeds 0.5 LU"
                ),
            )
        if not report["true_peak"]["compliant"]:
            raise BusinessError(
                "E_TRUE_PEAK_LIMIT",
                output_path,
                (
                    f"rendered true peak {report['true_peak']['dbtp']:.3f} dBTP exceeds "
                    f"limit {context.true_peak_limit_dbtp:.3f} dBTP"
                ),
            )
        atomic_write_json(report_path, report)
        return report
    finally:
        if not handle.closed:
            handle.close()
