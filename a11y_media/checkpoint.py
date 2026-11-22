"""检查点持久化: 原子、串行、版本校验。

每个已提交分段把该段台词窗口、字符排程、误差统计、工程内容哈希、
时间单位参数版本与算法版本写入本地持久化存储:
- 原子: 先写临时文件再 os.replace, 崩溃不会留下半文件;
- 串行: 全局写锁保证写入不交错;
- 恢复: 重启加载时若内容哈希或任一版本不匹配, 整体作废快照并全量重算,
  禁止部分混用旧分段结果。
"""

from __future__ import annotations

import json
import os
import shutil
import threading

from .timebase import ALGO_VERSION, TIME_UNIT_VERSION

_WRITE_LOCK = threading.Lock()


class CheckpointStore:
    def __init__(self, root: str):
        self.root = root
        os.makedirs(root, exist_ok=True)

    def _dir(self, content_hash: str) -> str:
        return os.path.join(self.root, content_hash[:16])

    @staticmethod
    def _atomic_write(path: str, payload: dict) -> None:
        tmp = f"{path}.tmp.{os.getpid()}.{threading.get_ident()}"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(payload, f, ensure_ascii=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)

    def save_chunk(self, content_hash: str, chunk: int, result: dict) -> None:
        """原子且串行地写入一个已提交分段。"""
        payload = {
            "content_hash": content_hash,
            "time_unit_version": TIME_UNIT_VERSION,
            "algo_version": ALGO_VERSION,
            "chunk": chunk,
            "result": result,
        }
        with _WRITE_LOCK:
            d = self._dir(content_hash)
            os.makedirs(d, exist_ok=True)
            self._atomic_write(os.path.join(d, f"chunk_{chunk:05d}.json"), payload)

    def load_snapshot(self, content_hash: str) -> dict | None:
        """加载快照; 哈希或版本不匹配 -> 整体作废并返回 None。"""
        d = self._dir(content_hash)
        if not os.path.isdir(d):
            return None
        chunks = {}
        try:
            for name in sorted(os.listdir(d)):
                if not name.startswith("chunk_") or not name.endswith(".json"):
                    continue
                with open(os.path.join(d, name), "r", encoding="utf-8") as f:
                    payload = json.load(f)
                if (
                    payload.get("content_hash") != content_hash
                    or payload.get("time_unit_version") != TIME_UNIT_VERSION
                    or payload.get("algo_version") != ALGO_VERSION
                ):
                    self.invalidate(content_hash)
                    return None
                chunks[payload["chunk"]] = payload["result"]
        except (OSError, json.JSONDecodeError):
            self.invalidate(content_hash)
            return None
        return chunks

    def invalidate(self, content_hash: str) -> None:
        """整体作废快照(禁止部分混用旧分段结果)。"""
        with _WRITE_LOCK:
            d = self._dir(content_hash)
            if os.path.isdir(d):
                shutil.rmtree(d, ignore_errors=True)
