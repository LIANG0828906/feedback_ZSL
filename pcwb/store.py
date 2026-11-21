"""版本化点云存储与原子提交。

目录布局：
    <root>/versions/v000000.pcwb, v000001.pcwb, ...
    <root>/versions/log.jsonl          每次提交追加一条版本记录

保存采用临时文件 + 原子重命名（os.replace），提交失败抛出
CommitFailed 且上一版本文件与日志保持不变。回退到历史版本时
把历史内容作为新版本追加，不改写历史文件。
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field

import numpy as np

from .blocks import build_blocks
from .errors import CommitFailed
from .pcformat import PointCloud, read, write_atomic


@dataclass
class VersionRecord:
    version: int
    op: str
    params: dict
    input_hash: str
    output_hash: str
    replaced_blocks: list = field(default_factory=list)
    parent: int = -1

    def to_json(self) -> str:
        return json.dumps({
            "version": self.version,
            "op": self.op,
            "params": self.params,
            "input_hash": self.input_hash,
            "output_hash": self.output_hash,
            "replaced_blocks": [list(b) for b in self.replaced_blocks],
            "parent": self.parent,
        }, sort_keys=True, ensure_ascii=False)


class VersionStore:
    def __init__(self, root: str, edge_length: float = 10.0):
        self.root = root
        self.versions_dir = os.path.join(root, "versions")
        os.makedirs(self.versions_dir, exist_ok=True)
        self.edge_length = float(edge_length)
        self.records: list = []
        self._current_cloud: PointCloud | None = None
        self._current_index = None

    @property
    def current_version(self) -> int:
        return self.records[-1].version if self.records else -1

    @property
    def current_cloud(self) -> PointCloud:
        return self._current_cloud

    @property
    def current_index(self):
        return self._current_index

    def _version_path(self, version: int) -> str:
        return os.path.join(self.versions_dir, f"v{version:06d}.pcwb")

    def _log_path(self) -> str:
        return os.path.join(self.versions_dir, "log.jsonl")

    def load_initial(self, path: str) -> VersionRecord:
        """把磁盘上的基线点云登记为版本 0。"""
        cloud = read(path)
        self._current_index = build_blocks(cloud, self.edge_length)
        self._current_cloud = cloud
        record = VersionRecord(
            version=0, op="load", params={"source": os.path.basename(path)},
            input_hash="", output_hash=cloud.content_hash(),
            replaced_blocks=sorted(self._current_index.blocks.keys()),
            parent=-1,
        )
        self.records = [record]
        try:
            write_atomic(self._version_path(0), cloud)
            self._append_log(record)
        except OSError as exc:
            raise CommitFailed("基线版本写入失败", cause=str(exc)) from exc
        return record

    def commit(self, op: str, params: dict,
               new_points: np.ndarray, new_colors: np.ndarray) -> VersionRecord:
        """原子提交新版本。失败抛出 CommitFailed，上一版本不受影响。"""
        if self._current_cloud is None:
            raise CommitFailed("尚未载入基线版本")
        new_cloud = PointCloud(new_points, new_colors)
        new_index = build_blocks(new_cloud, self.edge_length)
        replaced = _diff_blocks(self._current_index, new_index)
        record = VersionRecord(
            version=self.current_version + 1,
            op=op,
            params=params,
            input_hash=self._current_cloud.content_hash(),
            output_hash=new_cloud.content_hash(),
            replaced_blocks=replaced,
            parent=self.current_version,
        )
        try:
            write_atomic(self._version_path(record.version), new_cloud)
            self._append_log(record)
        except OSError as exc:
            raise CommitFailed("版本提交失败", version=record.version,
                               cause=str(exc)) from exc
        self.records.append(record)
        self._current_cloud = new_cloud
        self._current_index = new_index
        return record

    def rollback(self, target_version: int) -> VersionRecord:
        """回退到历史版本：读取历史内容并作为新版本追加。"""
        if target_version < 0 or target_version > self.current_version:
            raise CommitFailed("目标版本不存在", target=target_version)
        try:
            historical = read(self._version_path(target_version))
        except OSError as exc:
            raise CommitFailed("历史版本读取失败", target=target_version,
                               cause=str(exc)) from exc
        return self.commit(
            op="rollback",
            params={"target_version": int(target_version)},
            new_points=historical.points,
            new_colors=historical.colors,
        )

    def _append_log(self, record: VersionRecord):
        line = record.to_json() + "\n"
        tmp = self._log_path() + ".tmp"
        existing = ""
        if os.path.exists(self._log_path()):
            with open(self._log_path(), "r", encoding="utf-8") as fh:
                existing = fh.read()
        with open(tmp, "w", encoding="utf-8") as fh:
            fh.write(existing + line)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, self._log_path())


def _diff_blocks(old_index, new_index) -> list:
    if old_index is None:
        return sorted(new_index.blocks.keys())
    changed = []
    for cid in sorted(set(old_index.blocks) | set(new_index.blocks)):
        ob = old_index.blocks.get(cid)
        nb = new_index.blocks.get(cid)
        if ob is None or nb is None or ob.content_hash != nb.content_hash:
            changed.append(cid)
    return changed
