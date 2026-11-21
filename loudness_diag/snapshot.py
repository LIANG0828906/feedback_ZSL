"""Atomic, hash-chained observation snapshots and crash recovery."""

from __future__ import annotations

import copy
import json
import re
import threading
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import E_RECOVERY_INVALID, DiagnosticError
from .jsonutil import (
    atomic_write_json,
    canonical_bytes,
    load_json,
    sha256_bytes,
    sha256_json,
)

_SCHEMA = 1
_BLOCK_FILE = re.compile(r"^(\d{8})\.json$")
_REQUIRED_BLOCK_FIELDS = {
    "block_index",
    "block_id",
    "window_start",
    "window_end",
    "sync_marker",
    "content_hash",
    "ungated_energy",
    "short_term_loudness",
    "true_peak",
}


def _fail(
    message: str, first_broken_block: int | None, **details: Any
) -> DiagnosticError:
    return DiagnosticError(
        E_RECOVERY_INVALID,
        message,
        first_broken_block=first_broken_block,
        details=details or None,
    )


def _record_hash(record: dict[str, Any]) -> str:
    unsigned = copy.deepcopy(record)
    unsigned.pop("record_hash", None)
    return sha256_bytes(canonical_bytes(unsigned))


def _safe_component(value: str) -> str:
    sanitized = re.sub(r"[^A-Za-z0-9_.-]+", "_", value)
    return sanitized[:48] or "asset"


@dataclass(frozen=True)
class RecoveryState:
    directory: Path
    committed_count: int
    head_hash: str
    block_records: tuple[dict[str, Any], ...]
    gate_result: dict[str, Any] | None
    adopted_orphan_blocks: tuple[int, ...]
    total_blocks: int | None


class SnapshotStore:
    """A per-material, per-parameter hash chain persisted as JSON files."""

    def __init__(
        self,
        root: str | Path,
        *,
        asset_id: str,
        parameters: dict[str, Any],
        source_content_hash: str,
        parameter_version: str = "v1",
        total_blocks: int | None = None,
        create: bool = True,
    ) -> None:
        if not isinstance(parameters, dict):
            raise TypeError("snapshot parameters must be an object")
        if total_blocks is not None and total_blocks < 0:
            raise ValueError("total_blocks must be non-negative")
        self.root = Path(root)
        self.asset_id = asset_id
        self.parameters = copy.deepcopy(parameters)
        self.parameter_version = parameter_version
        self.source_content_hash = source_content_hash
        self.total_blocks = total_blocks
        self.params_hash = sha256_json(
            {
                "parameter_version": parameter_version,
                "parameters": self.parameters,
                "source_content_hash": source_content_hash,
            }
        )
        component = (
            f"{_safe_component(asset_id)}-"
            f"{sha256_bytes(asset_id.encode('utf-8'))[:8]}-"
            f"{self.params_hash[:12]}"
        )
        self.directory = self.root / component
        self.blocks_directory = self.directory / "blocks"
        self.meta_path = self.directory / "meta.json"
        self.manifest_path = self.directory / "manifest.json"
        self.gate_path = self.directory / "gate.json"
        self.chain_genesis = sha256_json(
            {
                "schema": "loudness-diag/snapshot-chain/v1",
                "asset_id": asset_id,
                "parameter_version": parameter_version,
                "parameters": self.parameters,
                "source_content_hash": source_content_hash,
            }
        )
        self._lock = threading.RLock()
        self._records: list[dict[str, Any]] = []
        self._head_hash = self.chain_genesis
        self._gate_result: dict[str, Any] | None = None
        self._adopted_orphans: list[int] = []

        if create:
            self.directory.mkdir(parents=True, exist_ok=True)
            self.blocks_directory.mkdir(parents=True, exist_ok=True)
        self._load_or_initialize(create=create)

    @property
    def committed_count(self) -> int:
        return len(self._records)

    @property
    def head_hash(self) -> str:
        return self._head_hash

    @property
    def block_records(self) -> tuple[dict[str, Any], ...]:
        return tuple(copy.deepcopy(self._records))

    @property
    def gate_result(self) -> dict[str, Any] | None:
        return copy.deepcopy(self._gate_result)

    def state(self) -> RecoveryState:
        return RecoveryState(
            directory=self.directory,
            committed_count=self.committed_count,
            head_hash=self.head_hash,
            block_records=self.block_records,
            gate_result=self.gate_result,
            adopted_orphan_blocks=tuple(self._adopted_orphans),
            total_blocks=self.total_blocks,
        )

    def commit_block(self, block: dict[str, Any]) -> bool:
        with self._lock:
            if not isinstance(block, dict):
                raise TypeError("block record must be an object")
            missing = _REQUIRED_BLOCK_FIELDS.difference(block)
            index = block.get("block_index")
            if missing:
                raise _fail(
                    f"block record is missing fields: {sorted(missing)}",
                    index if isinstance(index, int) else self.committed_count,
                )
            if not isinstance(index, int) or index < 0:
                raise _fail(
                    "block_index must be a non-negative integer",
                    self.committed_count,
                )
            if self.total_blocks is not None and index >= self.total_blocks:
                raise _fail(
                    "block index exceeds the declared block count",
                    index,
                    total_blocks=self.total_blocks,
                )
            if index < self.committed_count:
                return False
            if self._gate_result is not None:
                raise _fail(
                    "cannot append a block after the gate result is committed",
                    index,
                )
            if index > self.committed_count:
                raise _fail(
                    "cannot commit a block out of sequence",
                    self.committed_count,
                    attempted_block=index,
                )
            expected_id = f"{index:08d}"
            if block.get("block_id") != expected_id:
                raise _fail(
                    "block_id does not match block_index",
                    index,
                    expected_block_id=expected_id,
                )
            target = self.blocks_directory / f"{index:08d}.json"
            if target.exists():
                raise _fail(
                    "refusing to overwrite an existing committed block",
                    index,
                )
            record = copy.deepcopy(block)
            record["prev_hash"] = self._head_hash
            record["record_hash"] = _record_hash(record)
            atomic_write_json(target, record)
            self._records.append(record)
            self._head_hash = record["record_hash"]
            self._write_manifest()
            return True

    def commit_gate_result(self, gate_result: dict[str, Any]) -> str:
        if not isinstance(gate_result, dict):
            raise TypeError("gate result must be an object")
        required = {
            "absolute_threshold_energy",
            "relative_threshold_energy",
            "integrated_loudness",
            "gated_block_ids",
            "removed_block_ids",
        }
        missing = required.difference(gate_result)
        if missing:
            raise _fail(
                f"gate result is missing fields: {sorted(missing)}",
                self.committed_count,
            )
        if (
            self.total_blocks is not None
            and self.committed_count != self.total_blocks
        ):
            raise _fail(
                "gate result requires all declared blocks to be committed",
                self.committed_count,
            )
        stored = copy.deepcopy(gate_result)
        stored["schema_version"] = _SCHEMA
        stored["committed_block_count"] = self.committed_count
        stored["head_hash"] = self._head_hash
        with self._lock:
            atomic_write_json(self.gate_path, stored)
            self._gate_result = stored
            self._write_manifest()
        return sha256_json(stored)

    def resume(
        self,
        *,
        total_blocks: int,
        compute_block: Callable[[int], dict[str, Any]],
        build_gate: Callable[[list[dict[str, Any]]], dict[str, Any]] | None = None,
        commit_gate: bool = True,
    ) -> tuple[list[dict[str, Any]], dict[str, Any] | None, list[int]]:
        if total_blocks < self.committed_count:
            raise _fail(
                "committed block count exceeds requested total",
                total_blocks,
            )
        if self.total_blocks is not None and self.total_blocks != total_blocks:
            raise _fail(
                "total block count disagrees with snapshot metadata",
                0,
            )
        computed: list[int] = []
        for index in range(self.committed_count, total_blocks):
            row = compute_block(index)
            if row.get("block_index") != index:
                raise _fail(
                    "computed block index does not match the requested index",
                    index,
                )
            if self.commit_block(row):
                computed.append(index)
        if build_gate is not None and self._gate_result is None:
            gate_result = build_gate(list(self.block_records))
            if commit_gate:
                self.commit_gate_result(gate_result)
        return list(self.block_records), self.gate_result, computed

    def _meta(self) -> dict[str, Any]:
        return {
            "schema_version": _SCHEMA,
            "asset_id": self.asset_id,
            "parameter_version": self.parameter_version,
            "parameters": self.parameters,
            "params_hash": self.params_hash,
            "source_content_hash": self.source_content_hash,
            "total_blocks": self.total_blocks,
            "chain_genesis": self.chain_genesis,
        }

    def _load_or_initialize(self, *, create: bool) -> None:
        if not self.meta_path.exists():
            if not create:
                raise _fail("snapshot metadata does not exist", 0)
            atomic_write_json(self.meta_path, self._meta())
            self._write_manifest()
            return
        try:
            meta = load_json(self.meta_path)
        except (OSError, json.JSONDecodeError) as exc:
            raise _fail("snapshot metadata is unreadable", 0) from exc
        expected = self._meta()
        for key in (
            "schema_version",
            "asset_id",
            "parameter_version",
            "params_hash",
            "source_content_hash",
            "chain_genesis",
        ):
            if meta.get(key) != expected[key]:
                raise _fail(
                    f"snapshot metadata field {key!r} does not match",
                    0,
                )
        if "parameters" not in meta or "total_blocks" not in meta:
            raise _fail("snapshot metadata is missing required fields", 0)
        if meta["parameters"] != self.parameters:
            raise _fail("snapshot metadata parameters do not match", 0)
        if meta["total_blocks"] != self.total_blocks:
            raise _fail("snapshot total_blocks does not match", 0)
        self._recover_from_disk()

    def _recover_from_disk(self) -> None:
        if not self.manifest_path.exists():
            raise _fail("snapshot manifest does not exist", 0)
        try:
            manifest = load_json(self.manifest_path)
        except (OSError, json.JSONDecodeError) as exc:
            raise _fail("snapshot manifest is unreadable", 0) from exc
        if not isinstance(manifest, dict):
            raise _fail("snapshot manifest is not an object", 0)
        claimed_manifest_hash = manifest.get("manifest_hash")
        unsigned_manifest = copy.deepcopy(manifest)
        unsigned_manifest.pop("manifest_hash", None)
        if claimed_manifest_hash != sha256_json(unsigned_manifest):
            raise _fail("snapshot manifest hash chain is broken", 0)
        required_manifest = {
            "schema_version",
            "asset_id",
            "params_hash",
            "committed_blocks",
            "head_hash",
            "records",
        }
        missing_manifest = required_manifest.difference(manifest)
        if missing_manifest:
            raise _fail(
                f"snapshot manifest is missing fields: {sorted(missing_manifest)}",
                0,
            )
        if (
            manifest["asset_id"] != self.asset_id
            or manifest["params_hash"] != self.params_hash
            or manifest["schema_version"] != _SCHEMA
        ):
            raise _fail("snapshot manifest identity does not match metadata", 0)
        manifest_records = manifest["records"]
        if not isinstance(manifest_records, list):
            raise _fail("snapshot manifest records are invalid", 0)
        manifest_by_index: dict[int, dict[str, Any]] = {}
        for entry in manifest_records:
            if not isinstance(entry, dict) or not isinstance(
                entry.get("block_index"), int
            ):
                raise _fail("snapshot manifest has a malformed record entry", 0)
            entry_index = entry["block_index"]
            if entry_index in manifest_by_index:
                raise _fail(
                    "snapshot manifest contains a duplicate block index",
                    entry_index,
                )
            manifest_by_index[entry_index] = entry
        committed_claim = manifest["committed_blocks"]
        if not isinstance(committed_claim, int) or committed_claim < 0:
            raise _fail("snapshot committed_blocks is invalid", 0)
        if sorted(manifest_by_index) != list(range(committed_claim)):
            first_bad = 0
            for candidate in range(committed_claim):
                if candidate not in manifest_by_index:
                    first_bad = candidate
                    break
            else:
                first_bad = committed_claim
            raise _fail(
                "snapshot manifest record range is inconsistent",
                first_bad,
            )

        block_files: dict[int, Path] = {}
        for path in self.blocks_directory.iterdir() if self.blocks_directory.exists() else []:
            if path.name.startswith(".") and path.name.endswith(".tmp"):
                continue
            match = _BLOCK_FILE.match(path.name)
            if not match:
                raise _fail(
                    f"unexpected file in snapshot blocks directory: {path.name}",
                    0,
                )
            block_files[int(match.group(1))] = path

        max_index = max(block_files, default=-1)
        records: list[dict[str, Any]] = []
        head = self.chain_genesis
        for index in range(max_index + 1):
            path = block_files.get(index)
            if path is None:
                raise _fail(
                    "snapshot block sequence has a gap",
                    index,
                )
            try:
                record = load_json(path)
            except (OSError, json.JSONDecodeError) as exc:
                raise _fail(
                    "snapshot block is unreadable",
                    index,
                ) from exc
            if not isinstance(record, dict):
                raise _fail("snapshot block is not an object", index)
            missing = _REQUIRED_BLOCK_FIELDS.difference(record)
            if missing:
                raise _fail(
                    f"snapshot block is missing fields: {sorted(missing)}",
                    index,
                )
            if record.get("block_index") != index:
                raise _fail("snapshot block index does not match its filename", index)
            if record.get("block_id") != f"{index:08d}":
                raise _fail("snapshot block_id does not match its index", index)
            claimed_record_hash = record.get("record_hash")
            if not isinstance(claimed_record_hash, str):
                raise _fail("snapshot block has no record_hash", index)
            if _record_hash(record) != claimed_record_hash:
                raise _fail("snapshot block record hash mismatch", index)
            if record.get("prev_hash") != head:
                raise _fail("snapshot block hash chain is broken", index)
            entry = manifest_by_index.get(index)
            if entry is not None and entry.get("record_hash") != claimed_record_hash:
                raise _fail(
                    "snapshot block hash disagrees with the manifest",
                    index,
                )
            head = claimed_record_hash
            records.append(record)

        if len(records) < committed_claim:
            raise _fail(
                "committed block file is missing",
                len(records),
            )
        for index in range(committed_claim):
            if index not in block_files:
                raise _fail("manifest references a missing block", index)
        if committed_claim > 0:
            claimed_head = manifest["head_hash"]
            claimed_record = records[committed_claim - 1]["record_hash"]
            if claimed_head != claimed_record:
                raise _fail(
                    "manifest head hash does not match committed blocks",
                    committed_claim - 1,
                )

        gate_result: dict[str, Any] | None = None
        claimed_gate_hash = manifest.get("gate_result_hash")
        if claimed_gate_hash is not None:
            if not self.gate_path.exists():
                raise _fail("snapshot gate result file is missing", len(records))
            try:
                gate_result = load_json(self.gate_path)
            except (OSError, json.JSONDecodeError) as exc:
                raise _fail("snapshot gate result is unreadable", len(records)) from exc
            if sha256_json(gate_result) != claimed_gate_hash:
                raise _fail("snapshot gate result hash mismatch", len(records))
            if gate_result.get("head_hash") != head:
                raise _fail(
                    "snapshot gate result belongs to a different block head",
                    len(records),
                )
            if gate_result.get("committed_block_count") != len(records):
                raise _fail(
                    "snapshot gate result block count is inconsistent",
                    len(records),
                )
        elif self.gate_path.exists():
            try:
                gate_result = load_json(self.gate_path)
            except (OSError, json.JSONDecodeError) as exc:
                raise _fail("snapshot gate result is unreadable", len(records)) from exc
            if gate_result.get("head_hash") != head:
                raise _fail(
                    "snapshot gate result belongs to a different block head",
                    len(records),
                )
            if gate_result.get("committed_block_count") != len(records):
                raise _fail(
                    "snapshot gate result block count is inconsistent",
                    len(records),
                )

        self._records = records
        self._head_hash = head
        self._gate_result = gate_result
        self._adopted_orphans = list(range(committed_claim, len(records)))
        if self._adopted_orphans or (
            manifest["committed_blocks"] != len(records)
            or manifest["head_hash"] != head
            or (
                gate_result is not None
                and manifest.get("gate_result_hash") is None
            )
        ):
            self._write_manifest()

    def _write_manifest(self) -> None:
        manifest: dict[str, Any] = {
            "schema_version": _SCHEMA,
            "asset_id": self.asset_id,
            "params_hash": self.params_hash,
            "parameter_version": self.parameter_version,
            "source_content_hash": self.source_content_hash,
            "total_blocks": self.total_blocks,
            "committed_blocks": len(self._records),
            "head_hash": self._head_hash,
            "records": [
                {
                    "block_index": record["block_index"],
                    "block_id": record["block_id"],
                    "record_hash": record["record_hash"],
                    "file": f"blocks/{record['block_index']:08d}.json",
                }
                for record in self._records
            ],
            "gate_result_hash": (
                sha256_json(self._gate_result)
                if self._gate_result is not None
                else None
            ),
        }
        manifest["manifest_hash"] = sha256_json(manifest)
        atomic_write_json(self.manifest_path, manifest)


def snapshot_parameter_version(
    *,
    block_samples: int,
    oversample: int,
    gate_parameters: dict[str, Any],
    scheduler: dict[str, Any] | None = None,
) -> dict[str, Any]:
    value = {
        "block_samples": block_samples,
        "oversample": oversample,
        "gate_parameters": gate_parameters,
    }
    if scheduler is not None:
        value["scheduler"] = scheduler
    return value

