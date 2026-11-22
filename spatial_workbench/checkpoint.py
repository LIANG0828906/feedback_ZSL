"""Atomic, serialized and integrity-checked frame checkpoints."""

from __future__ import annotations

import json
import os
import tempfile
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator

from .canonical import sha256_json
from .errors import ErrorCode, WorkbenchError
from .models import (
    CHECKPOINT_FORMAT,
    FrameParameters,
    FrameRenderStats,
)


@dataclass(frozen=True)
class PersistedFrame:
    frame_index: int
    params: FrameParameters
    render_stats: FrameRenderStats | None
    input_hash: str
    algorithm_version: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_index": self.frame_index,
            "status": "committed",
            "params": self.params.to_dict(),
            "direct_delays_samples": list(self.params.direct_delays_samples),
            "energy_statistics": self.params.energy_statistics(self.render_stats),
            "render_statistics": (
                self.render_stats.to_dict() if self.render_stats is not None else None
            ),
            "input_hash": self.input_hash,
            "algorithm_version": self.algorithm_version,
            "parameter_fingerprint": self.params.fingerprint(),
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "PersistedFrame":
        try:
            params = FrameParameters.from_dict(value["params"])
            index = int(value["frame_index"])
            input_hash = str(value["input_hash"])
            algorithm_version = str(value["algorithm_version"])
            stats_value = value.get("render_statistics")
            stats = (
                FrameRenderStats.from_dict(stats_value)
                if isinstance(stats_value, dict)
                else None
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise WorkbenchError(
                ErrorCode.E_STATE,
                "checkpoint frame record is malformed",
                stage="checkpoint-load",
            ) from exc
        if index != params.frame_index:
            raise WorkbenchError(
                ErrorCode.E_STATE,
                "checkpoint frame index does not match its parameters",
                stage="checkpoint-load",
                frame_index=index,
            )
        return cls(
            frame_index=index,
            params=params,
            render_stats=stats,
            input_hash=input_hash,
            algorithm_version=algorithm_version,
        )


@dataclass(frozen=True)
class CheckpointLoadResult:
    valid: bool
    found: bool
    invalid_reason: str | None
    generation: int
    records: tuple[PersistedFrame, ...]

    @property
    def frame_count(self) -> int:
        return len(self.records)


class _FileLock:
    def __init__(self, path: Path) -> None:
        self.path = path
        self.handle = None

    def __enter__(self) -> "_FileLock":
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.handle = self.path.open("a+b")
        try:
            import fcntl

            fcntl.flock(self.handle.fileno(), fcntl.LOCK_EX)
        except ImportError:
            import msvcrt

            self.handle.seek(0, os.SEEK_END)
            if self.handle.tell() == 0:
                self.handle.write(b"\0")
                self.handle.flush()
            self.handle.seek(0)
            msvcrt.locking(self.handle.fileno(), msvcrt.LK_LOCK, 1)
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        if self.handle is None:
            return
        try:
            import fcntl

            fcntl.flock(self.handle.fileno(), fcntl.LOCK_UN)
        except ImportError:
            import msvcrt

            self.handle.seek(0)
            msvcrt.locking(self.handle.fileno(), msvcrt.LK_UNLCK, 1)
        finally:
            self.handle.close()
            self.handle = None


def _payload_integrity(payload: dict[str, Any]) -> str:
    return sha256_json(payload)


class CheckpointStore:
    """A single-snapshot checkpoint with atomic full-file replacement.

    Every commit rewrites the complete committed prefix. A file lock plus a
    thread lock serializes commits across threads and processes. On input-hash
    or algorithm-version mismatch the whole snapshot is invalidated; no old
    frame can be mixed into a new generation.
    """

    def __init__(
        self,
        path: str | Path,
        *,
        input_hash: str,
        algorithm_version: str,
    ) -> None:
        self.path = Path(path)
        self.lock_path = self.path.with_name(self.path.name + ".lock")
        self.input_hash = input_hash
        self.algorithm_version = algorithm_version
        self._thread_lock = threading.RLock()
        self._records: list[PersistedFrame] = []
        self._generation = 0
        self._loaded = False
        self._last_load: CheckpointLoadResult | None = None

    @property
    def generation(self) -> int:
        return self._generation

    @property
    def records(self) -> tuple[PersistedFrame, ...]:
        with self._thread_lock:
            return tuple(self._records)

    def _empty_payload(self) -> dict[str, Any]:
        return {
            "input_hash": self.input_hash,
            "algorithm_version": self.algorithm_version,
            "generation": self._generation,
            "frames": [],
        }

    def reset(self) -> None:
        with self._thread_lock, _FileLock(self.lock_path):
            self._records = []
            self._generation += 1
            payload = self._empty_payload()
            self._write_payload(payload)
            self._loaded = True
            self._last_load = CheckpointLoadResult(
                valid=True,
                found=True,
                invalid_reason=None,
                generation=self._generation,
                records=(),
            )

    def load(self) -> CheckpointLoadResult:
        with self._thread_lock, _FileLock(self.lock_path):
            if self._loaded and self._last_load is not None:
                return self._last_load
            result = self._load_unlocked()
            self._loaded = True
            self._last_load = result
            return result

    def _load_unlocked(self) -> CheckpointLoadResult:
        if not self.path.exists():
            self._records = []
            self._generation = 0
            return CheckpointLoadResult(
                valid=True,
                found=False,
                invalid_reason=None,
                generation=0,
                records=(),
            )
        try:
            raw = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, ValueError, UnicodeDecodeError) as exc:
            self._records = []
            return CheckpointLoadResult(
                valid=False,
                found=True,
                invalid_reason=f"CHECKPOINT_DECODE:{type(exc).__name__}",
                generation=0,
                records=(),
            )
        if not isinstance(raw, dict) or raw.get("format") != CHECKPOINT_FORMAT:
            self._records = []
            return CheckpointLoadResult(
                valid=False,
                found=True,
                invalid_reason="CHECKPOINT_FORMAT_MISMATCH",
                generation=0,
                records=(),
            )
        payload = raw.get("payload")
        integrity = raw.get("integrity")
        if not isinstance(payload, dict) or not isinstance(integrity, str):
            self._records = []
            return CheckpointLoadResult(
                valid=False,
                found=True,
                invalid_reason="CHECKPOINT_INTEGRITY_FIELD_MISSING",
                generation=0,
                records=(),
            )
        if _payload_integrity(payload) != integrity:
            self._records = []
            return CheckpointLoadResult(
                valid=False,
                found=True,
                invalid_reason="CHECKPOINT_INTEGRITY_MISMATCH",
                generation=0,
                records=(),
            )
        if payload.get("input_hash") != self.input_hash:
            self._records = []
            return CheckpointLoadResult(
                valid=False,
                found=True,
                invalid_reason="INPUT_HASH_MISMATCH",
                generation=0,
                records=(),
            )
        if payload.get("algorithm_version") != self.algorithm_version:
            self._records = []
            return CheckpointLoadResult(
                valid=False,
                found=True,
                invalid_reason="ALGORITHM_VERSION_MISMATCH",
                generation=0,
                records=(),
            )
        try:
            generation = int(payload.get("generation", 0))
            raw_frames = payload.get("frames", [])
            if not isinstance(raw_frames, list):
                raise ValueError("frames must be a list")
            records = [PersistedFrame.from_dict(item) for item in raw_frames]
        except (TypeError, ValueError, WorkbenchError) as exc:
            self._records = []
            return CheckpointLoadResult(
                valid=False,
                found=True,
                invalid_reason=f"CHECKPOINT_RECORD_INVALID:{type(exc).__name__}",
                generation=0,
                records=(),
            )
        for expected, record in enumerate(records):
            if record.frame_index != expected:
                self._records = []
                return CheckpointLoadResult(
                    valid=False,
                    found=True,
                    invalid_reason="CHECKPOINT_FRAME_ORDER_INVALID",
                    generation=0,
                    records=(),
                )
            if (
                record.input_hash != self.input_hash
                or record.algorithm_version != self.algorithm_version
            ):
                self._records = []
                return CheckpointLoadResult(
                    valid=False,
                    found=True,
                    invalid_reason="CHECKPOINT_FRAME_METADATA_MISMATCH",
                    generation=0,
                    records=(),
                )
        self._records = records
        self._generation = generation
        return CheckpointLoadResult(
            valid=True,
            found=True,
            invalid_reason=None,
            generation=generation,
            records=tuple(records),
        )

    def commit(
        self,
        params: FrameParameters,
        render_stats: FrameRenderStats | None = None,
    ) -> PersistedFrame:
        with self._thread_lock, _FileLock(self.lock_path):
            if not self._loaded:
                result = self._load_unlocked()
                self._loaded = True
                self._last_load = result
            if params.frame_index != len(self._records):
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "checkpoint commits must be serial and in frame order",
                    stage="checkpoint-commit",
                    frame_index=params.frame_index,
                    details={"expected_frame_index": len(self._records)},
                )
            record = PersistedFrame(
                frame_index=params.frame_index,
                params=params.clone(),
                render_stats=render_stats,
                input_hash=self.input_hash,
                algorithm_version=self.algorithm_version,
            )
            records = [*self._records, record]
            payload = {
                "input_hash": self.input_hash,
                "algorithm_version": self.algorithm_version,
                "generation": self._generation + 1,
                "frames": [item.to_dict() for item in records],
            }
            self._write_payload(payload)
            self._records = records
            self._generation += 1
            self._last_load = CheckpointLoadResult(
                valid=True,
                found=True,
                invalid_reason=None,
                generation=self._generation,
                records=tuple(records),
            )
            return record

    def _write_payload(self, payload: dict[str, Any]) -> None:
        wrapper = {
            "format": CHECKPOINT_FORMAT,
            "integrity": _payload_integrity(payload),
            "payload": payload,
        }
        self.path.parent.mkdir(parents=True, exist_ok=True)
        handle = tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            prefix=f".{self.path.name}.",
            suffix=".tmp",
            dir=self.path.parent,
            delete=False,
        )
        temp_path = Path(handle.name)
        try:
            with handle:
                json.dump(
                    wrapper,
                    handle,
                    ensure_ascii=False,
                    sort_keys=True,
                    separators=(",", ":"),
                )
                handle.write("\n")
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp_path, self.path)
            try:
                directory_fd = os.open(self.path.parent, os.O_RDONLY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
            except (AttributeError, OSError):
                pass
        except Exception as exc:
            try:
                temp_path.unlink(missing_ok=True)
            except OSError:
                pass
            raise WorkbenchError.wrap(
                ErrorCode.E_IO,
                "failed to write checkpoint atomically",
                exc,
                stage="checkpoint-commit",
            )

    def iter_records(self) -> Iterator[PersistedFrame]:
        yield from self.records

