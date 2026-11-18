from __future__ import annotations

import math
import os
import signal
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .dsp import (
    SILENCE_DB,
    band_energy,
    band_sir_db,
    duck_envelope,
    fold_to_mono,
    integrated_loudness,
    normalize_loudness_and_peak,
    true_peak_dbtp,
)
from .errors import BusinessError, BusinessFailure, EXIT_INTERRUPTED, EXIT_RENDER, EXIT_RESUME_MISMATCH
from .manifest import Manifest, TrackSpec, ValidatedInput, load_manifest
from .snapshot import SnapshotState, load_snapshot, write_snapshot
from .util import atomic_write_json, db_from_linear, rounded, sample_round, sha256_file
from .wavfile import write_wav


@dataclass(frozen=True)
class TrackPlan:
    track: TrackSpec
    samples: np.ndarray
    alignment_shift_samples: int
    placement_start_samples: int
    detected_local_onsets: tuple[int, ...]
    sync_cue_indices: tuple[int, ...]

    @property
    def aligned_duration_samples(self) -> int:
        return self.placement_start_samples + self.samples.shape[0]


@dataclass(frozen=True)
class RenderResult:
    exit_code: int
    interrupted: bool
    report_path: Path
    snapshot_path: Path
    completed_blocks: int
    total_blocks: int


@dataclass
class _SignalState:
    interrupt_requested: bool = False


def _onset_detector(
    samples: np.ndarray,
    expected_local: int,
    search_radius: int,
    *,
    track_name: str,
) -> int:
    start = max(0, expected_local - search_radius)
    end = min(samples.shape[0], expected_local + search_radius + 1)
    if end <= start:
        raise BusinessFailure(
            [
                BusinessError(
                    "E_TRANSIENT_NOT_FOUND",
                    "transient search window lies outside track",
                    track_name,
                    {"expected_local_sample": expected_local},
                )
            ],
            EXIT_RENDER,
        )
    envelope = np.max(np.abs(samples[start:end]), axis=1)
    peak = float(np.max(envelope))
    if peak <= 1e-8:
        raise BusinessFailure(
            [
                BusinessError(
                    "E_TRANSIENT_NOT_FOUND",
                    "no transient found near expected position",
                    track_name,
                    {"expected_local_sample": expected_local},
                )
            ],
            EXIT_RENDER,
        )
    threshold = max(peak * 0.10, 1e-7)
    candidates = np.flatnonzero(envelope >= threshold)
    return start + int(candidates[0])


def _sync_events(manifest: Manifest, track: TrackSpec) -> tuple[list[int], list[int]]:
    if track.kind == "dialogue":
        cue_indices = [int(value) for value in track.synth["cue_indices"]]
    elif track.kind == "narration":
        cue_indices = [int(segment["cue_index"]) for segment in track.synth["segments"]]
    else:
        return [], []
    cues = [manifest.dialogue_points_samples[index] for index in cue_indices]
    return cues, cue_indices


def _build_plans(parsed: ValidatedInput) -> tuple[list[TrackPlan], int]:
    manifest = parsed.manifest
    plans: list[TrackPlan] = []
    errors: list[BusinessError] = []
    search_radius = max(16, sample_round(0.025, manifest.sample_rate))
    for track in manifest.tracks:
        samples = parsed.audio[track.name].samples
        cues, cue_indices = _sync_events(manifest, track)
        detected: list[int] = []
        shift = 0
        if cues:
            for cue_sample in cues:
                expected_local = cue_sample - track.offset_samples
                try:
                    detected.append(
                        _onset_detector(
                            samples,
                            expected_local,
                            search_radius,
                            track_name=track.name,
                        )
                    )
                except BusinessFailure as failure:
                    errors.extend(failure.errors)
            if len(detected) == len(cues):
                candidate_shifts = [
                    cue_sample - (track.offset_samples + onset)
                    for cue_sample, onset in zip(cues, detected)
                ]
                shift = candidate_shifts[0]
                residuals = [
                    abs(cue_sample - (track.offset_samples + onset + shift))
                    for cue_sample, onset in zip(cues, detected)
                ]
                if max(residuals) > 1:
                    errors.append(
                        BusinessError(
                            "E_ALIGNMENT_FAILED",
                            "one integer shift cannot align all transients within one sample",
                            track.name,
                            {
                                "candidate_shifts": candidate_shifts,
                                "post_alignment_errors": residuals,
                            },
                        )
                    )
        placement = track.offset_samples + shift
        if placement < 0:
            errors.append(
                BusinessError(
                    "E_ALIGNMENT_FAILED",
                    "alignment would move track before master origin",
                    track.name,
                    {"placement_start_samples": placement},
                )
            )
        plans.append(
            TrackPlan(
                track=track,
                samples=samples,
                alignment_shift_samples=shift,
                placement_start_samples=placement,
                detected_local_onsets=tuple(detected),
                sync_cue_indices=tuple(cue_indices),
            )
        )
    if errors:
        raise BusinessFailure(errors, EXIT_RENDER)
    total_frames = max(plan.aligned_duration_samples for plan in plans)
    if total_frames <= 0:
        raise BusinessFailure(
            [BusinessError("E_ALIGNMENT_FAILED", "render result is empty")],
            EXIT_RENDER,
        )
    return plans, total_frames


def _placement_stats(plan: TrackPlan) -> dict[str, Any]:
    onsets = list(plan.detected_local_onsets)
    post_alignment = [
        plan.placement_start_samples + onset for onset in onsets
    ]
    return {
        "name": plan.track.name,
        "kind": plan.track.kind,
        "path": plan.track.relative_path,
        "declared_offset_samples": plan.track.offset_samples,
        "alignment_shift_samples": plan.alignment_shift_samples,
        "applied_offset_samples": plan.placement_start_samples,
        "input_frames": int(plan.samples.shape[0]),
        "aligned_duration_samples": plan.aligned_duration_samples,
        "sync_cue_indices": list(plan.sync_cue_indices),
        "detected_local_onsets": onsets,
        "post_alignment_onsets": post_alignment,
    }


def _display_time(sample: int, sample_rate: int) -> float:
    return rounded(sample / sample_rate, 9)


def _duck_parameters(
    parsed: ValidatedInput,
    plans: list[TrackPlan],
    total_frames: int,
) -> tuple[np.ndarray, list[dict[str, Any]]]:
    manifest = parsed.manifest
    plans_by_name = {plan.track.name: plan for plan in plans}
    intervals: list[dict[str, int]] = []
    for narration in manifest.narration_tracks:
        plan = plans_by_name[narration.name]
        for segment, cue_index in zip(narration.synth["segments"], plan.sync_cue_indices):
            # The narration starts exactly on its cue after integer alignment.
            start = manifest.dialogue_points_samples[cue_index]
            end = start + sample_round(float(segment["duration_seconds"]), manifest.sample_rate)
            start = max(0, min(total_frames, start))
            end = max(0, min(total_frames, end))
            if end > start:
                intervals.append({"start_sample": start, "end_sample": end})

    intervals.sort(key=lambda item: (item["start_sample"], item["end_sample"]))
    merged: list[dict[str, int]] = []
    for interval in intervals:
        if merged and interval["start_sample"] <= merged[-1]["end_sample"]:
            merged[-1]["end_sample"] = max(merged[-1]["end_sample"], interval["end_sample"])
        else:
            merged.append(dict(interval))

    gain, applied = duck_envelope(
        total_frames,
        merged,
        manifest.sample_rate,
        manifest.ducking["depth_db"],
        manifest.ducking["attack_ms"],
        manifest.ducking["recovery_ms"],
    )
    return gain, applied


def _snapshot_parameters(
    plans: list[TrackPlan],
    total_frames: int,
    applied_ducking: list[dict[str, Any]],
) -> dict[str, Any]:
    return {
        "total_frames": int(total_frames),
        "tracks": [
            {
                "name": plan.track.name,
                "alignment_shift_samples": int(plan.alignment_shift_samples),
                "placement_start_samples": int(plan.placement_start_samples),
                "detected_local_onsets": [int(value) for value in plan.detected_local_onsets],
                "sync_cue_indices": [int(value) for value in plan.sync_cue_indices],
                "source_sha256": sha256_file(plan.track.path),
            }
            for plan in plans
        ],
        "ducking_intervals": [
            {
                "start_sample": int(item["start_sample"]),
                "end_sample": int(item["end_sample"]),
                "attack_samples": int(item["attack_samples"]),
                "recovery_samples": int(item["recovery_samples"]),
                "gain_db": rounded(float(item["gain_db"]), 9),
            }
            for item in applied_ducking
        ],
    }


def _add_plan_to_buffer(
    destination: np.ndarray,
    plan: TrackPlan,
    block_start: int,
    block_end: int,
    *,
    music_gain: np.ndarray | None = None,
) -> None:
    source_start = max(block_start, plan.placement_start_samples)
    source_end = min(block_end, plan.aligned_duration_samples)
    if source_end <= source_start:
        return
    source_offset = source_start - plan.placement_start_samples
    destination_offset = source_start - block_start
    length = source_end - source_start
    block = plan.samples[source_offset : source_offset + length]
    if block.shape[1] == 1:
        block = np.repeat(block, 2, axis=1)
    if music_gain is not None:
        gain_segment = music_gain[source_start:source_end, None]
        block = block * gain_segment
    destination[destination_offset : destination_offset + length] += block


def _mix_block(
    plans: list[TrackPlan],
    duck_gain: np.ndarray,
    block_start: int,
    block_end: int,
    *,
    include: Callable[[TrackPlan], bool] | None = None,
) -> np.ndarray:
    output = np.zeros((block_end - block_start, 2), dtype=np.float64)
    for plan in plans:
        if include is not None and not include(plan):
            continue
        gain = duck_gain if plan.track.kind == "music" else None
        _add_plan_to_buffer(output, plan, block_start, block_end, music_gain=gain)
    return output


def _stem_segment(
    plans: list[TrackPlan],
    duck_gain: np.ndarray,
    start: int,
    end: int,
    kinds: set[str],
) -> np.ndarray:
    output = np.zeros((end - start, 2), dtype=np.float64)
    for plan in plans:
        if plan.track.kind not in kinds:
            continue
        gain = duck_gain if plan.track.kind == "music" else None
        _add_plan_to_buffer(output, plan, start, end, music_gain=gain)
    return output


def _energy_ratio(numerator: np.ndarray, denominator: np.ndarray, sample_rate: int, band: tuple[float, float]) -> float:
    numerator_energy = band_energy(numerator, sample_rate, band)
    denominator_energy = band_energy(denominator, sample_rate, band)
    if denominator_energy <= 1e-30:
        return 0.0
    return numerator_energy / denominator_energy


def _fold_key_interval(
    plans: list[TrackPlan],
    duck_gain: np.ndarray,
    start: int,
    end: int,
    sample_rate: int,
    fold_rule: dict[str, Any],
    band: tuple[float, float],
    label: str,
) -> dict[str, Any]:
    dialogue = _stem_segment(plans, duck_gain, start, end, {"dialogue"})
    narration = _stem_segment(plans, duck_gain, start, end, {"narration"})
    total = _stem_segment(plans, duck_gain, start, end, {"dialogue", "narration", "music"})
    dialogue_mono = fold_to_mono(dialogue, fold_rule)
    narration_mono = fold_to_mono(narration, fold_rule)
    total_mono = fold_to_mono(total, fold_rule)

    stereo_dialogue = _energy_ratio(dialogue, total, sample_rate, band)
    stereo_narration = _energy_ratio(narration, total, sample_rate, band)
    mono_dialogue = _energy_ratio(dialogue_mono, total_mono, sample_rate, band)
    mono_narration = _energy_ratio(narration_mono, total_mono, sample_rate, band)
    return {
        "label": label,
        "start_sample": int(start),
        "end_sample": int(end),
        "stereo_dialogue_ratio": rounded(stereo_dialogue, 9),
        "mono_dialogue_ratio": rounded(mono_dialogue, 9),
        "dialogue_deviation": rounded(abs(mono_dialogue - stereo_dialogue), 9),
        "stereo_narration_ratio": rounded(stereo_narration, 9),
        "mono_narration_ratio": rounded(mono_narration, 9),
        "narration_deviation": rounded(abs(mono_narration - stereo_narration), 9),
    }


def _fold_analysis(
    parsed: ValidatedInput,
    plans: list[TrackPlan],
    duck_gain: np.ndarray,
    total_frames: int,
    applied_ducking: list[dict[str, Any]],
) -> dict[str, Any]:
    manifest = parsed.manifest
    band = tuple(float(value) for value in manifest.mono_fold.get("band_hz", [300.0, 3400.0]))
    overall = _fold_key_interval(
        plans,
        duck_gain,
        0,
        total_frames,
        manifest.sample_rate,
        manifest.mono_fold,
        band,
        "overall",
    )
    keys = [overall]
    for cue_index, cue_sample in enumerate(manifest.dialogue_points_samples):
        end = min(total_frames, cue_sample + sample_round(0.180, manifest.sample_rate))
        if end > cue_sample:
            keys.append(
                _fold_key_interval(
                    plans,
                    duck_gain,
                    cue_sample,
                    end,
                    manifest.sample_rate,
                    manifest.mono_fold,
                    band,
                    f"dialogue_cue_{cue_index}",
                )
            )
    for interval_index, interval in enumerate(applied_ducking):
        keys.append(
            _fold_key_interval(
                plans,
                duck_gain,
                int(interval["start_sample"]),
                int(interval["end_sample"]),
                manifest.sample_rate,
                manifest.mono_fold,
                band,
                f"narration_{interval_index}",
            )
        )
    max_deviation = max(
        max(item["dialogue_deviation"], item["narration_deviation"]) for item in keys
    )
    tolerance = float(manifest.mono_fold["energy_tolerance"])
    return {
        "rule": manifest.mono_fold["rule"],
        "band_hz": [band[0], band[1]],
        "energy_tolerance": rounded(tolerance, 9),
        "stereo_dialogue_ratio": overall["stereo_dialogue_ratio"],
        "mono_dialogue_ratio": overall["mono_dialogue_ratio"],
        "stereo_narration_ratio": overall["stereo_narration_ratio"],
        "mono_narration_ratio": overall["mono_narration_ratio"],
        "max_ratio_deviation": rounded(max_deviation, 9),
        "within_tolerance": bool(max_deviation <= tolerance + 1e-12),
        "key_intervals": keys,
    }


def _ducking_report(
    parsed: ValidatedInput,
    plans: list[TrackPlan],
    duck_gain: np.ndarray,
    applied: list[dict[str, Any]],
) -> dict[str, Any]:
    manifest = parsed.manifest
    band = tuple(float(value) for value in manifest.mono_fold.get("band_hz", [300.0, 3400.0]))
    intervals: list[dict[str, Any]] = []
    for index, item in enumerate(applied):
        start = int(item["start_sample"])
        end = int(item["end_sample"])
        narration = _stem_segment(plans, duck_gain, start, end, {"narration"})
        music_before = _stem_segment(
            plans,
            np.ones_like(duck_gain),
            start,
            end,
            {"music"},
        )
        music_after = music_before * duck_gain[start:end, None]
        before = band_sir_db(narration, music_before, manifest.sample_rate, band)
        after = band_sir_db(narration, music_after, manifest.sample_rate, band)
        intervals.append(
            {
                "index": index,
                "start_sample": start,
                "end_sample": end,
                "start_seconds": _display_time(start, manifest.sample_rate),
                "end_seconds": _display_time(end, manifest.sample_rate),
                "applied_gain_db": rounded(float(item["gain_db"]), 9),
                "attack_ms": rounded(item["attack_samples"] * 1000.0 / manifest.sample_rate, 6),
                "recovery_ms": rounded(item["recovery_samples"] * 1000.0 / manifest.sample_rate, 6),
                "sir_before_db": rounded(before, 6),
                "sir_after_db": rounded(after, 6),
                "sir_improvement_db": rounded(after - before, 6),
            }
        )
    return {
        "depth_db": rounded(manifest.ducking["depth_db"], 6),
        "attack_ms": rounded(manifest.ducking["attack_ms"], 6),
        "recovery_ms": rounded(manifest.ducking["recovery_ms"], 6),
        "sir_min_improvement_db": rounded(manifest.ducking["sir_min_improvement_db"], 6),
        "interval_count": len(intervals),
        "intervals": intervals,
    }


def _alignment_report(
    manifest: Manifest,
    plans: list[TrackPlan],
    total_frames: int,
) -> dict[str, Any]:
    tracks: list[dict[str, Any]] = []
    max_error = 0
    for plan in plans:
        item = _placement_stats(plan)
        errors: list[int] = []
        for cue_index, onset in zip(plan.sync_cue_indices, plan.detected_local_onsets):
            expected = manifest.dialogue_points_samples[cue_index]
            actual = plan.placement_start_samples + onset
            errors.append(abs(actual - expected))
        item["alignment_error_samples"] = errors
        if errors:
            max_error = max(max_error, max(errors))
        tracks.append(item)
    return {
        "total_frames": int(total_frames),
        "max_alignment_error_samples": int(max_error),
        "tracks": tracks,
    }


def _business_error(code: str, message: str, **details: Any) -> BusinessFailure:
    return BusinessFailure([BusinessError(code, message, details=details)], EXIT_RENDER)


def _write_recovery_report(
    manifest: Manifest,
    snapshot_state: SnapshotState,
    *,
    total_frames: int,
    total_blocks: int,
) -> Path:
    path = manifest.output_dir / manifest.render["recovery_report"]
    payload = {
        "event": "render_interrupted",
        "code": "E_INTERRUPTED_RESUMABLE",
        "message": "current block committed; resume with --resume",
        "manifest_digest": manifest.digest,
        "snapshot_path": str(manifest.snapshot_path),
        "completed_blocks": int(snapshot_state.completed_blocks),
        "total_blocks": int(total_blocks),
        "completed_frames": int(snapshot_state.prefix.shape[0]),
        "total_frames": int(total_frames),
        "commits": int(snapshot_state.commits),
    }
    atomic_write_json(path, payload)
    return path


def _build_report(
    parsed: ValidatedInput,
    plans: list[TrackPlan],
    total_frames: int,
    duck_gain: np.ndarray,
    applied_ducking: list[dict[str, Any]],
    master: np.ndarray,
    mono: np.ndarray,
    loudness: dict[str, Any],
    snapshot_commits: int,
    resumed_from_block: int,
) -> dict[str, Any]:
    manifest = parsed.manifest
    alignment = _alignment_report(manifest, plans, total_frames)
    ducking = _ducking_report(parsed, plans, duck_gain, applied_ducking)
    fold = _fold_analysis(parsed, plans, duck_gain, total_frames, applied_ducking)
    mono_peak = true_peak_dbtp(mono)

    errors: list[BusinessError] = []
    if alignment["max_alignment_error_samples"] > 1:
        errors.append(
            BusinessError(
                "E_ALIGNMENT_FAILED",
                "post-alignment transient error exceeds one sample",
                details={"max_alignment_error_samples": alignment["max_alignment_error_samples"]},
            )
        )
    for interval in ducking["intervals"]:
        if interval["sir_improvement_db"] + 1e-9 < ducking["sir_min_improvement_db"]:
            errors.append(
                BusinessError(
                    "E_DUCKING_SIR",
                    "narration-to-music SIR improvement is below the declared minimum",
                    details={
                        "interval_index": interval["index"],
                        "measured_db": interval["sir_improvement_db"],
                        "required_db": ducking["sir_min_improvement_db"],
                    },
                )
            )
    if not fold["within_tolerance"]:
        errors.append(
            BusinessError(
                "E_FOLD_ENERGY",
                "mono fold energy ratio deviation exceeds tolerance",
                details={
                    "max_ratio_deviation": fold["max_ratio_deviation"],
                    "tolerance": fold["energy_tolerance"],
                },
            )
        )
    if mono_peak > manifest.loudness["true_peak_dbtp"] + 1e-6:
        errors.append(
            BusinessError(
                "E_MONO_TRUE_PEAK",
                "mono fold exceeds true peak ceiling",
                details={
                    "mono_true_peak_dbtp": rounded(mono_peak, 6),
                    "ceiling_dbtp": manifest.loudness["true_peak_dbtp"],
                },
            )
        )
    if errors:
        raise BusinessFailure(errors, EXIT_RENDER)

    return {
        "schema": "mastering-report/v1",
        "manifest": {
            "path": str(manifest.path.name),
            "digest": manifest.digest,
            "seed": manifest.seed,
        },
        "master": {
            "sample_rate": manifest.sample_rate,
            "channels": 2,
            "bit_depth": manifest.bit_depth,
            "frames": int(master.shape[0]),
            "duration_seconds": _display_time(int(master.shape[0]), manifest.sample_rate),
            "stereo_file": manifest.render["master_stereo"],
            "mono_file": manifest.render["master_mono"],
            "stereo_true_peak_dbtp": loudness["true_peak_dbtp"],
            "mono_true_peak_dbtp": rounded(mono_peak, 6),
        },
        "alignment": alignment,
        "fold": fold,
        "ducking": ducking,
        "loudness": loudness,
        "snapshot": {
            "path": str(manifest.snapshot_path.name),
            "commits": int(snapshot_commits),
            "resumed_from_block": int(resumed_from_block),
            "completed_blocks": int((total_frames + manifest.render["block_frames"] - 1) // manifest.render["block_frames"]),
        },
        "source_files": {
            track.name: sha256_file(track.path) for track in manifest.tracks
        },
    }


def render_project(manifest_path: Path, *, resume: bool) -> RenderResult:
    parsed = load_manifest(manifest_path, validate_audio=True)
    manifest = parsed.manifest
    plans, total_frames = _build_plans(parsed)
    duck_gain, applied_ducking = _duck_parameters(parsed, plans, total_frames)
    parameters = _snapshot_parameters(plans, total_frames, applied_ducking)

    if not resume:
        try:
            manifest.snapshot_path.unlink(missing_ok=True)
        except OSError as exc:
            raise _business_error("E_SNAPSHOT_IO", f"cannot clear old snapshot: {exc}") from exc
        snapshot_state = None
    else:
        snapshot_state = load_snapshot(
            manifest.snapshot_path,
            manifest_digest=manifest.digest,
            sample_rate=manifest.sample_rate,
            block_frames=manifest.render["block_frames"],
            total_frames=total_frames,
            parameters=parameters,
        )
    if snapshot_state is None:
        snapshot_state = SnapshotState(
            completed_blocks=0,
            prefix=np.zeros((0, 2), dtype=np.float64),
            commits=0,
        )

    resumed_from_block = snapshot_state.completed_blocks
    block_frames = int(manifest.render["block_frames"])
    total_blocks = (total_frames + block_frames - 1) // block_frames
    prefix = snapshot_state.prefix
    commits = snapshot_state.commits
    interrupt_state = _SignalState()

    def handle_sigint(_signum: int, _frame: Any) -> None:
        interrupt_state.interrupt_requested = True

    previous_handler = signal.getsignal(signal.SIGINT)
    signal.signal(signal.SIGINT, handle_sigint)
    test_crash_after = int(os.environ.get("MASTERING_TEST_CRASH_AFTER_BLOCK", "0") or "0")
    test_interrupt_after = int(os.environ.get("MASTERING_TEST_INTERRUPT_AFTER_BLOCK", "0") or "0")
    latest_state = snapshot_state
    interrupted = False

    try:
        for completed_before in range(resumed_from_block, total_blocks):
            block_start = completed_before * block_frames
            block_end = min(total_frames, block_start + block_frames)
            block = _mix_block(plans, duck_gain, block_start, block_end)
            prefix = np.concatenate((prefix, block), axis=0)
            commits += 1
            latest_state = SnapshotState(
                completed_blocks=completed_before + 1,
                prefix=prefix,
                commits=commits,
            )
            write_snapshot(
                manifest.snapshot_path,
                manifest_digest=manifest.digest,
                sample_rate=manifest.sample_rate,
                block_frames=block_frames,
                total_frames=total_frames,
                completed_blocks=latest_state.completed_blocks,
                commits=latest_state.commits,
                parameters=parameters,
                prefix=prefix,
            )
            if test_crash_after and latest_state.completed_blocks >= test_crash_after:
                os._exit(137)
            if (
                interrupt_state.interrupt_requested
                or (test_interrupt_after and latest_state.completed_blocks >= test_interrupt_after)
            ):
                interrupted = True
                break
    finally:
        signal.signal(signal.SIGINT, previous_handler)

    if interrupted and latest_state.completed_blocks < total_blocks:
        recovery_path = _write_recovery_report(
            manifest,
            latest_state,
            total_frames=total_frames,
            total_blocks=total_blocks,
        )
        return RenderResult(
            exit_code=EXIT_INTERRUPTED,
            interrupted=True,
            report_path=recovery_path,
            snapshot_path=manifest.snapshot_path,
            completed_blocks=latest_state.completed_blocks,
            total_blocks=total_blocks,
        )

    if latest_state.completed_blocks != total_blocks or prefix.shape[0] != total_frames:
        raise _business_error(
            "E_RENDER_STATE",
            "render did not produce the complete master timeline",
            completed_blocks=latest_state.completed_blocks,
            total_blocks=total_blocks,
            frames=int(prefix.shape[0]),
            expected_frames=total_frames,
        )

    try:
        master, loudness = normalize_loudness_and_peak(
            prefix,
            manifest.sample_rate,
            manifest.loudness["target_lufs"],
            manifest.loudness["true_peak_dbtp"],
            manifest.loudness["tolerance_lu"],
        )
    except ValueError as exc:
        raise _business_error("E_LOUDNESS_COMPLIANCE", str(exc)) from exc

    mono = fold_to_mono(master, manifest.mono_fold)
    mono_peak = true_peak_dbtp(mono)
    if mono_peak > manifest.loudness["true_peak_dbtp"] + 1e-6:
        trim_db = manifest.loudness["true_peak_dbtp"] - mono_peak
        trim = 10.0 ** (trim_db / 20.0)
        master = master * trim
        mono = fold_to_mono(master, manifest.mono_fold)
        loudness["measured_lufs"] = rounded(integrated_loudness(master, manifest.sample_rate), 6)
        loudness["deviation_lu"] = rounded(
            loudness["measured_lufs"] - manifest.loudness["target_lufs"], 6
        )
        loudness["true_peak_dbtp"] = rounded(true_peak_dbtp(master), 6)
        loudness["after_limiter_lufs"] = loudness["measured_lufs"]
        loudness["after_limiter_true_peak_dbtp"] = loudness["true_peak_dbtp"]
        if abs(loudness["deviation_lu"]) > manifest.loudness["tolerance_lu"]:
            raise _business_error(
                "E_MONO_TRUE_PEAK",
                "mono peak safety trim also violates the loudness tolerance",
                deviation_lu=loudness["deviation_lu"],
                tolerance_lu=manifest.loudness["tolerance_lu"],
            )

    report = _build_report(
        parsed,
        plans,
        total_frames,
        duck_gain,
        applied_ducking,
        master,
        mono,
        loudness,
        latest_state.commits,
        resumed_from_block,
    )
    manifest.output_dir.mkdir(parents=True, exist_ok=True)
    write_wav(
        manifest.output_dir / manifest.render["master_stereo"],
        master,
        manifest.sample_rate,
        manifest.bit_depth,
    )
    write_wav(
        manifest.output_dir / manifest.render["master_mono"],
        mono,
        manifest.sample_rate,
        manifest.bit_depth,
    )
    report_path = manifest.output_dir / manifest.render["report"]
    atomic_write_json(report_path, report)
    recovery_path = manifest.output_dir / manifest.render["recovery_report"]
    try:
        recovery_path.unlink(missing_ok=True)
    except OSError:
        pass
    return RenderResult(
        exit_code=0,
        interrupted=False,
        report_path=report_path,
        snapshot_path=manifest.snapshot_path,
        completed_blocks=latest_state.completed_blocks,
        total_blocks=total_blocks,
    )
