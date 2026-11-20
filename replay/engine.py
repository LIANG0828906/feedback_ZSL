"""操作缓冲与固定时间步确定性结算器。

操作按玩家分别缓存（带客户端序号与到达序号）。每个时间步边界按
(arrival_seq, player_id, client_seq) 稳定顺序取出应用：同玩家重复客户端序号
仅首个生效；目标 tick 早于当前窗口下界的操作以 E_OP_LATE 拒绝并记录。
结算结果只依赖操作内容，不依赖解析/输入顺序。
"""
from __future__ import annotations

from dataclasses import dataclass, field

from .errors import ErrorCode, ReplayError
from .models import (
    RoomState,
    Operation,
    move_entity,
    OP_MOVE,
    OP_KILL,
    OP_REVIVE,
    OP_SPEND,
    OP_SUPPLY,
)


@dataclass
class RejectedOp:
    code: str
    operation: Operation
    reason: str

    def to_dict(self) -> dict:
        return {"code": self.code, "reason": self.reason, "op": self.operation.to_dict()}


@dataclass
class TickReport:
    tick: int
    applied: list[Operation] = field(default_factory=list)
    rejected: list[RejectedOp] = field(default_factory=list)
    water_level: int = 0
    state_hash: str | None = None
    prev_hash: str | None = None

    def applied_keys(self) -> list[str]:
        return [f"p{op.player_id}#c{op.client_seq}:{op.op_type}" for op in self.applied]


class OperationBuffer:
    """分玩家操作缓冲，负责重复去重与水位统计。"""

    def __init__(self) -> None:
        self._pending: list[Operation] = []
        self._seen: set[tuple[int, int]] = set()
        self.duplicates: list[Operation] = []

    def add(self, op: Operation) -> bool:
        """加入缓冲；重复客户端序号返回 False（仅首个保留）。"""
        if op.key in self._seen:
            self.duplicates.append(op)
            return False
        self._seen.add(op.key)
        self._pending.append(op)
        return True

    def drain_due(self, current_tick: int, late_window: int) -> tuple[list[Operation], list[RejectedOp]]:
        due: list[Operation] = []
        rejected: list[RejectedOp] = []
        window_lower = current_tick - late_window
        remaining: list[Operation] = []
        for op in self._pending:
            if op.tick < window_lower:
                rejected.append(
                    RejectedOp(
                        code=ErrorCode.E_OP_LATE.value,
                        operation=op,
                        reason=f"op tick {op.tick} below window lower bound {window_lower} at tick {current_tick}",
                    )
                )
            elif op.tick <= current_tick:
                due.append(op)
            else:
                remaining.append(op)
        due.sort(key=Operation.sort_key)
        self._pending = remaining
        return due, rejected

    def water_level(self) -> int:
        return len(self._pending)

    def snapshot(self) -> dict:
        return {
            "pending": [op.to_dict() for op in sorted(self._pending, key=Operation.sort_key)],
            "seen": sorted(list(self._seen)),
            "water_level": len(self._pending),
        }

    @classmethod
    def restore(cls, data: dict) -> "OperationBuffer":
        buffer = cls()
        buffer._pending = [Operation.from_dict(item) for item in data.get("pending", [])]
        buffer._seen = {tuple(pair) for pair in data.get("seen", [])}
        return buffer


class DeterministicEngine:
    """权威状态机：固定时间步推进 + 哈希链计算。"""

    GENESIS_HASH = "0" * 64

    def __init__(self, initial_state: RoomState, late_window: int) -> None:
        self.state = initial_state.clone()
        self.late_window = late_window
        self.buffer = OperationBuffer()
        self.current_tick = initial_state.tick
        self.prev_hash = self.GENESIS_HASH
        self.incoming_prev_hash = self.GENESIS_HASH
        self.state_hash = self.GENESIS_HASH
        self.reports: list[TickReport] = []

    def ingest(self, operations: list[Operation]) -> None:
        # 入口先按稳定键排序，保证即使输入乱序，缓冲内容也确定。
        for op in sorted(operations, key=Operation.sort_key):
            self.buffer.add(op)

    def step(self) -> TickReport:
        self.current_tick += 1
        due, rejected = self.buffer.drain_due(self.current_tick, self.late_window)
        incoming_prev_hash = self.prev_hash
        self.incoming_prev_hash = incoming_prev_hash
        report = TickReport(tick=self.current_tick, rejected=rejected, prev_hash=incoming_prev_hash)
        for op in due:
            self._apply(op)
            report.applied.append(op)
        self.state.tick = self.current_tick
        state_hash = self.compute_hash()
        report.state_hash = state_hash
        self.state_hash = state_hash
        report.water_level = self.buffer.water_level()
        self.prev_hash = state_hash
        self.reports.append(report)
        return report

    def compute_hash(self) -> str:
        """状态哈希只来自规范化字段内容 + 上一步链摘要（链式）。"""
        import hashlib

        body = self.state.canonical_bytes()
        return hashlib.sha256(self.prev_hash.encode("ascii") + body).hexdigest()

    def _apply(self, op: Operation) -> None:
        if op.op_type == OP_MOVE:
            entity = self._require_entity(op, "entity_id")
            if not entity["alive"]:
                return
            dx = float(op.payload.get("dx", 0.0))
            dy = float(op.payload.get("dy", 0.0))
            move_entity(entity, dx, dy)
        elif op.op_type == OP_KILL:
            target = self._require_entity(op, "target_id")
            target["alive"] = False
            target["vel"] = [0.0, 0.0]
        elif op.op_type == OP_REVIVE:
            target = self._require_entity(op, "target_id")
            target["alive"] = True
        elif op.op_type == OP_SPEND:
            name = str(op.payload["resource"])
            amount = int(op.payload["amount"])
            if amount < 0:
                raise ReplayError(
                    ErrorCode.E_FORMAT_INVALID,
                    "spend amount must be non-negative",
                    field_path="payload.amount",
                )
            current = int(self.state.resources.get(name, 0))
            self.state.resources[name] = current - amount
        elif op.op_type == OP_SUPPLY:
            name = str(op.payload["resource"])
            amount = int(op.payload["amount"])
            if amount < 0:
                raise ReplayError(
                    ErrorCode.E_FORMAT_INVALID,
                    "supply amount must be non-negative",
                    field_path="payload.amount",
                )
            current = int(self.state.resources.get(name, 0))
            self.state.resources[name] = current + amount

    def _require_entity(self, op: Operation, key: str) -> dict:
        entity_id = op.payload.get(key)
        if entity_id is None or entity_id not in self.state.entities:
            raise ReplayError(
                ErrorCode.E_FORMAT_INVALID,
                f"unknown entity '{entity_id}'",
                field_path=f"payload.{key}",
            )
        return self.state.entities[entity_id]
