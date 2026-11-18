"""End-to-end mastering pipeline with atomic block snapshots."""

from __future__ import annotations

import hashlib
import json
import math
import shutil
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

import numpy as np

from .analysis import (
    BreakpointInterval,
    CompensationStats,
    FrameBandData,
    compensate_intervals,
    detect_all_breakpoints,
)
from .errors import BusinessError, EXIT_RESUME_MISMATCH, fail, issue
from .manifest import Project, input_digest, load_project, manifest_digest
from .mastering import MasterResult, balance_and_master, read_project_audio
from .pcm import (
    _atomic_write,
    encode_pcm,
    write_wav_from_blocks,
)


SNAPSHOT_SCHEMA_VERSION = 1
PIPELINE_VERSION = "1.0"
VOICE_ORDER = ("女高", "女低", "男高", "男低")


@dataclass
class PreparedRender:
    project: Project
    manifest_sha256: str
    input_sha256: str
    total_frames: int
    block_frames: int
    intervals: dict[str, list[BreakpointInterval]]
    frame_data: dict[str, FrameBandData]
    compensation: list[CompensationStats]
    voice_buses: dict[str, np.ndarray]
    master: MasterResult
    pcm_bytes: bytes
    analysis: dict


@dataclass
class RenderOutcome:
    status: str
    completed_blocks: int
    total_blocks: int
    resume_count: int
    output_path: str
    report_path: str
    recovery_report_path: str | None

    def as_dict(self) -> dict:
        result = {
            "status": self.status,
            "completed_blocks": self.completed_blocks,
            "total_blocks": self.total_blocks,
            "resume_count": self.resume_count,
            "output": Path(self.output_path).name,
            "report": Path(self.report_path).name,
        }
        if self.recovery_report_path is not None:
            result["recovery_report"] = Path(self.recovery_report_path).name
        return result


def _atomic_json(path: Path, value: dict) -> None:
    text = json.dumps(value, ensure_ascii=False, indent=2) + "\n"
    _atomic_write(path, text.encode("utf-8"))


def _read_json(path: Path) -> dict:
    with open(path, "r", encoding="utf-8") as handle:
        value = json.load(handle)
    if not isinstance(value, dict):
        raise ValueError("state root is not an object")
    return value


def _analysis_document(
    intervals: dict[str, list[BreakpointInterval]],
    correction_gains: dict[str, float],
    total_frames: int,
) -> dict:
    return {
        "total_frames": int(total_frames),
        "voice_intervals": [
            {
                "voice": voice,
                "intervals": [item.as_dict() for item in intervals[voice]],
            }
            for voice in VOICE_ORDER
        ],
        "voice_correction_gains": [
            {
                "voice": voice,
                "gain": round(float(correction_gains[voice]), 12),
                "gain_db": round(
                    20.0 * math.log10(max(float(correction_gains[voice]), 1e-30)),
                    9,
                ),
            }
            for voice in VOICE_ORDER
        ],
    }


def _concatenate_mics(mics: list[np.ndarray]) -> np.ndarray:
    arrays = [np.asarray(mic, dtype=np.float64) for mic in mics]
    return np.concatenate(arrays, axis=1)


def _split_compensated(
    combined: np.ndarray,
    channel_counts: list[int],
) -> list[np.ndarray]:
    result: list[np.ndarray] = []
    cursor = 0
    for channels in channel_counts:
        result.append(combined[:, cursor : cursor + channels])
        cursor += channels
    return result


def prepare_render(
    project: Project,
    *,
    block_frames: int | None = None,
) -> PreparedRender:
    voice_signals, total_frames = read_project_audio(project)
    band_by_voice = {
        voice: (
            project.voices[voice].band_low_hz,
            project.voices[voice].band_high_hz,
        )
        for voice in VOICE_ORDER
    }
    intervals, frame_data = detect_all_breakpoints(
        voice_signals,
        project.sample_rate,
        band_by_voice,
    )

    compensated_signals: dict[str, list[np.ndarray]] = {}
    compensation_stats: list[CompensationStats] = []
    for voice in VOICE_ORDER:
        if voice == "男高" and intervals[voice]:
            combined = _concatenate_mics(voice_signals[voice])
            processed, voice_stats = compensate_intervals(
                voice,
                combined,
                project.sample_rate,
                project.voices[voice].band_low_hz,
                project.voices[voice].band_high_hz,
                intervals[voice],
            )
            counts = [mic.shape[1] for mic in voice_signals[voice]]
            compensated_signals[voice] = _split_compensated(processed, counts)
            compensation_stats.extend(voice_stats)
        else:
            compensated_signals[voice] = [
                mic.copy() for mic in voice_signals[voice]
            ]

    # Build voice buses in manifest microphone order so arbitrary channel
    # counts and downmix weights remain deterministic.
    buses: dict[str, np.ndarray] = {}
    for voice in VOICE_ORDER:
        bus = np.zeros(total_frames, dtype=np.float64)
        signals = compensated_signals[voice]
        mics = sorted(project.mics_for_voice(voice), key=lambda item: item.name)
        for mic, mic_audio in zip(mics, signals):
            mono = np.mean(mic_audio, axis=1)
            bus += mic.downmix_weight * mono
        buses[voice] = bus

    master = balance_and_master(buses, project)
    pcm_bytes = encode_pcm(master.quantized, project.bit_depth)
    gains = {balance.voice: balance.correction_gain for balance in master.balances}
    analysis = _analysis_document(intervals, gains, total_frames)
    return PreparedRender(
        project=project,
        manifest_sha256=manifest_digest(project.manifest),
        input_sha256=input_digest(project),
        total_frames=total_frames,
        block_frames=int(block_frames or project.block_frames),
        intervals=intervals,
        frame_data=frame_data,
        compensation=compensation_stats,
        voice_buses=buses,
        master=master,
        pcm_bytes=pcm_bytes,
        analysis=analysis,
    )


def _state_document(
    prepared: PreparedRender,
    *,
    completed_blocks: int,
    total_blocks: int,
    resume_count: int,
    status: str,
    block_hashes: list[str],
    output_sha256: str | None = None,
) -> dict:
    return {
        "snapshot_schema_version": SNAPSHOT_SCHEMA_VERSION,
        "pipeline_version": PIPELINE_VERSION,
        "manifest_sha256": prepared.manifest_sha256,
        "input_sha256": prepared.input_sha256,
        "sample_rate": prepared.project.sample_rate,
        "bit_depth": prepared.project.bit_depth,
        "channels": 2,
        "total_frames": prepared.total_frames,
        "block_frames": prepared.block_frames,
        "total_blocks": total_blocks,
        "completed_blocks": completed_blocks,
        "resume_count": resume_count,
        "status": status,
        "analysis": prepared.analysis,
        "committed_block_sha256": block_hashes,
        "output_sha256": output_sha256,
    }


def _report_document(
    prepared: PreparedRender,
    *,
    status: str,
    completed_blocks: int,
    total_blocks: int,
    resume_count: int,
    output_sha256: str | None,
) -> dict:
    master = prepared.master
    intervals_by_voice = {
        voice: [item.as_dict() for item in prepared.intervals[voice]]
        for voice in VOICE_ORDER
    }
    all_intervals = [
        item for voice in VOICE_ORDER for item in prepared.intervals[voice]
    ]
    max_balance_deviation = max(
        abs(balance.achieved_ratio - balance.target_ratio)
        for balance in master.balances
    )
    min_improvement = (
        min(stat.improvement_db for stat in prepared.compensation)
        if prepared.compensation
        else None
    )
    loudness_deviation = (
        master.integrated_loudness_lufs - prepared.project.target_loudness_lufs
    )
    return {
        "schema_version": 1,
        "status": status,
        "manifest_sha256": prepared.manifest_sha256,
        "input_sha256": prepared.input_sha256,
        "output_sha256": output_sha256,
        "sample_rate": prepared.project.sample_rate,
        "bit_depth": prepared.project.bit_depth,
        "channels": 2,
        "total_frames": prepared.total_frames,
        "block_frames": prepared.block_frames,
        "total_blocks": total_blocks,
        "completed_blocks": completed_blocks,
        "recovery_count": resume_count,
        "breakpoint_intervals": [
            {"voice": voice, "intervals": intervals_by_voice[voice]}
            for voice in VOICE_ORDER
        ],
        "compensation": [stat.as_dict() for stat in prepared.compensation],
        "voice_balance": [balance.as_dict() for balance in master.balances],
        "voice_contribution_percent": {
            balance.voice: round(balance.achieved_ratio * 100.0, 6)
            for balance in master.balances
        },
        "master_loudness_lufs": round(master.integrated_loudness_lufs, 6),
        "loudness_target_lufs": round(
            prepared.project.target_loudness_lufs, 6
        ),
        "loudness_deviation_lu": round(loudness_deviation, 6),
        "true_peak_dbtp": round(master.true_peak_dbtp, 6),
        "true_peak_limit_dbtp": round(
            prepared.project.true_peak_limit_dbtp, 6
        ),
        "sample_peak_dbfs": round(master.sample_peak_dbfs, 6),
        "limiter_triggered_samples": int(master.limiter_triggered_samples),
        "limiter_active": bool(master.limiter_active),
        "checks": {
            "breakpoint_intervals_detected": len(all_intervals),
            "compensation_interval_count": len(prepared.compensation),
            "minimum_compensation_improvement_db": (
                round(min_improvement, 6)
                if min_improvement is not None
                else None
            ),
            "maximum_voice_balance_deviation": round(
                max_balance_deviation, 9
            ),
            "loudness_within_0_5_lu": abs(loudness_deviation) <= 0.5,
            "true_peak_within_limit": (
                master.true_peak_dbtp
                <= prepared.project.true_peak_limit_dbtp + 1e-9
            ),
        },
    }


def _block_path(snapshot_dir: Path, index: int) -> Path:
    return snapshot_dir / "blocks" / f"block-{index:08d}.pcm"


def _validate_snapshot(
    snapshot_dir: Path,
    prepared: PreparedRender,
    state: dict,
    total_blocks: int,
) -> list[str]:
    expected = {
        "snapshot_schema_version": SNAPSHOT_SCHEMA_VERSION,
        "pipeline_version": PIPELINE_VERSION,
        "manifest_sha256": prepared.manifest_sha256,
        "input_sha256": prepared.input_sha256,
        "sample_rate": prepared.project.sample_rate,
        "bit_depth": prepared.project.bit_depth,
        "channels": 2,
        "total_frames": prepared.total_frames,
        "block_frames": prepared.block_frames,
        "total_blocks": total_blocks,
    }
    for key, value in expected.items():
        if state.get(key) != value:
            raise ValueError(f"snapshot field {key} does not match current input")
    if state.get("analysis") != prepared.analysis:
        raise ValueError("confirmed breakpoint or gain results changed")
    completed = state.get("completed_blocks")
    if (
        isinstance(completed, bool)
        or not isinstance(completed, int)
        or completed < 0
        or completed > total_blocks
    ):
        raise ValueError("snapshot completed_blocks is invalid")
    hashes = state.get("committed_block_sha256")
    if not isinstance(hashes, list) or len(hashes) != completed:
        raise ValueError("snapshot block hash list is invalid")
    block_bytes = (
        prepared.block_frames
        * 2
        * (prepared.project.bit_depth // 8)
    )
    for index in range(completed):
        path = _block_path(snapshot_dir, index)
        if not path.is_file():
            raise ValueError("committed snapshot block is missing")
        expected_size = (
            block_bytes
            if index < total_blocks - 1
            else prepared.pcm_bytes.__len__() - index * block_bytes
        )
        if path.stat().st_size != expected_size:
            raise ValueError("committed snapshot block has an unexpected size")
        actual_hash = hashlib.sha256(path.read_bytes()).hexdigest()
        if actual_hash != hashes[index]:
            raise ValueError("committed snapshot block hash is invalid")
    return hashes


def _discard_progress(
    snapshot_dir: Path,
    output_path: Path,
    report_path: Path,
) -> None:
    if snapshot_dir.exists():
        shutil.rmtree(snapshot_dir, ignore_errors=True)
    recovery_path = report_path.with_name(
        report_path.stem + ".recovery" + report_path.suffix
    )
    for stale in (output_path, report_path, recovery_path):
        if not stale.exists():
            continue
        try:
            stale.unlink()
        except OSError:
            pass


def _resume_mismatch(reason: str) -> None:
    raise BusinessError(
        [
            issue(
                "E_RESUME_MISMATCH",
                "<master>",
                "existing progress was discarded because it does not match the current input",
                reason=reason,
            )
        ],
        exit_code=EXIT_RESUME_MISMATCH,
    )


def render_project(
    manifest_path: str | Path,
    *,
    output_path: str | Path,
    report_path: str | Path,
    snapshot_dir: str | Path,
    block_frames: int | None = None,
    fresh: bool = False,
    block_pause_ms: float = 0.0,
    should_interrupt: Callable[[], bool] | None = None,
) -> RenderOutcome:
    project = load_project(manifest_path, check_files=True)
    output = Path(output_path).expanduser().resolve()
    report = Path(report_path).expanduser().resolve()
    snapshot = Path(snapshot_dir).expanduser().resolve()
    current_manifest_digest = manifest_digest(project.manifest)
    current_input_digest = input_digest(project)
    chosen_block_frames = int(block_frames or project.block_frames)
    if chosen_block_frames <= 0:
        fail(
            "E_BLOCK_SIZE_INVALID",
            "<manifest>",
            "block_frames must be a positive integer",
        )

    if fresh:
        _discard_progress(snapshot, output, report)

    existing_state: dict | None = None
    if snapshot.exists():
        try:
            if not snapshot.is_dir():
                raise ValueError("snapshot path is not a directory")
            existing_state = _read_json(snapshot / "state.json")
        except (OSError, ValueError, json.JSONDecodeError) as exc:
            _discard_progress(snapshot, output, report)
            _resume_mismatch(f"snapshot state cannot be read: {type(exc).__name__}")
        if existing_state.get("manifest_sha256") != current_manifest_digest:
            _discard_progress(snapshot, output, report)
            _resume_mismatch("manifest summary changed")
        if existing_state.get("input_sha256") != current_input_digest:
            _discard_progress(snapshot, output, report)
            _resume_mismatch("audio input summary changed")
        if existing_state.get("block_frames") != chosen_block_frames:
            _discard_progress(snapshot, output, report)
            _resume_mismatch("block size changed")

    prepared = prepare_render(project, block_frames=chosen_block_frames)
    total_blocks = int(math.ceil(prepared.total_frames / prepared.block_frames))
    block_hashes: list[str] = []
    resume_count = 0

    if existing_state is not None:
        try:
            block_hashes = _validate_snapshot(
                snapshot,
                prepared,
                existing_state,
                total_blocks,
            )
        except (OSError, ValueError) as exc:
            _discard_progress(snapshot, output, report)
            _resume_mismatch(str(exc))
        completed = int(existing_state["completed_blocks"])
        resume_count = int(existing_state.get("resume_count", 0)) + 1
    else:
        completed = 0
        snapshot.mkdir(parents=True, exist_ok=True)
        (snapshot / "blocks").mkdir(parents=True, exist_ok=True)

    state = _state_document(
        prepared,
        completed_blocks=completed,
        total_blocks=total_blocks,
        resume_count=resume_count,
        status="partial",
        block_hashes=block_hashes,
    )
    _atomic_json(snapshot / "state.json", state)

    channels = 2
    bytes_per_sample = prepared.project.bit_depth // 8
    block_bytes = (
        prepared.block_frames * channels * bytes_per_sample
    )
    interrupted = False
    for index in range(completed, total_blocks):
        start = index * block_bytes
        end = min(prepared.pcm_bytes.__len__(), start + block_bytes)
        payload = prepared.pcm_bytes[start:end]
        path = _block_path(snapshot, index)
        _atomic_write(path, payload)
        block_hash = hashlib.sha256(payload).hexdigest()
        if index < len(block_hashes):
            block_hashes[index] = block_hash
        else:
            block_hashes.append(block_hash)
        completed = index + 1
        state = _state_document(
            prepared,
            completed_blocks=completed,
            total_blocks=total_blocks,
            resume_count=resume_count,
            status="partial",
            block_hashes=block_hashes,
        )
        _atomic_json(snapshot / "state.json", state)
        if block_pause_ms > 0:
            time.sleep(block_pause_ms / 1000.0)
        if should_interrupt is not None and should_interrupt():
            interrupted = True
            break

    recovery_path = report.with_name(report.stem + ".recovery" + report.suffix)
    if interrupted and completed < total_blocks:
        partial_report = _report_document(
            prepared,
            status="interrupted",
            completed_blocks=completed,
            total_blocks=total_blocks,
            resume_count=resume_count,
            output_sha256=None,
        )
        _atomic_json(report, partial_report)
        _atomic_json(recovery_path, partial_report)
        return RenderOutcome(
            status="interrupted",
            completed_blocks=completed,
            total_blocks=total_blocks,
            resume_count=resume_count,
            output_path=str(output),
            report_path=str(report),
            recovery_report_path=str(recovery_path),
        )

    blocks = [_block_path(snapshot, index) for index in range(total_blocks)]
    frames_written = write_wav_from_blocks(
        output,
        blocks,
        sample_rate=prepared.project.sample_rate,
        channels=channels,
        bit_depth=prepared.project.bit_depth,
    )
    if frames_written != prepared.total_frames:
        fail(
            "E_OUTPUT_LENGTH",
            "<master>",
            "assembled master frame count is incorrect",
            exit_code=4,
        )
    output_hash = hashlib.sha256(output.read_bytes()).hexdigest()
    state = _state_document(
        prepared,
        completed_blocks=total_blocks,
        total_blocks=total_blocks,
        resume_count=resume_count,
        status="complete",
        block_hashes=block_hashes,
        output_sha256=output_hash,
    )
    _atomic_json(snapshot / "state.json", state)
    final_report = _report_document(
        prepared,
        status="complete",
        completed_blocks=total_blocks,
        total_blocks=total_blocks,
        resume_count=resume_count,
        output_sha256=output_hash,
    )
    _atomic_json(report, final_report)
    try:
        if recovery_path.exists():
            recovery_path.unlink()
    except OSError:
        pass
    return RenderOutcome(
        status="complete",
        completed_blocks=total_blocks,
        total_blocks=total_blocks,
        resume_count=resume_count,
        output_path=str(output),
        report_path=str(report),
        recovery_report_path=None,
    )

