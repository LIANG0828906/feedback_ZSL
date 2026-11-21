from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from .errors import recovery_invalid
from .record import ProbeRecord
from .util import atomic_write_json, sha256_json

SNAPSHOT_FORMAT = "local-lightprobe-chunked-v1"
REQUIRED_META_FIELDS = {
    "format",
    "run_id",
    "scene_id",
    "target_version",
    "grid_hash",
    "factor_versions",
    "config",
    "strategy",
    "chunk_size",
    "probe_count",
    "chain_seed",
}


@dataclass(frozen=True)
class RecoveredSnapshot:
    directory: Path
    metadata: dict[str, Any]
    records: dict[str, ProbeRecord]
    chain_head: str
    chunk_count: int


class SnapshotWriter:
    """Write immutable probe chunks using fsync + atomic rename + hash chain."""

    def __init__(
        self,
        directory: str | Path,
        metadata: dict[str, Any],
        *,
        checkpoint: Callable[[int, int], None] | None = None,
    ) -> None:
        self.directory = Path(directory)
        self.metadata = dict(metadata)
        self.metadata.setdefault("format", SNAPSHOT_FORMAT)
        self.metadata.setdefault(
            "chain_seed",
            sha256_json(
                {
                    "run_id": self.metadata.get("run_id"),
                    "scene_id": self.metadata.get("scene_id"),
                    "target_version": self.metadata.get("target_version"),
                }
            ),
        )
        self._chain_head = str(self.metadata["chain_seed"])
        self._chunk_index = 0
        self.checkpoint = checkpoint
        self.directory.mkdir(parents=True, exist_ok=True)
        atomic_write_json(self.directory / "meta.json", self.metadata)

    @property
    def chain_head(self) -> str:
        return self._chain_head

    def commit_chunk(self, records: list[ProbeRecord]) -> str:
        if not records:
            raise ValueError("cannot commit an empty snapshot chunk")
        payload: dict[str, Any] = {
            "format": SNAPSHOT_FORMAT,
            "chunk_index": self._chunk_index,
            "prev_hash": self._chain_head,
            "records": [record.to_dict() for record in records],
        }
        chunk_hash = sha256_json(payload)
        payload["chunk_hash"] = chunk_hash
        path = self.directory / f"chunk_{self._chunk_index:06d}.json"
        atomic_write_json(path, payload)
        self._chain_head = chunk_hash
        self._chunk_index += 1
        if self.checkpoint is not None:
            self.checkpoint(self._chunk_index, len(records))
        return chunk_hash

    def commit_all(self, records: list[ProbeRecord]) -> dict[str, Any]:
        chunk_size = int(self.metadata["chunk_size"])
        for start in range(0, len(records), chunk_size):
            self.commit_chunk(records[start : start + chunk_size])
        return {
            "directory": str(self.directory),
            "chunks": self._chunk_index,
            "chain_head": self._chain_head,
        }


def load_snapshot(directory: str | Path) -> RecoveredSnapshot:
    path = Path(directory)
    metadata_path = path / "meta.json"
    try:
        metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise recovery_invalid(str(metadata_path), "snapshot metadata is missing") from exc
    except (json.JSONDecodeError, OSError) as exc:
        raise recovery_invalid(str(metadata_path), f"snapshot metadata is unreadable: {exc}") from exc
    if not isinstance(metadata, dict):
        raise recovery_invalid(str(metadata_path), "snapshot metadata must be an object")
    missing = sorted(REQUIRED_META_FIELDS - set(metadata))
    if missing:
        raise recovery_invalid(
            str(metadata_path), f"snapshot metadata is missing fields: {', '.join(missing)}"
        )
    if metadata["format"] != SNAPSHOT_FORMAT:
        raise recovery_invalid(str(metadata_path), "unsupported snapshot format")
    if (
        isinstance(metadata["target_version"], bool)
        or not isinstance(metadata["target_version"], int)
        or metadata["target_version"] < 1
    ):
        raise recovery_invalid(str(metadata_path), "invalid snapshot target_version")
    if (
        isinstance(metadata["probe_count"], bool)
        or not isinstance(metadata["probe_count"], int)
        or metadata["probe_count"] < 0
    ):
        raise recovery_invalid(str(metadata_path), "invalid snapshot probe_count")
    if (
        isinstance(metadata["chunk_size"], bool)
        or not isinstance(metadata["chunk_size"], int)
        or metadata["chunk_size"] <= 0
    ):
        raise recovery_invalid(str(metadata_path), "invalid snapshot chunk_size")
    if not isinstance(metadata["factor_versions"], dict):
        raise recovery_invalid(str(metadata_path), "factor_versions must be an object")
    if not isinstance(metadata["config"], dict):
        raise recovery_invalid(str(metadata_path), "config must be an object")

    chunk_paths = sorted(path.glob("chunk_*.json"))
    if not chunk_paths:
        return RecoveredSnapshot(
            directory=path,
            metadata=metadata,
            records={},
            chain_head=str(metadata["chain_seed"]),
            chunk_count=0,
        )

    expected_chain = str(metadata["chain_seed"])
    records: dict[str, ProbeRecord] = {}
    expected_index = 0
    for chunk_path in chunk_paths:
        try:
            payload = json.loads(chunk_path.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError) as exc:
            raise recovery_invalid(str(chunk_path), f"chunk is unreadable: {exc}") from exc
        if not isinstance(payload, dict):
            raise recovery_invalid(str(chunk_path), "chunk must be an object")
        for field in ("format", "chunk_index", "prev_hash", "records", "chunk_hash"):
            if field not in payload:
                raise recovery_invalid(str(chunk_path), f"chunk is missing field '{field}'")
        if payload["format"] != SNAPSHOT_FORMAT:
            raise recovery_invalid(str(chunk_path), "chunk format mismatch")
        if int(payload["chunk_index"]) != expected_index:
            raise recovery_invalid(
                str(chunk_path),
                f"broken chunk sequence: expected {expected_index}, got {payload['chunk_index']}",
            )
        if payload["prev_hash"] != expected_chain:
            raise recovery_invalid(str(chunk_path), "hash chain predecessor mismatch")
        unsigned = dict(payload)
        actual_hash = str(unsigned.pop("chunk_hash"))
        if sha256_json(unsigned) != actual_hash:
            raise recovery_invalid(str(chunk_path), "chunk hash mismatch")
        if not isinstance(payload["records"], list):
            raise recovery_invalid(str(chunk_path), "chunk records must be an array")
        for raw_record in payload["records"]:
            if not isinstance(raw_record, dict):
                raise recovery_invalid(str(chunk_path), "probe record must be an object")
            try:
                record = ProbeRecord.from_dict(raw_record)
            except (KeyError, TypeError, ValueError) as exc:
                raise recovery_invalid(
                    str(chunk_path), f"probe record is malformed or missing fields: {exc}"
                ) from exc
            if record.probe_id in records:
                raise recovery_invalid(
                    str(chunk_path), f"duplicate probe id in snapshot: {record.probe_id}"
                )
            if not record.verify_content_hash():
                raise recovery_invalid(
                    str(chunk_path),
                    f"probe record '{record.probe_id}' failed version/content hash validation",
                )
            records[record.probe_id] = record
        if len(payload["records"]) > int(metadata["chunk_size"]):
            raise recovery_invalid(
                str(chunk_path), "chunk contains more records than the declared chunk_size"
            )
        if len(records) > int(metadata["probe_count"]):
            raise recovery_invalid(
                str(chunk_path), "snapshot contains more probe records than probe_count"
            )
        expected_chain = actual_hash
        expected_index += 1

    return RecoveredSnapshot(
        directory=path,
        metadata=metadata,
        records=records,
        chain_head=expected_chain,
        chunk_count=len(chunk_paths),
    )


def recovery_error(path: Path, message: str) -> ProbeError:
    return recovery_invalid(str(path), message)

