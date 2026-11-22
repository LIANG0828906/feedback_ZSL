from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import threading
import time
import uuid
from typing import Any

import numpy as np

from .config import ALGORITHM_VERSION
from .errors import CheckpointError
from .hashing import sha256_array
from .stages import STAGE_ORDER, Stage


CHECKPOINT_SCHEMA = 1


def _canonical_json(payload: Any) -> bytes:
    return json.dumps(
        payload,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
    ).encode("utf-8")


def run_fingerprint(run_meta: dict[str, Any]) -> str:
    payload = {key: value for key, value in run_meta.items() if key != "run_fingerprint"}
    return hashlib.sha256(_canonical_json(payload)).hexdigest()


def build_run_meta(
    *,
    sample_id: str,
    image_sha256: str,
    truth_sha256: str,
    width: int,
    height: int,
    channels: int,
    tile_size: int,
    params_version: str,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "schema_version": CHECKPOINT_SCHEMA,
        "sample_id": sample_id,
        "image_sha256": image_sha256,
        "truth_sha256": truth_sha256,
        "width": int(width),
        "height": int(height),
        "channels": int(channels),
        "tile_size": int(tile_size),
        "params_version": params_version,
        "algorithm_version": ALGORITHM_VERSION,
        "commit_protocol": "single-npz-atomic-replace",
    }
    payload["run_fingerprint"] = run_fingerprint(payload)
    return payload


class CheckpointStore:
    def __init__(self, path: str | Path, run_meta: dict[str, Any]) -> None:
        self.path = Path(path)
        self.run_meta = run_meta
        self.run_fingerprint = str(run_meta["run_fingerprint"])
        self.records_dir = self.path / "records"
        self.meta_path = self.path / "run_meta.json"
        self.invalidated = False
        self.invalidation_reason: str | None = None
        self.invalidated_path: Path | None = None
        self._lock = threading.RLock()
        self._records: dict[tuple[Stage, int], np.ndarray] = {}
        self._record_meta: dict[tuple[Stage, int], dict[str, Any]] = {}
        self.next_seq = 0
        self.commit_order: list[dict[str, Any]] = []
        self._open_or_initialize()

    @property
    def records(self) -> dict[tuple[Stage, int], np.ndarray]:
        with self._lock:
            return dict(self._records)

    @property
    def record_metadata(self) -> dict[tuple[Stage, int], dict[str, Any]]:
        with self._lock:
            return {key: dict(value) for key, value in self._record_meta.items()}

    def _open_or_initialize(self) -> None:
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
        except OSError as exc:
            raise CheckpointError(
                "无法创建检查点父目录",
                details={"path": str(self.path.parent), "reason": str(exc)},
            ) from exc

        if self.path.exists():
            reason = self._validate_existing()
            if reason is not None:
                self._invalidate(reason)
                return
            return

        self.path.mkdir(parents=True, exist_ok=False)
        self.records_dir.mkdir(parents=True, exist_ok=False)
        self._write_meta()

    def _validate_existing(self) -> str | None:
        if not self.path.is_dir():
            return "snapshot_path_not_directory"
        if not self.meta_path.is_file():
            return "run_meta_missing"
        try:
            loaded_meta = json.loads(self.meta_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return "run_meta_unreadable"
        if loaded_meta != self.run_meta:
            return "run_meta_mismatch"
        if not self.records_dir.is_dir():
            return "records_directory_missing"

        record_files = sorted(
            path
            for path in self.records_dir.glob("*.npz")
            if not path.name.startswith(".tmp-")
        )
        loaded: list[tuple[int, Stage, int, np.ndarray, dict[str, Any]]] = []
        for record_path in record_files:
            try:
                with np.load(record_path, allow_pickle=False) as data:
                    required = {
                        "array",
                        "content_hash",
                        "stage",
                        "block_id",
                        "seq",
                        "params_version",
                        "algorithm_version",
                        "run_fingerprint",
                    }
                    if not required.issubset(data.files):
                        return f"record_missing_fields:{record_path.name}"
                    array = np.array(data["array"], copy=True)
                    content_hash = _scalar_str(data["content_hash"])
                    stage_name = _scalar_str(data["stage"])
                    block_id = int(data["block_id"])
                    seq = int(data["seq"])
                    params_version = _scalar_str(data["params_version"])
                    algorithm_version = _scalar_str(data["algorithm_version"])
                    stored_fingerprint = _scalar_str(data["run_fingerprint"])
            except (OSError, ValueError, KeyError) as exc:
                return f"record_unreadable:{record_path.name}:{exc}"

            if sha256_array(array) != content_hash:
                return f"record_hash_mismatch:{record_path.name}"
            if params_version != self.run_meta["params_version"]:
                return f"record_params_version_mismatch:{record_path.name}"
            if algorithm_version != ALGORITHM_VERSION:
                return f"record_algorithm_version_mismatch:{record_path.name}"
            if stored_fingerprint != self.run_fingerprint:
                return f"record_run_fingerprint_mismatch:{record_path.name}"
            try:
                stage = Stage(stage_name)
            except ValueError:
                return f"record_stage_invalid:{record_path.name}"
            if stage not in STAGE_ORDER:
                return f"record_stage_invalid:{record_path.name}"
            if block_id < 0:
                return f"record_block_invalid:{record_path.name}"
            loaded.append((seq, stage, block_id, array, {"path": str(record_path)}))

        loaded.sort(key=lambda item: item[0])
        seqs = [item[0] for item in loaded]
        if seqs != list(range(len(seqs))):
            return "record_sequence_not_contiguous"
        seen: set[tuple[Stage, int]] = set()
        for seq, stage, block_id, array, record_meta in loaded:
            key = (stage, block_id)
            if key in seen:
                return f"record_duplicate_key:{stage.value}:{block_id}"
            seen.add(key)
            self._records[key] = array
            record_meta["seq"] = seq
            self._record_meta[key] = record_meta
            self.commit_order.append(
                {
                    "seq": seq,
                    "stage": stage.value,
                    "block_id": block_id,
                    "restored": True,
                }
            )
        self.next_seq = len(loaded)
        return None

    def _write_meta(self) -> None:
        temp = self.path / f".run_meta.tmp-{os.getpid()}-{uuid.uuid4().hex}"
        try:
            with temp.open("wb") as handle:
                handle.write(
                    (json.dumps(self.run_meta, indent=2, sort_keys=True) + "\n").encode(
                        "utf-8"
                    )
                )
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp, self.meta_path)
        except OSError as exc:
            try:
                temp.unlink(missing_ok=True)
            except OSError:
                pass
            raise CheckpointError(
                "无法写入检查点元数据",
                details={"path": str(self.meta_path), "reason": str(exc)},
            ) from exc

    def _invalidate(self, reason: str) -> None:
        self.invalidated = True
        self.invalidation_reason = reason
        stamp = time.strftime("%Y%m%dT%H%M%S", time.gmtime())
        target = self.path.with_name(f"{self.path.name}.invalid-{stamp}-{time.time_ns()}")
        try:
            os.replace(self.path, target)
            self.invalidated_path = target
        except OSError as exc:
            raise CheckpointError(
                "检查点快照无效且无法整体作废",
                details={
                    "path": str(self.path),
                    "reason": reason,
                    "rename_error": str(exc),
                },
            ) from exc
        self._records = {}
        self._record_meta = {}
        self.next_seq = 0
        self.commit_order = []
        self.path.mkdir(parents=True, exist_ok=False)
        self.records_dir.mkdir(parents=True, exist_ok=False)
        self._write_meta()

    def invalidate_snapshot(self, reason: str) -> None:
        with self._lock:
            self._invalidate(reason)

    def commit(self, stage: Stage, block_id: int, array: np.ndarray) -> dict[str, Any]:
        if stage not in STAGE_ORDER:
            raise CheckpointError(
                "未知阶段不能提交",
                stage=stage.value if isinstance(stage, Stage) else str(stage),
                block_id=block_id,
            )
        if block_id < 0:
            raise CheckpointError(
                "分块编号不能为负",
                block_id=block_id,
                stage=stage.value,
            )
        contiguous = np.ascontiguousarray(array)
        content_hash = sha256_array(contiguous)
        with self._lock:
            if (stage, block_id) in self._records:
                raise CheckpointError(
                    "同一分块阶段不能重复提交",
                    stage=stage.value,
                    block_id=block_id,
                )
            seq = self.next_seq
            final_path = self.records_dir / (
                f"stage-{STAGE_ORDER.index(stage):02d}_"
                f"block-{block_id:05d}_seq-{seq:06d}.npz"
            )
            temp_path = self.records_dir / (
                f".tmp-{os.getpid()}-{threading.get_ident()}-{uuid.uuid4().hex}.npz"
            )
            try:
                with temp_path.open("wb") as handle:
                    np.savez_compressed(
                        handle,
                        array=contiguous,
                        content_hash=np.array(content_hash),
                        stage=np.array(stage.value),
                        block_id=np.array(block_id, dtype=np.int64),
                        seq=np.array(seq, dtype=np.int64),
                        params_version=np.array(str(self.run_meta["params_version"])),
                        algorithm_version=np.array(ALGORITHM_VERSION),
                        run_fingerprint=np.array(self.run_fingerprint),
                    )
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(temp_path, final_path)
            except (OSError, ValueError) as exc:
                try:
                    temp_path.unlink(missing_ok=True)
                except OSError:
                    pass
                raise CheckpointError(
                    "分块阶段检查点提交失败",
                    stage=stage.value,
                    block_id=block_id,
                    details={"path": str(final_path), "reason": str(exc)},
                ) from exc
            self._records[(stage, block_id)] = contiguous
            metadata = {
                "seq": seq,
                "content_hash": content_hash,
                "params_version": self.run_meta["params_version"],
                "algorithm_version": ALGORITHM_VERSION,
                "path": str(final_path),
            }
            self._record_meta[(stage, block_id)] = metadata
            self.commit_order.append(
                {
                    "seq": seq,
                    "stage": stage.value,
                    "block_id": block_id,
                    "content_hash": content_hash,
                    "restored": False,
                }
            )
            self.next_seq += 1
            return dict(metadata)

    def record_for(self, stage: Stage, block_id: int) -> np.ndarray | None:
        with self._lock:
            array = self._records.get((stage, block_id))
            return None if array is None else np.array(array, copy=True)


def _scalar_str(value: Any) -> str:
    array = np.asarray(value)
    if array.shape == ():
        return str(array.item())
    if array.size == 1:
        return str(array.reshape(-1)[0].item())
    return str(value)
