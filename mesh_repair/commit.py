from __future__ import annotations

import fcntl
import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from threading import RLock
from typing import Any, Iterable

from .binary import atomic_write_bytes
from .errors import (
    BusinessError,
    E_BUSY,
    E_RECOVERY_INVALID,
    SUB_BLOCK_TABLE_MISMATCH,
    SUB_CORRUPT_COMMIT,
)


SESSION_FORMAT = "mesh-repair-session"
BLOCK_FORMAT = "mesh-repair-block"
SESSION_VERSION = 1
BLOCK_TABLE_VERSION = 1
HASH_ALGORITHM = "sha256"
GENESIS_PREFIX = b"mesh-repair-genesis:"


def canonical_json(value: Any, *, sort_keys: bool = True) -> bytes:
    return json.dumps(
        value,
        sort_keys=sort_keys,
        separators=(",", ":"),
        allow_nan=False,
        ensure_ascii=False,
    ).encode("utf-8")


def sha256_json(value: Any) -> str:
    return hashlib.sha256(canonical_json(value)).hexdigest()


def derive_session_id(
    source_sha256: str, config_hash: str, block_table_hash: str
) -> str:
    digest = hashlib.sha256(
        f"{source_sha256}:{config_hash}:{block_table_hash}".encode("utf-8")
    ).hexdigest()
    return f"{source_sha256[:12]}-{digest[:12]}"


@dataclass(frozen=True)
class Manifest:
    source_sha256: str
    config_hash: str
    block_table_version: int
    block_table_hash: str
    block_ids: tuple[str, ...]
    block_table: tuple[dict[str, Any], ...]
    session_id: str
    manifest_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "format": SESSION_FORMAT,
            "version": SESSION_VERSION,
            "hash_algorithm": HASH_ALGORITHM,
            "source_sha256": self.source_sha256,
            "config_hash": self.config_hash,
            "block_table_version": self.block_table_version,
            "block_table_hash": self.block_table_hash,
            "block_ids": list(self.block_ids),
            "block_table": list(self.block_table),
            "session_id": self.session_id,
        }


def make_block_table_hash(block_table: Iterable[dict[str, Any]]) -> str:
    return sha256_json(
        {
            "block_table_version": BLOCK_TABLE_VERSION,
            "blocks": list(block_table),
        }
    )


class SessionLock:
    """Non-blocking cross-process lock scoped to the source mesh."""

    def __init__(self, path: str | os.PathLike[str]) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._stream = None

    def acquire(self) -> None:
        stream = self.path.open("a+")
        try:
            fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            stream.close()
            raise BusinessError(
                E_BUSY,
                "the same source mesh is already being repaired",
                context={"lock_path": str(self.path)},
            ) from exc
        stream.seek(0)
        stream.truncate()
        stream.write(f"{os.getpid()}\n")
        stream.flush()
        os.fsync(stream.fileno())
        self._stream = stream

    def release(self) -> None:
        if self._stream is None:
            return
        try:
            fcntl.flock(self._stream.fileno(), fcntl.LOCK_UN)
        finally:
            self._stream.close()
            self._stream = None

    def __enter__(self) -> "SessionLock":
        self.acquire()
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.release()


class CommitStore:
    """Manifest plus immutable, atomically renamed block records."""

    def __init__(
        self,
        session_dir: str | os.PathLike[str],
        *,
        source_sha256: str,
        config_hash: str,
        block_table: list[dict[str, Any]],
    ) -> None:
        self.session_dir = Path(session_dir)
        self.blocks_dir = self.session_dir / "blocks"
        self.manifest_path = self.session_dir / "manifest.json"
        self.blocks_dir.mkdir(parents=True, exist_ok=True)
        self.source_sha256 = source_sha256
        self.config_hash = config_hash
        self.block_table = [dict(item) for item in block_table]
        self.block_ids = tuple(str(item["block_id"]) for item in self.block_table)
        if len(set(self.block_ids)) != len(self.block_ids):
            raise ValueError("block table contains duplicate ids")
        self.block_table_hash = make_block_table_hash(self.block_table)
        self.session_id = derive_session_id(
            source_sha256, config_hash, self.block_table_hash
        )
        self._lock = RLock()
        self.manifest: Manifest | None = None
        self._sequence = 0
        self._last_hash = ""
        self._committed: dict[str, dict[str, Any]] = {}

    def initialize(self) -> Manifest:
        with self._lock:
            if self.manifest_path.exists():
                manifest = self._load_manifest()
                self._validate_manifest(manifest)
                self.manifest = manifest
                self.load_committed()
                return manifest
            if any(self.blocks_dir.glob("block-*.json")):
                raise BusinessError(
                    E_RECOVERY_INVALID,
                    "committed block records exist without a session manifest",
                    subcode=SUB_CORRUPT_COMMIT,
                    location={"session_dir": str(self.session_dir)},
                )
            manifest = Manifest(
                source_sha256=self.source_sha256,
                config_hash=self.config_hash,
                block_table_version=BLOCK_TABLE_VERSION,
                block_table_hash=self.block_table_hash,
                block_ids=self.block_ids,
                block_table=tuple(self.block_table),
                session_id=self.session_id,
                manifest_hash="",
            )
            payload = manifest.to_dict()
            manifest_hash = sha256_json(payload)
            stored = dict(payload)
            stored["manifest_hash"] = manifest_hash
            atomic_write_bytes(self.manifest_path, canonical_json(stored) + b"\n")
            manifest = Manifest(
                source_sha256=self.source_sha256,
                config_hash=self.config_hash,
                block_table_version=BLOCK_TABLE_VERSION,
                block_table_hash=self.block_table_hash,
                block_ids=self.block_ids,
                block_table=tuple(self.block_table),
                session_id=self.session_id,
                manifest_hash=manifest_hash,
            )
            self.manifest = manifest
            self._sequence = 0
            self._last_hash = manifest_hash
            self.trace_free_write_json(
                self.session_dir / "block-table.json",
                {
                    "block_table_version": BLOCK_TABLE_VERSION,
                    "block_table_hash": self.block_table_hash,
                    "blocks": self.block_table,
                },
            )
            return manifest

    def _load_manifest(self) -> Manifest:
        try:
            stored = json.loads(self.manifest_path.read_text(encoding="utf-8"))
        except Exception as exc:
            raise BusinessError(
                E_RECOVERY_INVALID,
                "session manifest is unreadable",
                subcode=SUB_CORRUPT_COMMIT,
                location={"path": str(self.manifest_path)},
            ) from exc
        try:
            stored_hash = str(stored.pop("manifest_hash"))
            expected_hash = sha256_json(stored)
            if stored_hash != expected_hash:
                raise ValueError("manifest content hash mismatch")
            if stored.get("format") != SESSION_FORMAT or stored.get("version") != SESSION_VERSION:
                raise ValueError("unsupported session manifest format")
            return Manifest(
                source_sha256=str(stored["source_sha256"]),
                config_hash=str(stored["config_hash"]),
                block_table_version=int(stored["block_table_version"]),
                block_table_hash=str(stored["block_table_hash"]),
                block_ids=tuple(str(item) for item in stored["block_ids"]),
                block_table=tuple(dict(item) for item in stored["block_table"]),
                session_id=str(stored["session_id"]),
                manifest_hash=stored_hash,
            )
        except BusinessError:
            raise
        except Exception as exc:
            raise BusinessError(
                E_RECOVERY_INVALID,
                "session manifest failed hash or schema validation",
                subcode=SUB_CORRUPT_COMMIT,
                location={"path": str(self.manifest_path)},
            ) from exc

    def _validate_manifest(self, manifest: Manifest) -> None:
        if manifest.block_table_version != BLOCK_TABLE_VERSION:
            raise BusinessError(
                E_RECOVERY_INVALID,
                "block table version does not match this binary",
                subcode=SUB_BLOCK_TABLE_MISMATCH,
                context={
                    "expected": BLOCK_TABLE_VERSION,
                    "actual": manifest.block_table_version,
                },
            )
        if manifest.source_sha256 != self.source_sha256:
            raise BusinessError(
                E_RECOVERY_INVALID,
                "source mesh hash does not match the recovery session",
                subcode=SUB_CORRUPT_COMMIT,
                context={
                    "expected": self.source_sha256,
                    "actual": manifest.source_sha256,
                },
            )
        if manifest.config_hash != self.config_hash:
            raise BusinessError(
                E_RECOVERY_INVALID,
                "repair configuration does not match the recovery session",
                subcode=SUB_CORRUPT_COMMIT,
            )
        if manifest.block_table_hash != self.block_table_hash:
            raise BusinessError(
                E_RECOVERY_INVALID,
                "block table hash does not match the recovery session",
                subcode=SUB_BLOCK_TABLE_MISMATCH,
                context={
                    "expected": self.block_table_hash,
                    "actual": manifest.block_table_hash,
                },
            )
        if manifest.block_ids != self.block_ids:
            raise BusinessError(
                E_RECOVERY_INVALID,
                "block ids do not match the recovery session",
                subcode=SUB_BLOCK_TABLE_MISMATCH,
            )

    def _block_path(self, block_id: str) -> Path:
        safe = block_id.replace("/", "_").replace("\\", "_")
        return self.blocks_dir / f"{safe}.json"

    def _record_hash(
        self,
        *,
        sequence: int,
        block_id: str,
        prev_hash: str,
        payload: Any,
    ) -> str:
        assert self.manifest is not None
        return sha256_json(
            {
                "sequence": sequence,
                "block_id": block_id,
                "prev_hash": prev_hash,
                "manifest_hash": self.manifest.manifest_hash,
                "payload": payload,
            }
        )

    def commit_block(self, block_id: str, payload: Any) -> dict[str, Any]:
        if self.manifest is None:
            raise RuntimeError("CommitStore.initialize must be called first")
        if block_id not in self.block_ids:
            raise ValueError(f"unknown block id {block_id}")
        path = self._block_path(block_id)
        with self._lock:
            if path.exists():
                existing = self._load_block(path)
                if sha256_json(existing["payload"]) != sha256_json(payload):
                    raise BusinessError(
                        E_RECOVERY_INVALID,
                        "a committed block cannot be overwritten with different content",
                        subcode=SUB_CORRUPT_COMMIT,
                        location={"block_id": block_id},
                    )
                return existing
            sequence = self._sequence
            prev_hash = self._last_hash or self.manifest.manifest_hash
            content_hash = self._record_hash(
                sequence=sequence,
                block_id=block_id,
                prev_hash=prev_hash,
                payload=payload,
            )
            record = {
                "format": BLOCK_FORMAT,
                "version": SESSION_VERSION,
                "hash_algorithm": HASH_ALGORITHM,
                "manifest_hash": self.manifest.manifest_hash,
                "source_sha256": self.source_sha256,
                "config_hash": self.config_hash,
                "block_table_version": BLOCK_TABLE_VERSION,
                "block_table_hash": self.block_table_hash,
                "sequence": sequence,
                "block_id": block_id,
                "prev_hash": prev_hash,
                "payload_sha256": sha256_json(payload),
                "content_hash": content_hash,
                "payload": payload,
            }
            atomic_write_bytes(path, canonical_json(record) + b"\n")
            self._sequence += 1
            self._last_hash = content_hash
            self._committed[block_id] = record
            return record

    def _load_block(self, path: Path) -> dict[str, Any]:
        if self.manifest is None:
            raise RuntimeError("manifest is not initialized")
        try:
            record = json.loads(path.read_text(encoding="utf-8"))
            if record.get("format") != BLOCK_FORMAT or record.get("version") != SESSION_VERSION:
                raise ValueError("unsupported block record format")
            if record.get("manifest_hash") != self.manifest.manifest_hash:
                raise ValueError("block belongs to a different manifest")
            if record.get("source_sha256") != self.source_sha256:
                raise ValueError("block source hash mismatch")
            if record.get("config_hash") != self.config_hash:
                raise ValueError("block config hash mismatch")
            if record.get("block_table_version") != BLOCK_TABLE_VERSION:
                raise ValueError("block table version mismatch")
            if record.get("block_table_hash") != self.block_table_hash:
                raise ValueError("block table hash mismatch")
            payload = record["payload"]
            if record.get("payload_sha256") != sha256_json(payload):
                raise ValueError("block payload hash mismatch")
            expected = self._record_hash(
                sequence=int(record["sequence"]),
                block_id=str(record["block_id"]),
                prev_hash=str(record["prev_hash"]),
                payload=payload,
            )
            if record.get("content_hash") != expected:
                raise ValueError("block content hash mismatch")
            return record
        except BusinessError:
            raise
        except Exception as exc:
            raise BusinessError(
                E_RECOVERY_INVALID,
                "committed block failed hash or schema validation",
                subcode=SUB_CORRUPT_COMMIT,
                location={"path": str(path)},
            ) from exc

    def load_committed(self) -> dict[str, dict[str, Any]]:
        if self.manifest is None:
            raise RuntimeError("CommitStore.initialize must be called first")
        paths = sorted(self.blocks_dir.glob("block-*.json"))
        records: list[dict[str, Any]] = []
        with self._lock:
            for path in paths:
                record = self._load_block(path)
                block_id = str(record["block_id"])
                if block_id not in self.block_ids:
                    raise BusinessError(
                        E_RECOVERY_INVALID,
                        "committed block is not present in the block table",
                        subcode=SUB_BLOCK_TABLE_MISMATCH,
                        location={"block_id": block_id, "path": str(path)},
                    )
                records.append(record)
            records.sort(key=lambda item: int(item["sequence"]))
            expected_prev = self.manifest.manifest_hash
            for expected_sequence, record in enumerate(records):
                if int(record["sequence"]) != expected_sequence:
                    raise BusinessError(
                        E_RECOVERY_INVALID,
                        "block commit sequence has a gap or duplicate",
                        subcode=SUB_CORRUPT_COMMIT,
                        context={
                            "expected_sequence": expected_sequence,
                            "actual_sequence": int(record["sequence"]),
                        },
                    )
                if record["prev_hash"] != expected_prev:
                    raise BusinessError(
                        E_RECOVERY_INVALID,
                        "block hash chain is broken",
                        subcode=SUB_CORRUPT_COMMIT,
                        context={
                            "expected_prev_hash": expected_prev,
                            "actual_prev_hash": record["prev_hash"],
                            "block_id": record["block_id"],
                        },
                    )
                expected_prev = str(record["content_hash"])
            self._committed = {str(record["block_id"]): record for record in records}
            self._sequence = len(records)
            self._last_hash = expected_prev
            return dict(self._committed)

    @property
    def committed(self) -> dict[str, dict[str, Any]]:
        with self._lock:
            return dict(self._committed)

    @staticmethod
    def trace_free_write_json(path: Path, payload: Any) -> None:
        atomic_write_bytes(path, canonical_json(payload) + b"\n")
