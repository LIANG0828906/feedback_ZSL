"""确定性回合制规则引擎。

规则只依赖对局描述与随机种子：
- 随机数全部来自 numpy PCG64(seed)，RNG 状态随快照保存/恢复；
- 不使用系统时间、uuid、dict 哈希随机化等任何外部熵源；
- 同输入 + 同调度顺序产出逐字节一致的事件流与状态哈希。
"""

from __future__ import annotations

from collections.abc import Callable

import numpy as np

from .hashing import GENESIS_HASH, chain_hash, hash_json
from .model import MatchDescription, ResourceUsage, make_initial_runtime_state
from .resources import ResourceMeter

ENERGY_CAP = 100

ACTION_ENERGY_COST = {
    "attack": 10,
    "defend": 5,
    "charge": 0,
    "skill": 30,
}

ACTION_BASE_POWER = {
    "attack": 18,
    "defend": 0,
    "charge": 0,
    "skill": 35,
}


class RuleEngine:
    def __init__(
        self,
        desc: MatchDescription,
        compute_budget: int,
        memory_budget: int,
        usage: ResourceUsage | None = None,
        state: dict | None = None,
        cancel_checkpoint: Callable[[], None] | None = None,
    ) -> None:
        self.desc = desc
        self.meter = ResourceMeter(
            compute_budget,
            memory_budget,
            usage=usage,
            cancel_checkpoint=cancel_checkpoint,
        )
        self.state = state if state is not None else make_initial_runtime_state(desc)
        self._rng: np.random.Generator | None = None
        if self.state.get("rng_state") is not None:
            # 必须直接从保存的 bit_generator state 恢复；
            # 重新 new PCG64() 再赋 state 在不同 numpy 版本下可能得到不同流。
            bit_generator = np.random.PCG64()
            bit_generator.state = self.state["rng_state"]
            self._rng = np.random.Generator(bit_generator)
        self.events: list[dict] = []

    @property
    def rng(self) -> np.random.Generator:
        if self._rng is None:
            self._rng = np.random.Generator(np.random.PCG64(self.desc.rng_seed))
        return self._rng

    def _store_rng_state(self) -> None:
        # PCG64 state 只含整数/字符串，可直接 JSON 序列化。
        self.state["rng_state"] = self.rng.bit_generator.state

    def current_turn(self) -> int:
        return int(self.state["turn"])

    def run_turn(self) -> dict:
        """执行下一回合，返回该回合快照内容（仅内存，尚未提交到磁盘）。"""
        turn = self.state["turn"] + 1
        if turn > self.desc.turn_count:
            raise IndexError("所有回合均已执行")

        # 每回合开始重置护盾（上回合护盾不跨回合）。
        for player in self.state["players"]:
            player["shield"] = 0

        turn_events: list[dict] = []
        turn_actions = self.desc.actions[turn - 1]

        for slot, action in enumerate(turn_actions):
            player_id = int(self.state["players"][slot]["id"])
            action_type = action["type"]
            # 资源检查点：取消在计量内部响应，超预算抛出 BudgetExceeded。
            compute, memory = self.meter.sample(turn, player_id, action_type)
            seq_base = len(self.events) + len(turn_events)
            turn_events.extend(
                self._apply_action(
                    turn, slot, action_type, compute, memory, seq_base
                )
            )

        self.state["turn"] = turn
        self._store_rng_state()

        prev_prefix = self.state.get("event_prefix_hash", GENESIS_HASH)
        prefix_hash = chain_hash(
            prev_prefix, {"turn": turn, "events": turn_events}
        )
        self.state["event_prefix_hash"] = prefix_hash
        self.events.extend(turn_events)

        public_state = self._public_state()
        state_hash = hash_json(public_state)
        prev_snapshot_hash = self.state.get("snapshot_hash", GENESIS_HASH)
        snapshot = {
            "match_id": self.desc.match_id,
            "turn": turn,
            "state": public_state,
            "state_hash": state_hash,
            "event_prefix_hash": prefix_hash,
            "events": turn_events,
            "resource_usage": self.meter.usage.to_dict(),
            "prev_snapshot_hash": prev_snapshot_hash,
        }
        snapshot["snapshot_hash"] = chain_hash(
            prev_snapshot_hash,
            {
                "turn": turn,
                "state_hash": state_hash,
                "event_prefix_hash": prefix_hash,
                "resource_usage": snapshot["resource_usage"],
            },
        )
        self.state["snapshot_hash"] = snapshot["snapshot_hash"]
        return snapshot

    def _public_state(self) -> dict:
        """对外状态：不含内部缓存的 snapshot_hash。"""
        return {
            "turn": self.state["turn"],
            "players": [dict(player) for player in self.state["players"]],
            "rng_state": self.state["rng_state"],
            "event_prefix_hash": self.state["event_prefix_hash"],
        }

    def _apply_action(
        self,
        turn: int,
        slot: int,
        action_type: str,
        compute: int,
        memory: int,
        seq_base: int,
    ) -> list[dict]:
        actors = self.state["players"]
        actor = actors[slot]
        target = actors[1 - slot]
        cost = ACTION_ENERGY_COST[action_type]
        events: list[dict] = [
            {
                "turn": turn,
                "seq": seq_base,
                "player_id": actor["id"],
                "type": "action",
                "action": action_type,
                "compute": compute,
                "memory": memory,
            }
        ]

        if action_type == "charge":
            actor["energy"] = min(ENERGY_CAP, actor["energy"] + 25)
            events.append(
                self._event(
                    turn, seq_base + 1, actor["id"], "energy_gain", amount=25
                )
            )
            return events

        if actor["energy"] < cost:
            # 能量不足：操作空转，规则确定且可重放，不引入随机数。
            events.append(
                self._event(
                    turn,
                    seq_base + 1,
                    actor["id"],
                    "insufficient_energy",
                    required=cost,
                    available=actor["energy"],
                )
            )
            return events

        actor["energy"] -= cost

        if action_type == "defend":
            block = int(self.rng.integers(8, 16))
            actor["shield"] += block
            events.append(
                self._event(
                    turn, seq_base + 1, actor["id"], "shield_gain", amount=block
                )
            )
            return events

        # attack / skill：命中检定 + 伤害结算。
        hit_rate = 85 if action_type == "attack" else 70
        hit = bool(self.rng.integers(0, 100) < hit_rate)
        if not hit:
            events.append(
                self._event(turn, seq_base + 1, actor["id"], "miss")
            )
            return events

        base_power = ACTION_BASE_POWER[action_type]
        variance = int(self.rng.integers(-3, 4))
        raw_damage = max(0, base_power + variance)
        absorbed = min(target["shield"], raw_damage)
        target["shield"] -= absorbed
        damage = raw_damage - absorbed
        target["hp"] = max(0, target["hp"] - damage)
        events.append(
            self._event(
                turn,
                seq_base + 1,
                actor["id"],
                "damage",
                target_id=target["id"],
                raw=raw_damage,
                absorbed=absorbed,
                hp_remaining=target["hp"],
            )
        )
        if target["hp"] == 0:
            events.append(
                self._event(
                    turn,
                    seq_base + 2,
                    target["id"],
                    "defeated",
                    by=actor["id"],
                )
            )
        return events

    @staticmethod
    def _event(turn: int, seq: int, player_id: int, name: str, **fields) -> dict:
        event = {"turn": turn, "seq": seq, "player_id": player_id, "type": name}
        event.update(fields)
        return event

    def verdict(self) -> dict:
        players = self.state["players"]
        alive = [player for player in players if player["hp"] > 0]
        outcome = "ongoing"
        winner = None
        if len(alive) == 1:
            winner = alive[0]["id"]
            outcome = "winner"
        elif len(alive) == 0:
            outcome = "draw"
        if self.state["turn"] >= self.desc.turn_count and outcome == "ongoing":
            p0, p1 = players
            if p0["hp"] == p1["hp"]:
                outcome = "draw"
            else:
                winner = p0["id"] if p0["hp"] > p1["hp"] else p1["id"]
                outcome = "winner"
        return {
            "match_id": self.desc.match_id,
            "turn": self.state["turn"],
            "outcome": outcome,
            "winner": winner,
            "players": [dict(player) for player in players],
        }
