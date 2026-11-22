"""检查点机制：可暂停、可退出、可重启续跑.

每次小节提交把 {模型片段, 表达事件快照, 内容哈希, 参数版本, 算法版本}
原子（tmp + rename + fsync）且串行（单锁）写入本地持久化目录，
提交顺序由 seq 严格递增保证。

重启加载时校验 manifest 与每个片段的版本与内容哈希：
任一不匹配则整体快照作废并从源样本重新加载，禁止部分混用旧片段。
"""

from __future__ import annotations

import json
import os
import threading
from pathlib import Path

from .errors import CheckpointError
from .hashing import content_hash
from .model import Score
from .units import ALGO_VERSION, PARAM_VERSION

MANIFEST_NAME = "manifest.json"


def _atomic_write_json(path: Path, obj) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(obj, fh, indent=2, ensure_ascii=False)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


class CheckpointStore:
    def __init__(self, root, project_id: str):
        self.dir = Path(root) / project_id
        self.dir.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    @property
    def manifest_path(self) -> Path:
        return self.dir / MANIFEST_NAME

    def _fragment_path(self, seq: int) -> Path:
        return self.dir / f"fragment_{seq:05d}.json"

    def write_fragment(self, record: dict) -> None:
        """record 含 seq/part_id/measure_index/fragment/content_hash/版本号。
        调用方保证 seq 递增；锁内串行落盘，写入不交错."""
        with self._lock:
            seq = record["seq"]
            manifest = self._read_manifest() or {
                "project_id": self.dir.name,
                "param_version": PARAM_VERSION,
                "algo_version": ALGO_VERSION,
                "commits": [],
            }
            if manifest["commits"] and seq != manifest["commits"][-1]["seq"] + 1:
                raise CheckpointError(
                    f"commit seq {seq} out of order, "
                    f"expected {manifest['commits'][-1]['seq'] + 1}",
                    path=str(self.manifest_path),
                )
            _atomic_write_json(self._fragment_path(seq), record)
            manifest["commits"].append({
                "seq": seq,
                "part_id": record["part_id"],
                "measure_index": record["measure_index"],
                "content_hash": record["content_hash"],
                "file": self._fragment_path(seq).name,
            })
            _atomic_write_json(self.manifest_path, manifest)

    def _read_manifest(self) -> dict | None:
        if not self.manifest_path.exists():
            return None
        try:
            return json.loads(self.manifest_path.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError):
            return None

    def load_validated(self) -> list[dict] | None:
        """返回按 seq 排序的片段列表；任一校验失败则整体作废并返回 None."""
        with self._lock:
            manifest = self._read_manifest()
            if manifest is None:
                return None
            if (manifest.get("param_version") != PARAM_VERSION
                    or manifest.get("algo_version") != ALGO_VERSION):
                self._invalidate_locked(
                    f"version mismatch: manifest "
                    f"param={manifest.get('param_version')} algo={manifest.get('algo_version')} "
                    f"vs current param={PARAM_VERSION} algo={ALGO_VERSION}"
                )
                return None
            fragments = []
            try:
                for entry in sorted(manifest["commits"], key=lambda e: e["seq"]):
                    record = json.loads(
                        (self.dir / entry["file"]).read_text(encoding="utf-8")
                    )
                    if (record.get("param_version") != PARAM_VERSION
                            or record.get("algo_version") != ALGO_VERSION):
                        raise CheckpointError("fragment version mismatch")
                    actual = content_hash(record["fragment"])
                    if actual != record["content_hash"] or actual != entry["content_hash"]:
                        raise CheckpointError("fragment content hash mismatch")
                    fragments.append(record["fragment"])
            except (OSError, json.JSONDecodeError, KeyError, CheckpointError) as exc:
                self._invalidate_locked(f"snapshot invalidated: {exc}")
                return None
            return fragments

    def _invalidate_locked(self, reason: str) -> None:
        """整体作废：删除全部片段与 manifest，禁止部分混用旧片段."""
        for path in self.dir.glob("fragment_*.json"):
            path.unlink(missing_ok=True)
        self.manifest_path.unlink(missing_ok=True)
        self.last_invalidation_reason = reason

    def status(self) -> dict:
        manifest = self._read_manifest()
        return {
            "dir": str(self.dir),
            "commits": len(manifest["commits"]) if manifest else 0,
            "valid": manifest is not None,
        }


def restore_session(project_data: dict, store: CheckpointStore):
    """从检查点恢复编辑会话；快照无效时从源样本重新加载."""
    from .session import CommitRecord, EditSession

    score = Score(project_data)
    session = EditSession(score, store=store)
    fragments = store.load_validated()
    if fragments:
        for fragment in fragments:
            score.replace_measure(fragment)
            session.working.replace_measure(fragment)
            key = (fragment["part_id"], fragment["measure_index"])
            session.committed.add(key)
            session.commit_log.append(CommitRecord(
                key[0], key[1], seq=len(session.commit_log),
                content_hash=content_hash(fragment), fragment=fragment,
            ))
    return session
