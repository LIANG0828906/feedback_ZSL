"""持久化检查点与断点恢复。

目录布局::

    checkpoint_dir/
      manifest.json            # 全局快照清单（原子替换）
      segment_000000.npz       # 每段一个不可变检查点（原子替换）
      segment_000001.npz ...

语义：

* 每提交一段串行原子写入，文件先写临时名再 ``os.replace``，不会交错
* manifest 记录参数指纹、算法版本、内容哈希与段顺序
* 加载时任一段哈希/版本/指纹不匹配 -> 整体快照作废（清空目录）全量重算，
  禁止部分混用旧检查点
"""
from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass

import numpy as np

from ..core.config import ALGORITHM_VERSION, PipelineConfig

MANIFEST_NAME = "manifest.json"


@dataclass
class SegmentCheckpoint:
    segment_index: int
    input_hash: str
    output_samples: np.ndarray
    loudness: dict
    gain_record: dict
    gain_curve: np.ndarray


class CheckpointStore:
    def __init__(self, directory: str, config: PipelineConfig):
        self.directory = directory
        self.config = config
        self.fingerprint = config.parameter_fingerprint()
        os.makedirs(directory, exist_ok=True)
        self._lock_path = os.path.join(directory, ".write.lock")

    # ---- 底层原子 IO -------------------------------------------------
    def _atomic_write_bytes(self, final_path: str, data: bytes) -> None:
        fd, tmp = tempfile.mkstemp(dir=self.directory, suffix=".tmp")
        try:
            with os.fdopen(fd, "wb") as fh:
                fh.write(data)
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, final_path)
        finally:
            if os.path.exists(tmp):
                os.unlink(tmp)

    def _atomic_write_json(self, path: str, payload: dict) -> None:
        self._atomic_write_bytes(
            path, json.dumps(payload, indent=2, sort_keys=True).encode())

    # ---- manifest ----------------------------------------------------
    def _manifest_path(self) -> str:
        return os.path.join(self.directory, MANIFEST_NAME)

    def write_manifest(self, input_hashes: list[str],
                       committed: list[int],
                       accumulated_energy: float = 0.0,
                       accumulated_weight: int = 0) -> None:
        manifest = {
            "algorithm_version": ALGORITHM_VERSION,
            "parameter_fingerprint": self.fingerprint,
            "input_hashes": input_hashes,
            "committed_segments": committed,
            "accumulated_energy": accumulated_energy,
            "accumulated_weight": accumulated_weight,
        }
        self._atomic_write_json(self._manifest_path(), manifest)

    def read_manifest(self) -> dict | None:
        path = self._manifest_path()
        if not os.path.exists(path):
            return None
        with open(path, "rb") as fh:
            return json.loads(fh.read().decode())

    # ---- 段检查点 ----------------------------------------------------
    def _segment_path(self, index: int) -> str:
        return os.path.join(self.directory, f"segment_{index:06d}.npz")

    def save_segment(self, cp: SegmentCheckpoint) -> None:
        """串行原子提交一段结果。"""
        payload = {
            "segment_index": np.int64(cp.segment_index),
            "input_hash": cp.input_hash,
            "output_samples": np.ascontiguousarray(
                cp.output_samples, dtype=np.float64),
            "gain_curve": np.ascontiguousarray(
                cp.gain_curve, dtype=np.float64),
            "loudness_json": json.dumps(cp.loudness, sort_keys=True),
            "gain_record_json": json.dumps(cp.gain_record, sort_keys=True),
            "algorithm_version": ALGORITHM_VERSION,
            "parameter_fingerprint": self.fingerprint,
        }
        # np.savez 会自动追加 .npz，因此临时名不带后缀，再原子替换，
        # 保证检查点不可部分可见、不残留临时文件
        fd, tmp = tempfile.mkstemp(dir=self.directory, suffix=".tmp")
        os.close(fd)
        try:
            np.savez(tmp, **payload)
            written = tmp + ".npz"
            os.replace(written, self._segment_path(cp.segment_index))
        finally:
            for stale in (tmp, tmp + ".npz"):
                if os.path.exists(stale):
                    os.unlink(stale)

    def load_segment(self, index: int) -> SegmentCheckpoint:
        data = np.load(self._segment_path(index), allow_pickle=False)
        return SegmentCheckpoint(
            segment_index=int(data["segment_index"]),
            input_hash=str(data["input_hash"]),
            output_samples=data["output_samples"],
            loudness=json.loads(str(data["loudness_json"])),
            gain_record=json.loads(str(data["gain_record_json"])),
            gain_curve=data["gain_curve"],
        )

    # ---- 快照校验与作废 ---------------------------------------------
    def validate_snapshot(self, expected_hashes: list[str]
                          ) -> tuple[bool, str]:
        """检查快照是否完整且与当前输入/参数/算法版本匹配。"""
        manifest = self.read_manifest()
        if manifest is None:
            return False, "manifest 缺失"
        if manifest.get("algorithm_version") != ALGORITHM_VERSION:
            return False, "算法版本不匹配"
        if manifest.get("parameter_fingerprint") != self.fingerprint:
            return False, "参数版本不匹配"
        if manifest.get("input_hashes") != expected_hashes:
            return False, "内容哈希不匹配"
        committed = manifest.get("committed_segments", [])
        for index in committed:
            path = self._segment_path(int(index))
            if not os.path.exists(path):
                return False, f"段 {index} 检查点文件缺失"
            try:
                data = np.load(path, allow_pickle=False)
                if str(data["algorithm_version"]) != ALGORITHM_VERSION:
                    return False, f"段 {index} 算法版本不匹配"
                if str(data["parameter_fingerprint"]) != self.fingerprint:
                    return False, f"段 {index} 参数版本不匹配"
                if str(data["input_hash"]) != expected_hashes[int(index)]:
                    return False, f"段 {index} 内容哈希不匹配"
            except Exception as exc:
                return False, f"段 {index} 检查点损坏: {exc}"
        return True, "ok"

    def invalidate_all(self) -> None:
        """整体快照作废：清空检查点目录中的全部快照文件。"""
        if not os.path.isdir(self.directory):
            return
        for name in os.listdir(self.directory):
            if name == ".write.lock":
                continue
            os.unlink(os.path.join(self.directory, name))

    def restore_or_reset(self, expected_hashes: list[str]
                         ) -> list[SegmentCheckpoint]:
        """加载有效快照；任何不匹配则整体作废并返回空列表。"""
        ok, _reason = self.validate_snapshot(expected_hashes)
        if not ok:
            self.invalidate_all()
            return []
        manifest = self.read_manifest()
        return [self.load_segment(int(i))
                for i in manifest["committed_segments"]]

    def is_complete(self, total_segments: int) -> bool:
        """强制终止边界：仅当 manifest 完整覆盖全部段时才允许出报告。"""
        manifest = self.read_manifest()
        if manifest is None:
            return False
        committed = sorted(int(i) for i in manifest.get(
            "committed_segments", []))
        return committed == list(range(total_segments))
