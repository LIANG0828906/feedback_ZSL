"""End-to-end synthesis, deterministic chunk export, and resumable snapshots."""

from __future__ import annotations

import hashlib
import json
import math
import os
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .crosstalk import CrosstalkResult, estimate_and_cancel_crosstalk
from .generate import stable_seed
from .loudness import MasterBusResult, master_binaural_bus
from .project import Project, load_project, resolve_channel_path
from .spatial import SpatialResult, render_binaural
from .wavio import (
    create_streaming_wav,
    finalize_streaming_wav,
    float_to_pcm_bytes,
    read_wav,
)


_SNAPSHOT_SCHEMA_VERSION = 1
_REPORT_SCHEMA_VERSION = 1


@dataclass
class InterruptFlag:
    requested: bool = False


@dataclass
class SynthesisResult:
    status: str
    project: Project
    report: dict[str, Any]
    master_path: str | None
    report_path: str | None
    recovery_path: str | None
    resume_code: str | None = None


@dataclass
class _ResumeSelection:
    snapshot: dict[str, Any] | None
    event_code: str | None
    event_message: str
    corrupt_fallback: bool = False


def synthesize(
    project_path: str | Path,
    audio_dir: str | Path,
    output_dir: str | Path,
    *,
    resume: bool = True,
    fresh: bool = False,
    interrupt_flag: InterruptFlag | None = None,
    interrupt_after_blocks: int | None = None,
) -> SynthesisResult:
    """Validate, process, and export a binaural master.

    All input validation and business acceptance checks happen before any bus
    output path is created. Export is performed in fixed-size PCM blocks, each
    guarded by an atomically committed progress snapshot.
    """

    project = load_project(project_path, audio_dir, validate_files=True)
    input_digest = project.input_digest(audio_dir)
    actor_mics, speaker_signals = _load_inputs(project, Path(audio_dir))

    crosstalk = estimate_and_cancel_crosstalk(project, actor_mics, speaker_signals)
    spatial = render_binaural(project, {key: value.clean for key, value in crosstalk.actors.items()})
    contributions = _contributions(project, spatial)
    mixed_left, mixed_right = _mix(project, spatial)
    master = master_binaural_bus(mixed_left, mixed_right, project.export)

    output_dir = Path(output_dir)
    progress_dir = output_dir / ".progress"
    snapshot_dir = progress_dir / "snapshots"
    master_path = output_dir / "binaural_master.wav"
    report_path = output_dir / "production_report.json"
    recovery_path = output_dir / "recovery_report.json"

    if fresh:
        _discard_outputs(output_dir)
        resume = False

    selection = (
        _select_resume_snapshot(
            snapshot_dir,
            master_path,
            project,
            input_digest,
            crosstalk,
            spatial,
            master,
        )
        if resume
        else _ResumeSelection(None, None, "resume_disabled")
    )
    resume_code = selection.event_code
    if selection.event_code == "E_RESUME_MISMATCH":
        _discard_outputs(output_dir)
        selection = _ResumeSelection(None, "E_RESUME_MISMATCH", selection.event_message)

    output_dir.mkdir(parents=True, exist_ok=True)
    snapshot_dir.mkdir(parents=True, exist_ok=True)
    if recovery_path.exists():
        recovery_path.unlink()

    report = _build_report(
        project,
        input_digest,
        crosstalk,
        spatial,
        master,
        contributions,
        resume_code=resume_code,
        resume_message=selection.event_message,
        corrupt_fallback=selection.corrupt_fallback,
    )
    if selection.snapshot is None:
        _reset_export(project, master_path, snapshot_dir)
        completed_blocks = 0
        committed_bytes = 0
        prefix_hash = hashlib.sha256()
    else:
        completed_blocks = int(selection.snapshot["completed_blocks"])
        committed_bytes = int(selection.snapshot["output"]["committed_bytes"])
        with master_path.open("r+b") as fh:
            fh.truncate(44 + committed_bytes)
            fh.flush()
            os.fsync(fh.fileno())
        with master_path.open("rb") as fh:
            fh.seek(44)
            prefix_hash = hashlib.sha256(fh.read(committed_bytes))

    total_frames = project.frames
    block_frames = project.export.block_frames
    total_blocks = int(math.ceil(total_frames / block_frames))
    width = project.export.bit_depth // 8
    channels = project.export.channels
    frame_bytes = channels * width

    with master_path.open("r+b") as fh:
        fh.seek(44 + committed_bytes)
        for block_index in range(completed_blocks, total_blocks):
            start = block_index * block_frames
            end = min(total_frames, start + block_frames)
            block = np.stack(
                [master.left[start:end], master.right[start:end]], axis=0
            )
            payload = float_to_pcm_bytes(block, project.export.bit_depth)
            fh.write(payload)
            fh.flush()
            os.fsync(fh.fileno())
            committed_bytes += len(payload)
            prefix_hash.update(payload)
            completed_blocks = block_index + 1

            snapshot = _make_snapshot(
                project,
                input_digest,
                crosstalk,
                spatial,
                completed_blocks,
                end,
                committed_bytes,
                prefix_hash.hexdigest(),
                total_blocks,
            )
            _write_json_atomic(
                snapshot_dir / f"snapshot_{completed_blocks:06d}.json",
                snapshot,
            )

            interrupt_requested = bool(interrupt_flag and interrupt_flag.requested)
            if (
                interrupt_after_blocks is not None
                and completed_blocks >= interrupt_after_blocks
            ):
                interrupt_requested = True
            if interrupt_requested and completed_blocks < total_blocks:
                recovery = _recovery_report(
                    project,
                    input_digest,
                    crosstalk,
                    spatial,
                    completed_blocks,
                    total_blocks,
                    end,
                )
                _write_json_atomic(recovery_path, recovery)
                return SynthesisResult(
                    status="interrupted",
                    project=project,
                    report=report,
                    master_path=str(master_path),
                    report_path=None,
                    recovery_path=str(recovery_path),
                    resume_code=resume_code,
                )

    finalize_streaming_wav(
        master_path,
        project.export.sample_rate,
        project.export.bit_depth,
        project.export.channels,
        total_frames,
    )
    _write_json_atomic(report_path, report)
    return SynthesisResult(
        status="complete",
        project=project,
        report=report,
        master_path=str(master_path),
        report_path=str(report_path),
        recovery_path=None,
        resume_code=resume_code,
    )


def _load_inputs(
    project: Project, audio_dir: Path
) -> tuple[dict[str, np.ndarray], dict[str, np.ndarray]]:
    actors: dict[str, np.ndarray] = {}
    speakers: dict[str, np.ndarray] = {}
    for actor in project.actors:
        samples, _ = read_wav(
            resolve_channel_path(audio_dir, actor.channel), actor.channel
        )
        actors[actor.id] = samples[0, : project.frames].copy()
    for speaker in project.speakers:
        samples, _ = read_wav(
            resolve_channel_path(audio_dir, speaker.channel), speaker.channel
        )
        speakers[speaker.id] = samples[0, : project.frames].copy()
    return actors, speakers


def _mix(
    project: Project, spatial: SpatialResult
) -> tuple[np.ndarray, np.ndarray]:
    left = np.zeros(project.frames, dtype=np.float64)
    right = np.zeros(project.frames, dtype=np.float64)
    for actor in project.actors:
        left += spatial.actors[actor.id].left
        right += spatial.actors[actor.id].right
    return left, right


def _contributions(project: Project, spatial: SpatialResult) -> list[dict[str, Any]]:
    entries = []
    total_energy = 0.0
    for actor in project.actors:
        rendered = spatial.actors[actor.id]
        energy = float(np.dot(rendered.left, rendered.left) + np.dot(rendered.right, rendered.right))
        entries.append({"actor_id": actor.id, "energy": energy})
        total_energy += energy
    return [
        {
            "actor_id": entry["actor_id"],
            "energy": entry["energy"],
            "ratio": entry["energy"] / total_energy if total_energy > 0 else 0.0,
            "percent": (
                100.0 * entry["energy"] / total_energy if total_energy > 0 else 0.0
            ),
        }
        for entry in entries
    ]


def _build_report(
    project: Project,
    input_digest: str,
    crosstalk: CrosstalkResult,
    spatial: SpatialResult,
    master: MasterBusResult,
    contributions: list[dict[str, Any]],
    *,
    resume_code: str | None,
    resume_message: str,
    corrupt_fallback: bool,
) -> dict[str, Any]:
    total_blocks = int(math.ceil(project.frames / project.export.block_frames))
    report: dict[str, Any] = {
        "schema_version": _REPORT_SCHEMA_VERSION,
        "status": "complete",
        "project_id": project.project_id,
        "source": {
            "project_digest": project.content_digest(),
            "input_digest": input_digest,
            "channels_verified": len(project.actors) + len(project.speakers),
        },
        "format": {
            "container": "wav",
            "sample_rate": project.export.sample_rate,
            "bit_depth": project.export.bit_depth,
            "channels": project.export.channels,
            "frames": project.frames,
            "duration_seconds": project.duration_seconds,
        },
        "crosstalk": crosstalk.summary_dict(),
        "spatial": spatial.summary,
        "bus": {
            **master.summary_dict(),
            "downmix": "discrete_L_R_sum_v1",
            "contributions": contributions,
        },
        "delivery": {
            "master_file": "binaural_master.wav",
            "block_frames": project.export.block_frames,
            "blocks": total_blocks,
            "snapshot_schema_version": _SNAPSHOT_SCHEMA_VERSION,
        },
        "resume": {
            "status": (
                "mismatch_restarted"
                if resume_code == "E_RESUME_MISMATCH"
                else "corrupt_snapshot_fallback"
                if corrupt_fallback
                else "not_used"
            ),
            "code": resume_code,
            "message": resume_message,
        },
    }
    return report


def _recovery_report(
    project: Project,
    input_digest: str,
    crosstalk: CrosstalkResult,
    spatial: SpatialResult,
    completed_blocks: int,
    total_blocks: int,
    completed_frames: int,
) -> dict[str, Any]:
    return {
        "schema_version": _REPORT_SCHEMA_VERSION,
        "status": "interrupted",
        "exit_code": 0,
        "project_id": project.project_id,
        "project_digest": project.content_digest(),
        "input_digest": input_digest,
        "completed_blocks": completed_blocks,
        "total_blocks": total_blocks,
        "completed_frames": completed_frames,
        "next_block": completed_blocks,
        "block_frames": project.export.block_frames,
        "crosstalk_fingerprint": crosstalk.fingerprint(),
        "spatial_fingerprint": spatial.fingerprint(),
        "resume_available": True,
        "snapshot_schema_version": _SNAPSHOT_SCHEMA_VERSION,
    }


def _make_snapshot(
    project: Project,
    input_digest: str,
    crosstalk: CrosstalkResult,
    spatial: SpatialResult,
    completed_blocks: int,
    completed_frames: int,
    committed_bytes: int,
    prefix_sha256: str,
    total_blocks: int,
) -> dict[str, Any]:
    payload = {
        "schema_version": _SNAPSHOT_SCHEMA_VERSION,
        "project_id": project.project_id,
        "project_digest": project.content_digest(),
        "input_digest": input_digest,
        "completed_blocks": completed_blocks,
        "total_blocks": total_blocks,
        "completed_frames": completed_frames,
        "block_frames": project.export.block_frames,
        "crosstalk_estimates": crosstalk.summary_dict(),
        "crosstalk_fingerprint": crosstalk.fingerprint(),
        "spatial_parameters": spatial.summary,
        "spatial_fingerprint": spatial.fingerprint(),
        "output": {
            "file": "binaural_master.wav",
            "data_offset": 44,
            "committed_bytes": committed_bytes,
            "prefix_sha256": prefix_sha256,
        },
    }
    payload["checksum"] = _payload_checksum(payload)
    return payload


def _select_resume_snapshot(
    snapshot_dir: Path,
    master_path: Path,
    project: Project,
    input_digest: str,
    crosstalk: CrosstalkResult,
    spatial: SpatialResult,
    master: MasterBusResult,
) -> _ResumeSelection:
    if not snapshot_dir.is_dir():
        return _ResumeSelection(None, None, "no_progress_snapshot")
    candidates = sorted(snapshot_dir.glob("snapshot_*.json"), reverse=True)
    if not candidates:
        return _ResumeSelection(None, None, "no_progress_snapshot")

    corrupt_found = False
    valid_snapshots: list[dict[str, Any]] = []
    for path in candidates:
        snapshot = _read_snapshot(path)
        if snapshot is None:
            corrupt_found = True
            continue
        valid_snapshots.append(snapshot)
        complete = int(snapshot.get("completed_blocks", -1))
        frames = int(snapshot.get("completed_frames", -1))
        if (
            snapshot.get("input_digest") != input_digest
            or snapshot.get("project_digest") != project.content_digest()
            or snapshot.get("crosstalk_fingerprint") != crosstalk.fingerprint()
            or snapshot.get("spatial_fingerprint") != spatial.fingerprint()
        ):
            return _ResumeSelection(
                None,
                "E_RESUME_MISMATCH",
                "Persisted project digest or processing parameters do not match current input; old progress was discarded.",
            )
        if complete < 0 or frames < 0 or frames > project.frames:
            corrupt_found = True
            continue
        if not _snapshot_output_matches(snapshot, master_path):
            corrupt_found = True
            continue
        return _ResumeSelection(
            snapshot,
            "E_SNAPSHOT_CORRUPT_FALLBACK" if corrupt_found else None,
            (
                "A newer snapshot was incomplete or corrupt; resumed from the most recent valid commit."
                if corrupt_found
                else "Resumed from the most recent committed snapshot."
            ),
            corrupt_fallback=corrupt_found,
        )

    if corrupt_found:
        return _ResumeSelection(
            None,
            "E_SNAPSHOT_CORRUPT_FALLBACK",
            "No valid committed snapshot remained; progress was discarded and export restarted from zero.",
            corrupt_fallback=True,
        )
    return _ResumeSelection(None, None, "no_progress_snapshot")


def _snapshot_output_matches(
    snapshot: dict[str, Any], master_path: Path
) -> bool:
    if not master_path.is_file():
        return False
    output = snapshot.get("output", {})
    offset = int(output.get("data_offset", -1))
    committed = int(output.get("committed_bytes", -1))
    expected_hash = str(output.get("prefix_sha256", ""))
    if offset != 44 or committed < 0 or len(expected_hash) != 64:
        return False
    if master_path.stat().st_size < offset + committed:
        return False
    digest = hashlib.sha256()
    with master_path.open("rb") as fh:
        fh.seek(offset)
        remaining = committed
        while remaining:
            chunk = fh.read(min(1024 * 1024, remaining))
            if not chunk:
                return False
            digest.update(chunk)
            remaining -= len(chunk)
    return digest.hexdigest() == expected_hash


def _read_snapshot(path: Path) -> dict[str, Any] | None:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(payload, dict):
            return None
        checksum = payload.get("checksum")
        if not isinstance(checksum, str):
            return None
        unsigned = {key: value for key, value in payload.items() if key != "checksum"}
        if _payload_checksum(unsigned) != checksum:
            return None
        if payload.get("schema_version") != _SNAPSHOT_SCHEMA_VERSION:
            return None
        return payload
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError, TypeError):
        return None


def _payload_checksum(payload: dict[str, Any]) -> str:
    encoded = json.dumps(
        payload, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    )
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


def _write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    encoded = json.dumps(payload, ensure_ascii=True, indent=2) + "\n"
    with temporary.open("w", encoding="utf-8", newline="\n") as fh:
        fh.write(encoded)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(temporary, path)
    try:
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        pass


def _reset_export(project: Project, master_path: Path, snapshot_dir: Path) -> None:
    if master_path.exists():
        master_path.unlink()
    if snapshot_dir.exists():
        shutil.rmtree(snapshot_dir)
    snapshot_dir.mkdir(parents=True, exist_ok=True)
    create_streaming_wav(
        master_path,
        project.export.sample_rate,
        project.export.bit_depth,
        project.export.channels,
        reserved_frames=project.frames,
    )


def _discard_outputs(output_dir: Path) -> None:
    if not output_dir.exists():
        return
    for name in ("binaural_master.wav", "production_report.json", "recovery_report.json"):
        path = output_dir / name
        if path.exists():
            path.unlink()
    progress = output_dir / ".progress"
    if progress.exists():
        shutil.rmtree(progress)

