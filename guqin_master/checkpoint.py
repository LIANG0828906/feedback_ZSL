from __future__ import annotations

import hashlib
import json
import os
import signal
import time
from dataclasses import dataclass
from pathlib import Path
from types import FrameType
from typing import Any, Callable

import numpy as np

from .errors import BusinessError
from .manifest import ProjectSpec
from .mastering import StereoMixer
from .reporting import write_json_atomic


SNAPSHOT_SCHEMA_VERSION = 1


def _sha256_bytes(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def canonical_hash(value: Any) -> str:
    payload = json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")
    return _sha256_bytes(payload)


def build_parameters(
    decoupling_parameters: tuple[dict[str, Any], ...],
    alignment_parameters: tuple[dict[str, Any], ...],
) -> dict[str, Any]:
    return {
        "decoupling": [dict(item) for item in decoupling_parameters],
        "alignment": [dict(item) for item in alignment_parameters],
    }


def parameters_sha256(parameters: dict[str, Any]) -> str:
    return canonical_hash(parameters)


@dataclass(frozen=True)
class ResumeValidation:
    snapshot: dict[str, Any] | None
    resumed: bool


@dataclass
class RenderResult:
    status: str
    completed_blocks: int
    total_blocks: int
    committed_samples: int
    recovery_report: Path | None
    partial_path: Path
    snapshot_path: Path


def _safe_unlink(path: Path) -> None:
    try:
        path.unlink()
    except FileNotFoundError:
        pass


def discard_progress(snapshot_path: Path, partial_path: Path, recovery_path: Path) -> None:
    _safe_unlink(snapshot_path)
    _safe_unlink(partial_path)
    _safe_unlink(recovery_path)


def _mismatch(
    reason: str,
    snapshot_path: Path,
    partial_path: Path,
    recovery_path: Path,
    **details: Any,
) -> BusinessError:
    discard_progress(snapshot_path, partial_path, recovery_path)
    return BusinessError(
        "E_RESUME_MISMATCH",
        reason,
        "project",
        {"progress_discarded": True, **details},
    )


def validate_existing_snapshot(
    project: ProjectSpec,
    input_metadata: dict[str, dict[str, Any]],
    current_parameters_sha256: str,
    snapshot_path: Path,
    partial_path: Path,
    recovery_path: Path,
) -> ResumeValidation:
    if not snapshot_path.exists():
        if partial_path.exists():
            _safe_unlink(partial_path)
        if recovery_path.exists():
            _safe_unlink(recovery_path)
        return ResumeValidation(None, False)
    try:
        snapshot = json.loads(snapshot_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise _mismatch(
            "stored snapshot is unreadable",
            snapshot_path,
            partial_path,
            recovery_path,
            error=str(exc),
        ) from exc
    if not isinstance(snapshot, dict):
        raise _mismatch(
            "stored snapshot must be an object",
            snapshot_path,
            partial_path,
            recovery_path,
        )
    if snapshot.get("schema_version") != SNAPSHOT_SCHEMA_VERSION:
        raise _mismatch(
            "stored snapshot schema is unsupported",
            snapshot_path,
            partial_path,
            recovery_path,
        )
    if snapshot.get("manifest_sha256") != project.manifest_sha256:
        raise _mismatch(
            "manifest digest changed since the last committed block",
            snapshot_path,
            partial_path,
            recovery_path,
        )
    if snapshot.get("parameters_sha256") != current_parameters_sha256:
        raise _mismatch(
            "determined decoupling or alignment parameters changed",
            snapshot_path,
            partial_path,
            recovery_path,
        )
    if snapshot.get("input_files") != input_metadata:
        raise _mismatch(
            "channel audio input changed since the last committed block",
            snapshot_path,
            partial_path,
            recovery_path,
        )
    if (
        snapshot.get("block_size") != project.block_size
        or snapshot.get("total_samples") != project.total_samples
        or snapshot.get("total_blocks")
        != (project.total_samples + project.block_size - 1) // project.block_size
    ):
        raise _mismatch(
            "render geometry changed since the last committed block",
            snapshot_path,
            partial_path,
            recovery_path,
        )
    completed = snapshot.get("completed_blocks")
    if (
        isinstance(completed, bool)
        or not isinstance(completed, int)
        or completed < 0
        or completed > snapshot["total_blocks"]
    ):
        raise _mismatch(
            "snapshot completed_blocks is invalid",
            snapshot_path,
            partial_path,
            recovery_path,
        )
    if completed and not partial_path.exists():
        raise _mismatch(
            "committed partial master is missing",
            snapshot_path,
            partial_path,
            recovery_path,
        )
    return ResumeValidation(snapshot, True)


def _block_hashes(chunk: np.ndarray) -> dict[str, str]:
    left = np.asarray(chunk[:, 0], dtype="<f8")
    right = np.asarray(chunk[:, 1], dtype="<f8")
    return {
        "L": _sha256_bytes(left.tobytes()),
        "R": _sha256_bytes(right.tobytes()),
    }


def verify_partial_blocks(
    snapshot: dict[str, Any],
    partial_path: Path,
    block_size: int,
    total_samples: int,
) -> None:
    completed = int(snapshot["completed_blocks"])
    committed_samples = min(total_samples, completed * block_size)
    expected_bytes = committed_samples * 2 * 8
    if completed == 0:
        if partial_path.exists() and partial_path.stat().st_size != 0:
            with partial_path.open("r+b") as handle:
                handle.truncate(0)
        return
    try:
        actual_bytes = partial_path.stat().st_size
    except FileNotFoundError as exc:
        raise BusinessError(
            "E_RESUME_DIFF",
            "committed partial master disappeared",
            "file",
            {
                "block_index": 0,
                "sample_range": [0, min(committed_samples, block_size)],
                "channel": "L/R",
            },
        ) from exc
    if actual_bytes < expected_bytes:
        raise BusinessError(
            "E_RESUME_DIFF",
            "partial master is shorter than the committed snapshot",
            "file",
            {
                "block_index": completed - 1,
                "sample_range": [
                    max(0, min(total_samples, (completed - 1) * block_size)),
                    committed_samples,
                ],
                "channel": "L/R",
                "expected_bytes": expected_bytes,
                "actual_bytes": actual_bytes,
            },
        )
    if actual_bytes != expected_bytes:
        with partial_path.open("r+b") as handle:
            handle.truncate(expected_bytes)

    hashes = snapshot.get("block_hashes", {})
    left_hashes = hashes.get("L", []) if isinstance(hashes, dict) else []
    right_hashes = hashes.get("R", []) if isinstance(hashes, dict) else []
    if len(left_hashes) < completed or len(right_hashes) < completed:
        raise BusinessError(
            "E_RESUME_DIFF",
            "snapshot is missing committed block hashes",
            "L/R",
            {
                "block_index": min(len(left_hashes), len(right_hashes)),
                "sample_range": [
                    min(total_samples, min(len(left_hashes), len(right_hashes)) * block_size),
                    min(
                        total_samples,
                        (min(len(left_hashes), len(right_hashes)) + 1) * block_size,
                    ),
                ],
                "channel": "L/R",
            },
        )
    with partial_path.open("rb") as handle:
        for block_index in range(completed):
            start = block_index * block_size
            count = min(block_size, total_samples - start)
            payload = handle.read(count * 16)
            data = np.frombuffer(payload, dtype="<f8").reshape(-1, 2)
            actual = _block_hashes(data)
            if actual["L"] != left_hashes[block_index]:
                raise BusinessError(
                    "E_RESUME_DIFF",
                    "committed partial master differs in the left channel",
                    "L",
                    {
                        "block_index": block_index,
                        "sample_range": [start, start + count],
                        "channel": "L",
                        "expected_sha256": left_hashes[block_index],
                        "actual_sha256": actual["L"],
                    },
                )
            if actual["R"] != right_hashes[block_index]:
                raise BusinessError(
                    "E_RESUME_DIFF",
                    "committed partial master differs in the right channel",
                    "R",
                    {
                        "block_index": block_index,
                        "sample_range": [start, start + count],
                        "channel": "R",
                        "expected_sha256": right_hashes[block_index],
                        "actual_sha256": actual["R"],
                    },
                )


def _initial_snapshot(
    project: ProjectSpec,
    input_metadata: dict[str, dict[str, Any]],
    parameters: dict[str, Any],
    total_blocks: int,
) -> dict[str, Any]:
    return {
        "schema_version": SNAPSHOT_SCHEMA_VERSION,
        "project_id": project.project_id,
        "manifest_sha256": project.manifest_sha256,
        "parameters_sha256": parameters_sha256(parameters),
        "parameters": parameters,
        "input_files": input_metadata,
        "block_size": project.block_size,
        "total_samples": project.total_samples,
        "total_blocks": total_blocks,
        "completed_blocks": 0,
        "committed_samples": 0,
        "block_hashes": {"L": [], "R": []},
        "partial_format": "interleaved_float64_le_LR",
    }


def _recovery_report(snapshot: dict[str, Any]) -> dict[str, Any]:
    return {
        "business_code": "OK",
        "status": "INTERRUPTED",
        "project_id": snapshot["project_id"],
        "manifest_sha256": snapshot["manifest_sha256"],
        "parameters_sha256": snapshot["parameters_sha256"],
        "block_size": snapshot["block_size"],
        "total_blocks": snapshot["total_blocks"],
        "completed_blocks": snapshot["completed_blocks"],
        "committed_samples": snapshot["committed_samples"],
        "next_sample": snapshot["committed_samples"],
        "resume_mode": "rerun_same_project_command",
    }


class _InterruptState:
    def __init__(self) -> None:
        self.requested = False
        self.handler_installed = False
        self.previous: Any = None

    def install(self) -> None:
        try:
            self.previous = signal.getsignal(signal.SIGINT)
            signal.signal(signal.SIGINT, self.handle)
            self.handler_installed = True
        except (ValueError, OSError):
            self.handler_installed = False

    def handle(self, signum: int, frame: FrameType | None) -> None:
        del signum, frame
        self.requested = True

    def restore(self) -> None:
        if self.handler_installed:
            signal.signal(signal.SIGINT, self.previous)
            self.handler_installed = False


def render_with_checkpoint(
    project: ProjectSpec,
    mixer: StereoMixer,
    snapshot: dict[str, Any],
    state_directory: Path,
    recovery_path: Path,
    progress_reporter: Callable[[dict[str, Any]], None] | None = None,
) -> RenderResult:
    state_directory.mkdir(parents=True, exist_ok=True)
    snapshot_path = state_directory / "progress.json"
    partial_path = state_directory / "master.partial.f64"
    total = project.total_samples
    block_size = project.block_size
    total_blocks = (total + block_size - 1) // block_size

    start_block = int(snapshot["completed_blocks"])
    mode = "ab" if start_block else "wb"
    partial_path.parent.mkdir(parents=True, exist_ok=True)
    interrupt = _InterruptState()
    interrupt.install()
    interrupt_after = os.environ.get("GUQIN_TEST_INTERRUPT_AFTER_BLOCKS")
    block_sleep_ms = float(os.environ.get("GUQIN_TEST_BLOCK_SLEEP_MS", "0") or 0)

    try:
        with partial_path.open(mode + "+") as handle:
            handle.seek(start_block * block_size * 16)
            for block_index in range(start_block, total_blocks):
                start = block_index * block_size
                stop = min(total, start + block_size)
                chunk, _ = mixer.render_block(start, stop)
                handle.write(np.asarray(chunk, dtype="<f8").tobytes(order="C"))
                handle.flush()
                os.fsync(handle.fileno())

                digest = _block_hashes(chunk)
                snapshot["block_hashes"]["L"].append(digest["L"])
                snapshot["block_hashes"]["R"].append(digest["R"])
                snapshot["completed_blocks"] = block_index + 1
                snapshot["committed_samples"] = stop
                write_json_atomic(snapshot_path, snapshot)
                if progress_reporter is not None:
                    progress_reporter(
                        {
                            "status": "PROGRESS",
                            "completed_blocks": snapshot["completed_blocks"],
                            "total_blocks": total_blocks,
                            "committed_samples": stop,
                        }
                    )
                if block_sleep_ms > 0:
                    time.sleep(block_sleep_ms / 1000.0)
                requested = interrupt.requested or (
                    interrupt_after is not None
                    and block_index + 1 >= int(interrupt_after)
                )
                if requested and block_index + 1 < total_blocks:
                    write_json_atomic(recovery_path, _recovery_report(snapshot))
                    return RenderResult(
                        status="interrupted",
                        completed_blocks=block_index + 1,
                        total_blocks=total_blocks,
                        committed_samples=stop,
                        recovery_report=recovery_path,
                        partial_path=partial_path,
                        snapshot_path=snapshot_path,
                    )
                if requested and block_index + 1 == total_blocks:
                    write_json_atomic(recovery_path, _recovery_report(snapshot))
                    return RenderResult(
                        status="interrupted",
                        completed_blocks=block_index + 1,
                        total_blocks=total_blocks,
                        committed_samples=stop,
                        recovery_report=recovery_path,
                        partial_path=partial_path,
                        snapshot_path=snapshot_path,
                    )
    finally:
        interrupt.restore()

    return RenderResult(
        status="complete",
        completed_blocks=total_blocks,
        total_blocks=total_blocks,
        committed_samples=total,
        recovery_report=None,
        partial_path=partial_path,
        snapshot_path=snapshot_path,
    )


def initialize_snapshot(
    project: ProjectSpec,
    input_metadata: dict[str, dict[str, Any]],
    parameters: dict[str, Any],
    state_directory: Path,
) -> dict[str, Any]:
    total_blocks = (project.total_samples + project.block_size - 1) // project.block_size
    snapshot = _initial_snapshot(
        project, input_metadata, parameters, total_blocks
    )
    state_directory.mkdir(parents=True, exist_ok=True)
    write_json_atomic(state_directory / "progress.json", snapshot)
    partial_path = state_directory / "master.partial.f64"
    with partial_path.open("wb"):
        pass
    return snapshot


def read_partial_master(
    project: ProjectSpec,
    partial_path: Path,
) -> np.ndarray:
    expected_values = project.total_samples * 2
    try:
        data = np.fromfile(partial_path, dtype="<f8", count=expected_values)
    except OSError as exc:
        raise BusinessError(
            "E_RESUME_DIFF",
            f"cannot read committed partial master: {exc}",
            "file",
            {"block_index": 0, "sample_range": [0, 0], "channel": "L/R"},
        ) from exc
    if data.size != expected_values:
        completed = data.size // (2 * project.block_size)
        raise BusinessError(
            "E_RESUME_DIFF",
            "final partial master has an unexpected sample count",
            "file",
            {
                "block_index": completed,
                "sample_range": [
                    min(project.total_samples, completed * project.block_size),
                    project.total_samples,
                ],
                "channel": "L/R",
                "expected_values": expected_values,
                "actual_values": int(data.size),
            },
        )
    return data.reshape(-1, 2)
