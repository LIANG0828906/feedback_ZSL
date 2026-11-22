"""Authoritative deterministic tick engine.

The engine advances the authoritative state one tick at a time given the
input intents for that tick.  All randomness comes from
:func:`syncdemo.rng.derive_rng`, keyed by ``(seed, tick)`` only, so a tick
produces identical results regardless of the execution path that reached it.

State is represented as plain JSON-serialisable dicts so it can be hashed,
checkpointed and compared field-by-field.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field

from .rng import derive_rng

ALGO_VERSION = "1.0.0"

UNIT_NUMERIC_FIELDS = ("x", "y", "facing", "hp", "max_hp", "resource", "speed")
UNIT_INT_FIELDS = ("cooldown", "respawn_tick")
COMPARE_FIELDS = ("x", "y", "facing", "hp", "resource", "cooldown", "alive")


@dataclass(frozen=True)
class EngineConfig:
    algo_version: str = ALGO_VERSION
    map_size: float = 100.0
    skill_cost: float = 10.0
    skill_cooldown: int = 5
    skill_damage_base: float = 15.0
    skill_damage_var: int = 6
    gather_min: int = 3
    gather_max: int = 8
    item_heal: float = 20.0
    item_cost: float = 5.0
    respawn_delay: int = 10
    resource_target: float = 100.0


def initial_state(units) -> dict:
    """Build the authoritative state at tick 0 from the unit table."""
    return {
        "tick": 0,
        "locked": False,
        "result": None,
        "units": {
            u["id"]: {
                "id": u["id"],
                "player": u["player"],
                "x": float(u["x"]),
                "y": float(u["y"]),
                "spawn_x": float(u["x"]),
                "spawn_y": float(u["y"]),
                "facing": float(u["facing"]),
                "hp": float(u["hp"]),
                "max_hp": float(u["max_hp"]),
                "resource": float(u["resource"]),
                "cooldown": int(u["cooldown"]),
                "speed": float(u["speed"]),
                "alive": True,
                "respawn_tick": -1,
            }
            for u in units
        },
    }


def _clamp(value, lo, hi):
    return max(lo, min(hi, value))


def _apply_move(unit, intent, cfg):
    dx, dy = float(intent.get("dx", 0.0)), float(intent.get("dy", 0.0))
    mag = math.hypot(dx, dy)
    if mag <= 0.0:
        return
    if mag > unit["speed"]:
        scale = unit["speed"] / mag
        dx, dy = dx * scale, dy * scale
    unit["x"] = _clamp(unit["x"] + dx, 0.0, cfg.map_size)
    unit["y"] = _clamp(unit["y"] + dy, 0.0, cfg.map_size)
    unit["facing"] = math.degrees(math.atan2(dy, dx)) % 360.0


def _kill(unit, tick, cfg):
    unit["hp"] = 0.0
    unit["alive"] = False
    unit["cooldown"] = 0
    unit["respawn_tick"] = tick + cfg.respawn_delay


def _apply_skill(state, unit, intent, rng, tick, cfg):
    if unit["cooldown"] > 0 or unit["resource"] < cfg.skill_cost:
        return  # prerequisite check failed: intent dropped
    target = state["units"].get(intent.get("target"))
    if target is None or target["id"] == unit["id"] or not target["alive"]:
        return
    damage = cfg.skill_damage_base + int(rng.integers(0, cfg.skill_damage_var))
    unit["resource"] -= cfg.skill_cost
    unit["cooldown"] = cfg.skill_cooldown
    target["hp"] -= damage
    if target["hp"] <= 0.0:
        _kill(target, tick, cfg)


def _apply_gather(unit, rng, cfg):
    unit["resource"] += float(int(rng.integers(cfg.gather_min, cfg.gather_max + 1)))


def _apply_item(unit, cfg):
    if unit["resource"] < cfg.item_cost or unit["hp"] >= unit["max_hp"]:
        return
    unit["resource"] -= cfg.item_cost
    unit["hp"] = min(unit["max_hp"], unit["hp"] + cfg.item_heal)


def settlement(state) -> dict:
    """Per-player resource/hp tally for the current state."""
    players = {}
    for unit in state["units"].values():
        entry = players.setdefault(unit["player"], {"resource": 0.0, "hp": 0.0})
        entry["resource"] += unit["resource"]
        entry["hp"] += unit["hp"]
    winner = None
    if players:
        winner = min(players, key=lambda p: (-players[p]["resource"], p))
    return {"tick": state["tick"], "players": players, "winner": winner}


def step(state, intents, seed, cfg: EngineConfig) -> dict:
    """Advance ``state`` by one tick, mutating and returning it.

    ``intents`` is the ordered list of input intents for the new tick.
    Once the match is locked the state no longer changes.
    """
    if state["locked"]:
        return state
    tick = state["tick"] + 1
    state["tick"] = tick
    rng = derive_rng(seed, tick, "tick")

    for intent in intents:
        unit = state["units"].get(intent.get("unit"))
        if unit is None or not unit["alive"]:
            continue
        kind = intent.get("type")
        if kind == "move":
            _apply_move(unit, intent, cfg)
        elif kind == "skill":
            _apply_skill(state, unit, intent, rng, tick, cfg)
        elif kind == "gather":
            _apply_gather(unit, rng, cfg)
        elif kind == "item":
            _apply_item(unit, cfg)

    for uid in sorted(state["units"]):
        unit = state["units"][uid]
        if unit["alive"]:
            unit["cooldown"] = max(0, unit["cooldown"] - 1)
        elif tick >= unit["respawn_tick"] >= 0:
            unit["alive"] = True
            unit["hp"] = unit["max_hp"]
            unit["x"] = unit["spawn_x"]
            unit["y"] = unit["spawn_y"]
            unit["respawn_tick"] = -1

    for player_resources in _player_resources(state).values():
        if player_resources >= cfg.resource_target:
            state["locked"] = True
            state["result"] = settlement(state)
            break
    return state


def _player_resources(state) -> dict:
    totals = {}
    for unit in state["units"].values():
        totals[unit["player"]] = totals.get(unit["player"], 0.0) + unit["resource"]
    return totals


def diff_states(a, b) -> dict:
    """Field-level absolute differences between two states.

    Returns ``{unit_id: {field: abs_diff}}`` covering units present in both.
    """
    out = {}
    units_a, units_b = a["units"], b["units"]
    for uid in sorted(set(units_a) | set(units_b)):
        ua, ub = units_a.get(uid), units_b.get(uid)
        if ua is None or ub is None:
            out[uid] = {f: float("inf") for f in COMPARE_FIELDS}
            continue
        entry = {}
        for f in COMPARE_FIELDS:
            va, vb = ua[f], ub[f]
            if isinstance(va, bool):
                if va != vb:
                    entry[f] = 1.0
            else:
                d = abs(float(va) - float(vb))
                if d > 0.0:
                    entry[f] = d
        if entry:
            out[uid] = entry
    return out


def diff_total(magnitude) -> float:
    return float(sum(sum(fields.values()) for fields in magnitude.values()))
