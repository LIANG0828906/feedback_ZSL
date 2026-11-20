"""基线生成与回放结果验证比对。

比对依据只有版本号与内容哈希/字段值，不使用文件大小或时间戳。
版本不一致直接 E_BASELINE_MISMATCH 且不执行任何逐回合比对。
发现差异时报告第一个不一致的回合与字段路径。
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from .config import REPLAYER_VERSION
from .engine import RuleEngine
from .errors import E_BASELINE_MISMATCH, E_RECOVERY_INVALID
from .hashing import hash_json, read_json, write_json_atomic
from .model import MatchDescription
from .snapshot import SnapshotStore


def generate_baseline(
    desc: MatchDescription,
    compute_budget: int,
    memory_budget: int,
    baseline_path: str | Path,
) -> Path:
    """以规则引擎离线完整执行一次，生成权威基线（真值来源）。"""
    engine = RuleEngine(
        desc, compute_budget=compute_budget, memory_budget=memory_budget
    )
    turns: list[dict] = []
    all_events: list[dict] = []
    for _ in range(desc.turn_count):
        snapshot = engine.run_turn()
        all_events.extend(snapshot["events"])
        turns.append(
            {
                "turn": snapshot["turn"],
                "state_hash": snapshot["state_hash"],
                "event_prefix_hash": snapshot["event_prefix_hash"],
                "snapshot_hash": snapshot["snapshot_hash"],
            }
        )
    baseline = {
        "version": REPLAYER_VERSION,
        "match_id": desc.match_id,
        "rng_seed": desc.rng_seed,
        "turn_count": desc.turn_count,
        "verdict": engine.verdict(),
        "event_stream_hash": hash_json(all_events),
        "turns": turns,
    }
    return write_json_atomic(baseline_path, baseline)


@dataclass
class VerificationReport:
    code: str
    match_id: str
    passed: bool
    first_mismatch_turn: int | None = None
    field_path: str | None = None
    expected: object = None
    actual: object = None
    message: str = ""

    def to_dict(self) -> dict:
        return {
            "code": self.code,
            "match_id": self.match_id,
            "passed": self.passed,
            "first_mismatch_turn": self.first_mismatch_turn,
            "field_path": self.field_path,
            "expected": self.expected,
            "actual": self.actual,
            "message": self.message,
        }


def verify(
    match_id: str,
    task_dir: str | Path,
    baseline_path: str | Path,
) -> VerificationReport:
    baseline_path = Path(baseline_path)
    if not baseline_path.exists():
        return VerificationReport(
            E_RECOVERY_INVALID, match_id, False,
            message=f"基线文件缺失: {baseline_path}",
        )
    baseline = read_json(baseline_path)

    # 版本闸门：版本不同直接返回，不执行后续比对。
    if baseline.get("version") != REPLAYER_VERSION:
        return VerificationReport(
            E_BASELINE_MISMATCH,
            match_id,
            False,
            field_path="version",
            expected=baseline.get("version"),
            actual=REPLAYER_VERSION,
            message="基线版本与回放器版本不匹配，拒绝比对",
        )

    task_dir = Path(task_dir)
    result_path = task_dir / "result.json"
    if not result_path.exists():
        return VerificationReport(
            E_RECOVERY_INVALID,
            match_id,
            False,
            message="回放结果 result.json 不存在，任务可能未完成",
        )
    result = read_json(result_path)
    store = SnapshotStore(task_dir)
    # 恢复/校验快照哈希链，链断则无法信任快照哈希。
    snapshots = store.validate_chain()

    # 1) 判定结果比对。
    if result["verdict"] != baseline["verdict"]:
        field = _first_dict_diff(result["verdict"], baseline["verdict"], "verdict")
        return _mismatch(match_id, 0, field, baseline["verdict"], result["verdict"])

    # 2) 事件流总哈希比对。
    if result["event_stream_hash"] != baseline["event_stream_hash"]:
        return _mismatch(
            match_id,
            0,
            "event_stream_hash",
            baseline["event_stream_hash"],
            result["event_stream_hash"],
        )

    # 3) 逐回合快照哈希比对，报告第一个不一致回合与字段路径。
    if len(snapshots) != len(baseline["turns"]):
        return _mismatch(
            match_id,
            min(len(snapshots), len(baseline["turns"])) + 1,
            "turns.length",
            len(baseline["turns"]),
            len(snapshots),
        )
    for index, (snapshot, base_turn) in enumerate(
        zip(snapshots, baseline["turns"]), start=1
    ):
        turn = base_turn["turn"]
        for field_name in ("state_hash", "event_prefix_hash", "snapshot_hash"):
            if snapshot.get(field_name) != base_turn.get(field_name):
                return _mismatch(
                    match_id,
                    turn,
                    f"turns[{index}].{field_name}",
                    base_turn.get(field_name),
                    snapshot.get(field_name),
                )

    return VerificationReport("OK", match_id, True, message="全部比对通过")


def _mismatch(
    match_id: str,
    turn: int,
    field_path: str,
    expected: object,
    actual: object,
) -> VerificationReport:
    return VerificationReport(
        "E_VERIFICATION_FAILED",
        match_id,
        False,
        first_mismatch_turn=turn,
        field_path=field_path,
        expected=expected,
        actual=actual,
        message=f"首个差异位于回合 {turn} 字段 {field_path}",
    )


def _first_dict_diff(actual: dict, expected: dict, prefix: str) -> str:
    for key in sorted(set(actual) | set(expected)):
        if actual.get(key) != expected.get(key):
            return f"{prefix}.{key}"
    return prefix
