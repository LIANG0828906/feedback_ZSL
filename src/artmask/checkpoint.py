"""Atomic, version-consistent block-stage checkpoints and recovery."""

from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
import shutil
import tempfile
import threading
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any, Iterator

import numpy as np

from .config import ALGORITHM_VERSION, CHECKPOINT_FORMAT_VERSION
from .errors import BusinessError
from .state import STAGE_ORDER, STAGES, Stage


@dataclass(frozen=True)
class CheckpointIdentity:
    sample_id: str
    image_hash: str
    parameter_version: str
    algorithm_version: str
    budget_version: str
    mode: str
    factor: int
    block_size: int
    block_count: int

    def canonical(self) -> dict[str, Any]:
        return asdict(self)

    def snapshot_key(self) -> str:
        raw = f"sample:{self.sample_id}".encode("utf-8")
        return hashlib.sha256(raw).hexdigest()[:20]

    def mismatch_fields(self, metadata: dict[str, Any]) -> list[str]:
        expected = self.canonical()
        return [
            field
            for field, value in expected.items()
            if metadata.get(field) != value
        ]


@dataclass
class RecoveryRecord:
    block_index: int
    stage: Stage
    payload: bytes
    payload_hash: str
    sequence: int


@dataclass
class RecoveryState:
    snapshot_dir: Path
    reused: bool
    invalidated: bool
    reason: str
    records: dict[tuple[int, Stage], RecoveryRecord]
    metadata: dict[str, Any]

    @property
    def record_count(self) -> int:
        return len(self.records)


class CheckpointStore:
    def __init__(
        self,
        root: str | Path,
        identity: CheckpointIdentity,
        *,
        block_buffer_mb: float,
    ) -> None:
        self.root = Path(root).resolve()
        self.root.mkdir(parents=True, exist_ok=True)
        self.identity = identity
        self.block_buffer_bytes = int(block_buffer_mb * 1024 * 1024)
        self.snapshot_dir = self.root / f"snapshot_{identity.snapshot_key()}"
        self._thread_lock = threading.RLock()
        self._lock_path = self.root / ".checkpoint-write.lock"
        self._next_sequence = 1

    @contextlib.contextmanager
    def _locked(self) -> Iterator[None]:
        with self._thread_lock:
            with self._lock_path.open("a+b") as lock_handle:
                fcntl.flock(lock_handle.fileno(), fcntl.LOCK_EX)
                try:
                    yield
                finally:
                    fcntl.flock(lock_handle.fileno(), fcntl.LOCK_UN)

    def _metadata_payload(self) -> dict[str, Any]:
        return {
            "checkpoint_format_version": CHECKPOINT_FORMAT_VERSION,
            "created_with_algorithm_version": ALGORITHM_VERSION,
            **self.identity.canonical(),
        }

    def open(self) -> RecoveryState:
        with self._locked():
            return self._open_locked()

    def _open_locked(self) -> RecoveryState:
        invalidated = False
        reason = ""
        if self.snapshot_dir.exists():
            metadata = self._read_json(self.snapshot_dir / "metadata.json")
            if metadata is None:
                self._wipe_locked()
                invalidated = True
                reason = "检查点元数据缺失或损坏，整体作废"
            else:
                mismatches = self.identity.mismatch_fields(metadata)
                format_mismatch = (
                    metadata.get("checkpoint_format_version")
                    != CHECKPOINT_FORMAT_VERSION
                )
                if mismatches or format_mismatch:
                    self._wipe_locked()
                    invalidated = True
                    reason = (
                        "版本字段不匹配，整体作废："
                        + ", ".join(mismatches + (["checkpoint_format_version"] if format_mismatch else []))
                    )
        if not self.snapshot_dir.exists():
            self.snapshot_dir.mkdir(parents=True, exist_ok=False)
            self._write_json_atomic(
                self.snapshot_dir / "metadata.json", self._metadata_payload()
            )
            self._write_json_atomic(
                self.snapshot_dir / "index.json",
                {"records": [], "count": 0},
            )
            return RecoveryState(
                snapshot_dir=self.snapshot_dir,
                reused=False,
                invalidated=invalidated,
                reason=reason,
                records={},
                metadata=self._metadata_payload(),
            )

        records, load_error = self._load_records_locked()
        if load_error is not None:
            self._wipe_locked()
            invalidated = True
            reason = load_error
            return RecoveryState(
                snapshot_dir=self.snapshot_dir,
                reused=False,
                invalidated=True,
                reason=reason,
                records={},
                metadata=self._metadata_payload(),
            )
        self._next_sequence = max((item.sequence for item in records.values()), default=0) + 1
        return RecoveryState(
            snapshot_dir=self.snapshot_dir,
            reused=not invalidated,
            invalidated=invalidated,
            reason=reason,
            records=records,
            metadata=self._read_json(self.snapshot_dir / "metadata.json") or {},
        )

    def invalidate_all(self, reason: str) -> RecoveryState:
        with self._locked():
            self._wipe_locked()
            return RecoveryState(
                snapshot_dir=self.snapshot_dir,
                reused=False,
                invalidated=True,
                reason=reason,
                records={},
                metadata=self._metadata_payload(),
            )

    def _wipe_locked(self) -> None:
        if self.snapshot_dir.exists():
            shutil.rmtree(self.snapshot_dir)
        self.snapshot_dir.mkdir(parents=True, exist_ok=True)
        self._write_json_atomic(
            self.snapshot_dir / "metadata.json", self._metadata_payload()
        )
        self._write_json_atomic(
            self.snapshot_dir / "index.json", {"records": [], "count": 0}
        )
        self._next_sequence = 1

    def _load_records_locked(self) -> tuple[dict[tuple[int, Stage], RecoveryRecord], str | None]:
        records: dict[tuple[int, Stage], RecoveryRecord] = {}
        files = sorted(self.snapshot_dir.glob("b*_*.npz"))
        for path in files:
            try:
                with np.load(path, allow_pickle=False) as data:
                    payload = np.asarray(data["payload"], dtype=np.uint8).tobytes()
                    payload_hash = str(data["payload_hash"].item())
                    block_index = int(data["block_index"].item())
                    stage = Stage(str(data["stage"].item()))
                    sequence = int(data["sequence"].item())
                    embedded = {
                        "checkpoint_format_version": int(
                            data["checkpoint_format_version"].item()
                        ),
                        "sample_id": str(data["sample_id"].item()),
                        "image_hash": str(data["image_hash"].item()),
                        "parameter_version": str(data["parameter_version"].item()),
                        "algorithm_version": str(data["algorithm_version"].item()),
                        "budget_version": str(data["budget_version"].item()),
                        "mode": str(data["mode"].item()),
                        "factor": int(data["factor"].item()),
                        "block_size": int(data["block_size"].item()),
                        "block_count": int(data["block_count"].item()),
                    }
            except (OSError, ValueError, KeyError, EOFError) as exc:
                return {}, f"检查点文件损坏，整体作废：{path.name}: {exc}"
            mismatches = self.identity.mismatch_fields(embedded)
            if (
                embedded["checkpoint_format_version"]
                != CHECKPOINT_FORMAT_VERSION
            ):
                mismatches.append("checkpoint_format_version")
            if mismatches:
                return (
                    {},
                    f"检查点内嵌版本不匹配，整体作废：{path.name}: {', '.join(mismatches)}",
                )
            actual_hash = hashlib.sha256(payload).hexdigest()
            if actual_hash != payload_hash:
                return {}, f"检查点载荷哈希不匹配，整体作废：{path.name}"
            if not 0 <= block_index < self.identity.block_count:
                return {}, f"检查点块编号越界，整体作废：{path.name}"
            key = (block_index, stage)
            if key in records:
                return {}, f"检查点块阶段重复，整体作废：{path.name}"
            records[key] = RecoveryRecord(
                block_index=block_index,
                stage=stage,
                payload=payload,
                payload_hash=payload_hash,
                sequence=sequence,
            )
        for block_index in range(self.identity.block_count):
            committed_stages = [
                stage for stage in STAGES if (block_index, stage) in records
            ]
            expected_prefix = list(STAGES[: len(committed_stages)])
            if committed_stages != expected_prefix:
                return (
                    {},
                    f"块 {block_index} 的阶段提交链不连续，整体作废",
                )
        return records, None

    def commit(
        self,
        block_index: int,
        stage: Stage,
        payload: bytes,
        *,
        worker_id: str | None = None,
    ) -> tuple[str, int]:
        if len(payload) > self.block_buffer_bytes:
            raise BusinessError(
                "E_PARA",
                "单块阶段输出超过缓冲上限",
                field="budget.block_buffer_mb",
                details={
                    "block_index": block_index,
                    "stage": stage.value,
                    "payload_bytes": len(payload),
                    "limit_bytes": self.block_buffer_bytes,
                },
            )
        payload_hash = hashlib.sha256(payload).hexdigest()
        with self._locked():
            sequence = self._next_sequence
            self._next_sequence += 1
            filename = f"b{block_index:05d}_{stage.value}.npz"
            target = self.snapshot_dir / filename
            fd, temp_name = tempfile.mkstemp(
                prefix=f".{filename}.", suffix=".tmp", dir=self.snapshot_dir
            )
            os.close(fd)
            temp_path = Path(temp_name)
            try:
                with temp_path.open("wb") as handle:
                    np.savez(
                        handle,
                        payload=np.frombuffer(payload, dtype=np.uint8),
                        payload_hash=np.asarray(payload_hash),
                        block_index=np.asarray(block_index, dtype=np.int64),
                        stage=np.asarray(stage.value),
                        sequence=np.asarray(sequence, dtype=np.int64),
                        worker_id=np.asarray(worker_id or ""),
                        checkpoint_format_version=np.asarray(
                            CHECKPOINT_FORMAT_VERSION, dtype=np.int64
                        ),
                        sample_id=np.asarray(self.identity.sample_id),
                        image_hash=np.asarray(self.identity.image_hash),
                        parameter_version=np.asarray(
                            self.identity.parameter_version
                        ),
                        algorithm_version=np.asarray(
                            self.identity.algorithm_version
                        ),
                        budget_version=np.asarray(self.identity.budget_version),
                        mode=np.asarray(self.identity.mode),
                        factor=np.asarray(self.identity.factor, dtype=np.int64),
                        block_size=np.asarray(
                            self.identity.block_size, dtype=np.int64
                        ),
                        block_count=np.asarray(
                            self.identity.block_count, dtype=np.int64
                        ),
                    )
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(temp_path, target)
                directory_fd = os.open(self.snapshot_dir, os.O_RDONLY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
            finally:
                if temp_path.exists():
                    temp_path.unlink(missing_ok=True)
            records, error = self._load_records_locked()
            if error is not None:
                self._wipe_locked()
                raise BusinessError(
                    "E_STATE",
                    "检查点写入后校验失败，快照已整体作废",
                    details={"reason": error},
                )
            index = {
                "records": [
                    {
                        "block_index": record.block_index,
                        "stage": record.stage.value,
                        "sequence": record.sequence,
                        "payload_hash": record.payload_hash,
                        "file": f"b{record.block_index:05d}_{record.stage.value}.npz",
                    }
                    for record in sorted(
                        records.values(),
                        key=lambda item: (
                            item.sequence,
                            item.block_index,
                            STAGE_ORDER[item.stage],
                        ),
                    )
                ],
                "count": len(records),
            }
            self._write_json_atomic(self.snapshot_dir / "index.json", index)
            return payload_hash, sequence

    def read_record(self, block_index: int, stage: Stage) -> bytes | None:
        path = self.snapshot_dir / f"b{block_index:05d}_{stage.value}.npz"
        if not path.is_file():
            return None
        try:
            with np.load(path, allow_pickle=False) as data:
                return np.asarray(data["payload"], dtype=np.uint8).tobytes()
        except (OSError, ValueError, KeyError, EOFError):
            return None

    @staticmethod
    def _read_json(path: Path) -> dict[str, Any] | None:
        if not path.is_file():
            return None
        try:
            with path.open("r", encoding="utf-8") as handle:
                value = json.load(handle)
            return value if isinstance(value, dict) else None
        except (OSError, UnicodeError, json.JSONDecodeError):
            return None

    @staticmethod
    def _write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
        fd, temp_name = tempfile.mkstemp(
            prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
        )
        os.close(fd)
        temp_path = Path(temp_name)
        try:
            with temp_path.open("w", encoding="utf-8") as handle:
                json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=True)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp_path, path)
        finally:
            if temp_path.exists():
                temp_path.unlink(missing_ok=True)
