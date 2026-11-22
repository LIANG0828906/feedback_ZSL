"""Atomic, versioned and serialized local checkpoints."""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import fail
from .serialization import canonical_hash


CHECKPOINT_FORMAT = 1
ALGORITHM_VERSION = "diffrender-1.0.0"


class _FileLock:
    def __init__(self, path: Path) -> None:
        self.path = path
        self.handle: Any = None

    def __enter__(self) -> "_FileLock":
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.handle = open(self.path, "a+b")
        if os.name == "nt":
            import msvcrt

            self.handle.seek(0)
            if self.handle.read(1) == b"":
                self.handle.write(b"\0")
                self.handle.flush()
            self.handle.seek(0)
            msvcrt.locking(self.handle.fileno(), msvcrt.LK_LOCK, 1)
        else:
            import fcntl

            fcntl.flock(self.handle.fileno(), fcntl.LOCK_EX)
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        if self.handle is None:
            return
        try:
            if os.name == "nt":
                import msvcrt

                self.handle.seek(0)
                msvcrt.locking(self.handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(self.handle.fileno(), fcntl.LOCK_UN)
        finally:
            self.handle.close()
            self.handle = None


@dataclass(frozen=True)
class CheckpointLoadResult:
    valid: bool
    payload: dict[str, Any] | None
    reason: str
    checkpoint_path: Path
    content_hash: str | None = None

    @property
    def requires_full_recompute(self) -> bool:
        return not self.valid

    def to_dict(self) -> dict[str, Any]:
        return {
            "valid": self.valid,
            "reason": self.reason,
            "checkpoint_path": str(self.checkpoint_path),
            "content_hash": self.content_hash,
            "requires_full_recompute": self.requires_full_recompute,
        }


class CheckpointStore:
    def __init__(self, directory: str | os.PathLike[str]) -> None:
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.path = self.directory / "checkpoint.json"
        self.lock_path = self.directory / ".checkpoint.lock"

    def save(
        self,
        state: dict[str, Any],
        *,
        parameter_version: int,
        config_fingerprint: str,
        dataset_fingerprint: str,
        algorithm_version: str = ALGORITHM_VERSION,
        checkpoint_format: int = CHECKPOINT_FORMAT,
    ) -> dict[str, Any]:
        if isinstance(parameter_version, bool) or not isinstance(parameter_version, int):
            fail("E_CHECKPOINT", "parameter_version must be an integer", "parameter_version")
        if parameter_version < 0:
            fail("E_CHECKPOINT", "parameter_version must be non-negative", "parameter_version")
        payload = dict(state)
        payload.update(
            {
                "checkpoint_format": checkpoint_format,
                "algorithm_version": algorithm_version,
                "parameter_version": parameter_version,
                "config_fingerprint": config_fingerprint,
                "dataset_fingerprint": dataset_fingerprint,
            }
        )
        content_hash = canonical_hash(payload)
        payload["content_hash"] = content_hash
        try:
            encoded = (
                json.dumps(
                    payload,
                    ensure_ascii=False,
                    sort_keys=True,
                    separators=(",", ":"),
                    allow_nan=False,
                )
                + "\n"
            ).encode("utf-8")
        except (TypeError, ValueError) as exc:
            fail(
                "E_CHECKPOINT",
                f"checkpoint state cannot be serialized: {exc}",
                "checkpoint",
            )
        with _FileLock(self.lock_path):
            fd, temporary = tempfile.mkstemp(
                prefix=".checkpoint.", suffix=".tmp", dir=self.directory
            )
            temporary_path = Path(temporary)
            try:
                with os.fdopen(fd, "wb") as handle:
                    handle.write(encoded)
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(temporary_path, self.path)
                self._fsync_directory()
            except OSError as exc:
                try:
                    temporary_path.unlink()
                except OSError:
                    pass
                fail(
                    "E_CHECKPOINT",
                    f"failed to write checkpoint atomically: {exc}",
                    "checkpoint",
                )
        return {
            "path": str(self.path),
            "content_hash": content_hash,
            "parameter_version": parameter_version,
            "bytes": len(encoded),
        }

    def load(
        self,
        *,
        expected_config_fingerprint: str,
        expected_dataset_fingerprint: str,
        expected_algorithm_version: str = ALGORITHM_VERSION,
        expected_checkpoint_format: int = CHECKPOINT_FORMAT,
        expected_parameter_version: int | None = None,
    ) -> CheckpointLoadResult:
        if not self.path.is_file():
            return CheckpointLoadResult(
                valid=False,
                payload=None,
                reason="checkpoint_missing",
                checkpoint_path=self.path,
            )
        with _FileLock(self.lock_path):
            try:
                raw = self.path.read_bytes()
                payload = json.loads(raw.decode("utf-8"))
            except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
                return CheckpointLoadResult(
                    valid=False,
                    payload=None,
                    reason=f"checkpoint_parse_error:{exc}",
                    checkpoint_path=self.path,
                )
        if not isinstance(payload, dict):
            return CheckpointLoadResult(
                valid=False,
                payload=None,
                reason="checkpoint_payload_not_object",
                checkpoint_path=self.path,
            )
        stored_hash = payload.get("content_hash")
        without_hash = {key: value for key, value in payload.items() if key != "content_hash"}
        computed_hash = canonical_hash(without_hash)
        if not isinstance(stored_hash, str) or stored_hash != computed_hash:
            return CheckpointLoadResult(
                valid=False,
                payload=None,
                reason="content_hash_mismatch",
                checkpoint_path=self.path,
                content_hash=computed_hash,
            )
        checks = (
            ("checkpoint_format", expected_checkpoint_format),
            ("algorithm_version", expected_algorithm_version),
            ("config_fingerprint", expected_config_fingerprint),
            ("dataset_fingerprint", expected_dataset_fingerprint),
        )
        for field_name, expected in checks:
            if payload.get(field_name) != expected:
                return CheckpointLoadResult(
                    valid=False,
                    payload=None,
                    reason=f"version_mismatch:{field_name}",
                    checkpoint_path=self.path,
                    content_hash=computed_hash,
                )
        parameter_version = payload.get("parameter_version")
        if (
            isinstance(parameter_version, bool)
            or not isinstance(parameter_version, int)
            or parameter_version < 0
        ):
            return CheckpointLoadResult(
                valid=False,
                payload=None,
                reason="invalid_parameter_version",
                checkpoint_path=self.path,
                content_hash=computed_hash,
            )
        if (
            expected_parameter_version is not None
            and parameter_version != expected_parameter_version
        ):
            return CheckpointLoadResult(
                valid=False,
                payload=None,
                reason="parameter_version_mismatch",
                checkpoint_path=self.path,
                content_hash=computed_hash,
            )
        return CheckpointLoadResult(
            valid=True,
            payload=payload,
            reason="ok",
            checkpoint_path=self.path,
            content_hash=computed_hash,
        )

    def _fsync_directory(self) -> None:
        if os.name == "nt":
            return
        try:
            descriptor = os.open(self.directory, os.O_RDONLY)
            try:
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
        except OSError:
            pass

