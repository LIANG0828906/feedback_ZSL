"""窗口检查点持久化：原子、串行写入，整体作废策略。

目录布局::

    <store>/manifest.json                 # 工程级元数据与窗口索引
    <store>/window_<idx>.npz              # 单窗口快照（采样摘要/噪声谱/哈希/版本）

每个窗口先写临时文件再 ``os.replace`` 原子落盘，随后串行更新 manifest；
写操作全程持有同一把锁，保证检查点永不交错。
重启时若内容哈希、源版本、参数版本或算法版本任一不匹配，
整个快照目录作废并触发全量重算，禁止混用部分旧窗口。
"""

from __future__ import annotations

import json
import os
import shutil
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from ..model import ALGO_VERSION, PARAM_VERSION, RestoreProject
from .hashing import digest_samples, digest_spectrum

MANIFEST_NAME = "manifest.json"


@dataclass(frozen=True)
class WindowSnapshot:
    window_index: int
    samples: np.ndarray
    noise_spectrum: np.ndarray
    content_hash: str
    source_version: str
    param_version: str
    algo_version: str
    param_snapshot: dict[str, Any]


class CheckpointStore:
    def __init__(self, root: str | Path, project: RestoreProject, *,
                 source_content_hash: str):
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)
        self.project = project
        self._lock = threading.Lock()
        self._source_content_hash = source_content_hash
        self._manifest_path = self.root / MANIFEST_NAME

    def _expected_manifest(self) -> dict[str, Any]:
        return {
            "project_id": self.project.project_id,
            "source_version": self.project.source_version,
            "param_version": PARAM_VERSION,
            "algo_version": ALGO_VERSION,
            "source_content_hash": self._source_content_hash,
            "window_count": None,  # 首次写入时补齐
            "windows": [],
        }

    def reset(self) -> None:
        """整体作废：清空快照目录，禁止部分复用。"""
        with self._lock:
            self._clear_unlocked()

    def _clear_unlocked(self) -> None:
        for child in self.root.glob("*"):
            if child.is_file() or child.is_symlink():
                child.unlink()
            else:
                shutil.rmtree(child)

    def commit_window(self, snapshot: WindowSnapshot, window_count: int) -> None:
        """串行原子写入单窗口快照并更新 manifest。"""
        with self._lock:
            final_path = self.root / f"window_{snapshot.window_index}.npz"
            npz_tmp_stem = self.root / f"window_{snapshot.window_index}.tmp"
            np.savez(
                npz_tmp_stem,
                samples=snapshot.samples,
                noise_spectrum=snapshot.noise_spectrum,
                content_hash=np.array(snapshot.content_hash),
                source_version=np.array(snapshot.source_version),
                param_version=np.array(snapshot.param_version),
                algo_version=np.array(snapshot.algo_version),
                param_snapshot=np.array(
                    json.dumps(snapshot.param_snapshot, sort_keys=True), dtype=object
                ),
            )
            os.replace(self.root / f"window_{snapshot.window_index}.tmp.npz", final_path)

            manifest = self._read_manifest_unlocked()
            if manifest is None:
                manifest = self._expected_manifest()
                manifest["window_count"] = window_count
            entries = [w for w in manifest["windows"] if w["window_index"] != snapshot.window_index]
            entries.append(
                {
                    "window_index": snapshot.window_index,
                    "content_hash": snapshot.content_hash,
                    "noise_spectrum_hash": digest_spectrum(snapshot.noise_spectrum),
                    "sample_digest": digest_samples(snapshot.samples),
                }
            )
            entries.sort(key=lambda item: item["window_index"])
            manifest["windows"] = entries
            self._write_manifest_unlocked(manifest)

    def _read_manifest_unlocked(self) -> dict[str, Any] | None:
        if not self._manifest_path.exists():
            return None
        with open(self._manifest_path, "r", encoding="utf-8") as handle:
            return json.load(handle)

    def _write_manifest_unlocked(self, manifest: dict[str, Any]) -> None:
        tmp = self.root / f"{MANIFEST_NAME}.tmp"
        with open(tmp, "w", encoding="utf-8") as handle:
            json.dump(manifest, handle, ensure_ascii=False, indent=2, sort_keys=True)
        os.replace(tmp, self._manifest_path)

    def load_valid(self, window_count: int) -> dict[int, WindowSnapshot] | None:
        """加载并校验全部快照；任一不匹配则整体作废返回 None。"""
        with self._lock:
            manifest = self._read_manifest_unlocked()
            if manifest is None:
                return None
            expected = self._expected_manifest()
            identity_keys = ("project_id", "source_version", "param_version",
                             "algo_version", "source_content_hash")
            for key in identity_keys:
                if manifest.get(key) != expected[key]:
                    self._invalidate_unlocked(f"manifest.{key} 不匹配")
                    return None
            if manifest.get("window_count") != window_count:
                self._invalidate_unlocked("窗口总数不匹配")
                return None

            snapshots: dict[int, WindowSnapshot] = {}
            for entry in manifest["windows"]:
                index = entry["window_index"]
                npz_path = self.root / f"window_{index}.npz"
                if not npz_path.exists():
                    self._invalidate_unlocked(f"window_{index}.npz 缺失")
                    return None
                with np.load(npz_path, allow_pickle=True) as bundle:
                    samples = bundle["samples"]
                    noise_spectrum = bundle["noise_spectrum"]
                    content_hash = str(bundle["content_hash"])
                    source_version = str(bundle["source_version"])
                    param_version = str(bundle["param_version"])
                    algo_version = str(bundle["algo_version"])
                    param_snapshot = json.loads(str(bundle["param_snapshot"]))
                if content_hash != digest_samples(samples) or content_hash != entry["content_hash"]:
                    self._invalidate_unlocked(f"window_{index} 内容哈希不匹配")
                    return None
                if (source_version != self.project.source_version
                        or param_version != PARAM_VERSION
                        or algo_version != ALGO_VERSION):
                    self._invalidate_unlocked(f"window_{index} 版本不匹配")
                    return None
                snapshots[index] = WindowSnapshot(
                    window_index=index,
                    samples=samples,
                    noise_spectrum=noise_spectrum,
                    content_hash=content_hash,
                    source_version=source_version,
                    param_version=param_version,
                    algo_version=algo_version,
                    param_snapshot=param_snapshot,
                )
            return snapshots

    def _invalidate_unlocked(self, reason: str) -> None:
        self._clear_unlocked()
        self.invalidation_reason = reason
