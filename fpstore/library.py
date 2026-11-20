"""Crash-recoverable, versioned local fingerprint library."""

from __future__ import annotations

import errno
import fcntl
import json
import os
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .audio import read_wav
from .config import FingerprintConfig
from .errors import (
    BizError,
    E_BUSY,
    E_LIB_MISMATCH,
    E_RECOVERY_INVALID,
    E_STATE_INVALID,
)
from .fingerprint import SegmentFingerprint, extract_fingerprint
from .migration import MigrationContext, MigrationReport, migrate_records
from .records import FingerprintRecord, build_record, parse_record_bytes, record_key
from .util import (
    GENESIS_HASH,
    TraceLogger,
    atomic_write_bytes,
    atomic_write_json,
    canonical_bytes,
    content_hash,
    hash_label,
    make_chain,
    read_json,
    sha256_bytes,
    stable_version_id,
    verify_chain,
)


MANIFEST_SCHEMA = "fp-library-manifest/1"
SESSION_SCHEMA = "fp-staging-session/1"
POINTER_SCHEMA = "fp-published-pointer/1"
STAGING_POINTER_SCHEMA = "fp-staging-pointer/1"

ENTRY_FIELDS = {
    "key",
    "ordinal",
    "track_id",
    "segment_index",
    "fingerprint_version",
    "config_hash",
    "segment_hash",
    "fingerprint_segment_hash",
    "record_checksum",
    "segment_file",
    "sample_rate",
    "hop_length",
    "hash_algo",
    "time_basis",
    "segment_start_ms",
    "segment_end_ms",
    "prev_chain_hash",
    "chain_hash",
}


@dataclass(frozen=True)
class TrackSpec:
    track_id: str
    wav_path: Path
    config: FingerprintConfig

    def to_plan(self, library_root: Path) -> dict[str, Any]:
        try:
            relative = os.path.relpath(self.wav_path.resolve(), library_root.resolve())
        except ValueError:
            relative = str(self.wav_path.resolve())
        return {
            "track_id": self.track_id,
            "wav": relative.replace(os.sep, "/"),
            "config": self.config.as_dict(),
        }

    @classmethod
    def from_plan(cls, value: dict[str, Any], library_root: Path) -> "TrackSpec":
        required = {"track_id", "wav", "config"}
        missing = sorted(required.difference(value))
        if missing:
            raise BizError(
                E_RECOVERY_INVALID,
                "staging session track is missing fields",
                {"field": missing[0], "track": value.get("track_id")},
            )
        path = Path(value["wav"])
        if not path.is_absolute():
            path = library_root / path
        return cls(str(value["track_id"]), path, FingerprintConfig.from_dict(value["config"]))


@dataclass
class OperationResult:
    status: str
    version_id: str
    staging_manifest_hash: str
    published_version_hash: str | None
    committed_segments: int
    skipped_segments: int
    traces: list[dict[str, Any]] = field(default_factory=list)

    def as_dict(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "version_id": self.version_id,
            "staging_manifest_hash": self.staging_manifest_hash,
            "published_version_hash": self.published_version_hash,
            "committed_segments": self.committed_segments,
            "skipped_segments": self.skipped_segments,
        }


class OperationLock:
    """Non-blocking cross-process lock for import/migration/publish operations."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self._handle: Any = None

    def __enter__(self) -> "OperationLock":
        self.path.parent.mkdir(parents=True, exist_ok=True)
        handle = self.path.open("a+b")
        try:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as exc:
            handle.close()
            if exc.errno in (errno.EACCES, errno.EAGAIN):
                raise BizError(E_BUSY, "another import or migration task owns this library") from exc
            raise
        self._handle = handle
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        if self._handle is not None:
            fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
            self._handle.close()
            self._handle = None


class LibrarySnapshot:
    """An immutable in-memory snapshot of one complete published library version."""

    def __init__(
        self,
        manifest: dict[str, Any],
        manifest_hash: str,
        entries: list[dict[str, Any]],
        records: list[FingerprintRecord],
        raw_files: list[bytes],
    ) -> None:
        self.manifest = manifest
        self.manifest_hash = manifest_hash
        self.entries = entries
        self.records = records
        self._raw_files = raw_files
        self.closed = False

    @property
    def library_version_hash(self) -> str:
        return self.manifest_hash

    def close(self) -> None:
        self.closed = True
        self._raw_files = []
        self.records = []
        self.entries = []

    def __enter__(self) -> "LibrarySnapshot":
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.close()


class Library:
    def __init__(self, root: Path) -> None:
        self.root = root.resolve()
        self.lock_path = self.root / "locks" / "operation.lock"
        self.published_pointer = self.root / "published" / "current.json"
        self.staging_pointer = self.root / "staging" / "current.json"
        self.manifest_dir = self.root / "manifests"
        self.segment_dir = self.root / "segments"
        self.session_dir = self.root / "staging" / "sessions"
        for directory in (
            self.lock_path.parent,
            self.published_pointer.parent,
            self.staging_pointer.parent,
            self.manifest_dir,
            self.segment_dir,
            self.session_dir,
        ):
            directory.mkdir(parents=True, exist_ok=True)
        self._process_guard = threading.Lock()
        self._process_locked = False

    # ------------------------------------------------------------------ paths
    def _manifest_path(self, manifest_hash: str) -> Path:
        digest = manifest_hash.split(":", 1)[1] if manifest_hash.startswith("sha256:") else manifest_hash
        return self.manifest_dir / f"{digest}.json"

    def _manifest_relative(self, manifest_hash: str) -> str:
        return f"manifests/{self._manifest_path(manifest_hash).name}"

    def _segment_path(self, digest: str) -> Path:
        return self.segment_dir / digest[:2] / f"{digest}.json"

    def _segment_relative(self, digest: str) -> str:
        return f"segments/{digest[:2]}/{digest}.json"

    def _resolve_relative(self, relative: str, *, code: str) -> Path:
        if not isinstance(relative, str) or not relative:
            raise BizError(code, "library metadata path is empty", {"field": "path"})
        path = (self.root / relative).resolve()
        try:
            path.relative_to(self.root)
        except ValueError as exc:
            raise BizError(code, "library metadata path escapes the library root", {"path": relative}) from exc
        return path

    # -------------------------------------------------------------- operations
    def _acquire_operation(self) -> OperationLock:
        if not self._process_guard.acquire(blocking=False):
            raise BizError(E_BUSY, "another operation is active in this process")
        try:
            lock = OperationLock(self.lock_path)
            lock.__enter__()
            self._process_locked = True
            return lock
        except BaseException:
            self._process_guard.release()
            raise

    def _release_operation(self, lock: OperationLock) -> None:
        try:
            lock.__exit__(None, None, None)
        finally:
            self._process_locked = False
            self._process_guard.release()

    # --------------------------------------------------------------- manifest
    def _manifest_core(
        self,
        *,
        version_id: str,
        operation: str,
        parent_manifest_hash: str | None,
        plan_digest: str,
        entries: Iterable[dict[str, Any]],
        complete: bool,
    ) -> dict[str, Any]:
        seed = parent_manifest_hash or GENESIS_HASH
        ordered = sorted(
            (
                {key: value for key, value in entry.items() if key not in {"prev_chain_hash", "chain_hash", "ordinal"}}
                for entry in entries
            ),
            key=lambda item: (
                item.get("track_id", ""),
                int(item.get("fingerprint_version", 0)),
                item.get("config_hash", ""),
                int(item.get("segment_index", 0)),
            ),
        )
        for ordinal, entry in enumerate(ordered, start=1):
            entry["ordinal"] = ordinal
        chained, root_chain_hash = make_chain(ordered, seed)
        return {
            "schema": MANIFEST_SCHEMA,
            "version_id": version_id,
            "operation": operation,
            "parent_manifest_hash": parent_manifest_hash,
            "plan_digest": plan_digest,
            "complete": bool(complete),
            "entry_count": len(chained),
            "root_chain_hash": root_chain_hash,
            "entries": chained,
        }

    def _seal_manifest(self, core: dict[str, Any]) -> dict[str, Any]:
        return {**core, "manifest_hash": hash_label(content_hash(core))}

    def _write_manifest_snapshot(self, manifest: dict[str, Any]) -> str:
        manifest_hash = manifest["manifest_hash"]
        core = {key: value for key, value in manifest.items() if key != "manifest_hash"}
        if hash_label(content_hash(core)) != manifest_hash:
            raise BizError(E_RECOVERY_INVALID, "manifest hash does not match its content")
        path = self._manifest_path(manifest_hash)
        data = canonical_bytes(manifest)
        if path.exists():
            if path.read_bytes() != data:
                raise BizError(E_RECOVERY_INVALID, "manifest content-address collision", {"manifest_hash": manifest_hash})
        else:
            atomic_write_bytes(path, data)
        return manifest_hash

    def _validate_manifest(
        self,
        raw: Any,
        *,
        code: str,
        expected_hash: str | None = None,
        require_complete: bool | None = None,
    ) -> dict[str, Any]:
        if not isinstance(raw, dict):
            raise BizError(code, "library manifest must be an object", {"field": "<manifest>"})
        for field_name in (
            "schema",
            "version_id",
            "operation",
            "parent_manifest_hash",
            "plan_digest",
            "complete",
            "entry_count",
            "root_chain_hash",
            "entries",
            "manifest_hash",
        ):
            if field_name not in raw:
                raise BizError(
                    code,
                    "library manifest required field is missing",
                    {"field": field_name, "manifest": raw.get("version_id")},
                )
        if raw["schema"] != MANIFEST_SCHEMA:
            raise BizError(code, "unsupported library manifest schema", {"field": "schema", "value": raw["schema"]})
        if not isinstance(raw["entries"], list):
            raise BizError(code, "manifest entries must be a list", {"field": "entries"})
        if raw["entry_count"] != len(raw["entries"]):
            raise BizError(
                code,
                "manifest entry count mismatch",
                {"field": "entry_count", "expected": len(raw["entries"]), "actual": raw["entry_count"]},
            )

        previous = raw["parent_manifest_hash"] or GENESIS_HASH
        for index, entry in enumerate(raw["entries"]):
            if not isinstance(entry, dict):
                raise BizError(code, "manifest entry must be an object", {"field": f"entries[{index}]"})
            missing = sorted(ENTRY_FIELDS.difference(entry))
            if missing:
                raise BizError(
                    code,
                    "manifest entry required field is missing",
                    {
                        "field": f"entries[{index}].{missing[0]}",
                        "first_broken_segment": entry.get("key", f"<entry#{index}>"),
                        "expected_ordinal": index + 1,
                    },
                )
            if entry.get("prev_chain_hash") != previous:
                raise BizError(
                    code,
                    "manifest hash chain is broken",
                    {
                        "field": "prev_chain_hash",
                        "first_broken_segment": entry.get("key"),
                        "ordinal": entry.get("ordinal"),
                        "expected": previous,
                        "actual": entry.get("prev_chain_hash"),
                    },
                )
            entry_core = {
                key: value
                for key, value in entry.items()
                if key not in {"prev_chain_hash", "chain_hash"}
            }
            entry_core["prev_chain_hash"] = previous
            expected_chain = hash_label(sha256_bytes(previous.encode("utf-8") + canonical_bytes(entry_core)))
            if entry.get("chain_hash") != expected_chain:
                raise BizError(
                    code,
                    "manifest hash chain is broken",
                    {
                        "field": "chain_hash",
                        "first_broken_segment": entry.get("key"),
                        "ordinal": entry.get("ordinal"),
                        "expected": expected_chain,
                        "actual": entry.get("chain_hash"),
                    },
                )
            previous = expected_chain
        if previous != raw["root_chain_hash"]:
            raise BizError(
                code,
                "manifest root chain hash mismatch",
                {"field": "root_chain_hash", "first_broken_segment": "root", "expected": previous, "actual": raw["root_chain_hash"]},
            )
        core = {key: value for key, value in raw.items() if key != "manifest_hash"}
        actual_hash = hash_label(content_hash(core))
        if actual_hash != raw["manifest_hash"]:
            raise BizError(
                code,
                "manifest hash mismatch",
                {"field": "manifest_hash", "expected": actual_hash, "actual": raw["manifest_hash"]},
            )
        if expected_hash is not None and raw["manifest_hash"] != expected_hash:
            raise BizError(
                code,
                "published pointer and library manifest hash disagree",
                {
                    "field": "manifest_hash",
                    "expected": expected_hash,
                    "actual": raw["manifest_hash"],
                    "version_id": raw.get("version_id"),
                },
            )
        if require_complete is not None and bool(raw["complete"]) is not require_complete:
            raise BizError(
                code,
                "library manifest completeness is invalid",
                {"field": "complete", "required": require_complete, "actual": raw["complete"]},
            )
        return raw

    def _load_manifest_file(self, manifest_hash: str, *, code: str) -> dict[str, Any]:
        path = self._manifest_path(manifest_hash)
        if not path.exists():
            raise BizError(code, "library manifest file is missing", {"manifest_hash": manifest_hash, "field": "manifest_file"})
        try:
            raw = read_json(path)
        except (OSError, ValueError) as exc:
            raise BizError(code, "library manifest is not valid JSON", {"manifest_hash": manifest_hash, "reason": str(exc)}) from exc
        return self._validate_manifest(raw, code=code, expected_hash=manifest_hash)

    def _published_manifest(self, *, code: str = E_LIB_MISMATCH) -> tuple[dict[str, Any], str] | None:
        if not self.published_pointer.exists():
            return None
        try:
            pointer = read_json(self.published_pointer)
        except (OSError, ValueError) as exc:
            raise BizError(code, "published pointer is not valid JSON", {"field": "published/current.json", "reason": str(exc)}) from exc
        if not isinstance(pointer, dict):
            raise BizError(code, "published pointer must be an object", {"field": "published/current.json"})
        for field_name in ("schema", "version_id", "manifest_hash", "manifest_file"):
            if field_name not in pointer:
                raise BizError(code, "published pointer field is missing", {"field": field_name})
        if pointer["schema"] != POINTER_SCHEMA:
            raise BizError(code, "unsupported published pointer schema", {"field": "schema"})
        manifest = self._load_manifest_file(pointer["manifest_hash"], code=code)
        if manifest["version_id"] != pointer["version_id"]:
            raise BizError(
                code,
                "published pointer version id disagrees with manifest",
                {"field": "version_id", "expected": pointer["version_id"], "actual": manifest["version_id"]},
            )
        expected_path = self._manifest_relative(pointer["manifest_hash"])
        if pointer["manifest_file"] != expected_path:
            raise BizError(
                code,
                "published pointer manifest path disagrees with hash",
                {"field": "manifest_file", "expected": expected_path, "actual": pointer["manifest_file"]},
            )
        self._validate_manifest(manifest, code=code, expected_hash=pointer["manifest_hash"], require_complete=True)
        return manifest, pointer["manifest_hash"]

    # -------------------------------------------------------------- staging IO
    def _staging_state(self) -> tuple[dict[str, Any], dict[str, Any], str] | None:
        if not self.staging_pointer.exists():
            return None
        try:
            pointer = read_json(self.staging_pointer)
        except (OSError, ValueError) as exc:
            raise BizError(
                E_RECOVERY_INVALID,
                "staging pointer is not valid JSON",
                {"field": "staging/current.json", "reason": str(exc)},
            ) from exc
        for field_name in ("schema", "version_id", "manifest_hash", "manifest_file", "session_file"):
            if field_name not in pointer:
                raise BizError(E_RECOVERY_INVALID, "staging pointer field is missing", {"field": field_name})
        if pointer["schema"] != STAGING_POINTER_SCHEMA:
            raise BizError(E_RECOVERY_INVALID, "unsupported staging pointer schema", {"field": "schema"})
        session_path = self._resolve_relative(pointer["session_file"], code=E_RECOVERY_INVALID)
        if not session_path.exists():
            raise BizError(
                E_RECOVERY_INVALID,
                "staging session snapshot is missing",
                {"field": "session_file", "path": pointer["session_file"]},
            )
        try:
            session = read_json(session_path)
        except (OSError, ValueError) as exc:
            raise BizError(E_RECOVERY_INVALID, "staging session is not valid JSON", {"field": "session_file"}) from exc
        self._validate_session(session)
        manifest = self._load_manifest_file(pointer["manifest_hash"], code=E_RECOVERY_INVALID)
        if manifest["version_id"] != pointer["version_id"] or manifest["version_id"] != session["version_id"]:
            raise BizError(
                E_RECOVERY_INVALID,
                "staging pointer, session and manifest version ids disagree",
                {"field": "version_id", "version_id": pointer["version_id"]},
            )
        if pointer["manifest_file"] != self._manifest_relative(pointer["manifest_hash"]):
            raise BizError(E_RECOVERY_INVALID, "staging pointer manifest path is inconsistent", {"field": "manifest_file"})
        self._validate_committed_segment_files(manifest)
        return session, manifest, pointer["manifest_hash"]

    def _validate_session(self, session: Any) -> None:
        if not isinstance(session, dict):
            raise BizError(E_RECOVERY_INVALID, "staging session must be an object", {"field": "<session>"})
        for field_name in (
            "schema",
            "version_id",
            "operation",
            "parent_manifest_hash",
            "plan_digest",
            "plan",
            "state",
        ):
            if field_name not in session:
                raise BizError(E_RECOVERY_INVALID, "staging session field is missing", {"field": field_name})
        if session["schema"] != SESSION_SCHEMA:
            raise BizError(E_RECOVERY_INVALID, "unsupported staging session schema", {"field": "schema"})
        if not isinstance(session["plan"], list):
            raise BizError(E_RECOVERY_INVALID, "staging plan must be a list", {"field": "plan"})

    def _write_staging_pointer(
        self,
        *,
        version_id: str,
        manifest_hash: str,
        session_file: str,
    ) -> None:
        atomic_write_json(
            self.staging_pointer,
            {
                "schema": STAGING_POINTER_SCHEMA,
                "version_id": version_id,
                "manifest_hash": manifest_hash,
                "manifest_file": self._manifest_relative(manifest_hash),
                "session_file": session_file,
            },
        )

    def _validate_committed_segment_files(self, manifest: dict[str, Any]) -> None:
        for entry in manifest["entries"]:
            path = self._resolve_relative(entry["segment_file"], code=E_RECOVERY_INVALID)
            if not path.exists():
                raise BizError(
                    E_RECOVERY_INVALID,
                    "committed segment file is missing",
                    {
                        "first_broken_segment": entry["key"],
                        "field": "segment_file",
                        "ordinal": entry["ordinal"],
                        "path": entry["segment_file"],
                    },
                )
            try:
                data = path.read_bytes()
            except OSError as exc:
                raise BizError(
                    E_RECOVERY_INVALID,
                    "committed segment file is unreadable",
                    {"first_broken_segment": entry["key"], "field": "segment_file", "reason": str(exc)},
                ) from exc
            if sha256_bytes(data) != entry["segment_hash"]:
                raise BizError(
                    E_RECOVERY_INVALID,
                    "committed segment content hash mismatch",
                    {
                        "first_broken_segment": entry["key"],
                        "field": "segment_hash",
                        "ordinal": entry["ordinal"],
                        "expected": entry["segment_hash"],
                        "actual": sha256_bytes(data),
                    },
                )

    # ---------------------------------------------------------------- imports
    def import_tracks(
        self,
        tracks: Iterable[TrackSpec],
        *,
        resume: bool = False,
        crash_after: int | None = None,
        cancel_after: int | None = None,
        cancellation: threading.Event | None = None,
        trace: TraceLogger | None = None,
    ) -> OperationResult:
        lock = self._acquire_operation()
        traces: list[dict[str, Any]] = []

        def emit(event: str, **fields: Any) -> None:
            if trace is not None:
                traces.append(trace.emit(event, **fields))

        try:
            if resume:
                state = self._staging_state()
                if state is None:
                    raise BizError(
                        E_RECOVERY_INVALID,
                        "no committed staging snapshot exists to resume",
                        {"field": "staging/current.json"},
                    )
                session, manifest, staging_hash = state
                if session["operation"] != "import":
                    raise BizError(
                        E_RECOVERY_INVALID,
                        "staging snapshot belongs to a different operation",
                        {"field": "operation", "value": session["operation"]},
                    )
                if session["state"] == "published":
                    published = self._published_manifest()
                    return OperationResult(
                        "already_published",
                        session["version_id"],
                        staging_hash,
                        published[1] if published else None,
                        0,
                        0,
                        traces,
                    )
            else:
                session, manifest, staging_hash = self._start_import(tracks)

            emit(
                "session_open",
                version_id=session["version_id"],
                operation=session["operation"],
                state=session["state"],
                staging_manifest_hash=staging_hash,
                parent_manifest_hash=session["parent_manifest_hash"],
                published_version_hash=self._published_hash_or_none(),
            )

            entry_map = {entry["key"]: dict(entry) for entry in manifest["entries"]}
            committed_this_run = 0
            skipped_this_run = 0
            cancel_requested = False
            for plan_row in session["plan"]:
                spec = TrackSpec.from_plan(plan_row, self.root)
                audio = read_wav(spec.wav_path)
                fingerprint = extract_fingerprint(audio, spec.config, track_id=spec.track_id)
                for segment in fingerprint.segments:
                    if cancellation is not None and cancellation.is_set():
                        cancel_requested = True
                        break
                    if cancel_after is not None and committed_this_run >= cancel_after:
                        cancel_requested = True
                        break
                    record = build_record(segment, fingerprint, spec.config)
                    record_bytes = record.to_bytes()
                    file_digest = sha256_bytes(record_bytes)
                    key = record.key
                    existing = entry_map.get(key)
                    if existing is not None and existing["segment_hash"] == file_digest:
                        skipped_this_run += 1
                        emit(
                            "segment_skip",
                            version_id=session["version_id"],
                            track_id=spec.track_id,
                            segment_index=segment.segment_index,
                            fingerprint_version=record.version,
                            segment_hash=file_digest,
                            chain_hash=existing["chain_hash"],
                            reason="already_committed",
                        )
                        continue
                    self._write_segment(file_digest, record_bytes)
                    entry = self._entry_from_record(
                        record,
                        file_digest,
                        segment,
                        spec.config,
                    )
                    entry_map[key] = entry
                    manifest = self._commit_staging_manifest(session, entry_map, complete=False)
                    staging_hash = manifest["manifest_hash"]
                    committed_this_run += 1
                    new_entry = next(item for item in manifest["entries"] if item["key"] == key)
                    emit(
                        "segment_commit",
                        version_id=session["version_id"],
                        track_id=spec.track_id,
                        segment_index=segment.segment_index,
                        fingerprint_version=record.version,
                        segment_status="replaced" if existing else "committed",
                        segment_hash=file_digest,
                        fingerprint_segment_hash=record.segment_hash,
                        chain_hash=new_entry["chain_hash"],
                        manifest_hash=staging_hash,
                    )
                    if crash_after is not None and committed_this_run >= crash_after:
                        emit(
                            "forced_termination",
                            version_id=session["version_id"],
                            committed_segments=committed_this_run,
                            staging_manifest_hash=staging_hash,
                            published_version_hash=self._published_hash_or_none(),
                        )
                        os._exit(137)
                if cancel_requested:
                    break

            session["state"] = "cancelled" if cancel_requested else "ready"
            self._write_session(session)
            status = "cancelled" if cancel_requested else "ready_to_publish"
            emit(
                "operation_complete",
                version_id=session["version_id"],
                status=status,
                committed_segments=committed_this_run,
                skipped_segments=skipped_this_run,
                staging_manifest_hash=staging_hash,
                published_version_hash=self._published_hash_or_none(),
            )
            return OperationResult(
                status,
                session["version_id"],
                staging_hash,
                self._published_hash_or_none(),
                committed_this_run,
                skipped_this_run,
                traces,
            )
        finally:
            self._release_operation(lock)

    def _start_import(self, tracks: Iterable[TrackSpec]) -> tuple[dict[str, Any], dict[str, Any], str]:
        track_list = list(tracks)
        if not track_list:
            raise BizError(E_STATE_INVALID, "import requires at least one track")
        plan = [track.to_plan(self.root) for track in track_list]
        plan_core = {"operation": "import", "tracks": plan}
        plan_digest = content_hash(plan_core)
        published = self._published_manifest(code=E_LIB_MISMATCH)
        parent_hash = published[1] if published else None
        base_entries = published[0]["entries"] if published else []
        version_id = stable_version_id("import", parent_hash or GENESIS_HASH, plan_core)
        session_file = f"staging/sessions/{version_id}.json"
        session = {
            "schema": SESSION_SCHEMA,
            "version_id": version_id,
            "operation": "import",
            "parent_manifest_hash": parent_hash,
            "plan_digest": plan_digest,
            "plan": plan,
            "state": "importing",
        }
        self._write_session(session, session_file=session_file)
        manifest = self._seal_manifest(
            self._manifest_core(
                version_id=version_id,
                operation="import",
                parent_manifest_hash=parent_hash,
                plan_digest=plan_digest,
                entries=base_entries,
                complete=False,
            )
        )
        manifest_hash = self._write_manifest_snapshot(manifest)
        self._write_staging_pointer(version_id=version_id, manifest_hash=manifest_hash, session_file=session_file)
        return session, manifest, manifest_hash

    def _write_session(self, session: dict[str, Any], *, session_file: str | None = None) -> None:
        relative = session_file or f"staging/sessions/{session['version_id']}.json"
        path = self._resolve_relative(relative, code=E_RECOVERY_INVALID)
        atomic_write_json(path, session)

    def _write_segment(self, digest: str, data: bytes) -> None:
        path = self._segment_path(digest)
        if path.exists():
            if path.read_bytes() != data:
                raise BizError(
                    E_RECOVERY_INVALID,
                    "segment content-address collision",
                    {"field": "segment_hash", "segment_hash": digest},
                )
            return
        atomic_write_bytes(path, data)

    def _entry_from_record(
        self,
        record: FingerprintRecord,
        file_digest: str,
        segment: SegmentFingerprint,
        config: FingerprintConfig,
    ) -> dict[str, Any]:
        return {
            "key": record.key,
            "track_id": record.track_id,
            "segment_index": record.segment_index,
            "fingerprint_version": record.version,
            "config_hash": config.config_hash,
            "segment_hash": file_digest,
            "fingerprint_segment_hash": record.segment_hash,
            "record_checksum": record.checksum,
            "segment_file": self._segment_relative(file_digest),
            "sample_rate": record.sample_rate,
            "hop_length": record.hop_length,
            "hash_algo": record.hash_algo,
            "time_basis": record.time_basis.as_dict(),
            "segment_start_ms": segment.start_ms,
            "segment_end_ms": segment.end_ms,
        }

    def _commit_staging_manifest(
        self,
        session: dict[str, Any],
        entry_map: dict[str, dict[str, Any]],
        *,
        complete: bool,
    ) -> dict[str, Any]:
        manifest = self._seal_manifest(
            self._manifest_core(
                version_id=session["version_id"],
                operation=session["operation"],
                parent_manifest_hash=session["parent_manifest_hash"],
                plan_digest=session["plan_digest"],
                entries=entry_map.values(),
                complete=complete,
            )
        )
        manifest_hash = self._write_manifest_snapshot(manifest)
        if not complete:
            self._write_staging_pointer(
                version_id=session["version_id"],
                manifest_hash=manifest_hash,
                session_file=f"staging/sessions/{session['version_id']}.json",
            )
        return manifest

    def _published_hash_or_none(self) -> str | None:
        if not self.published_pointer.exists():
            return None
        try:
            pointer = read_json(self.published_pointer)
        except (OSError, ValueError):
            return None
        return pointer.get("manifest_hash") if isinstance(pointer, dict) else None

    # --------------------------------------------------------------- publish
    def publish(self, *, trace: TraceLogger | None = None) -> dict[str, Any]:
        lock = self._acquire_operation()
        try:
            state = self._staging_state()
            if state is None:
                raise BizError(E_STATE_INVALID, "no staging version is available to publish", {"field": "staging"})
            session, manifest, _ = state
            if manifest["operation"] != "import":
                raise BizError(E_STATE_INVALID, "only completed imports can be published", {"field": "operation"})
            if session["state"] not in {"ready", "published"}:
                raise BizError(
                    E_STATE_INVALID,
                    "staging session has not reached a publishable checkpoint",
                    {"field": "state", "value": session["state"]},
                )
            old_hash = self._published_hash_or_none()
            if manifest["complete"]:
                complete_manifest = manifest
            else:
                complete_manifest = self._seal_manifest(
                    self._manifest_core(
                        version_id=session["version_id"],
                        operation=session["operation"],
                        parent_manifest_hash=session["parent_manifest_hash"],
                        plan_digest=session["plan_digest"],
                        entries=manifest["entries"],
                        complete=True,
                    )
                )
                self._write_manifest_snapshot(complete_manifest)
            new_hash = complete_manifest["manifest_hash"]
            atomic_write_json(
                self.published_pointer,
                {
                    "schema": POINTER_SCHEMA,
                    "version_id": session["version_id"],
                    "manifest_hash": new_hash,
                    "manifest_file": self._manifest_relative(new_hash),
                },
            )
            self._write_staging_pointer(
                version_id=session["version_id"],
                manifest_hash=new_hash,
                session_file=f"staging/sessions/{session['version_id']}.json",
            )
            session["state"] = "published"
            self._write_session(session)
            event = None
            if trace is not None:
                event = trace.emit(
                    "library_publish",
                    version_id=session["version_id"],
                    old_published_version_hash=old_hash,
                    published_version_hash=new_hash,
                    entry_count=complete_manifest["entry_count"],
                )
            return {
                "status": "published",
                "version_id": session["version_id"],
                "old_published_version_hash": old_hash,
                "published_version_hash": new_hash,
                "entry_count": complete_manifest["entry_count"],
                "trace": event,
            }
        finally:
            self._release_operation(lock)

    # --------------------------------------------------------------- snapshot
    def open_snapshot(self) -> LibrarySnapshot:
        published = self._published_manifest(code=E_LIB_MISMATCH)
        if published is None:
            raise BizError(E_LIB_MISMATCH, "no published library version exists", {"field": "published/current.json"})
        manifest, manifest_hash = published
        entries: list[dict[str, Any]] = []
        records: list[FingerprintRecord] = []
        raw_files: list[bytes] = []
        for entry in manifest["entries"]:
            path = self._resolve_relative(entry["segment_file"], code=E_LIB_MISMATCH)
            if not path.exists():
                raise BizError(
                    E_LIB_MISMATCH,
                    "published segment file is missing",
                    {"manifest_hash": manifest_hash, "segment": entry["key"], "field": "segment_file"},
                )
            data = path.read_bytes()
            actual = sha256_bytes(data)
            if actual != entry["segment_hash"]:
                raise BizError(
                    E_LIB_MISMATCH,
                    "published segment content hash mismatch",
                    {
                        "manifest_hash": manifest_hash,
                        "segment": entry["key"],
                        "field": "segment_hash",
                        "expected": entry["segment_hash"],
                        "actual": actual,
                    },
                )
            try:
                record = parse_record_bytes(data)
            except BizError as exc:
                raise BizError(
                    E_LIB_MISMATCH,
                    "published fingerprint record is invalid",
                    {"manifest_hash": manifest_hash, "segment": entry["key"], "cause": exc.to_dict()},
                ) from exc
            if record.key != entry["key"]:
                raise BizError(
                    E_LIB_MISMATCH,
                    "published record key disagrees with manifest",
                    {"manifest_hash": manifest_hash, "segment": entry["key"], "actual": record.key},
                )
            if record.checksum != entry["record_checksum"]:
                raise BizError(
                    E_LIB_MISMATCH,
                    "published record checksum disagrees with manifest",
                    {"manifest_hash": manifest_hash, "segment": entry["key"], "field": "record_checksum"},
                )
            entries.append(entry)
            records.append(record)
            raw_files.append(data)
        return LibrarySnapshot(manifest, manifest_hash, entries, records, raw_files)

    def status(self) -> dict[str, Any]:
        published = self._published_manifest(code=E_LIB_MISMATCH)
        staging_state = None
        if self.staging_pointer.exists():
            try:
                staging = self._staging_state()
                if staging is not None:
                    staging_state = {
                        "version_id": staging[0]["version_id"],
                        "state": staging[0]["state"],
                        "manifest_hash": staging[2],
                        "entry_count": staging[1]["entry_count"],
                    }
            except BizError as exc:
                staging_state = {"error": exc.to_dict()}
        return {
            "published": None
            if published is None
            else {
                "version_id": published[0]["version_id"],
                "manifest_hash": published[1],
                "entry_count": published[0]["entry_count"],
            },
            "staging": staging_state,
        }

    def staging_details(self) -> dict[str, Any] | None:
        state = self._staging_state()
        if state is None:
            return None
        session, manifest, manifest_hash = state
        return {
            "version_id": session["version_id"],
            "operation": session["operation"],
            "state": session["state"],
            "parent_manifest_hash": session["parent_manifest_hash"],
            "manifest_hash": manifest_hash,
            "entry_count": manifest["entry_count"],
            "root_chain_hash": manifest["root_chain_hash"],
            "committed_segments": [
                {
                    "key": entry["key"],
                    "track_id": entry["track_id"],
                    "segment_index": entry["segment_index"],
                    "fingerprint_version": entry["fingerprint_version"],
                    "segment_hash": entry["segment_hash"],
                    "chain_hash": entry["chain_hash"],
                }
                for entry in manifest["entries"]
            ],
            "published_version_hash": self._published_hash_or_none(),
        }

    def search(
        self,
        query: Any,
        *,
        config: FingerprintConfig | None = None,
        track_id: str | None = None,
        top_k: int = 5,
        expected_version_hash: str | None = None,
    ) -> dict[str, Any]:
        from .retrieval import search_library

        return search_library(
            self,
            query,
            config=config,
            track_id=track_id,
            top_k=top_k,
            expected_version_hash=expected_version_hash,
        )

    # --------------------------------------------------------------- migration
    def migrate(
        self,
        context: MigrationContext,
        *,
        target_version: int = 3,
        trace: TraceLogger | None = None,
    ) -> dict[str, Any]:
        if target_version != 3:
            raise BizError(E_STATE_INVALID, "only v3 is supported as a migration target", {"field": "target_version"})
        lock = self._acquire_operation()
        try:
            published = self._published_manifest(code=E_LIB_MISMATCH)
            if published is None:
                raise BizError(E_STATE_INVALID, "no published library is available to migrate")
            manifest, old_hash = published
            snapshot = self.open_snapshot()
            try:
                source_records = [record for record in snapshot.records if record.version in (1, 2)]
                if not source_records:
                    raise BizError(E_STATE_INVALID, "library has no v1/v2 records to migrate")
                report: MigrationReport = migrate_records(source_records, context, strict=True)
                by_source = {record.record_id: entry for record, entry in zip(snapshot.records, snapshot.entries)}
                target_entries: dict[str, dict[str, Any]] = {
                    entry["key"]: dict(entry)
                    for entry in manifest["entries"]
                    if entry["fingerprint_version"] == 3
                }
                for outcome in report.outcomes:
                    assert outcome.target_record is not None
                    target = outcome.target_record
                    data = target.to_bytes()
                    file_digest = sha256_bytes(data)
                    self._write_segment(file_digest, data)
                    source_entry = by_source[outcome.source_record_id]
                    target_entries[target.key] = {
                        "key": target.key,
                        "track_id": target.track_id,
                        "segment_index": target.segment_index,
                        "fingerprint_version": target.version,
                        "config_hash": content_hash(
                            {
                                "migrated_from": outcome.source_version,
                                "source_config_hash": source_entry["config_hash"],
                                "time_basis": target.time_basis.as_dict(),
                                "hash_algo": target.hash_algo,
                            }
                        ),
                        "segment_hash": file_digest,
                        "fingerprint_segment_hash": target.segment_hash,
                        "record_checksum": target.checksum,
                        "segment_file": self._segment_relative(file_digest),
                        "sample_rate": target.sample_rate,
                        "hop_length": target.hop_length,
                        "hash_algo": target.hash_algo,
                        "time_basis": target.time_basis.as_dict(),
                        "segment_start_ms": source_entry["segment_start_ms"],
                        "segment_end_ms": source_entry["segment_end_ms"],
                    }
                plan_core = {
                    "operation": "migrate",
                    "parent": old_hash,
                    "sources": [
                        {
                            "record_id": record.record_id,
                            "checksum": record.checksum,
                            "version": record.version,
                        }
                        for record in source_records
                    ],
                    "context": {
                        "channel_merge": context.channel_merge,
                        "compression": context.compression,
                    },
                }
                version_id = stable_version_id("migrate", old_hash, plan_core)
                plan_digest = content_hash(plan_core)
                new_manifest = self._seal_manifest(
                    self._manifest_core(
                        version_id=version_id,
                        operation="migrate",
                        parent_manifest_hash=old_hash,
                        plan_digest=plan_digest,
                        entries=target_entries.values(),
                        complete=True,
                    )
                )
                new_hash = self._write_manifest_snapshot(new_manifest)
            finally:
                snapshot.close()
            atomic_write_json(
                self.published_pointer,
                {
                    "schema": POINTER_SCHEMA,
                    "version_id": version_id,
                    "manifest_hash": new_hash,
                    "manifest_file": self._manifest_relative(new_hash),
                },
            )
            event = None
            if trace is not None:
                event = trace.emit(
                    "migration_publish",
                    version_id=version_id,
                    old_published_version_hash=old_hash,
                    published_version_hash=new_hash,
                    migrated_records=len(report.outcomes),
                    entry_count=new_manifest["entry_count"],
                )
            return {
                "status": "published",
                "version_id": version_id,
                "old_published_version_hash": old_hash,
                "published_version_hash": new_hash,
                "migrated_records": len(report.outcomes),
                "report": report.as_dict(),
                "trace": event,
            }
        finally:
            self._release_operation(lock)
