"""每步原子快照提交与恢复校验。

写入采用 临时文件 + fsync + os.replace 原子重命名：写入失败不会
覆盖上一有效快照。恢复只依赖磁盘已提交内容，逐节校验哈希链，
定位首个断链步号。
"""

from __future__ import annotations

import json
import os
import re

from .canon import GENESIS_CHAIN, chain_hash, hash_obj
from .errors import RecoveryInvalidError, SnapshotWriteError
from .models import SNAPSHOT_VERSION

_STEP_RE = re.compile(r"^step_(\d{6})\.json$")

REQUIRED_FIELDS = ("version", "state_version", "match_id", "step", "state",
                   "buffer", "buffer_watermark", "settled",
                   "quota_used_total", "state_hash", "prev_chain_hash",
                   "chain_hash", "hash_prefix_digest")


class SnapshotStore:
    def __init__(self, directory: str):
        self.directory = directory
        os.makedirs(directory, exist_ok=True)

    def _path(self, step: int) -> str:
        return os.path.join(self.directory, f"step_{step:06d}.json")

    def commit(self, payload: dict):
        """原子提交一步快照；失败抛 E_SNAPSHOT_WRITE 且不破坏旧快照。"""
        step = int(payload["step"])
        final = self._path(step)
        tmp = final + ".tmp"
        try:
            data = json.dumps(payload, sort_keys=True,
                              separators=(",", ":")).encode("utf-8")
            with open(tmp, "wb") as fh:
                fh.write(data)
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, final)
            dir_fd = os.open(self.directory, os.O_RDONLY)
            try:
                os.fsync(dir_fd)
            finally:
                os.close(dir_fd)
        except Exception as exc:  # noqa: BLE001 - 统一翻译为 E_SNAPSHOT_WRITE
            try:
                if os.path.exists(tmp):
                    os.unlink(tmp)
            except OSError:
                pass
            raise SnapshotWriteError(step, exc) from exc

    def steps(self) -> list[int]:
        found = []
        for name in os.listdir(self.directory):
            m = _STEP_RE.match(name)
            if m:
                found.append(int(m.group(1)))
        return sorted(found)

    def latest_step(self) -> int | None:
        steps = self.steps()
        return steps[-1] if steps else None

    def load(self, step: int) -> dict:
        with open(self._path(step), "rb") as fh:
            return json.loads(fh.read().decode("utf-8"))

    def validate_chain(self) -> dict:
        """从创世步逐节校验到最新快照，返回 tip 快照。

        字段缺失、版本不符、状态哈希与内容不符、链指针断链，
        均以 E_RECOVERY_INVALID 中止并定位首个断链步号。
        """
        latest = self.latest_step()
        if latest is None:
            raise RecoveryInvalidError("no committed snapshot found")
        prev_chain = GENESIS_CHAIN
        tip = None
        for step in range(0, latest + 1):
            if not os.path.exists(self._path(step)):
                raise RecoveryInvalidError("snapshot file missing", step=step)
            try:
                snap = self.load(step)
            except (OSError, json.JSONDecodeError) as exc:
                raise RecoveryInvalidError(f"unreadable snapshot: {exc}",
                                           step=step) from exc
            missing = [f for f in REQUIRED_FIELDS if f not in snap]
            if missing:
                raise RecoveryInvalidError(
                    f"missing fields: {missing}", step=step)
            if snap["version"] != SNAPSHOT_VERSION:
                raise RecoveryInvalidError(
                    f"snapshot version {snap['version']} != "
                    f"{SNAPSHOT_VERSION}", step=step)
            if snap["step"] != step:
                raise RecoveryInvalidError("step number mismatch", step=step)
            if hash_obj(snap["state"]) != snap["state_hash"]:
                raise RecoveryInvalidError("state hash mismatch", step=step)
            if snap["prev_chain_hash"] != prev_chain:
                raise RecoveryInvalidError("chain link broken", step=step)
            if chain_hash(prev_chain, snap["state_hash"]) != snap["chain_hash"]:
                raise RecoveryInvalidError("chain hash mismatch", step=step)
            prev_chain = snap["chain_hash"]
            tip = snap
        return tip
