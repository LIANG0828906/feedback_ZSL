"""Atomic, versioned, all-or-nothing chunk-stage checkpoint storage."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import threading
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple

import numpy as np

from .config import STAGES
from .errors import EdgePipeError, ErrorCode
from .ops import BlockGrid, iter_stage_block_pairs

SNAPSHOT_SCHEMA = "edgepipe.snapshot/v1"


def mask_content_hash(mask: np.ndarray) -> str:
    canonical = np.ascontiguousarray(mask)
    digest = hashlib.sha256()
    digest.update(f"{canonical.shape}:{canonical.dtype}".encode("ascii"))
    digest.update(canonical.tobytes(order="C"))
    return digest.hexdigest()


def _atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(
        f".{path.name}.tmp.{os.getpid()}.{threading.get_ident()}"
    )
    try:
        with open(temporary, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def _atomic_write_npz(
    path: Path, mask: np.ndarray, metadata: Mapping[str, Any]
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(
        f".{path.name}.tmp.{os.getpid()}.{threading.get_ident()}"
    )
    meta_bytes = np.frombuffer(
        json.dumps(dict(metadata), sort_keys=True, ensure_ascii=False).encode("utf-8"),
        dtype=np.uint8,
    )
    try:
        with open(temporary, "wb") as handle:
            np.savez_compressed(handle, mask=mask, meta=meta_bytes)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(str(path), os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    except OSError:
        pass
    finally:
        os.close(descriptor)


@dataclass
class SnapshotValidation:
    status: str
    reason: Optional[str] = None
    manifest: Optional[Dict[str, Any]] = None
    entries: List[Dict[str, Any]] = field(default_factory=list)

    @property
    def valid(self) -> bool:
        return self.status == "valid"

    @property
    def exists(self) -> bool:
        return self.status != "missing"


class CheckpointStore:
    """Single-snapshot store.

    The manifest is the commit point. A blob written immediately before a
    crash but not referenced by the manifest is ignored on restart. All public
    write operations are serialized with an in-process lock; the engine also
    invokes ``commit`` from one coordinator thread only.
    """

    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.manifest_path = self.root / "snapshot.json"
        self.blocks_dir = self.root / "blocks"
        self._lock = threading.RLock()
        self.manifest: Optional[Dict[str, Any]] = None

    def reset(self) -> None:
        with self._lock:
            self.manifest = None
            if self.root.exists():
                for child in self.root.iterdir():
                    if child.is_dir():
                        shutil.rmtree(child)
                    else:
                        child.unlink()
            self.root.mkdir(parents=True, exist_ok=True)
            self.blocks_dir.mkdir(parents=True, exist_ok=True)

    def initialize(
        self,
        *,
        input_hash: str,
        params_version: str,
        algorithm_version: str,
        config: Mapping[str, Any],
        grid: BlockGrid,
        total_commits: int,
    ) -> Dict[str, Any]:
        with self._lock:
            self.root.mkdir(parents=True, exist_ok=True)
            self.blocks_dir.mkdir(parents=True, exist_ok=True)
            manifest = {
                "schema": SNAPSHOT_SCHEMA,
                "created_at_utc": datetime.now(timezone.utc).isoformat(),
                "input_hash": input_hash,
                "params_version": params_version,
                "algorithm_version": algorithm_version,
                "config": dict(config),
                "image": {
                    "height": grid.height,
                    "width": grid.width,
                    "block_size": grid.block_size,
                    "rows": grid.rows,
                    "cols": grid.cols,
                    "num_blocks": len(grid.blocks),
                },
                "stages": list(STAGES),
                "total_commits": int(total_commits),
                "next_commit_seq": 0,
                "entries": [],
            }
            self._write_manifest(manifest)
            self.manifest = manifest
            return manifest

    def _write_manifest(self, manifest: Mapping[str, Any]) -> None:
        data = json.dumps(
            dict(manifest), sort_keys=True, ensure_ascii=False, indent=2
        ).encode("utf-8")
        _atomic_write_bytes(self.manifest_path, data)

    def validate_snapshot(
        self,
        *,
        expected_input_hash: str,
        expected_params_version: str,
        expected_algorithm_version: str,
        grid: BlockGrid,
    ) -> SnapshotValidation:
        with self._lock:
            if not self.manifest_path.exists():
                self.manifest = None
                return SnapshotValidation("missing", "snapshot 不存在")
            try:
                manifest = json.loads(self.manifest_path.read_text(encoding="utf-8"))
            except (OSError, UnicodeError, json.JSONDecodeError) as exc:
                self.manifest = None
                return SnapshotValidation(
                    "invalid", f"manifest 不可读或损坏: {exc}"
                )

            if manifest.get("schema") != SNAPSHOT_SCHEMA:
                return SnapshotValidation("invalid", "snapshot schema 不匹配")
            if manifest.get("input_hash") != expected_input_hash:
                return SnapshotValidation("invalid", "输入内容哈希不匹配")
            if manifest.get("params_version") != expected_params_version:
                return SnapshotValidation("invalid", "参数版本不匹配")
            if manifest.get("algorithm_version") != expected_algorithm_version:
                return SnapshotValidation("invalid", "算法版本不匹配")

            image_meta = manifest.get("image", {})
            expected_image = {
                "height": grid.height,
                "width": grid.width,
                "block_size": grid.block_size,
                "rows": grid.rows,
                "cols": grid.cols,
                "num_blocks": len(grid.blocks),
            }
            for key, value in expected_image.items():
                if image_meta.get(key) != value:
                    return SnapshotValidation("invalid", f"图像布局字段 {key} 不匹配")
            if manifest.get("stages") != list(STAGES):
                return SnapshotValidation("invalid", "阶段列表不匹配")

            entries = manifest.get("entries")
            if not isinstance(entries, list):
                return SnapshotValidation("invalid", "manifest entries 格式错误")
            total_commits = int(image_meta["num_blocks"]) * len(STAGES)
            if int(manifest.get("total_commits", -1)) != total_commits:
                return SnapshotValidation("invalid", "总提交数不匹配")
            if int(manifest.get("next_commit_seq", -1)) != len(entries):
                return SnapshotValidation("invalid", "next_commit_seq 与 entries 不一致")

            expected_pairs = list(iter_stage_block_pairs(len(grid.blocks)))
            for offset, entry in enumerate(entries):
                if not isinstance(entry, dict):
                    return SnapshotValidation("invalid", "entry 格式错误")
                if entry.get("commit_seq") != offset:
                    return SnapshotValidation("invalid", "提交序列不连续")
                pair = expected_pairs[offset]
                if (
                    entry.get("stage") != pair[1]
                    or entry.get("block_index") != pair[2]
                    or entry.get("block_id") != f"b{pair[2]:04d}"
                ):
                    return SnapshotValidation("invalid", "提交顺序或分块阶段标识不匹配")
                if entry.get("params_version") != expected_params_version:
                    return SnapshotValidation("invalid", "分块参数版本不匹配")
                if entry.get("algorithm_version") != expected_algorithm_version:
                    return SnapshotValidation("invalid", "分块算法版本不匹配")
                try:
                    mask = self._load_entry_mask(entry)
                except Exception as exc:
                    return SnapshotValidation(
                        "invalid", f"分块文件不可读: {entry.get('path')}: {exc}"
                    )
                block = grid.blocks[pair[2]]
                expected_shape = (block.height, block.width) + (
                    () if len(mask.shape) == 2 else mask.shape[2:]
                )
                if mask.shape != expected_shape:
                    return SnapshotValidation("invalid", "分块输出尺寸不匹配")
                if mask.dtype != np.uint8:
                    return SnapshotValidation("invalid", "分块输出 dtype 不匹配")
                if mask_content_hash(mask) != entry.get("content_hash"):
                    return SnapshotValidation("invalid", "分块内容哈希不匹配")

            manifest["entries"] = entries
            self.manifest = manifest
            return SnapshotValidation("valid", None, manifest, entries)

    def _load_entry_mask(self, entry: Mapping[str, Any]) -> np.ndarray:
        relative = Path(str(entry["path"]))
        if relative.is_absolute() or ".." in relative.parts:
            raise ValueError("非法相对路径")
        path = self.root / relative
        with np.load(str(path), allow_pickle=False) as archive:
            if "mask" not in archive or "meta" not in archive:
                raise ValueError("缺少 mask/meta")
            mask = np.array(archive["mask"], copy=True)
            meta = json.loads(bytes(archive["meta"]).decode("utf-8"))
        for key in (
            "commit_seq",
            "block_id",
            "block_index",
            "stage",
            "stage_index",
            "content_hash",
            "params_version",
            "algorithm_version",
            "dtype",
        ):
            if meta.get(key) != entry.get(key):
                raise ValueError(f"blob 元数据字段 {key} 与 manifest 不一致")
        if list(meta.get("shape", [])) != list(mask.shape):
            raise ValueError("blob 元数据 shape 与实际数组不一致")
        if meta.get("content_hash") != entry.get("content_hash"):
            raise ValueError("blob 元数据哈希与 manifest 不一致")
        return mask

    def load_entry_mask(self, entry: Mapping[str, Any]) -> np.ndarray:
        return self._load_entry_mask(entry)

    def commit(
        self,
        *,
        block_id: str,
        block_index: int,
        stage: str,
        stage_index: int,
        mask: np.ndarray,
        params_version: str,
        algorithm_version: str,
    ) -> Dict[str, Any]:
        with self._lock:
            if self.manifest is None:
                raise EdgePipeError(
                    ErrorCode.STATE,
                    "检查点尚未初始化",
                    field="checkpoint",
                )
            next_seq = int(self.manifest["next_commit_seq"])
            expected = list(
                iter_stage_block_pairs(int(self.manifest["image"]["num_blocks"]))
            )
            if next_seq >= len(expected):
                raise EdgePipeError(
                    ErrorCode.STATE,
                    "检查点提交已满",
                    field="checkpoint",
                )
            expected_seq, expected_stage, expected_block = expected[next_seq]
            if (
                next_seq != expected_seq
                or stage != expected_stage
                or block_index != expected_block
                or block_id != f"b{expected_block:04d}"
            ):
                raise EdgePipeError(
                    ErrorCode.STATE,
                    "检查点写入顺序不符合阶段优先、分块升序",
                    field="checkpoint_order",
                    details={
                        "next_commit_seq": next_seq,
                        "expected": {
                            "stage": expected_stage,
                            "block_index": expected_block,
                        },
                        "actual": {
                            "stage": stage,
                            "block_index": block_index,
                        },
                    },
                )

            content_hash = mask_content_hash(mask)
            relative_path = Path("blocks") / (
                f"{next_seq:04d}_{block_id}_{stage}.npz"
            )
            metadata = {
                "schema": SNAPSHOT_SCHEMA,
                "commit_seq": next_seq,
                "block_id": block_id,
                "block_index": block_index,
                "stage": stage,
                "stage_index": stage_index,
                "content_hash": content_hash,
                "params_version": params_version,
                "algorithm_version": algorithm_version,
                "shape": list(mask.shape),
                "dtype": str(mask.dtype),
            }
            _atomic_write_npz(self.root / relative_path, mask, metadata)
            entry = {
                **metadata,
                "path": relative_path.as_posix(),
            }
            updated = dict(self.manifest)
            updated["entries"] = list(self.manifest["entries"]) + [entry]
            updated["next_commit_seq"] = next_seq + 1
            self._write_manifest(updated)
            self.manifest = updated
            return entry

    @property
    def commit_count(self) -> int:
        with self._lock:
            if self.manifest is None:
                return 0
            return int(self.manifest.get("next_commit_seq", 0))

    @property
    def total_commits(self) -> int:
        with self._lock:
            if self.manifest is None:
                return 0
            return int(self.manifest.get("total_commits", 0))

    def canonical_commit_sequence(self) -> List[Dict[str, Any]]:
        with self._lock:
            if self.manifest is None:
                return []
            return [
                {
                    "commit_seq": entry["commit_seq"],
                    "block_id": entry["block_id"],
                    "stage": entry["stage"],
                    "content_hash": entry["content_hash"],
                }
                for entry in sorted(
                    self.manifest["entries"], key=lambda item: item["commit_seq"]
                )
            ]

    def snapshot_size_bytes(self) -> int:
        total = 0
        if not self.root.exists():
            return 0
        for path in self.root.rglob("*"):
            if path.is_file():
                total += path.stat().st_size
        return total
