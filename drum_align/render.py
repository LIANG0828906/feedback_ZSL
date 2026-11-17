"""Block rendering, atomic checkpoints, and interrupt-safe resume."""

from __future__ import annotations

import hashlib
import os
import signal
import threading
import wave
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .audio import AudioTrack
from .drift import DriftCurve
from .errors import BusinessError
from .jsonio import atomic_write_json, read_json, sha256_file
from .project import LoadedProject
from .resample import sample_at_reference_positions


STATE_SCHEMA_VERSION = 1


@dataclass
class RenderOutcome:
    status: str
    completed_blocks: int
    total_blocks: int
    resume_count: int
    recovery_events: list[dict[str, Any]]
    master_sha256: str | None


class GracefulSignal:
    """Convert the first Ctrl+C into a cooperative render-loop stop request."""

    def __init__(self) -> None:
        self.requested = False
        self._old_handler: Any = None
        self._installed = False

    def install(self) -> None:
        if threading.current_thread() is not threading.main_thread():
            return
        try:
            self._old_handler = signal.getsignal(signal.SIGINT)
            signal.signal(signal.SIGINT, self._handle)
            self._installed = True
        except (ValueError, OSError):
            self._installed = False

    def restore(self) -> None:
        if not self._installed:
            return
        try:
            signal.signal(signal.SIGINT, self._old_handler)
        except (ValueError, OSError):
            pass
        self._installed = False

    def _handle(self, _signum: int, _frame: Any) -> None:
        self.requested = True


def curve_payload(curve: DriftCurve) -> dict[str, list[int] | list[float]]:
    return {
        "ticks_samples": [int(value) for value in curve.ticks],
        "offsets_samples": [int(value) for value in curve.offsets],
    }


def _state_curves_match(
    state_curves: Any, curves: dict[str, DriftCurve], base_camera: str
) -> bool:
    if not isinstance(state_curves, dict):
        return False
    if set(state_curves) != set(curves) - {base_camera}:
        return False
    for camera, curve in curves.items():
        if camera == base_camera:
            continue
        expected = curve_payload(curve)
        actual = state_curves.get(camera)
        if not isinstance(actual, dict):
            return False
        if actual.get("ticks_samples") != expected["ticks_samples"]:
            return False
        if actual.get("offsets_samples") != expected["offsets_samples"]:
            return False
    return True


def _total_blocks(total_samples: int, block_samples: int) -> int:
    return (total_samples + block_samples - 1) // block_samples


def _write_wav_from_pcm(
    *,
    pcm_path: Path,
    master_path: Path,
    sample_rate: int,
    channels: int,
    total_samples: int,
) -> None:
    master_path.parent.mkdir(parents=True, exist_ok=True)
    expected_bytes = total_samples * channels * 2
    try:
        actual_bytes = pcm_path.stat().st_size
    except OSError as exc:
        raise BusinessError(
            "E_RENDER_INCOMPLETE", "partial PCM checkpoint is missing"
        ) from exc
    if actual_bytes != expected_bytes:
        raise BusinessError(
            "E_RENDER_INCOMPLETE",
            f"partial PCM has {actual_bytes} bytes; expected {expected_bytes}",
        )
    temporary = master_path.with_name(f".{master_path.name}.writing.wav")
    try:
        with wave.open(str(temporary), "wb") as wav:
            wav.setnchannels(channels)
            wav.setsampwidth(2)
            wav.setframerate(sample_rate)
            wav.setnframes(total_samples)
            with pcm_path.open("rb") as pcm:
                while True:
                    chunk = pcm.read(1024 * 1024)
                    if not chunk:
                        break
                    wav.writeframesraw(chunk)
        with temporary.open("r+b") as handle:
            os.fsync(handle.fileno())
        os.replace(temporary, master_path)
        try:
            directory_fd = os.open(master_path.parent, os.O_RDONLY)
        except OSError:
            directory_fd = -1
        if directory_fd >= 0:
            try:
                os.fsync(directory_fd)
            except OSError:
                pass
            finally:
                os.close(directory_fd)
    except BaseException:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise


def _convert_mix(mix: np.ndarray) -> np.ndarray:
    rounded = np.rint(np.clip(mix, -32768.0, 32767.0))
    return rounded.astype("<i2")


def _render_block(
    *,
    start: int,
    end: int,
    reference: AudioTrack,
    reference_gain: float,
    aux_tracks: dict[str, AudioTrack],
    curves: dict[str, DriftCurve],
    effective_gains: dict[str, float],
) -> np.ndarray:
    out_channels = reference.channels
    positions = np.arange(start, end, dtype=np.float64)
    base = reference.samples[start:end].astype(np.float64)
    mix = base * float(reference_gain)
    for camera in sorted(aux_tracks):
        track = aux_tracks[camera]
        sampled = sample_at_reference_positions(
            track.samples, positions, curves[camera], out_channels=out_channels
        )
        mix += sampled * float(effective_gains[camera])
    return _convert_mix(mix)


def render_master(
    *,
    project: LoadedProject,
    project_digest: str,
    reference: AudioTrack,
    aux_tracks: dict[str, AudioTrack],
    curves: dict[str, DriftCurve],
    effective_gains: dict[str, float],
    reference_gain: float,
    oneshot: bool = False,
    interrupt_after_blocks: int | None = None,
) -> RenderOutcome:
    """Render by fixed blocks, checkpointing after every completed block."""
    total_samples = reference.frame_count
    block_samples = project.block_samples
    total_blocks = _total_blocks(total_samples, block_samples)
    project.master_path.parent.mkdir(parents=True, exist_ok=True)
    project.snapshot_path.parent.mkdir(parents=True, exist_ok=True)
    partial_path = project.master_path.with_name(
        f".{project.master_path.name}.partial.pcm"
    )

    recovery_events: list[dict[str, Any]] = []
    resume_count = 0
    completed_blocks = 0
    completed_samples = 0
    running_hash = hashlib.sha256()

    state: dict[str, Any] | None = None
    if not oneshot and project.snapshot_path.exists():
        try:
            state = read_json(project.snapshot_path)
        except (OSError, ValueError):
            state = None
            recovery_events.append(
                {
                    "code": "E_RESUME_DAMAGED",
                    "severity": "recovery",
                    "reason": "snapshot JSON is unreadable; discarded old progress",
                }
            )
        if state is not None and not isinstance(state, dict):
            state = None
            recovery_events.append(
                {
                    "code": "E_RESUME_DAMAGED",
                    "severity": "recovery",
                    "reason": "snapshot root is not an object; discarded old progress",
                }
            )
        if state is not None and state.get("project_digest") != project_digest:
            recovery_events.append(
                {
                    "code": "E_RESUME_MISMATCH",
                    "severity": "recovery",
                    "reason": "project digest differs; discarded old progress",
                }
            )
            state = None
        if state is not None and not _state_curves_match(
            state.get("drift_curves"), curves, project.base_camera
        ):
            recovery_events.append(
                {
                    "code": "E_RESUME_MISMATCH",
                    "severity": "recovery",
                    "reason": "recomputed drift curve differs from snapshot",
                }
            )
            state = None
        if state is not None:
            valid = (
                isinstance(state, dict)
                and state.get("state_schema_version") == STATE_SCHEMA_VERSION
                and state.get("block_samples") == block_samples
                and state.get("total_samples") == total_samples
                and state.get("total_blocks") == total_blocks
                and state.get("channels") == reference.channels
                and state.get("sample_rate") == reference.sample_rate
                and isinstance(state.get("completed_blocks"), int)
                and not isinstance(state.get("completed_blocks"), bool)
                and 0 <= state["completed_blocks"] <= total_blocks
                and isinstance(state.get("resume_count"), int)
                and not isinstance(state.get("resume_count"), bool)
                and state["resume_count"] >= 0
            )
            if valid:
                completed_blocks = int(state["completed_blocks"])
                completed_samples = min(total_samples, completed_blocks * block_samples)
                expected_bytes = completed_samples * reference.channels * 2
                expected_hash = state.get("pcm_sha256")
                if (
                    not isinstance(expected_hash, str)
                    or state.get("pcm_bytes") != expected_bytes
                    or not partial_path.exists()
                    or partial_path.stat().st_size < expected_bytes
                ):
                    valid = False
                else:
                    digest = hashlib.sha256()
                    with partial_path.open("rb") as handle:
                        remaining = expected_bytes
                        while remaining > 0:
                            chunk = handle.read(min(1024 * 1024, remaining))
                            if not chunk:
                                valid = False
                                break
                            digest.update(chunk)
                            remaining -= len(chunk)
                    if digest.hexdigest() != expected_hash:
                        valid = False
            else:
                valid = False
            if not valid:
                recovery_events.append(
                    {
                        "code": "E_RESUME_DAMAGED",
                        "severity": "recovery",
                        "reason": "snapshot/PCM checkpoint is incomplete or corrupt; "
                        "discarded old progress",
                    }
                )
                state = None
                completed_blocks = 0
                completed_samples = 0

    if state is not None:
        resume_count = int(state.get("resume_count", 0)) + 1
        existing_events = state.get("recovery_events", [])
        if isinstance(existing_events, list):
            recovery_events = existing_events + recovery_events
        expected_bytes = completed_samples * reference.channels * 2
        with partial_path.open("r+b") as handle:
            handle.truncate(expected_bytes)
            handle.seek(0, os.SEEK_END)
            digest = hashlib.sha256()
            handle.seek(0)
            remaining = expected_bytes
            while remaining > 0:
                chunk = handle.read(min(1024 * 1024, remaining))
                digest.update(chunk)
                remaining -= len(chunk)
            running_hash = digest
    else:
        if project.snapshot_path.exists():
            try:
                project.snapshot_path.unlink()
            except OSError:
                pass
        partial_path.parent.mkdir(parents=True, exist_ok=True)
        with partial_path.open("wb"):
            pass
        try:
            project.master_path.unlink(missing_ok=True)
        except OSError:
            pass

    graceful = GracefulSignal()
    graceful.install()
    new_blocks_this_run = 0
    if completed_blocks < total_blocks:
        # A stale complete file must never be mistaken for this partial render.
        try:
            project.master_path.unlink(missing_ok=True)
        except OSError:
            pass
    try:
        with partial_path.open("ab", buffering=0) as pcm_handle:
            while completed_blocks < total_blocks:
                if graceful.requested:
                    break
                start = completed_blocks * block_samples
                end = min(total_samples, start + block_samples)
                block = _render_block(
                    start=start,
                    end=end,
                    reference=reference,
                    reference_gain=reference_gain,
                    aux_tracks=aux_tracks,
                    curves=curves,
                    effective_gains=effective_gains,
                )
                payload = np.asarray(block, dtype="<i2").tobytes(order="C")
                pcm_handle.write(payload)
                pcm_handle.flush()
                os.fsync(pcm_handle.fileno())
                running_hash.update(payload)
                completed_blocks += 1
                completed_samples = end
                new_blocks_this_run += 1
                state_payload = {
                    "state_schema_version": STATE_SCHEMA_VERSION,
                    "project_digest": project_digest,
                    "block_samples": block_samples,
                    "total_samples": total_samples,
                    "channels": reference.channels,
                    "sample_rate": reference.sample_rate,
                    "total_blocks": total_blocks,
                    "completed_blocks": completed_blocks,
                    "completed_samples": completed_samples,
                    "resume_count": resume_count,
                    "recovery_events": recovery_events,
                    "drift_curves": {
                        camera: curve_payload(curve)
                        for camera, curve in sorted(curves.items())
                        if camera != project.base_camera
                    },
                    "pcm_bytes": completed_samples * reference.channels * 2,
                    "pcm_sha256": running_hash.hexdigest(),
                    "partial_pcm": partial_path.name,
                }
                atomic_write_json(project.snapshot_path, state_payload)
                if (
                    interrupt_after_blocks is not None
                    and new_blocks_this_run >= interrupt_after_blocks
                ):
                    graceful.requested = True
                if graceful.requested:
                    break
    except KeyboardInterrupt:
        graceful.requested = True
    finally:
        graceful.restore()

    if completed_blocks >= total_blocks:
        _write_wav_from_pcm(
            pcm_path=partial_path,
            master_path=project.master_path,
            sample_rate=reference.sample_rate,
            channels=reference.channels,
            total_samples=total_samples,
        )
        master_hash = sha256_file(project.master_path)
        try:
            partial_path.unlink(missing_ok=True)
        except OSError:
            pass
        try:
            project.snapshot_path.unlink(missing_ok=True)
        except OSError:
            pass
        return RenderOutcome(
            status="complete",
            completed_blocks=completed_blocks,
            total_blocks=total_blocks,
            resume_count=resume_count,
            recovery_events=recovery_events,
            master_sha256=master_hash,
        )

    if not graceful.requested:
        raise BusinessError(
            "E_RENDER_INCOMPLETE",
            "render stopped without an interrupt request while blocks remain",
        )
    return RenderOutcome(
        status="interrupted",
        completed_blocks=completed_blocks,
        total_blocks=total_blocks,
        resume_count=resume_count,
        recovery_events=recovery_events,
        master_sha256=None,
    )
