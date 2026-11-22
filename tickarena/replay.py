"""Crash-safe streaming replay recording and independent format validation."""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator

from .atomic import replace_with_retry
from .codes import E_ARG, E_REPLAY, E_STATE, TickArenaError
from .model import MatchDef, parse_initial_state
from .numeric import canonical_bytes, canonical_json, require_int

REPLAY_FORMAT_VERSION = 1
REPLAY_PROTOCOL = "tickarena.replay/1"


def build_replay_header(
    match: MatchDef,
    event_fingerprint: str,
    *,
    snapshot_interval: int,
    event_buffer_limit: int,
) -> dict[str, Any]:
    return {
        "kind": "header",
        "protocol": REPLAY_PROTOCOL,
        "format_version": REPLAY_FORMAT_VERSION,
        "seed": match.seed,
        "max_ticks": match.tick_count,
        "initial_fingerprint": match.initial_fingerprint,
        "event_fingerprint": event_fingerprint,
        "snapshot_interval": snapshot_interval,
        "event_buffer_limit": event_buffer_limit,
        "initial_state": match.normalized_initial,
    }


def _fsync_directory(path: Path) -> None:
    if os.name != "posix":
        return
    try:
        fd = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    except OSError:
        pass
    finally:
        os.close(fd)


class ReplayRecorder:
    """Streaming recorder; the engine never keeps a replay in memory."""

    def __init__(
        self,
        path: str | Path,
        header: dict[str, Any],
        *,
        resume_progress: dict[str, Any] | None = None,
    ) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.partial_path = Path(str(self.path) + ".partial")
        self.header = header
        self._hasher = hashlib.sha256()
        self._records = 0
        self._last_tick = -1
        self._last_snapshot_tick = -1
        self._closed = False
        self._resumed = resume_progress is not None

        if resume_progress is not None:
            self._resume(resume_progress)
        else:
            if self.path.exists():
                self.path.unlink()
            self._fh = self.partial_path.open("wb")
            self._fh.write(canonical_bytes(header) + b"\n")
            self._fh.flush()

    def _resume(self, progress: dict[str, Any]) -> None:
        if not self.partial_path.is_file():
            raise TickArenaError(E_REPLAY, "replay partial file is missing")
        byte_count = require_int(progress.get("bytes"), "replay.bytes", minimum=1)
        expected_records = require_int(
            progress.get("records"), "replay.records", minimum=0
        )
        expected_hash = progress.get("body_sha256")
        if not isinstance(expected_hash, str):
            raise TickArenaError(E_REPLAY, "replay body hash is missing")
        data = self.partial_path.read_bytes()
        if len(data) < byte_count:
            raise TickArenaError(E_REPLAY, "replay partial is shorter than checkpoint")
        data = data[:byte_count]
        lines = data.splitlines(keepends=True)
        if not lines:
            raise TickArenaError(E_REPLAY, "replay partial has no header")
        try:
            stored_header = json.loads(lines[0])
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise TickArenaError(E_REPLAY, "invalid replay partial header") from exc
        if canonical_bytes(stored_header) != canonical_bytes(self.header):
            raise TickArenaError(E_REPLAY, "replay partial header mismatch")
        hasher = hashlib.sha256()
        records = 0
        last_tick = -1
        last_snapshot_tick = -1
        for line in lines[1:]:
            hasher.update(line)
            try:
                record = json.loads(line)
            except (json.JSONDecodeError, UnicodeDecodeError) as exc:
                raise TickArenaError(E_REPLAY, "invalid replay partial body") from exc
            if not isinstance(record, dict):
                raise TickArenaError(E_REPLAY, "replay partial record must be an object")
            if record.get("kind") == "footer":
                raise TickArenaError(E_REPLAY, "resume source replay is already finalized")
            records += 1
            if record.get("kind") == "tick":
                last_tick = require_int(record.get("tick"), "tick", minimum=0)
            elif record.get("kind") == "snapshot":
                last_snapshot_tick = require_int(record.get("tick"), "tick", minimum=0)
        if hasher.hexdigest() != expected_hash or records != expected_records:
            raise TickArenaError(E_REPLAY, "replay partial does not match checkpoint")
        if last_tick != progress.get("last_tick"):
            raise TickArenaError(E_REPLAY, "replay partial tick does not match checkpoint")
        if len(data) != byte_count:
            raise TickArenaError(E_REPLAY, "replay partial length mismatch")
        with self.partial_path.open("r+b") as fh:
            fh.truncate(byte_count)
        self._fh = self.partial_path.open("ab")
        self._hasher = hasher
        self._records = records
        self._last_tick = last_tick
        self._last_snapshot_tick = last_snapshot_tick

    def _append(self, record: dict[str, Any]) -> None:
        if self._closed:
            raise TickArenaError(E_STATE, "replay recorder is closed")
        line = canonical_bytes(record) + b"\n"
        self._fh.write(line)
        self._hasher.update(line)
        self._records += 1
        if record.get("kind") == "tick":
            tick = require_int(record.get("tick"), "tick", minimum=0)
            if tick != self._last_tick + 1:
                raise TickArenaError(
                    E_STATE,
                    f"replay tick order mismatch: expected {self._last_tick + 1}, got {tick}",
                )
            self._last_tick = tick
        elif record.get("kind") == "snapshot":
            tick = require_int(record.get("tick"), "snapshot.tick", minimum=0)
            if tick < self._last_snapshot_tick or tick > self._last_tick:
                raise TickArenaError(E_STATE, "replay snapshot tick order mismatch")
            self._last_snapshot_tick = tick
        else:
            raise TickArenaError(E_STATE, "only tick/snapshot records can be appended")

    def record_tick(self, record: dict[str, Any]) -> None:
        if record.get("kind") != "tick":
            raise TickArenaError(E_ARG, "record_tick requires kind=tick")
        self._append(record)

    def record_snapshot(self, record: dict[str, Any]) -> None:
        if record.get("kind") != "snapshot":
            raise TickArenaError(E_ARG, "record_snapshot requires kind=snapshot")
        self._append(record)

    def flush(self) -> None:
        if self._closed:
            return
        self._fh.flush()
        os.fsync(self._fh.fileno())

    def progress(self) -> dict[str, Any]:
        self.flush()
        return {
            "partial_path": str(self.partial_path),
            "bytes": self._fh.tell(),
            "records": self._records,
            "body_sha256": self._hasher.copy().hexdigest(),
            "last_tick": self._last_tick,
            "last_snapshot_tick": self._last_snapshot_tick,
        }

    def finalize(
        self,
        final_state: dict[str, Any],
        key_metrics: dict[str, Any],
        *,
        final_snapshot: dict[str, Any] | None = None,
    ) -> None:
        if self._closed:
            return
        final_tick = require_int(final_state.get("tick"), "final_state.tick", minimum=0)
        if self._last_tick != final_tick:
            raise TickArenaError(E_STATE, "final state tick does not match replay ticks")
        if (
            final_snapshot is not None
            and self._last_snapshot_tick != final_tick
        ):
            self.record_snapshot(final_snapshot)
        footer = {
            "kind": "footer",
            "protocol": REPLAY_PROTOCOL,
            "format_version": REPLAY_FORMAT_VERSION,
            "records": self._records,
            "body_sha256": self._hasher.hexdigest(),
            "final_tick": final_tick,
            "final_state": final_state,
            "key_metrics": key_metrics,
        }
        self._fh.write(canonical_bytes(footer) + b"\n")
        self._fh.flush()
        os.fsync(self._fh.fileno())
        self._fh.close()
        self._closed = True
        replace_with_retry(self.partial_path, self.path)
        _fsync_directory(self.path.parent)

    def close_partial(self) -> None:
        if not self._closed:
            self.flush()
            self._fh.close()
            self._closed = True


@dataclass(frozen=True)
class ReplayMeta:
    path: Path
    header: dict[str, Any]
    footer: dict[str, Any]
    records: int
    body_sha256: str
    file_size: int


def load_and_validate_replay(path: str | Path) -> ReplayMeta:
    """Validate framing and content hashes without loading all body records."""

    replay_path = Path(path)
    if not replay_path.is_file():
        raise TickArenaError(E_REPLAY, f"replay file not found: {replay_path}")
    try:
        file_size = replay_path.stat().st_size
        with replay_path.open("rb") as fh:
            first_line = fh.readline()
            if not first_line:
                raise TickArenaError(E_REPLAY, "empty replay file")
            try:
                header = json.loads(first_line)
            except (json.JSONDecodeError, UnicodeDecodeError) as exc:
                raise TickArenaError(E_REPLAY, "invalid replay header") from exc
            if not isinstance(header, dict):
                raise TickArenaError(E_REPLAY, "replay header must be an object")
            if header.get("kind") != "header":
                raise TickArenaError(E_REPLAY, "replay must start with kind=header")
            if header.get("protocol") != REPLAY_PROTOCOL:
                raise TickArenaError(E_REPLAY, "unknown replay protocol")
            version = header.get("format_version")
            if version != REPLAY_FORMAT_VERSION:
                raise TickArenaError(E_REPLAY, f"unknown replay format_version {version!r}")
            if canonical_bytes(header) + b"\n" != first_line:
                raise TickArenaError(E_REPLAY, "replay header is not canonical")
            try:
                initial_match = parse_initial_state(header.get("initial_state"))
            except TickArenaError as exc:
                raise TickArenaError(E_REPLAY, f"invalid replay initial state: {exc}") from exc
            if initial_match.initial_fingerprint != header.get("initial_fingerprint"):
                raise TickArenaError(E_REPLAY, "replay initial fingerprint mismatch")
            seed = require_int(header.get("seed"), "seed", minimum=0)
            if seed != initial_match.seed:
                raise TickArenaError(E_REPLAY, "replay seed does not match initial state")
            max_ticks = require_int(header.get("max_ticks"), "max_ticks", minimum=1)
            if max_ticks != initial_match.tick_count:
                raise TickArenaError(E_REPLAY, "replay max_ticks does not match initial state")
            require_int(
                header.get("snapshot_interval"),
                "snapshot_interval",
                minimum=1,
            )
            require_int(
                header.get("event_buffer_limit"),
                "event_buffer_limit",
                minimum=1,
            )

            hasher = hashlib.sha256()
            records = 0
            footer: dict[str, Any] | None = None
            for line in fh:
                if footer is not None:
                    raise TickArenaError(E_REPLAY, "data found after replay footer")
                if not line.endswith(b"\n"):
                    raise TickArenaError(E_REPLAY, "unterminated replay record")
                try:
                    record = json.loads(line)
                except (json.JSONDecodeError, UnicodeDecodeError) as exc:
                    raise TickArenaError(E_REPLAY, "invalid replay body record") from exc
                if not isinstance(record, dict):
                    raise TickArenaError(E_REPLAY, "replay body record must be an object")
                if canonical_bytes(record) + b"\n" != line:
                    raise TickArenaError(E_REPLAY, "replay body record is not canonical")
                if record.get("kind") == "footer":
                    footer = record
                    continue
                hasher.update(line)
                records += 1
            if footer is None:
                raise TickArenaError(E_REPLAY, "replay footer is missing")
            if footer.get("protocol") != REPLAY_PROTOCOL:
                raise TickArenaError(E_REPLAY, "unknown replay footer protocol")
            if footer.get("format_version") != REPLAY_FORMAT_VERSION:
                raise TickArenaError(E_REPLAY, "unknown replay footer version")
            if require_int(footer.get("records"), "footer.records", minimum=0) != records:
                raise TickArenaError(E_REPLAY, "replay record count mismatch")
            body_hash = hasher.hexdigest()
            if footer.get("body_sha256") != body_hash:
                raise TickArenaError(E_REPLAY, "replay body hash mismatch")
            if not isinstance(footer.get("final_state"), dict):
                raise TickArenaError(E_REPLAY, "replay final_state is missing")
            if not isinstance(footer.get("key_metrics"), dict):
                raise TickArenaError(E_REPLAY, "replay key_metrics is missing")
            require_int(footer.get("final_tick"), "footer.final_tick", minimum=0)
    except TickArenaError as exc:
        if exc.code == E_REPLAY:
            raise
        raise TickArenaError(E_REPLAY, f"invalid replay: {exc.message}") from exc
    except OSError as exc:
        raise TickArenaError(E_REPLAY, f"cannot read replay: {exc}") from exc
    return ReplayMeta(
        path=replay_path,
        header=header,
        footer=footer,
        records=records,
        body_sha256=body_hash,
        file_size=file_size,
    )


def iter_replay_body(path: str | Path) -> Iterator[dict[str, Any]]:
    replay_path = Path(path)
    with replay_path.open("rb") as fh:
        fh.readline()
        for line in fh:
            record = json.loads(line)
            if record.get("kind") == "footer":
                break
            yield record

