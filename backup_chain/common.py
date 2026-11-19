from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import tempfile
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Mapping


TAIL_BYTES = 64 * 1024
SCHEMA_VERSION = "backup-chain/v1"

# Stable business error codes.  API clients may branch on these values.
BC_MANIFEST_INVALID = "BC_MANIFEST_INVALID"
BC_MANIFEST_DUPLICATE_BLOCK = "BC_MANIFEST_DUPLICATE_BLOCK"
BC_MANIFEST_DUPLICATE_SEQUENCE = "BC_MANIFEST_DUPLICATE_SEQUENCE"
BC_MANIFEST_PATH_INVALID = "BC_MANIFEST_PATH_INVALID"
BC_MANIFEST_DIRECTORY_MISMATCH = "BC_MANIFEST_DIRECTORY_MISMATCH"
BC_MANIFEST_UNDECLARED_COPY = "BC_MANIFEST_UNDECLARED_COPY"
BC_BLOCK_MISSING = "BC_BLOCK_MISSING"
BC_BLOCK_LENGTH_MISMATCH = "BC_BLOCK_LENGTH_MISMATCH"
BC_BLOCK_TAIL_MISMATCH = "BC_BLOCK_TAIL_MISMATCH"
BC_BLOCK_DIGEST_MISMATCH = "BC_BLOCK_DIGEST_MISMATCH"
BC_BLOCK_CONFLICT = "BC_BLOCK_CONFLICT"
BC_BLOCK_UNCONFIRMED = "BC_BLOCK_UNCONFIRMED"
BC_BLOCK_ORDINAL_MISMATCH = "BC_BLOCK_ORDINAL_MISMATCH"
BC_CHAIN_BASELINE_MISMATCH = "BC_CHAIN_BASELINE_MISMATCH"
BC_CHAIN_SEQUENCE_GAP = "BC_CHAIN_SEQUENCE_GAP"
BC_CHAIN_STATE_DIGEST_MISMATCH = "BC_CHAIN_STATE_DIGEST_MISMATCH"
BC_UNINDEXED_FILE = "BC_UNINDEXED_FILE"
BC_DRILL_BLOCK_MISSING = "BC_DRILL_BLOCK_MISSING"
BC_DRILL_BLOCK_READ_ERROR = "BC_DRILL_BLOCK_READ_ERROR"
BC_DRILL_BLOCK_DIGEST_MISMATCH = "BC_DRILL_BLOCK_DIGEST_MISMATCH"
BC_DRILL_STAGING_MISSING = "BC_DRILL_STAGING_MISSING"
BC_DRILL_OUTPUT_INCOMPLETE = "BC_DRILL_OUTPUT_INCOMPLETE"
BC_DRILL_INTERRUPTED = "BC_DRILL_INTERRUPTED"
BC_RESTORE_POINT_UNAVAILABLE = "BC_RESTORE_POINT_UNAVAILABLE"
BC_DATABASE_PATH_INSIDE_REPO = "BC_DATABASE_PATH_INSIDE_REPO"
BC_OUTPUT_PATH_INSIDE_REPO = "BC_OUTPUT_PATH_INSIDE_REPO"


class BusinessError(Exception):
    def __init__(self, code: str, message: str, **context: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.context = context

    def as_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "context": self.context,
            "message": self.message,
        }


class ManifestError(BusinessError):
    pass


class SimulatedInterrupt(Exception):
    """Test-only interruption raised after a progress commit."""


class ProcessLock:
    """Small cross-process advisory lock using only the standard library."""

    def __init__(self, path: Path, *, operation: str) -> None:
        self.path = path
        self.operation = operation
        self._fd: int | None = None

    def acquire(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        fd = os.open(self.path, os.O_RDWR | os.O_CREAT, 0o600)
        try:
            if os.fstat(fd).st_size == 0:
                os.write(fd, b"\0")
            os.lseek(fd, 0, os.SEEK_SET)
            if os.name == "nt":
                import msvcrt

                msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
            else:
                import fcntl

                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as exc:
            os.close(fd)
            raise BusinessError(
                "BC_OPERATION_IN_PROGRESS",
                f"another process is already running {self.operation}",
                lock_path=str(self.path),
                operation=self.operation,
            ) from exc
        self._fd = fd

    def release(self) -> None:
        if self._fd is None:
            return
        try:
            if os.name == "nt":
                import msvcrt

                os.lseek(self._fd, 0, os.SEEK_SET)
                msvcrt.locking(self._fd, msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(self._fd, fcntl.LOCK_UN)
        finally:
            os.close(self._fd)
            self._fd = None

    def __enter__(self) -> "ProcessLock":
        self.acquire()
        return self

    def __exit__(self, exc_type: object, exc: object, tb: object) -> None:
        self.release()


@dataclass(frozen=True)
class FileObservation:
    rel_path: str
    partition: str
    size: int
    sha256: str
    tail_sha256: str
    mtime_ns: int


@dataclass(frozen=True)
class ReplicaSpec:
    rel_path: str
    priority: int
    partition: str


@dataclass(frozen=True)
class BlockSpec:
    backup_id: str
    sequence: int
    kind: str
    block_id: str
    ordinal: int
    length: int
    sha256: str
    tail_sha256: str | None
    replicas: tuple[ReplicaSpec, ...]
    extra: Mapping[str, Any]


@dataclass(frozen=True)
class BackupSpec:
    backup_id: str
    sequence: int
    kind: str
    blocks: tuple[BlockSpec, ...]
    baseline_digest: str | None
    expected_state_digest: str | None
    extra: Mapping[str, Any]


@dataclass(frozen=True)
class Manifest:
    chain_id: str
    schema_version: str
    full: BackupSpec
    increments: tuple[BackupSpec, ...]
    raw_sha256: str
    path: str
    block_file_suffix: str
    injection_log: str


def utc_now() -> str:
    return (
        datetime.now(timezone.utc)
        .isoformat(timespec="milliseconds")
        .replace("+00:00", "Z")
    )


def canonical_json(value: Any) -> str:
    return json.dumps(
        value, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    )


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def normalize_digest(value: Any) -> str | None:
    if not isinstance(value, str):
        return None
    digest = value.strip().lower()
    if digest.startswith("sha256:"):
        digest = digest[7:]
    if re.fullmatch(r"[0-9a-f]{64}", digest):
        return digest
    return None


def stable_state_digest(blocks: Iterable[Mapping[str, Any]]) -> str:
    """Digest the logical state, independent of physical replica order.

    The payload is the sorted list of [block_id, ordinal, length, sha256].
    Consequently two restore paths with the same logical blocks have the same
    state digest even if their physical partitions differ.
    """

    normalized = []
    for block in blocks:
        block_id = str(block["block_id"])
        ordinal = int(block["ordinal"])
        length = int(block["length"])
        digest = normalize_digest(block.get("sha256"))
        if digest is None:
            raise ValueError(f"invalid sha256 for block {block_id}")
        normalized.append([block_id, ordinal, length, digest])
    normalized.sort(key=lambda item: (item[1], item[0]))
    return sha256_bytes(canonical_json(normalized).encode("utf-8"))


def hash_file(path: Path, tail_bytes: int = TAIL_BYTES) -> FileObservation:
    full = hashlib.sha256()
    tail = bytearray()
    size = 0
    before = path.stat()
    with path.open("rb") as handle:
        while True:
            data = handle.read(1024 * 1024)
            if not data:
                break
            full.update(data)
            size += len(data)
            if len(data) >= tail_bytes:
                tail[:] = data[-tail_bytes:]
            else:
                tail.extend(data)
                if len(tail) > tail_bytes:
                    del tail[: len(tail) - tail_bytes]
    after = path.stat()
    if size != before.st_size or getattr(after, "st_mtime_ns", 0) != before.st_mtime_ns:
        raise BusinessError(
            BC_DRILL_BLOCK_READ_ERROR,
            "file changed while it was being hashed",
            path=str(path),
        )
    return FileObservation(
        rel_path="",
        partition="",
        size=size,
        sha256=full.hexdigest(),
        tail_sha256=hashlib.sha256(bytes(tail)).hexdigest(),
        mtime_ns=before.st_mtime_ns,
    )


def atomic_write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent)
    )
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
            handle.write(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise


def atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent)
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise


def path_is_inside(child: Path, parent: Path) -> bool:
    try:
        child.resolve().relative_to(parent.resolve())
        return True
    except ValueError:
        return False


def validate_separate_paths(repo: Path, db_path: Path, output_dir: Path) -> None:
    if path_is_inside(db_path, repo):
        raise BusinessError(
            BC_DATABASE_PATH_INSIDE_REPO,
            "the SQLite state file must be outside the read-only backup repository",
            database_path=str(db_path),
            repository=str(repo),
        )
    if path_is_inside(output_dir, repo):
        raise BusinessError(
            BC_OUTPUT_PATH_INSIDE_REPO,
            "restore output must be outside the read-only backup repository",
            output_dir=str(output_dir),
            repository=str(repo),
        )


def _require_dict(value: Any, label: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        raise ManifestError(BC_MANIFEST_INVALID, f"{label} must be an object")
    return value


def _require_int(value: Any, label: str, *, minimum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise ManifestError(BC_MANIFEST_INVALID, f"{label} must be an integer")
    if minimum is not None and value < minimum:
        raise ManifestError(
            BC_MANIFEST_INVALID, f"{label} must be at least {minimum}"
        )
    return value


def _first_present(mapping: Mapping[str, Any], keys: Iterable[str]) -> Any:
    for key in keys:
        if key in mapping:
            return mapping[key]
    return None


def _normalize_rel_path(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ManifestError(BC_MANIFEST_PATH_INVALID, f"{label} is empty")
    rel = value.replace("\\", "/").strip()
    while rel.startswith("./"):
        rel = rel[2:]
    pieces = Path(rel).parts
    if rel.startswith("/") or ".." in pieces:
        raise ManifestError(
            BC_MANIFEST_PATH_INVALID,
            "manifest replica path must be repository-relative",
            path=value,
        )
    return Path(*pieces).as_posix()


def _parse_replica(
    value: Any, *, backup_id: str, block_id: str, index: int
) -> ReplicaSpec:
    if isinstance(value, str):
        mapping: Mapping[str, Any] = {"path": value}
    else:
        mapping = _require_dict(
            value, f"replica for {backup_id}/{block_id} at index {index}"
        )
    raw_path = _first_present(mapping, ("path", "relative_path", "file", "location"))
    rel_path = _normalize_rel_path(
        raw_path, f"replica path for {backup_id}/{block_id}"
    )
    raw_priority = _first_present(mapping, ("priority", "copy_priority", "rank"))
    priority = int(raw_priority) if raw_priority is not None else index + 1
    raw_partition = mapping.get("partition")
    partition = (
        str(raw_partition)
        if raw_partition is not None and str(raw_partition).strip()
        else rel_path.split("/", 1)[0]
    )
    return ReplicaSpec(rel_path=rel_path, priority=priority, partition=partition)


def _parse_block(
    value: Any, *, backup_id: str, sequence: int, kind: str, index: int
) -> BlockSpec:
    mapping = _require_dict(value, f"block {index} of {backup_id}")
    raw_block_id = _first_present(mapping, ("block_id", "id", "logical_block_id"))
    if raw_block_id is None:
        raise ManifestError(
            BC_MANIFEST_INVALID,
            "block is missing block_id",
            backup_id=backup_id,
        )
    block_id = str(raw_block_id)
    if not block_id or "\x00" in block_id or "/" in block_id or "\\" in block_id:
        raise ManifestError(
            BC_MANIFEST_INVALID,
            "block_id is not a safe file identifier",
            backup_id=backup_id,
            block_id=block_id,
        )
    ordinal = _require_int(
        _first_present(mapping, ("ordinal", "chunk_ordinal", "block_ordinal")),
        f"ordinal of {backup_id}/{block_id}",
        minimum=1,
    )
    length = _require_int(
        _first_present(mapping, ("length", "size", "length_bytes")),
        f"length of {backup_id}/{block_id}",
        minimum=0,
    )
    digest = normalize_digest(
        _first_present(mapping, ("sha256", "digest", "content_sha256"))
    )
    if digest is None:
        raise ManifestError(
            BC_MANIFEST_INVALID,
            "block has no valid sha256 content digest",
            backup_id=backup_id,
            block_id=block_id,
        )
    tail_digest = normalize_digest(
        _first_present(mapping, ("tail_sha256", "tail_digest", "tail_content_sha256"))
    )
    raw_replicas = _first_present(mapping, ("replicas", "copies"))
    if not isinstance(raw_replicas, list) or not raw_replicas:
        raise ManifestError(
            BC_MANIFEST_INVALID,
            "block must declare at least one replica",
            backup_id=backup_id,
            block_id=block_id,
        )
    replicas = tuple(
        _parse_replica(replica, backup_id=backup_id, block_id=block_id, index=i)
        for i, replica in enumerate(raw_replicas)
    )
    paths = [replica.rel_path for replica in replicas]
    if len(paths) != len(set(paths)):
        raise ManifestError(
            BC_MANIFEST_INVALID,
            "the same physical path is declared twice for one logical block",
            backup_id=backup_id,
            block_id=block_id,
        )
    return BlockSpec(
        backup_id=backup_id,
        sequence=sequence,
        kind=kind,
        block_id=block_id,
        ordinal=ordinal,
        length=length,
        sha256=digest,
        tail_sha256=tail_digest,
        replicas=replicas,
        extra=mapping,
    )


def _parse_backup(
    value: Any, *, sequence: int, kind: str, default_id: str
) -> BackupSpec:
    mapping = _require_dict(value, kind)
    raw_id = _first_present(mapping, ("backup_id", "id", "snapshot_id")) or default_id
    backup_id = str(raw_id)
    if not backup_id or "\x00" in backup_id or "/" in backup_id or "\\" in backup_id:
        raise ManifestError(BC_MANIFEST_INVALID, f"invalid {kind} backup_id")
    raw_blocks = mapping.get("blocks")
    if not isinstance(raw_blocks, list) or not raw_blocks:
        raise ManifestError(
            BC_MANIFEST_INVALID,
            f"{kind} must contain a non-empty blocks list",
            backup_id=backup_id,
        )
    blocks: list[BlockSpec] = []
    seen: set[str] = set()
    for index, item in enumerate(raw_blocks):
        block = _parse_block(
            item, backup_id=backup_id, sequence=sequence, kind=kind, index=index
        )
        if block.block_id in seen:
            raise ManifestError(
                BC_MANIFEST_DUPLICATE_BLOCK,
                "logical block is declared more than once in one backup",
                backup_id=backup_id,
                block_id=block.block_id,
            )
        seen.add(block.block_id)
        blocks.append(block)
    baseline_digest = normalize_digest(
        _first_present(
            mapping, ("baseline_digest", "base_digest", "baseline_sha256")
        )
    )
    expected_state_digest = normalize_digest(
        _first_present(
            mapping,
            ("state_digest", "result_state_digest", "expected_state_digest"),
        )
    )
    return BackupSpec(
        backup_id=backup_id,
        sequence=sequence,
        kind=kind,
        blocks=tuple(blocks),
        baseline_digest=baseline_digest,
        expected_state_digest=expected_state_digest,
        extra=mapping,
    )


def load_manifest(path: Path) -> Manifest:
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise ManifestError(
            BC_MANIFEST_INVALID, f"cannot read manifest: {exc}", path=str(path)
        ) from exc
    try:
        data = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ManifestError(
            BC_MANIFEST_INVALID, f"manifest is not valid UTF-8 JSON: {exc}"
        ) from exc
    mapping = _require_dict(data, "manifest")
    chain_id = str(mapping.get("chain_id") or "").strip()
    if not chain_id:
        raise ManifestError(BC_MANIFEST_INVALID, "manifest has no chain_id")
    full_data = mapping.get("full") or mapping.get("baseline_full")
    if full_data is None:
        raise ManifestError(BC_MANIFEST_INVALID, "manifest has no full baseline")
    full = _parse_backup(full_data, sequence=0, kind="full", default_id="full-000")
    raw_increments = mapping.get("increments")
    if not isinstance(raw_increments, list) or not raw_increments:
        raise ManifestError(
            BC_MANIFEST_INVALID, "manifest must contain increments"
        )
    increments: list[BackupSpec] = []
    seen_sequences: set[int] = set()
    seen_ids: set[str] = {full.backup_id}
    for index, item in enumerate(raw_increments):
        item_map = _require_dict(item, f"increment {index}")
        raw_sequence = _first_present(item_map, ("sequence", "increment", "day"))
        sequence = _require_int(raw_sequence, f"sequence of increment {index}", minimum=1)
        if sequence in seen_sequences:
            raise ManifestError(
                BC_MANIFEST_DUPLICATE_SEQUENCE,
                "increment sequence is declared more than once",
                sequence=sequence,
            )
        seen_sequences.add(sequence)
        backup = _parse_backup(
            item_map,
            sequence=sequence,
            kind="increment",
            default_id=f"increment-{sequence:03d}",
        )
        if backup.backup_id in seen_ids:
            raise ManifestError(
                BC_MANIFEST_INVALID,
                "backup_id is not unique in the chain",
                backup_id=backup.backup_id,
            )
        seen_ids.add(backup.backup_id)
        increments.append(backup)
    increments.sort(key=lambda item: item.sequence)
    suffix = str(mapping.get("block_file_suffix") or ".chunk")
    if not suffix.startswith(".") or "/" in suffix or "\\" in suffix:
        raise ManifestError(BC_MANIFEST_INVALID, "block_file_suffix is invalid")
    injection_log = str(mapping.get("injection_log") or "injections.json")
    injection_log = _normalize_rel_path(injection_log, "injection_log")
    return Manifest(
        chain_id=chain_id,
        schema_version=str(mapping.get("schema_version") or SCHEMA_VERSION),
        full=full,
        increments=tuple(increments),
        raw_sha256=sha256_bytes(raw),
        path=str(path),
        block_file_suffix=suffix,
        injection_log=injection_log,
    )


def load_injection_log(repo: Path, manifest: Manifest) -> dict[str, Any] | None:
    path = repo / manifest.injection_log
    if not path.exists():
        return None
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        return {
            "error": {
                "code": BC_MANIFEST_INVALID,
                "message": "injection log is not valid JSON",
                "path": manifest.injection_log,
            }
        }
    if not isinstance(value, dict):
        return {
            "error": {
                "code": BC_MANIFEST_INVALID,
                "message": "injection log must be an object",
                "path": manifest.injection_log,
            }
        }
    return value


def disk_space_ok(path: Path, required_bytes: int) -> tuple[bool, int]:
    usage = shutil.disk_usage(path)
    return usage.free >= required_bytes, int(usage.free)


def safe_slug(value: str, limit: int = 48) -> str:
    slug = re.sub(r"[^A-Za-z0-9._-]+", "-", value).strip("-._")
    return (slug or "chain")[:limit]


def remove_tree(path: Path) -> None:
    if path.exists():
        shutil.rmtree(path)
