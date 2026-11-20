"""原子快照提交与强制终止后的恢复。

每个时间步先写临时文件再 ``os.replace`` 原子重命名，快照内容包含步号、状态、
操作缓冲水位与哈希链前缀摘要。恢复只信任磁盘上已完整提交（重命名成功）的快照；
字段缺失、哈希链断链或版本号不符时抛 E_RECOVERY_INVALID 并定位首个断链步。

快照同时保存 ``prev_hash``（入边）与 ``state_hash``（出边）。链校验分两层，
与快照落盘间隔无关：
1. ``state_hash == sha256(prev_hash + 规范化状态内容)``；
2. 相邻快照 ``上一.state_hash == 当前.prev_hash``，首快照锚定创世摘要。
"""
from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path

from .engine import DeterministicEngine
from .errors import ErrorCode, ReplayError
from .models import Operation, RoomState, canonical_dumps

SNAPSHOT_PREFIX = "snapshot_"
SNAPSHOT_SUFFIX = ".json"
TMP_SUFFIX = ".tmp"
SNAPSHOT_SCHEMA_VERSION = 1


def _recompute_hash(prev_hash: str, state: RoomState) -> str:
    return hashlib.sha256(prev_hash.encode("ascii") + state.canonical_bytes()).hexdigest()


def snapshot_path(directory: str | Path, tick: int) -> Path:
    return Path(directory) / f"{SNAPSHOT_PREFIX}{tick:08d}{SNAPSHOT_SUFFIX}"


@dataclass
class RecoveryResult:
    engine: DeterministicEngine
    last_tick: int
    source_tick: int


def build_snapshot_payload(engine: DeterministicEngine, late_window: int) -> dict:
    state_hash = engine.state_hash
    return {
        "snapshot_schema_version": SNAPSHOT_SCHEMA_VERSION,
        "tick": engine.current_tick,
        "state": engine.state.canonical_payload(),
        "buffer": engine.buffer.snapshot(),
        "water_level": engine.buffer.water_level(),
        # chain_prefix / prev_hash 都是本步入边（上一步 state_hash）。
        "chain_prefix": engine.incoming_prev_hash,
        "prev_hash": engine.incoming_prev_hash,
        "state_hash": state_hash,
        "late_window": late_window,
    }


def write_snapshot_atomic(directory: str | Path, engine: DeterministicEngine, late_window: int) -> Path:
    """临时文件 + 原子重命名；返回最终快照路径。"""
    out_dir = Path(directory)
    out_dir.mkdir(parents=True, exist_ok=True)
    final = snapshot_path(out_dir, engine.current_tick)
    tmp = out_dir / f".{final.name}{TMP_SUFFIX}"
    payload = build_snapshot_payload(engine, late_window)
    body = canonical_dumps(payload)
    with open(tmp, "wb") as handle:
        handle.write(body)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, final)
    return final


def list_committed_snapshots(directory: str | Path) -> list[Path]:
    out_dir = Path(directory)
    if not out_dir.exists():
        return []
    return sorted(
        path
        for path in out_dir.iterdir()
        if path.name.startswith(SNAPSHOT_PREFIX) and path.name.endswith(SNAPSHOT_SUFFIX)
    )


def _load_raw(path: Path) -> dict:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError) as exc:
        raise ReplayError(
            ErrorCode.E_RECOVERY_INVALID,
            f"unreadable snapshot {path.name}: {exc}",
            field_path=str(path),
        ) from exc


def _validate_payload(payload: dict, path: Path) -> None:
    required = (
        "snapshot_schema_version",
        "tick",
        "state",
        "buffer",
        "water_level",
        "chain_prefix",
        "prev_hash",
        "state_hash",
    )
    for key in required:
        if key not in payload:
            raise ReplayError(
                ErrorCode.E_RECOVERY_INVALID,
                f"snapshot missing field '{key}'",
                field_path=f"{path}#{key}",
            )
    if payload["snapshot_schema_version"] != SNAPSHOT_SCHEMA_VERSION:
        raise ReplayError(
            ErrorCode.E_RECOVERY_INVALID,
            f"snapshot schema version mismatch: {payload['snapshot_schema_version']}",
            record_index=int(payload.get("tick", -1)),
            field_path=f"{path}#snapshot_schema_version",
        )
    state = payload["state"]
    for key in ("tick", "entities", "resources"):
        if key not in state:
            raise ReplayError(
                ErrorCode.E_RECOVERY_INVALID,
                f"snapshot state missing '{key}'",
                record_index=int(payload["tick"]),
                field_path=f"{path}#state.{key}",
            )
    if int(payload["water_level"]) != len(payload["buffer"].get("pending", [])):
        raise ReplayError(
            ErrorCode.E_RECOVERY_INVALID,
            "water_level inconsistent with pending buffer length",
            record_index=int(payload["tick"]),
            field_path=f"{path}#water_level",
        )


def verify_and_recover(
    directory: str | Path,
    initial_state: RoomState,
    operations: list[Operation],
    late_window: int,
) -> RecoveryResult | None:
    """从初始状态确定性重放，校验磁盘快照的完整哈希链并从最近快照恢复。

    支持间隔落盘：重放覆盖每个快照之间的所有中间步，因此能在首个断链步精确
    中止。校验通过后，引擎状态重置为最近快照（含操作缓冲），不重复应用已结算操作。
    """
    paths = list_committed_snapshots(directory)
    if not paths:
        return None
    on_disk: dict[int, dict] = {}
    for path in paths:
        payload = _load_raw(path)
        _validate_payload(payload, path)
        on_disk[int(payload["tick"])] = payload

    engine = DeterministicEngine(initial_state, late_window)
    engine.ingest(operations)
    latest_payload: dict | None = None
    for tick in range(1, max(on_disk) + 1):
        report = engine.step()
        if tick not in on_disk:
            continue
        payload = on_disk[tick]
        if report.prev_hash != str(payload["prev_hash"]):
            raise ReplayError(
                ErrorCode.E_RECOVERY_INVALID,
                (
                    f"hash chain broken at tick {tick}: prev_hash {str(payload['prev_hash'])[:12]}... "
                    f"!= replayed {report.prev_hash[:12]}..."
                ),
                record_index=tick,
                field_path=f"snapshot_{tick:08d}#prev_hash",
            )
        if report.state_hash != str(payload["state_hash"]):
            raise ReplayError(
                ErrorCode.E_RECOVERY_INVALID,
                (
                    f"state hash mismatch at tick {tick}: snapshot {str(payload['state_hash'])[:12]}... "
                    f"!= replayed {report.state_hash[:12]}..."
                ),
                record_index=tick,
                field_path=f"snapshot_{tick:08d}#state_hash",
            )
        latest_payload = payload

    assert latest_payload is not None
    # 用最近磁盘快照重建引擎（携带其操作缓冲水位），保证从已提交点继续。
    state = RoomState.from_dict(latest_payload["state"])
    recovered = DeterministicEngine(state, late_window)
    from .engine import OperationBuffer

    recovered.buffer = OperationBuffer.restore(latest_payload["buffer"])
    recovered.current_tick = int(latest_payload["tick"])
    recovered.prev_hash = str(latest_payload["state_hash"])
    recovered.incoming_prev_hash = str(latest_payload["prev_hash"])
    recovered.state_hash = str(latest_payload["state_hash"])
    return RecoveryResult(
        engine=recovered,
        last_tick=recovered.current_tick,
        source_tick=int(latest_payload["tick"]),
    )
