"""Durable shard-session storage with atomic metadata and manifest publication."""

from __future__ import annotations

import contextlib
import hashlib
import os
import re
import shutil
import tempfile
import threading
from pathlib import Path
from typing import Any, Iterator

from .errors import (
    E_BUDGET_EXCEEDED,
    E_INVALID_ARGUMENT,
    E_INVALID_SHARD,
    E_METADATA_CORRUPT,
    E_SESSION_ABORTED,
    E_SESSION_COMMITTED,
    E_SESSION_CONFLICT,
    E_SESSION_NOT_FOUND,
    E_SHARD_DIGEST_MISMATCH,
    E_SHARD_GAP,
    GatewayError,
)
from .util import (
    atomic_write_bytes,
    atomic_write_json,
    canonical_json_bytes,
    file_size,
    fsync_dir,
    load_json,
    ranges_from_numbers,
    safe_manifest_name,
    sha256_bytes,
    sha256_file,
    validate_session_id,
    validate_sha256,
)

try:
    import fcntl
except ImportError:  # pragma: no cover - the task targets Linux/macOS.
    fcntl = None  # type: ignore[assignment]


FAULT_EXIT_CODE = 86
FAULT_PHASES = {"shard_landed", "metadata_updated", "manifest_published"}
_FORMAT = "shard-gateway-session-v1"
_MANIFEST_FORMAT = "shard-gateway-manifest-v1"
_TMP_META_RE = re.compile(r"^\.meta\.json\.tmp-")


class UploadGateway:
    """Create/write/commit/abort shard upload sessions under a local root."""

    def __init__(self, root: str | Path) -> None:
        self.root = Path(root).resolve()
        self.sessions_dir = self.root / "sessions"
        self.manifests_dir = self.root / "manifests"
        self.sessions_dir.mkdir(parents=True, exist_ok=True)
        self.manifests_dir.mkdir(parents=True, exist_ok=True)
        self._lock_guard = threading.Lock()
        self._thread_locks: dict[str, threading.RLock] = {}
        self._object_locks: dict[str, threading.RLock] = {}

    def session_dir(self, session_id: str) -> Path:
        return self.sessions_dir / validate_session_id(session_id)

    def _thread_lock(self, key: str) -> threading.RLock:
        with self._lock_guard:
            lock = self._thread_locks.get(key)
            if lock is None:
                lock = threading.RLock()
                self._thread_locks[key] = lock
            return lock

    @contextlib.contextmanager
    def _file_lock(self, lock_path: Path) -> Iterator[None]:
        lock_path.parent.mkdir(parents=True, exist_ok=True)
        fd = os.open(lock_path, os.O_RDWR | os.O_CREAT, 0o644)
        try:
            if fcntl is not None:
                fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            if fcntl is not None:
                fcntl.flock(fd, fcntl.LOCK_UN)
            os.close(fd)

    @contextlib.contextmanager
    def session_lock(self, session_id: str) -> Iterator[None]:
        directory = self.session_dir(session_id)
        lock = self._thread_lock(f"session:{session_id}")
        with lock:
            with self._file_lock(directory / ".lock"):
                yield

    @contextlib.contextmanager
    def object_lock(self, object_key: str) -> Iterator[None]:
        name = hashlib.sha256(object_key.encode("utf-8")).hexdigest()
        lock = self._thread_lock(f"object:{name}")
        with lock:
            with self._file_lock(self.manifests_dir / f".{name}.lock"):
                yield

    def _meta_path(self, session_id: str) -> Path:
        return self.session_dir(session_id) / "meta.json"

    def _read_meta_unlocked(self, session_id: str) -> dict[str, Any]:
        path = self._meta_path(session_id)
        try:
            value = load_json(path)
        except FileNotFoundError as exc:
            raise GatewayError(E_SESSION_NOT_FOUND, f"session {session_id} not found") from exc
        except (OSError, ValueError) as exc:
            raise GatewayError(
                E_METADATA_CORRUPT, f"session {session_id} metadata cannot be read"
            ) from exc
        if not isinstance(value, dict) or value.get("format") != _FORMAT:
            raise GatewayError(E_METADATA_CORRUPT, "unsupported session metadata format")
        return value

    def get_session(self, session_id: str) -> dict[str, Any]:
        with self.session_lock(session_id):
            meta = self._read_meta_unlocked(session_id)
            return self._public_session(meta)

    def _public_session(self, meta: dict[str, Any]) -> dict[str, Any]:
        return {
            "session_id": meta["session_id"],
            "object_key": meta["object_key"],
            "shard_size": meta["shard_size"],
            "total_shards": meta["total_shards"],
            "version": meta["version"],
            "status": meta.get("status", "active"),
            "shards": meta.get("shards", {}),
            "object_version": meta.get("object_version"),
            "manifest": meta.get("manifest"),
        }

    def _write_meta_unlocked(self, meta: dict[str, Any], *, fault_phase: str | None = None) -> None:
        atomic_write_json(self._meta_path(meta["session_id"]), meta)
        if fault_phase == "metadata_updated":
            self._inject_fault(fault_phase)

    @staticmethod
    def _inject_fault(phase: str | None) -> None:
        if phase is None:
            return
        if phase not in FAULT_PHASES:
            raise GatewayError(E_INVALID_ARGUMENT, f"unknown fault phase {phase}")
        os._exit(FAULT_EXIT_CODE)

    def create_session(
        self,
        *,
        session_id: str,
        object_key: str,
        shard_size: int,
        total_shards: int,
        expected_size: int | None = None,
        expected_sha256: str | None = None,
        session_quota: int | None = None,
    ) -> dict[str, Any]:
        validate_session_id(session_id)
        if not isinstance(object_key, str) or not object_key:
            raise GatewayError(E_INVALID_ARGUMENT, "object_key must be non-empty")
        if not isinstance(shard_size, int) or shard_size <= 0:
            raise GatewayError(E_INVALID_ARGUMENT, "shard_size must be positive")
        if not isinstance(total_shards, int) or total_shards <= 0:
            raise GatewayError(E_INVALID_ARGUMENT, "total_shards must be positive")
        if expected_size is not None and (
            not isinstance(expected_size, int) or expected_size < 0
        ):
            raise GatewayError(E_INVALID_ARGUMENT, "expected_size must be non-negative")
        if session_quota is not None and (
            not isinstance(session_quota, int) or session_quota <= 0
        ):
            raise GatewayError(E_INVALID_ARGUMENT, "session_quota must be positive")
        expected_sha256 = validate_sha256(expected_sha256)

        last_shard_size = None
        if expected_size is not None:
            expected_last = expected_size - (total_shards - 1) * shard_size
            if expected_last < 1 or expected_last > shard_size:
                raise GatewayError(
                    E_INVALID_ARGUMENT,
                    "expected_size is incompatible with shard_size and total_shards",
                )
            last_shard_size = expected_last
            if session_quota is not None and expected_size > session_quota:
                raise GatewayError(
                    E_BUDGET_EXCEEDED,
                    "object exceeds the configured session quota",
                    expected_size=expected_size,
                    session_quota=session_quota,
                )

        directory = self.session_dir(session_id)
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "shards").mkdir(exist_ok=True)
        (directory / "tmp").mkdir(exist_ok=True)
        with self.session_lock(session_id):
            meta_path = self._meta_path(session_id)
            if meta_path.exists():
                meta = self._read_meta_unlocked(session_id)
                signature = (
                    meta.get("object_key"),
                    meta.get("shard_size"),
                    meta.get("total_shards"),
                    meta.get("expected_size"),
                    meta.get("expected_sha256"),
                    meta.get("session_quota"),
                )
                requested = (
                    object_key,
                    shard_size,
                    total_shards,
                    expected_size,
                    expected_sha256,
                    session_quota,
                )
                if signature != requested:
                    raise GatewayError(
                        E_SESSION_CONFLICT,
                        "existing session configuration differs from create request",
                    )
                return self._public_session(meta)

            meta = {
                "format": _FORMAT,
                "session_id": session_id,
                "object_key": object_key,
                "shard_size": shard_size,
                "total_shards": total_shards,
                "last_shard_size": last_shard_size,
                "expected_size": expected_size,
                "expected_sha256": expected_sha256,
                "session_quota": session_quota,
                "version": 1,
                "status": "active",
                "shards": {},
                "manifest": None,
                "object_version": None,
            }
            self._write_meta_unlocked(meta)
            return self._public_session(meta)

    def _validate_shard_shape(
        self, meta: dict[str, Any], shard_number: int, length: int
    ) -> None:
        total = meta["total_shards"]
        shard_size = meta["shard_size"]
        if shard_number < 1 or shard_number > total:
            raise GatewayError(
                E_INVALID_SHARD,
                f"shard number must be in 1..{total}",
                shard_number=shard_number,
            )
        if shard_number < total:
            if length != shard_size:
                raise GatewayError(
                    E_INVALID_SHARD,
                    f"shard {shard_number} must contain exactly {shard_size} bytes",
                    shard_number=shard_number,
                    length=length,
                )
            return
        expected_last = meta.get("last_shard_size")
        if expected_last is not None:
            if length != expected_last:
                raise GatewayError(
                    E_INVALID_SHARD,
                    f"last shard must contain exactly {expected_last} bytes",
                    shard_number=shard_number,
                    length=length,
                )
        elif not 1 <= length <= shard_size:
            raise GatewayError(
                E_INVALID_SHARD,
                f"last shard must contain between 1 and {shard_size} bytes",
                shard_number=shard_number,
                length=length,
            )

    def _usage_unlocked(self, meta: dict[str, Any]) -> int:
        directory = self.session_dir(meta["session_id"])
        total = 0
        for record in meta.get("shards", {}).values():
            relative = record.get("file")
            if isinstance(relative, str):
                total += file_size(directory / relative)
        return total

    def write_shard(
        self,
        *,
        session_id: str,
        shard_number: int,
        data: bytes,
        expected_sha256: str | None = None,
        expected_length: int | None = None,
        fault_phase: str | None = None,
    ) -> dict[str, Any]:
        if not isinstance(data, bytes):
            raise GatewayError(E_INVALID_ARGUMENT, "shard data must be bytes")
        if not isinstance(shard_number, int):
            raise GatewayError(E_INVALID_ARGUMENT, "shard_number must be an integer")
        expected_sha256 = validate_sha256(expected_sha256)
        if expected_length is not None and (
            not isinstance(expected_length, int) or expected_length < 0
        ):
            raise GatewayError(E_INVALID_ARGUMENT, "expected_length must be non-negative")
        if fault_phase is not None and fault_phase not in FAULT_PHASES:
            raise GatewayError(E_INVALID_ARGUMENT, f"unknown fault phase {fault_phase}")

        directory = self.session_dir(session_id)
        with self.session_lock(session_id):
            meta = self._read_meta_unlocked(session_id)
            if (directory / "ABORTED").exists() or meta.get("status") == "aborted":
                raise GatewayError(E_SESSION_ABORTED, "session has been aborted")
            if meta.get("status") == "committed":
                raise GatewayError(E_SESSION_COMMITTED, "session has already been committed")
            self._validate_shard_shape(meta, shard_number, len(data))

        digest = sha256_bytes(data)
        length = len(data)
        if expected_sha256 is not None and digest != expected_sha256:
            raise GatewayError(
                E_SHARD_DIGEST_MISMATCH,
                f"shard {shard_number} digest does not match the request",
                shard_number=shard_number,
                expected=expected_sha256,
                actual=digest,
            )
        if expected_length is not None and length != expected_length:
            raise GatewayError(
                E_SHARD_DIGEST_MISMATCH,
                f"shard {shard_number} length does not match the request",
                shard_number=shard_number,
                expected=expected_length,
                actual=length,
            )

        tmp_dir = directory / "tmp"
        tmp_dir.mkdir(exist_ok=True)
        fd, staged_name = tempfile.mkstemp(
            prefix=f"shard-{shard_number:06d}-", suffix=".part", dir=tmp_dir
        )
        staged = Path(staged_name)
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(data)
                stream.flush()
                os.fsync(stream.fileno())

            with self.session_lock(session_id):
                meta = self._read_meta_unlocked(session_id)
                if (directory / "ABORTED").exists() or meta.get("status") == "aborted":
                    raise GatewayError(E_SESSION_ABORTED, "session has been aborted")
                if meta.get("status") == "committed":
                    raise GatewayError(
                        E_SESSION_COMMITTED, "session has already been committed"
                    )
                self._validate_shard_shape(meta, shard_number, length)
                if meta.get("session_quota") is not None:
                    old = meta.get("shards", {}).get(str(shard_number), {})
                    old_length = int(old.get("length", 0))
                    usage = self._usage_unlocked(meta)
                    if usage - old_length + length > meta["session_quota"]:
                        raise GatewayError(
                            E_BUDGET_EXCEEDED,
                            "session quota would be exceeded",
                            session_quota=meta["session_quota"],
                            current_bytes=usage,
                            incoming_bytes=length,
                        )

                new_version = int(meta["version"]) + 1
                relative = f"shards/shard-{shard_number:06d}.v{new_version:06d}.bin"
                final_path = directory / relative
                shards_dir = directory / "shards"
                os.replace(staged, final_path)
                fsync_dir(shards_dir)
                if fault_phase == "shard_landed":
                    self._inject_fault(fault_phase)

                old_record = meta.get("shards", {}).get(str(shard_number))
                old_relative = old_record.get("file") if isinstance(old_record, dict) else None
                meta.setdefault("shards", {})[str(shard_number)] = {
                    "number": shard_number,
                    "digest": digest,
                    "length": length,
                    "file": relative,
                    "version": new_version,
                }
                meta["version"] = new_version
                self._write_meta_unlocked(
                    meta,
                    fault_phase=(
                        "metadata_updated" if fault_phase == "metadata_updated" else None
                    ),
                )

            if isinstance(old_relative, str) and old_relative != relative:
                try:
                    (directory / old_relative).unlink()
                except FileNotFoundError:
                    pass
            return {
                "session_id": session_id,
                "shard_number": shard_number,
                "digest": digest,
                "length": length,
                "version": new_version,
                "replaced": old_record is not None,
            }
        finally:
            try:
                staged.unlink()
            except FileNotFoundError:
                pass

    def abort_session(self, session_id: str) -> dict[str, Any]:
        directory = self.session_dir(session_id)
        with self.session_lock(session_id):
            meta = self._read_meta_unlocked(session_id)
            if meta.get("status") == "committed":
                raise GatewayError(
                    E_SESSION_COMMITTED, "a committed session cannot be aborted"
                )
            marker = directory / "ABORTED"
            if not marker.exists():
                atomic_write_bytes(marker, b"aborted\n")
            if meta.get("status") != "aborted":
                meta["status"] = "aborted"
                meta["version"] = int(meta["version"]) + 1
                self._write_meta_unlocked(meta)
            return self._public_session(meta)

    def _verify_all_shards_unlocked(
        self, meta: dict[str, Any]
    ) -> tuple[dict[int, Path], str, int]:
        total = int(meta["total_shards"])
        shards = meta.get("shards", {})
        missing = [number for number in range(1, total + 1) if str(number) not in shards]
        if missing:
            raise GatewayError(
                E_SHARD_GAP,
                "one or more shard numbers are missing",
                missing_ranges=ranges_from_numbers(
                    [n for n in range(1, total + 1) if str(n) in shards], total
                ),
            )

        directory = self.session_dir(meta["session_id"])
        paths: dict[int, Path] = {}
        object_hasher = hashlib.sha256()
        total_length = 0
        for number in range(1, total + 1):
            record = shards[str(number)]
            relative = record.get("file")
            if not isinstance(relative, str):
                raise GatewayError(
                    E_SHARD_DIGEST_MISMATCH,
                    f"shard {number} has no file reference",
                    shard_number=number,
                )
            path = directory / relative
            if not path.is_file():
                raise GatewayError(
                    E_SHARD_DIGEST_MISMATCH,
                    f"shard {number} file is missing",
                    shard_number=number,
                )
            actual_digest, actual_length = sha256_file(path)
            if actual_length != record.get("length") or actual_digest != record.get("digest"):
                raise GatewayError(
                    E_SHARD_DIGEST_MISMATCH,
                    f"shard {number} does not match session metadata",
                    shard_number=number,
                    expected_digest=record.get("digest"),
                    actual_digest=actual_digest,
                    expected_length=record.get("length"),
                    actual_length=actual_length,
                )
            self._validate_shard_shape(meta, number, actual_length)
            paths[number] = path
            total_length += actual_length
            with path.open("rb") as stream:
                while True:
                    block = stream.read(1024 * 1024)
                    if not block:
                        break
                    object_hasher.update(block)

        expected_size = meta.get("expected_size")
        expected_digest = meta.get("expected_sha256")
        if expected_size is not None and total_length != expected_size:
            raise GatewayError(
                E_SHARD_DIGEST_MISMATCH,
                "assembled object length does not match the session expectation",
                expected_length=expected_size,
                actual_length=total_length,
            )
        if expected_digest is not None and object_hasher.hexdigest() != expected_digest:
            raise GatewayError(
                E_SHARD_DIGEST_MISMATCH,
                "assembled object digest does not match the session expectation",
                expected=expected_digest,
                actual=object_hasher.hexdigest(),
            )
        return paths, object_hasher.hexdigest(), total_length

    def _manifest_path(self, object_key: str) -> Path:
        return self.manifests_dir / safe_manifest_name(object_key)

    def get_manifest_for_key(self, object_key: str) -> dict[str, Any] | None:
        path = self._manifest_path(object_key)
        try:
            value = load_json(path)
        except FileNotFoundError:
            return None
        if not isinstance(value, dict) or value.get("format") != _MANIFEST_FORMAT:
            raise GatewayError(E_METADATA_CORRUPT, "manifest has an unsupported format")
        return value

    def _next_object_version(self, object_key: str) -> int:
        existing = self.get_manifest_for_key(object_key)
        if existing is None:
            return 1
        version = existing.get("object_version")
        if not isinstance(version, int) or version < 0:
            raise GatewayError(E_METADATA_CORRUPT, "existing manifest has no valid version")
        return version + 1

    def _build_manifest(
        self,
        meta: dict[str, Any],
        object_version: int,
        object_digest: str,
        total_length: int,
    ) -> dict[str, Any]:
        shards = []
        for number in range(1, int(meta["total_shards"]) + 1):
            record = meta["shards"][str(number)]
            shards.append(
                {
                    "number": number,
                    "sha256": record["digest"],
                    "length": record["length"],
                }
            )
        return {
            "format": _MANIFEST_FORMAT,
            "object_key": meta["object_key"],
            "object_version": object_version,
            "session_id": meta["session_id"],
            "session_version": meta["version"],
            "shard_size": meta["shard_size"],
            "total_shards": meta["total_shards"],
            "total_size": total_length,
            "object_sha256": object_digest,
            "shards": shards,
        }

    def commit_session(
        self, session_id: str, *, fault_phase: str | None = None
    ) -> dict[str, Any]:
        if fault_phase is not None and fault_phase not in FAULT_PHASES:
            raise GatewayError(E_INVALID_ARGUMENT, f"unknown fault phase {fault_phase}")
        directory = self.session_dir(session_id)
        with self.session_lock(session_id):
            meta = self._read_meta_unlocked(session_id)
            if (directory / "ABORTED").exists() or meta.get("status") == "aborted":
                raise GatewayError(E_SESSION_ABORTED, "session has been aborted")
            if meta.get("status") == "committed":
                manifest = self.get_manifest_for_key(meta["object_key"])
                if manifest is None:
                    raise GatewayError(
                        E_METADATA_CORRUPT,
                        "session is marked committed but its manifest is missing",
                    )
                return manifest

            _, object_digest, total_length = self._verify_all_shards_unlocked(meta)
            with self.object_lock(meta["object_key"]):
                object_version = self._next_object_version(meta["object_key"])
                manifest = self._build_manifest(
                    meta, object_version, object_digest, total_length
                )
                manifest_bytes = canonical_json_bytes(manifest)

                fd, tmp_name = tempfile.mkstemp(
                    prefix=".manifest-", suffix=".json.part", dir=self.manifests_dir
                )
                tmp_path = Path(tmp_name)
                try:
                    with os.fdopen(fd, "wb") as stream:
                        stream.write(manifest_bytes)
                        stream.flush()
                        os.fsync(stream.fileno())

                    staged_manifest = load_json(tmp_path)
                    if staged_manifest != manifest:
                        raise GatewayError(
                            E_METADATA_CORRUPT,
                            "temporary manifest failed validation",
                        )
                    if sha256_bytes(canonical_json_bytes(staged_manifest)) != sha256_bytes(
                        manifest_bytes
                    ):
                        raise GatewayError(
                            E_METADATA_CORRUPT,
                            "temporary manifest serialization is not stable",
                        )

                    final_path = self._manifest_path(meta["object_key"])
                    os.replace(tmp_path, final_path)
                    fsync_dir(self.manifests_dir)
                    if fault_phase == "manifest_published":
                        self._inject_fault(fault_phase)
                finally:
                    try:
                        tmp_path.unlink()
                    except FileNotFoundError:
                        pass

                meta["status"] = "committed"
                meta["manifest"] = final_path.name
                meta["object_version"] = object_version
                meta["version"] = int(meta["version"]) + 1
                meta["committed_manifest_sha256"] = sha256_bytes(manifest_bytes)
                self._write_meta_unlocked(meta)
            return manifest

    def cleanup_unreferenced(self, session_id: str) -> dict[str, int]:
        """Remove temporary and orphan shard files; keep referenced shards."""

        directory = self.session_dir(session_id)
        removed_files = 0
        removed_bytes = 0
        with self.session_lock(session_id):
            meta = self._read_meta_unlocked(session_id)
            referenced = {
                record.get("file")
                for record in meta.get("shards", {}).values()
                if isinstance(record, dict)
            }
            for path in (directory / "tmp").glob("*"):
                if path.is_file():
                    removed_bytes += file_size(path)
                    path.unlink(missing_ok=True)
                    removed_files += 1
            for path in (directory / "shards").glob("*"):
                if path.is_file() and f"shards/{path.name}" not in referenced:
                    removed_bytes += file_size(path)
                    path.unlink(missing_ok=True)
                    removed_files += 1
            for path in directory.glob(".meta.json.tmp-*"):
                if path.is_file():
                    removed_bytes += file_size(path)
                    path.unlink(missing_ok=True)
                    removed_files += 1
        return {"removed_files": removed_files, "removed_bytes": removed_bytes}

    def purge_committed_session_files(self, session_id: str) -> dict[str, int]:
        """Remove all shard data for a verified committed session."""

        directory = self.session_dir(session_id)
        with self.session_lock(session_id):
            meta = self._read_meta_unlocked(session_id)
            if meta.get("status") != "committed":
                raise GatewayError(
                    E_INVALID_ARGUMENT, "only committed sessions may be purged"
                )
            manifest = self.get_manifest_for_key(meta["object_key"])
            if manifest is None or manifest.get("session_id") != session_id:
                raise GatewayError(
                    E_METADATA_CORRUPT, "committed session manifest is not available"
                )
            removed_files = 0
            removed_bytes = 0
            for subdir in ("shards", "tmp"):
                for path in (directory / subdir).glob("*"):
                    if path.is_file():
                        removed_bytes += file_size(path)
                        path.unlink()
                        removed_files += 1
            meta["shards_cleaned"] = True
            meta["version"] = int(meta["version"]) + 1
            self._write_meta_unlocked(meta)
            return {"removed_files": removed_files, "removed_bytes": removed_bytes}

    def remove_session_directory(self, session_id: str) -> None:
        """Test/demo helper: remove a session after explicit administrative action."""

        directory = self.session_dir(session_id)
        if directory.exists():
            shutil.rmtree(directory)
