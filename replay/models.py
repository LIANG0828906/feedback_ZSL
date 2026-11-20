"""内部数据模型与确定性（规范化）序列化。

所有跨版本录像最终都归一为 :class:`NormalizedReplay`；状态哈希只来源于
规范化后的字段内容，固定字段顺序，不含时间戳或文件元数据。
"""
from __future__ import annotations
import json
from dataclasses import dataclass, field, asdict
from typing import Any

import numpy as np

# 世界坐标边界（含端点），越界坐标注入在入口处即被拦截。
WORLD_LIMIT = 1000.0

# 支持的操作类型（权威状态机只认这几类，保证跨版本语义一致）。
OP_MOVE = "move"
OP_KILL = "kill"
OP_REVIVE = "revive"
OP_SPEND = "spend"
OP_SUPPLY = "supply"
VALID_OP_TYPES = frozenset({OP_MOVE, OP_KILL, OP_REVIVE, OP_SPEND, OP_SUPPLY})

# 规范化状态字段的固定根顺序（字段路径内部再按字典序）。
STATE_ROOT_ORDER = ("entities", "resources")


def canonical_dumps(obj: Any) -> bytes:
    """规范化 JSON 序列化：sort_keys + 固定分隔符 + 无空白，结果逐字节确定。"""
    return json.dumps(
        obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False
    ).encode("utf-8")


@dataclass(frozen=True)
class Operation:
    """一条玩家操作。

    client_seq: 玩家客户端内单调序号，用于重复判定（同玩家同序号只首次生效）。
    arrival_seq: 服务器到达序号（全局唯一），是稳定结算顺序的最终仲裁键。
    tick: 声明的目标时间步。
    """

    player_id: int
    client_seq: int
    arrival_seq: int
    tick: int
    op_type: str
    payload: dict = field(default_factory=dict)

    @property
    def key(self) -> tuple[int, int]:
        return (self.player_id, self.client_seq)

    def sort_key(self) -> tuple[int, int, int]:
        # 稳定顺序：到达序号 -> 玩家 -> 客户端序号，绝不依赖输入解析顺序。
        return (self.arrival_seq, self.player_id, self.client_seq)

    def to_dict(self) -> dict:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict) -> "Operation":
        return cls(
            player_id=int(data["player_id"]),
            client_seq=int(data["client_seq"]),
            arrival_seq=int(data["arrival_seq"]),
            tick=int(data["tick"]),
            op_type=str(data["op_type"]),
            payload=dict(data.get("payload", {})),
        )


@dataclass
class RoomState:
    """权威房间状态：实体位置/速度/存活标记 + 房间资源计数。"""

    tick: int
    # entity_id -> {"pos": [x, y], "vel": [vx, vy], "alive": bool, "owner": player_id}
    entities: dict[str, dict] = field(default_factory=dict)
    # resource name -> integer count
    resources: dict[str, int] = field(default_factory=dict)

    def clone(self) -> "RoomState":
        return RoomState(
            tick=self.tick,
            entities=json.loads(json.dumps(self.entities)),
            resources=dict(self.resources),
        )

    def canonical_payload(self) -> dict:
        """按固定根顺序返回可哈希内容（tick 也纳入状态内容）。"""
        entities: dict[str, dict] = {}
        for entity_id in sorted(self.entities):
            src = self.entities[entity_id]
            entities[entity_id] = {
                "alive": bool(src["alive"]),
                "owner": int(src["owner"]),
                "pos": [float(src["pos"][0]), float(src["pos"][1])],
                "vel": [float(src["vel"][0]), float(src["vel"][1])],
            }
        resources = {name: int(value) for name, value in sorted(self.resources.items())}
        return {
            "tick": int(self.tick),
            "entities": entities,
            "resources": resources,
        }

    def canonical_bytes(self) -> bytes:
        return canonical_dumps(self.canonical_payload())

    def to_dict(self) -> dict:
        return self.canonical_payload()

    @classmethod
    def from_dict(cls, data: dict) -> "RoomState":
        return cls(
            tick=int(data["tick"]),
            entities=json.loads(json.dumps(data["entities"])),
            resources=dict(data["resources"]),
        )


@dataclass
class NormalizedReplay:
    """三版本录像归一后的内部表示。"""

    version: int
    room_id: str
    tick_rate: int
    total_ticks: int
    late_window: int
    capabilities: list[str]
    field_order: list[str]
    initial_state: RoomState
    operations: list[Operation]
    # v2/v3 携带的每步快照（tick -> RoomState）；v1 为空，由操作重放推导。
    snapshots: dict[int, RoomState] = field(default_factory=dict)
    # tick -> [event dict]，v2/v3 携带，仅审计用，不参与状态推进。
    events: dict[int, list] = field(default_factory=dict)
    # v3 原始差分记录（tick -> diff record），保留用于差分重建验证。
    diffs: dict[int, dict] = field(default_factory=dict)
    # 每个快照来源标注：recorded（录像携带）或 derived（操作重放推导）。
    snapshot_sources: dict[int, str] = field(default_factory=dict)

    def version_distribution(self) -> dict[str, int]:
        return {f"v{self.version}": self.total_ticks}


def move_entity(entity: dict, dx: float, dy: float) -> None:
    """确定性移动：位置与速度使用 numpy 浮点，位置在世界边界内裁剪到边界。"""
    pos = np.asarray(entity["pos"], dtype=np.float64)
    delta = np.asarray([dx, dy], dtype=np.float64)
    new_pos = pos + delta
    new_pos = np.clip(new_pos, -WORLD_LIMIT, WORLD_LIMIT)
    entity["vel"] = [float(delta[0]), float(delta[1])]
    entity["pos"] = [float(new_pos[0]), float(new_pos[1])]
