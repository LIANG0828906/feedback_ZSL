"""分块检查点的原子持久化与恢复边界（业务要求 7）。"""
from __future__ import annotations

import json
import os
import tempfile
import threading
from dataclasses import dataclass
from typing import Any, Dict, List, Optional, Tuple

import numpy as np

from .errors import E_IO, E_VER, IoError, VersionError
from .samplegen import ALGO_VERSION


@dataclass
class TileRecord:
    tile_id: int
    tx: int
    ty: int
    version: int
    content_hash: str
    algo_version: str
    color_file: str
    depth_file: str
    committed: bool

    def to_meta(self) -> Dict[str, Any]:
        return {
            "tile_id": self.tile_id, "tx": self.tx, "ty": self.ty,
            "version": self.version, "content_hash": self.content_hash,
            "algo_version": self.algo_version,
            "color_file": self.color_file, "depth_file": self.depth_file,
            "committed": self.committed,
        }


class CheckpointStore:
    """单写者锁保证写入串行；先写临时文件再原子 rename。"""

    def __init__(self, root: str) -> None:
        self.root = root
        self.tiles_dir = os.path.join(root, "tiles")
        os.makedirs(self.tiles_dir, exist_ok=True)
        self._lock = threading.Lock()
        self.index_path = os.path.join(root, "index.json")

    def _atomic_write_npy(self, path: str, arr: np.ndarray) -> None:
        d = os.path.dirname(path)
        fd, tmp = tempfile.mkstemp(dir=d, suffix=".tmp")
        os.close(fd)
        try:
            with open(tmp, "wb") as fh:
                np.save(fh, np.ascontiguousarray(arr))
            os.replace(tmp, path)
        except OSError as exc:
            if os.path.exists(tmp):
                os.unlink(tmp)
            raise IoError(f"写入检查点失败 {path}", module="checkpoint",
                          cause=exc)

    def _atomic_write_json(self, path: str, obj: Any) -> None:
        d = os.path.dirname(path)
        fd, tmp = tempfile.mkstemp(dir=d, suffix=".tmp")
        os.close(fd)
        try:
            with open(tmp, "w", encoding="utf-8") as fh:
                json.dump(obj, fh, indent=1)
            os.replace(tmp, path)
        except OSError as exc:
            if os.path.exists(tmp):
                os.unlink(tmp)
            raise IoError(f"写入索引失败 {path}", module="checkpoint",
                          cause=exc)

    def commit_tile(self, tile_id: int, tx: int, ty: int, version: int,
                    content_hash: str, color: np.ndarray,
                    depth: np.ndarray) -> TileRecord:
        """串行原子提交一个分块（颜色+深度+版本+哈希+算法版本）。"""
        with self._lock:
            base = f"v{version}_t{tile_id:04d}"
            color_file = f"{base}_color.npy"
            depth_file = f"{base}_depth.npy"
            self._atomic_write_npy(os.path.join(self.tiles_dir, color_file),
                                   color)
            self._atomic_write_npy(os.path.join(self.tiles_dir, depth_file),
                                   depth)
            record = TileRecord(tile_id, tx, ty, version, content_hash,
                                ALGO_VERSION, color_file, depth_file, True)
            index = self._read_index_unlocked()
            index["tiles"] = [t for t in index.get("tiles", [])
                              if not (t["tile_id"] == tile_id
                                      and t["version"] == version)]
            index["tiles"].append(record.to_meta())
            self._atomic_write_json(self.index_path, index)
            return record

    def _read_index_unlocked(self) -> Dict[str, Any]:
        if not os.path.exists(self.index_path):
            return {"tiles": []}
        try:
            with open(self.index_path, "r", encoding="utf-8") as fh:
                return json.load(fh)
        except (OSError, json.JSONDecodeError):
            return {"tiles": []}

    def load_tile(self, rec: Dict[str, Any]
                  ) -> Tuple[np.ndarray, np.ndarray]:
        color = np.load(os.path.join(self.tiles_dir, rec["color_file"]))
        depth = np.load(os.path.join(self.tiles_dir, rec["depth_file"]))
        return color, depth

    def recover(self, version: int, content_hash: str
                ) -> Dict[int, TileRecord]:
        """恢复已持久化分块。

        内容哈希或算法版本不匹配时整体作废并清空快照，返回空集合，
        由上层全量重算。仅返回真正 committed 的分块（崩溃时未 rename
        的临时文件不会出现）。
        """
        with self._lock:
            index = self._read_index_unlocked()
            tiles = index.get("tiles", [])
            valid: Dict[int, TileRecord] = {}
            mismatch = False
            for rec in tiles:
                if rec.get("algo_version") != ALGO_VERSION:
                    mismatch = True
                    break
                if rec["version"] != version or \
                        rec["content_hash"] != content_hash:
                    mismatch = True
                    break
                cf = os.path.join(self.tiles_dir, rec["color_file"])
                df = os.path.join(self.tiles_dir, rec["depth_file"])
                if not (os.path.exists(cf) and os.path.exists(df)):
                    mismatch = True
                    break
                if rec.get("committed"):
                    valid[rec["tile_id"]] = TileRecord(**rec)
            if mismatch:
                self._discard_unlocked()
                return {}
            return valid

    def _discard_unlocked(self) -> None:
        if os.path.exists(self.index_path):
            os.unlink(self.index_path)
        for fn in os.listdir(self.tiles_dir):
            os.unlink(os.path.join(self.tiles_dir, fn))

    def discard(self) -> None:
        with self._lock:
            self._discard_unlocked()

    def list_versions(self) -> List[int]:
        index = self._read_index_unlocked()
        return sorted({t["version"] for t in index.get("tiles", [])})

    def query_version(self, version: int) -> None:
        existing = self.list_versions()
        if version not in existing:
            raise VersionError(
                f"检查点中不存在版本 {version}，现存 {existing}",
                module="checkpoint", field="version",
                context={"requested": version, "existing": existing},
            )
