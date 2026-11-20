"""强制终止恢复：快照哈希链与最近有效快照加载。"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path

from .errors import RecoveryInvalidError

GENESIS = "GENESIS"
REQUIRED_FIELDS = ("seq", "input_hash", "config_hash", "completed", "prev_hash", "hash")


def _snapshot_hash(body: dict) -> str:
    blob = json.dumps(body, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


class SnapshotStore:
    def __init__(self, snap_dir):
        self.dir = Path(snap_dir)
        self.dir.mkdir(parents=True, exist_ok=True)

    def _path(self, seq: int) -> Path:
        return self.dir / f"snap_{seq:06d}.json"

    def write(self, seq: int, input_hash: str, config_hash: str, completed: dict) -> None:
        prev_hash = GENESIS
        if seq > 0:
            prev = self.load_one(seq - 1)
            prev_hash = prev["hash"]
        body = {
            "seq": seq,
            "input_hash": input_hash,
            "config_hash": config_hash,
            "completed": completed,
            "prev_hash": prev_hash,
        }
        body["hash"] = _snapshot_hash(body)
        tmp = self.dir / f"snap_{seq:06d}.json.tmp"
        with open(tmp, "w", encoding="utf-8", newline="\n") as fh:
            json.dump(body, fh, sort_keys=True, ensure_ascii=False)
        os.replace(tmp, self._path(seq))

    def load_one(self, seq: int) -> dict:
        with open(self._path(seq), "r", encoding="utf-8") as fh:
            return json.load(fh)

    def latest_seq(self):
        seqs = [
            int(p.stem.split("_")[1])
            for p in self.dir.glob("snap_*.json")
            if p.stem.split("_")[1].isdigit()
        ]
        return max(seqs) if seqs else None

    def load_valid_chain(self) -> dict:
        """加载并校验完整哈希链；断链或字段缺失时抛出 E_RECOVERY_INVALID。"""
        seq = self.latest_seq()
        if seq is None:
            raise RecoveryInvalidError("不存在可恢复快照")
        expected_prev = GENESIS
        snap = None
        for s in range(seq + 1):
            if not self._path(s).exists():
                raise RecoveryInvalidError("快照哈希链断链", missing_seq=s)
            snap = self.load_one(s)
            missing = [f for f in REQUIRED_FIELDS if f not in snap]
            if missing:
                raise RecoveryInvalidError("快照字段缺失", seq=s, missing=missing)
            if snap["seq"] != s or snap["prev_hash"] != expected_prev:
                raise RecoveryInvalidError("快照哈希链断链", seq=s)
            body = {k: snap[k] for k in REQUIRED_FIELDS if k != "hash"}
            if _snapshot_hash(body) != snap["hash"]:
                raise RecoveryInvalidError("快照内容哈希不匹配", seq=s)
            expected_prev = snap["hash"]
        return snap
