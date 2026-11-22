"""版本一致的本地检查点存储。

目录布局::

    root/
      snapshot.json               # 快照头（版本、哈希、块阶段索引）
      blocks/b00/grow.npy + grow.json
      blocks/b00/clean.npy + clean.json
      blocks/b00/stats.json
      snapshot.lock               # 跨线程/跨进程互斥锁

写入通过同一把锁串行化，并采用 “临时文件 + fsync + os.replace” 原子
替换，保证写入不交错、崩溃不留半截文件。
"""
from __future__ import annotations

import contextlib
import json
import os
import threading
from typing import Any, Dict, Iterator, List, Optional

import numpy as np

from segworkbench.core.errors import ErrorCode, fail
from segworkbench.core.versioning import (
    ALGORITHM_VERSION,
    SNAPSHOT_FORMAT_VERSION,
)
from segworkbench.utils.hashing import sha256_array, sha256_json

PHASE_ARTIFACTS = {
    "grow": ("grow.npy", "grow.json"),
    "clean": ("clean.npy", "clean.json"),
    "stats": (None, "stats.json"),
}


class _DirectoryLock:
    """线程锁 + 文件锁双重串行化（文件锁用于多进程）。"""

    def __init__(self, lock_path: str) -> None:
        self._thread_lock = threading.RLock()
        self._lock_path = lock_path
        self._handle = None

    def __enter__(self) -> "_DirectoryLock":
        self._thread_lock.acquire()
        try:
            self._handle = open(self._lock_path, "a+")
            try:
                import fcntl

                fcntl.flock(self._handle.fileno(), fcntl.LOCK_EX)
                self._has_flock = True
            except ImportError:  # Windows 无 fcntl，退化为线程锁 + msvcrt
                import msvcrt

                msvcrt.locking(self._handle.fileno(), msvcrt.LK_LOCK, 1)
                self._has_flock = False
        except Exception:
            self._thread_lock.release()
            raise
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        try:
            if self._handle is not None:
                try:
                    if getattr(self, "_has_flock", False):
                        import fcntl

                        fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
                finally:
                    self._handle.close()
        finally:
            self._handle = None
            self._thread_lock.release()


class CheckpointStore:
    def __init__(self, root: str) -> None:
        self.root = os.path.abspath(root)
        os.makedirs(os.path.join(self.root, "blocks"), exist_ok=True)
        self._lock = _DirectoryLock(os.path.join(self.root, "snapshot.lock"))

    @contextlib.contextmanager
    def locked(self) -> Iterator[None]:
        with self._lock:
            yield

    def _atomic_write_bytes(self, path: str, payload: bytes) -> None:
        tmp_path = f"{path}.tmp.{os.getpid()}.{threading.get_ident()}"
        with open(tmp_path, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp_path, path)

    def _atomic_write_json(self, path: str, payload: Dict[str, Any]) -> None:
        data = json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True).encode("utf-8")
        self._atomic_write_bytes(path, data)

    def init_snapshot(self, header: Dict[str, Any]) -> None:
        path = os.path.join(self.root, "snapshot.json")
        if os.path.exists(path):
            return
        header = dict(header)
        header["format_version"] = SNAPSHOT_FORMAT_VERSION
        header.setdefault("block_phases", {})
        self._atomic_write_json(path, header)

    def read_header(self) -> Optional[Dict[str, Any]]:
        path = os.path.join(self.root, "snapshot.json")
        if not os.path.exists(path):
            return None
        try:
            with open(path, "r", encoding="utf-8") as handle:
                return json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            raise fail(ErrorCode.E_IO, "快照头无法读取", path=path, detail=str(exc))

    def invalidate_all(self, reason: str) -> None:
        """整份快照作废：删除所有块产物与快照头，禁止部分混用旧块。"""
        blocks_dir = os.path.join(self.root, "blocks")
        if os.path.isdir(blocks_dir):
            for block_id in sorted(os.listdir(blocks_dir)):
                block_dir = os.path.join(blocks_dir, block_id)
                if os.path.isdir(block_dir):
                    for name in os.listdir(block_dir):
                        os.remove(os.path.join(block_dir, name))
        header_path = os.path.join(self.root, "snapshot.json")
        if os.path.exists(header_path):
            os.remove(header_path)
        self._append_tombstone(reason)

    def _append_tombstone(self, reason: str) -> None:
        path = os.path.join(self.root, "tombstone.log")
        line = json.dumps({"reason": reason}, ensure_ascii=False, sort_keys=True) + "\n"
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(line)

    def verify_header(self, expected_image_hash: str, param_version: str) -> Dict[str, Any]:
        """恢复前校验快照版本；任一不匹配则作废并抛 E_VERSION。"""
        header = self.read_header()
        if header is None:
            raise fail(ErrorCode.E_VERSION, "快照不存在，需要全量重算")
        mismatches: List[str] = []
        if header.get("image_sha256") != expected_image_hash:
            mismatches.append("image_sha256")
        if header.get("param_version") != param_version:
            mismatches.append("param_version")
        if header.get("algorithm_version") != ALGORITHM_VERSION:
            mismatches.append("algorithm_version")
        if header.get("format_version") != SNAPSHOT_FORMAT_VERSION:
            mismatches.append("format_version")
        if mismatches:
            self.invalidate_all(reason=f"version_mismatch:{','.join(mismatches)}")
            raise fail(ErrorCode.E_VERSION, "快照版本不匹配，已整份作废并全量重算",
                       mismatched_fields=mismatches)
        return header

    def commit_phase(
        self,
        block_id: str,
        phase: str,
        array: Optional[np.ndarray],
        metadata: Dict[str, Any],
        header: Dict[str, Any],
    ) -> Dict[str, Any]:
        """串行原子写入一个块阶段产物，并更新快照头。"""
        if phase not in PHASE_ARTIFACTS:
            raise fail(ErrorCode.E_PARA, "未知阶段", field="phase", value=phase)
        npy_name, json_name = PHASE_ARTIFACTS[phase]
        block_dir = os.path.join(self.root, "blocks", block_id)
        os.makedirs(block_dir, exist_ok=True)
        record: Dict[str, Any] = {
            "block_id": block_id,
            "phase": phase,
            "algorithm_version": ALGORITHM_VERSION,
            "param_version": header["param_version"],
            "image_sha256": header["image_sha256"],
        }
        if array is not None:
            array = np.ascontiguousarray(array)
            npy_path = os.path.join(block_dir, npy_name)
            self._atomic_write_bytes(npy_path, _npy_bytes(array))
            record["array_sha256"] = sha256_array(array)
            record["array_dtype"] = str(array.dtype)
            record["array_shape"] = list(array.shape)
        record["metadata_sha256"] = sha256_json(metadata)
        record["metadata"] = metadata
        self._atomic_write_json(os.path.join(block_dir, json_name), record)

        current = self.read_header() or {}
        block_phases = dict(current.get("block_phases", {}))
        block_phases.setdefault(block_id, {})[phase] = {
            "state": "COMMITTED",
            "array_sha256": record.get("array_sha256"),
            "metadata_sha256": record["metadata_sha256"],
        }
        new_header = {**current, **header, "block_phases": block_phases}
        new_header["format_version"] = SNAPSHOT_FORMAT_VERSION
        self._atomic_write_json(os.path.join(self.root, "snapshot.json"), new_header)
        return record

    def load_phase(self, block_id: str, phase: str) -> Optional[Dict[str, Any]]:
        npy_name, json_name = PHASE_ARTIFACTS[phase]
        block_dir = os.path.join(self.root, "blocks", block_id)
        json_path = os.path.join(block_dir, json_name)
        if not os.path.exists(json_path):
            return None
        with open(json_path, "r", encoding="utf-8") as handle:
            record = json.load(handle)
        if npy_name is not None:
            npy_path = os.path.join(block_dir, npy_name)
            array = np.load(npy_path, allow_pickle=False)
            actual_hash = sha256_array(array)
            if actual_hash != record.get("array_sha256"):
                raise fail(ErrorCode.E_HASH, "阶段产物哈希损坏", block_id=block_id, phase=phase,
                           expected=record.get("array_sha256"), actual=actual_hash)
            record["array"] = array
        if sha256_json(record["metadata"]) != record.get("metadata_sha256"):
            raise fail(ErrorCode.E_HASH, "阶段元数据哈希损坏", block_id=block_id, phase=phase)
        return record

    def committed_phases(self) -> Dict[str, List[str]]:
        header = self.read_header() or {}
        return {
            block_id: sorted(phases.keys())
            for block_id, phases in header.get("block_phases", {}).items()
        }


def _npy_bytes(array: np.ndarray) -> bytes:
    import io

    buffer = io.BytesIO()
    np.save(buffer, array, allow_pickle=False)
    return buffer.getvalue()
