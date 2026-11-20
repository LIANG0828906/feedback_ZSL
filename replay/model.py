"""对局描述、状态快照与回放结果的数据模型与 JSON IO。"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

from .errors import E_INVALID_INPUT, ReplayError
from .hashing import GENESIS_HASH, read_json, write_json_atomic

ACTION_TYPES = ("attack", "defend", "charge", "skill")


@dataclass(frozen=True)
class TurnBudget:
    compute: int
    memory: int

    def to_dict(self) -> dict:
        return {"compute": self.compute, "memory": self.memory}


@dataclass(frozen=True)
class MatchDescription:
    match_id: str
    rng_seed: int
    turn_count: int
    initial_state: dict
    actions: list[list[dict]]
    turn_budget: TurnBudget

    @classmethod
    def from_dict(cls, raw: dict) -> "MatchDescription":
        try:
            match_id = str(raw["match_id"])
            rng_seed = int(raw["rng_seed"])
            turn_count = int(raw["turn_count"])
            initial_state = dict(raw["initial_state"])
            actions = [list(turn) for turn in raw["actions"]]
            budget = raw["turn_budget"]
            turn_budget = TurnBudget(int(budget["compute"]), int(budget["memory"]))
        except (KeyError, TypeError, ValueError) as exc:
            raise ReplayError(E_INVALID_INPUT, f"对局描述字段非法: {exc}") from exc
        if turn_count <= 0:
            raise ReplayError(E_INVALID_INPUT, "turn_count 必须为正数")
        if len(actions) != turn_count:
            raise ReplayError(
                E_INVALID_INPUT,
                f"操作序列长度 {len(actions)} 与回合数 {turn_count} 不一致",
            )
        for turn_index, turn in enumerate(actions):
            if len(turn) != 2:
                raise ReplayError(
                    E_INVALID_INPUT, f"第 {turn_index + 1} 回合必须包含两名玩家操作"
                )
            for action in turn:
                if action.get("type") not in ACTION_TYPES:
                    raise ReplayError(
                        E_INVALID_INPUT,
                        f"未知操作类型: {action.get('type')!r}",
                    )
        players = initial_state.get("players")
        if not isinstance(players, list) or len(players) != 2:
            raise ReplayError(E_INVALID_INPUT, "initial_state.players 必须包含两名玩家")
        return cls(match_id, rng_seed, turn_count, initial_state, actions, turn_budget)

    @classmethod
    def load(cls, path: str | Path) -> "MatchDescription":
        return cls.from_dict(read_json(path))

    def to_dict(self) -> dict:
        return {
            "match_id": self.match_id,
            "rng_seed": self.rng_seed,
            "turn_count": self.turn_count,
            "initial_state": self.initial_state,
            "actions": self.actions,
            "turn_budget": self.turn_budget.to_dict(),
        }

    def save(self, path: str | Path) -> Path:
        return write_json_atomic(path, self.to_dict())


@dataclass
class ResourceUsage:
    """确定性资源计量结果（累计算力、累计内存、内存峰值）。"""

    compute_total: int = 0
    memory_total: int = 0
    memory_peak: int = 0
    per_turn: list[dict] = field(default_factory=list)

    def add(self, turn: int, compute: int, memory: int) -> None:
        self.compute_total += compute
        self.memory_total += memory
        self.memory_peak = max(self.memory_peak, memory)
        self.per_turn.append(
            {"turn": turn, "compute": compute, "memory": memory}
        )

    def to_dict(self) -> dict:
        return {
            "compute_total": self.compute_total,
            "memory_total": self.memory_total,
            "memory_peak": self.memory_peak,
            "per_turn": list(self.per_turn),
        }

    @classmethod
    def from_dict(cls, raw: dict) -> "ResourceUsage":
        usage = cls(
            int(raw["compute_total"]),
            int(raw["memory_total"]),
            int(raw["memory_peak"]),
            list(raw.get("per_turn", [])),
        )
        return usage


def make_initial_runtime_state(desc: MatchDescription) -> dict:
    """构造回合 0 的运行时状态（尚未执行任何回合）。"""
    players = []
    for player in desc.initial_state["players"]:
        players.append(
            {
                "id": int(player["id"]),
                "hp": int(player["hp"]),
                "energy": int(player["energy"]),
                "shield": int(player.get("shield", 0)),
            }
        )
    return {
        "turn": 0,
        "players": players,
        "rng_state": None,
        "event_prefix_hash": GENESIS_HASH,
    }
