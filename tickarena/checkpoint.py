"""Atomic, serialized checkpoint persistence."""

from __future__ import annotations

import json
import os
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .atomic import replace_with_retry
from .codes import E_ARG, E_OK, E_ORDER, E_STATE, Result
from .numeric import canonical_bytes, require_int

CHECKPOINT_FORMAT_VERSION = 1
CHECKPOINT_KIND = "tickarena.checkpoint"

_REGISTRY_LOCK = threading.Lock()
_PATH_LOCKS: dict[str, threading.RLock] = {}


def _path_lock(path: Path) -> threading.RLock:
    key = str(path.resolve())
    with _REGISTRY_LOCK:
        lock = _PATH_LOCKS.get(key)
        if lock is None:
            lock = threading.RLock()
            _PATH_LOCKS[key] = lock
        return lock


@dataclass(frozen=True)
class ResumeInfo:
    resumed: bool
    restarted: bool
    code: str
    reason: str = ""


@dataclass(frozen=True)
class CheckpointData:
    result: Result
    envelope: dict[str, Any] | None = None

    @property
    def payload(self) -> dict[str, Any]:
        return {} if self.envelope is None else dict(self.envelope.get("payload", {}))


class CheckpointStore:
    """One logical checkpoint slot per directory.

    Every commit is serialized by the same per-path re-entrant lock, written to
    a same-directory temporary file, fsynced, and atomically renamed.  A commit
    whose tick is not newer than the persisted tick is rejected with E_ORDER.
    """

    def __init__(self, directory: str | Path, name: str = "checkpoint.json") -> None:
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.path = self.directory / name
        self._lock = _path_lock(self.path)
        self._last_tick = -1
        self._load_last_tick()

    def _load_last_tick(self) -> None:
        if not self.path.is_file():
            return
        try:
            data = json.loads(self.path.read_bytes())
            tick = data.get("engine_tick")
            if isinstance(tick, int) and not isinstance(tick, bool):
                self._last_tick = tick
        except (OSError, json.JSONDecodeError, UnicodeDecodeError):
            return

    def commit(self, payload: dict[str, Any], tick: int) -> Result:
        tick = require_int(tick, "checkpoint tick", minimum=0)
        envelope = {
            "kind": CHECKPOINT_KIND,
            "format_version": CHECKPOINT_FORMAT_VERSION,
            "engine_tick": tick,
            "payload": payload,
        }
        with self._lock:
            self._load_last_tick()
            if tick <= self._last_tick:
                return Result(
                    E_ORDER,
                    f"checkpoint tick {tick} is not newer than persisted {self._last_tick}",
                    tick=tick,
                )
            temp = self.directory / (
                f".{self.path.name}.{os.getpid()}.{threading.get_ident()}.tmp"
            )
            try:
                with temp.open("wb") as fh:
                    fh.write(canonical_bytes(envelope) + b"\n")
                    fh.flush()
                    os.fsync(fh.fileno())
                replace_with_retry(temp, self.path)
                if os.name == "posix":
                    try:
                        fd = os.open(self.directory, os.O_RDONLY)
                        try:
                            os.fsync(fd)
                        finally:
                            os.close(fd)
                    except OSError:
                        pass
            except OSError as exc:
                try:
                    temp.unlink(missing_ok=True)
                except OSError:
                    pass
                return Result(E_STATE, f"checkpoint write failed: {exc}", tick=tick)
            self._last_tick = tick
        return Result(E_OK, tick=tick)

    def load(self) -> CheckpointData:
        with self._lock:
            if not self.path.is_file():
                return CheckpointData(Result(E_STATE, "checkpoint not found"))
            try:
                raw = json.loads(self.path.read_bytes())
            except (OSError, json.JSONDecodeError, UnicodeDecodeError) as exc:
                return CheckpointData(Result(E_STATE, f"invalid checkpoint: {exc}"))
            if not isinstance(raw, dict):
                return CheckpointData(Result(E_STATE, "checkpoint must be an object"))
            if raw.get("kind") != CHECKPOINT_KIND:
                return CheckpointData(Result(E_STATE, "unknown checkpoint kind"))
            if raw.get("format_version") != CHECKPOINT_FORMAT_VERSION:
                return CheckpointData(
                    Result(
                        E_STATE,
                        f"unknown checkpoint format_version {raw.get('format_version')!r}",
                    )
                )
            tick = raw.get("engine_tick")
            if isinstance(tick, bool) or not isinstance(tick, int) or tick < 0:
                return CheckpointData(Result(E_STATE, "invalid checkpoint engine_tick"))
            payload = raw.get("payload")
            if not isinstance(payload, dict):
                return CheckpointData(Result(E_STATE, "checkpoint payload is missing"))
            self._last_tick = max(self._last_tick, tick)
            return CheckpointData(Result(E_OK), raw)

    def invalidate(self, reason: str = "") -> Result:
        with self._lock:
            if not self.path.is_file():
                return Result(E_OK, "nothing to invalidate")
            invalid_path = self.directory / (self.path.stem + ".invalid.json")
            try:
                replace_with_retry(self.path, invalid_path)
            except OSError as exc:
                return Result(E_STATE, f"checkpoint invalidate failed: {exc}")
            self._last_tick = -1
            return Result(E_OK, message=reason)

    @property
    def last_tick(self) -> int:
        with self._lock:
            self._load_last_tick()
            return self._last_tick

