"""战场状态模型与指令模型。

状态内部用 NumPy 整数数组承载位置 / 速度 / 存活标记，序列化时按固定
字段顺序展开为纯 JSON 对象，保证哈希真值只来自字段内容。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

STATE_VERSION = 1
SNAPSHOT_VERSION = 1
BASELINE_VERSION = 1

OPS = ("move", "stop", "strike")


@dataclass(frozen=True)
class Command:
    player: str
    client_seq: int
    submit_tick: int
    recv_seq: int
    recv_tick: int
    op: str
    unit: str
    dx: int
    dy: int
    cost: int

    @property
    def cmd_id(self) -> str:
        return f"{self.player}:{self.client_seq}"

    @classmethod
    def from_dict(cls, raw: dict) -> "Command":
        missing = [k for k in ("player", "client_seq", "submit_tick",
                               "recv_seq", "recv_tick", "op", "cost")
                   if k not in raw]
        if missing:
            raise ValueError(f"command missing fields: {missing}")
        if raw["op"] not in OPS:
            raise ValueError(f"unknown op: {raw['op']}")
        return cls(
            player=str(raw["player"]),
            client_seq=int(raw["client_seq"]),
            submit_tick=int(raw["submit_tick"]),
            recv_seq=int(raw["recv_seq"]),
            recv_tick=int(raw["recv_tick"]),
            op=str(raw["op"]),
            unit=str(raw.get("unit", "")),
            dx=int(raw.get("dx", 0)),
            dy=int(raw.get("dy", 0)),
            cost=int(raw["cost"]),
        )

    def to_dict(self) -> dict:
        return {"client_seq": self.client_seq, "cost": self.cost,
                "dx": self.dx, "dy": self.dy, "op": self.op,
                "player": self.player, "recv_seq": self.recv_seq,
                "recv_tick": self.recv_tick, "submit_tick": self.submit_tick,
                "unit": self.unit}


@dataclass
class BattleState:
    unit_ids: list[str]
    owners: dict[str, str]
    pos: np.ndarray          # (N, 2) int64
    vel: np.ndarray          # (N, 2) int64
    alive: np.ndarray        # (N,) bool
    players: list[str]
    quota_used_total: dict[str, int] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, raw: dict) -> "BattleState":
        units = sorted(raw["units"], key=lambda u: u["id"])
        unit_ids = [u["id"] for u in units]
        owners = {u["id"]: str(u["owner"]) for u in units}
        pos = np.array([[int(u["x"]), int(u["y"])] for u in units],
                       dtype=np.int64)
        vel = np.array([[int(u.get("vx", 0)), int(u.get("vy", 0))]
                        for u in units], dtype=np.int64)
        alive = np.array([bool(u.get("alive", True)) for u in units],
                         dtype=np.bool_)
        players = sorted(str(p) for p in raw["players"])
        return cls(unit_ids=unit_ids, owners=owners, pos=pos, vel=vel,
                   alive=alive, players=players,
                   quota_used_total={p: 0 for p in players})

    def clone(self) -> "BattleState":
        return BattleState(
            unit_ids=list(self.unit_ids), owners=dict(self.owners),
            pos=self.pos.copy(), vel=self.vel.copy(),
            alive=self.alive.copy(), players=list(self.players),
            quota_used_total=dict(self.quota_used_total))

    def index(self, unit_id: str) -> int:
        return self.unit_ids.index(unit_id)

    def to_canonical(self, quota_remaining: dict[str, int] | None = None) -> dict:
        """按固定字段顺序展开：单位位置、速度、存活标记、玩家配额。"""
        quota_remaining = quota_remaining or {}
        units = []
        for i, uid in enumerate(self.unit_ids):
            units.append({
                "alive": bool(self.alive[i]),
                "id": uid,
                "owner": self.owners[uid],
                "vx": int(self.vel[i, 0]),
                "vy": int(self.vel[i, 1]),
                "x": int(self.pos[i, 0]),
                "y": int(self.pos[i, 1]),
            })
        players = [{
            "id": p,
            "quota_remaining": int(quota_remaining.get(p, 0)),
            "quota_used_total": int(self.quota_used_total.get(p, 0)),
        } for p in self.players]
        return {"players": players, "state_version": STATE_VERSION,
                "units": units}
