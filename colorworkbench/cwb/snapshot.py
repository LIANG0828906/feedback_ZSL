"""版本化持久化快照、原子串行写入与断点恢复。

目录布局::

    snapshot_dir/
      source.sha256           源图像内容哈希
      manifest.json.tmp/...   原子替换的清单
      batch_0000.npz ...      各批最终结果 + 内容哈希

写入规则：
  - 每个批文件先写 ``.tmp`` 再 ``os.replace``，且由执行器快照锁串行调用，
    写入不会交错。
  - 所有批完成后才原子写 manifest；进程在 manifest 落盘前被强杀时，
    重启找不到完整 manifest，整体作废并从源图像重算。
加载规则：
  - 算法版本、配置版本、参数版本、源哈希、任一批内容哈希不一致，
    整个快照作废，禁止部分混用旧批缓存。
"""

from __future__ import annotations

import hashlib
import json
import os

import numpy as np

from .errors import CwbError, E_SNAPSHOT

ALGORITHM_VERSION = "cwb-algo-1.0.0"


def sha256_bytes(blob: bytes) -> str:
    return hashlib.sha256(blob).hexdigest()


def sha256_image(image: np.ndarray) -> str:
    return sha256_bytes(np.ascontiguousarray(image).tobytes())


class SnapshotStore:
    def __init__(self, directory: str):
        self.directory = directory
        os.makedirs(directory, exist_ok=True)
        self.manifest_path = os.path.join(directory, "manifest.json")

    # ---- 写 ----
    def init_run(self, source_hash: str, config_version: str,
                 param_version: int, num_batches: int,
                 output_depth: int, shape: tuple) -> None:
        """新转换开始：清掉旧的不完整/过期批，保证不混用。"""
        for name in os.listdir(self.directory):
            if name.startswith("batch_") or name.startswith("manifest"):
                os.remove(os.path.join(self.directory, name))
        with open(os.path.join(self.directory, "source.sha256"), "w") as fh:
            fh.write(source_hash)
        self._pending = {
            "algorithm_version": ALGORITHM_VERSION,
            "config_version": config_version,
            "param_version": param_version,
            "source_hash": source_hash,
            "num_batches": num_batches,
            "output_depth": output_depth,
            "shape": list(shape),
            "batches": {},
        }

    def write_batch(self, batch_id: int, row_start: int, row_end: int,
                    data: np.ndarray) -> str:
        """串行写入单批（调用方持快照锁）。返回内容哈希。"""
        payload = np.ascontiguousarray(data).tobytes()
        digest = sha256_bytes(payload)
        tmp = os.path.join(self.directory, f"batch_{batch_id:04d}.npz.tmp")
        final = os.path.join(self.directory, f"batch_{batch_id:04d}.npz")
        np.savez(tmp, data=data, row_start=row_start, row_end=row_end,
                 sha256=digest)
        os.replace(_npz_name(tmp), final)
        self._pending["batches"][str(batch_id)] = {
            "row_start": row_start, "row_end": row_end, "sha256": digest}
        return digest

    def commit_manifest(self) -> None:
        tmp = self.manifest_path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump(self._pending, fh, sort_keys=True)
        os.replace(tmp, self.manifest_path)

    # ---- 读 / 校验 ----
    def load(self, source_hash: str, config_version: str,
             param_version: int) -> dict[int, np.ndarray]:
        """加载并整体校验；任一不匹配则整体作废并抛 E_SNAPSHOT。"""
        if not os.path.exists(self.manifest_path):
            self.discard()
            raise CwbError(E_SNAPSHOT, "无完整快照清单（可能被强制终止）")
        with open(self.manifest_path, encoding="utf-8") as fh:
            manifest = json.load(fh)
        checks = [
            (manifest.get("algorithm_version") == ALGORITHM_VERSION,
             "algorithm_version"),
            (manifest.get("config_version") == config_version,
             "config_version"),
            (manifest.get("param_version") == param_version,
             "param_version"),
            (manifest.get("source_hash") == source_hash, "source_hash"),
        ]
        for ok, label in checks:
            if not ok:
                self.discard()
                raise CwbError(E_SNAPSHOT,
                               f"快照 {label} 不匹配，整体作废并重算")
        loaded: dict[int, np.ndarray] = {}
        for bid_str, meta in manifest["batches"].items():
            bid = int(bid_str)
            path = os.path.join(self.directory, f"batch_{bid:04d}.npz")
            if not os.path.exists(path):
                self.discard()
                raise CwbError(E_SNAPSHOT, f"批 {bid} 文件缺失，整体作废")
            blob = np.load(path, allow_pickle=False)
            data = blob["data"]
            digest = sha256_bytes(np.ascontiguousarray(data).tobytes())
            if digest != meta["sha256"]:
                self.discard()
                raise CwbError(E_SNAPSHOT,
                               f"批 {bid} 内容哈希不匹配，整体作废")
            loaded[bid] = data
        if len(loaded) != manifest["num_batches"]:
            self.discard()
            raise CwbError(E_SNAPSHOT, "批数量不完整，整体作废")
        return loaded

    def discard(self) -> None:
        for name in os.listdir(self.directory):
            if name.startswith("batch_") or name.startswith("manifest") \
                    or name == "source.sha256":
                try:
                    os.remove(os.path.join(self.directory, name))
                except OSError:
                    pass


def _npz_name(path: str) -> str:
    # np.savez 自动补 .npz；统一封装避免散落魔法字符串
    return path if path.endswith(".npz") else path + ".npz"
