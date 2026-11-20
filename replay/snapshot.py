"""快照原子提交、哈希链校验与恢复。

提交策略：先写同目录 ``*.tmp`` 临时文件，fsync 后 ``os.replace`` 原子重命名；
因此在途回合（尚未执行完的回合）永远不会出现在正式快照路径，不会污染状态。
恢复时从回合 1 到最近快照逐节点验证哈希链，任一不符返回 E_RECOVERY_INVALID。
"""

from __future__ import annotations

from pathlib import Path

from .errors import E_RECOVERY_INVALID, ReplayError
from .hashing import GENESIS_HASH, chain_hash, hash_json, read_json, write_json_atomic
from .model import ResourceUsage


class SnapshotStore:
    def __init__(self, task_dir: str | Path) -> None:
        self.task_dir = Path(task_dir)
        self.snapshot_dir = self.task_dir / "snapshots"
        self.snapshot_dir.mkdir(parents=True, exist_ok=True)

    @staticmethod
    def snapshot_filename(turn: int) -> str:
        return f"snapshot_{turn:06d}.json"

    def path_for(self, turn: int) -> Path:
        return self.snapshot_dir / self.snapshot_filename(turn)
    def commit(self, snapshot: dict) -> Path:
        turn = int(snapshot["turn"])
        return write_json_atomic(self.path_for(turn), snapshot)

    def list_turns(self) -> list[int]:
        turns = []
        for path in self.snapshot_dir.glob("snapshot_*.json"):
            stem = path.stem  # snapshot_000003
            try:
                turns.append(int(stem.split("_")[1]))
            except (IndexError, ValueError):
                continue
        return sorted(turns)

    def load(self, turn: int) -> dict:
        path = self.path_for(turn)
        if not path.exists():
            raise ReplayError(
                E_RECOVERY_INVALID, f"快照缺失: {path.name}"
            )
        return read_json(path)

    def latest_committed_turn(self) -> int | None:
        turns = self.list_turns()
        return turns[-1] if turns else None

    def validate_chain(self) -> list[dict]:
        """校验全部已提交快照的哈希链与内容哈希，返回有序快照列表。"""
        turns = self.list_turns()
        expected_turns = list(range(1, len(turns) + 1))
        if turns != expected_turns:
            raise ReplayError(
                E_RECOVERY_INVALID,
                f"快照回合序列不连续: 期望 {expected_turns} 实际 {turns}",
            )
        prev_hash = GENESIS_HASH
        snapshots: list[dict] = []
        for turn in turns:
            snapshot = self.load(turn)
            if int(snapshot["turn"]) != turn:
                raise ReplayError(
                    E_RECOVERY_INVALID, f"快照 {turn} 内部回合号不符"
                )
            if snapshot.get("prev_snapshot_hash") != prev_hash:
                raise ReplayError(
                    E_RECOVERY_INVALID,
                    f"快照 {turn} 前驱哈希断裂",
                )
            expected_hash = chain_hash(
                prev_hash,
                {
                    "turn": turn,
                    "state_hash": snapshot["state_hash"],
                    "event_prefix_hash": snapshot["event_prefix_hash"],
                    "resource_usage": snapshot["resource_usage"],
                },
            )
            if snapshot.get("snapshot_hash") != expected_hash:
                raise ReplayError(
                    E_RECOVERY_INVALID, f"快照 {turn} 哈希链校验失败"
                )
            if hash_json(snapshot["state"]) != snapshot["state_hash"]:
                raise ReplayError(
                    E_RECOVERY_INVALID, f"快照 {turn} 状态内容哈希不符"
                )
            if (
                snapshot["state"].get("event_prefix_hash")
                != snapshot["event_prefix_hash"]
            ):
                raise ReplayError(
                    E_RECOVERY_INVALID, f"快照 {turn} 事件流前缀哈希不符"
                )
            prev_hash = expected_hash
            snapshots.append(snapshot)
        return snapshots

    def recovery_point(self) -> tuple[dict, ResourceUsage] | None:
        """返回最近可用已提交快照与累计资源用量；无快照返回 None。"""
        snapshots = self.validate_chain()
        if not snapshots:
            return None
        latest = snapshots[-1]
        return latest, ResourceUsage.from_dict(latest["resource_usage"])
