"""Independent reference model used solely for ground-truth generation.

This module deliberately does NOT import :mod:`syncdemo.engine`,
:mod:`syncdemo.client` or :mod:`syncdemo.runner`.  It re-derives the
authoritative tick states, the per-client visible states and the final
settlement directly from the sample definition (units, input intents,
latency tiers) with its own implementation of the game rules, so that
``verify`` cross-checks the production engine/client against an
independent implementation rather than against itself.

Only the sub-RNG derivation (:func:`syncdemo.rng.derive_rng`) is shared:
the rule "all randomness derives from (seed, tick, purpose)" is part of
the sample specification, not of the engine implementation.
"""

from __future__ import annotations

import copy
import math

from .rng import derive_rng

KEY_STATE_EVERY = 20


def _initial_units(units) -> dict:
    return {
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
    }


def _settlement(state) -> dict:
    players = {}
    for unit in state["units"].values():
        entry = players.setdefault(unit["player"], {"resource": 0.0, "hp": 0.0})
        entry["resource"] += unit["resource"]
        entry["hp"] += unit["hp"]
    winner = None
    if players:
        winner = min(players, key=lambda p: (-players[p]["resource"], p))
    return {"tick": state["tick"], "players": players, "winner": winner}


def _advance(state, intents, seed, cfg):
    """Advance ``state`` one tick over ``intents`` (reference rules)."""
    if state["locked"]:
        return
    tick = state["tick"] + 1
    state["tick"] = tick
    rng = derive_rng(seed, tick, "tick")

    for intent in intents:
        unit = state["units"].get(intent.get("unit"))
        if unit is None or not unit["alive"]:
            continue
        kind = intent.get("type")
        if kind == "move":
            dx = float(intent.get("dx", 0.0))
            dy = float(intent.get("dy", 0.0))
            mag = math.hypot(dx, dy)
            if mag > 0.0:
                if mag > unit["speed"]:
                    scale = unit["speed"] / mag
                    dx, dy = dx * scale, dy * scale
                unit["x"] = min(max(unit["x"] + dx, 0.0), cfg.map_size)
                unit["y"] = min(max(unit["y"] + dy, 0.0), cfg.map_size)
                unit["facing"] = math.degrees(math.atan2(dy, dx)) % 360.0
        elif kind == "skill":
            if unit["cooldown"] > 0 or unit["resource"] < cfg.skill_cost:
                continue
            target = state["units"].get(intent.get("target"))
            if target is None or target["id"] == unit["id"] \
                    or not target["alive"]:
                continue
            damage = cfg.skill_damage_base + int(
                rng.integers(0, cfg.skill_damage_var)
            )
            unit["resource"] -= cfg.skill_cost
            unit["cooldown"] = cfg.skill_cooldown
            target["hp"] -= damage
            if target["hp"] <= 0.0:
                target["hp"] = 0.0
                target["alive"] = False
                target["cooldown"] = 0
                target["respawn_tick"] = tick + cfg.respawn_delay
        elif kind == "gather":
            unit["resource"] += float(
                int(rng.integers(cfg.gather_min, cfg.gather_max + 1))
            )
        elif kind == "item":
            if unit["resource"] < cfg.item_cost or unit["hp"] >= unit["max_hp"]:
                continue
            unit["resource"] -= cfg.item_cost
            unit["hp"] = min(unit["max_hp"], unit["hp"] + cfg.item_heal)

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

    totals = {}
    for unit in state["units"].values():
        totals[unit["player"]] = (
            totals.get(unit["player"], 0.0) + unit["resource"]
        )
    if any(total >= cfg.resource_target for total in totals.values()):
        state["locked"] = True
        state["result"] = _settlement(state)


def _predict(confirmed, pending, seed, cfg):
    """Visible state: confirmed authoritative state + pending own inputs."""
    state = copy.deepcopy(confirmed)
    by_tick = {}
    for tick, intent in pending:
        by_tick.setdefault(tick, []).append(intent)
    for tick in sorted(by_tick):
        _advance(state, by_tick[tick], seed, cfg)
    return state


def _client_views(auth_states, inputs_by_tick, client, seed, cfg):
    """Reference visible-state sequence for one client.

    Mirrors the delivery semantics: the authoritative state of tick ``t``
    reaches the client at tick ``t + tier(t)``; the client predicts its
    own unconfirmed inputs on top of the latest confirmed state.
    """
    table = {obs["tick"]: obs["tier"] for obs in client["latency"]}

    def tier_at(tick):
        if tick in table:
            return table[tick]
        earlier = [t for t in table if t <= tick]
        return table[max(earlier)] if earlier else 0

    player = client["player"]
    max_tick = len(auth_states) - 1
    confirmed = None
    pending = []
    inflight = []
    views = []

    def deliver(upto_tick):
        nonlocal confirmed, pending
        due = [d for d in inflight if d[0] <= upto_tick]
        inflight[:] = [d for d in inflight if d[0] > upto_tick]
        for _, auth in sorted(due, key=lambda d: d[0]):
            confirmed = auth
            pending = [p for p in pending if p[0] > auth["tick"]]

    for tick in range(1, max_tick + 1):
        for intent in inputs_by_tick.get(tick, []):
            unit = auth_states[0]["units"].get(intent.get("unit"))
            if unit is not None and unit["player"] == player:
                pending.append((tick, copy.deepcopy(intent)))
        inflight.append((tick + tier_at(tick), auth_states[tick]))
        deliver(tick)
        if confirmed is None:
            views.append(None)
        else:
            views.append(_predict(confirmed, pending, seed, cfg))

    for extra in range(1, tier_at(max_tick) + 1):
        deliver(max_tick + extra)
        if confirmed is None:
            views.append(None)
        else:
            views.append(_predict(confirmed, pending, seed, cfg))
    return views


def generate_truth(seed, units, inputs, clients, cfg,
                   key_every=KEY_STATE_EVERY) -> dict:
    """Derive the ground truth for a sample definition independently."""
    inputs_by_tick = {e["tick"]: list(e["intents"]) for e in inputs}
    max_tick = max(inputs_by_tick) if inputs_by_tick else 0

    state = {"tick": 0, "locked": False, "result": None,
             "units": _initial_units(units)}
    auth_states = [copy.deepcopy(state)]
    for tick in range(1, max_tick + 1):
        _advance(state, inputs_by_tick.get(tick, []), seed, cfg)
        auth_states.append(copy.deepcopy(state))

    key_states = [
        {"tick": t, "units": auth_states[t]["units"]}
        for t in range(0, len(auth_states), key_every)
    ]
    if (len(auth_states) - 1) % key_every != 0:
        key_states.append(
            {"tick": len(auth_states) - 1,
             "units": auth_states[-1]["units"]}
        )

    client_visible = []
    for client in clients:
        views = _client_views(auth_states, inputs_by_tick, client, seed, cfg)
        for i in range(0, len(views), key_every):
            if views[i] is not None:
                # index i is the visible state right after match tick i+1
                client_visible.append({
                    "client": client["id"],
                    "tick": i + 1,
                    "units": views[i]["units"],
                })

    settlement = state["result"] or _settlement(state)
    return {
        "key_states": key_states,
        "client_visible": client_visible,
        "settlement": settlement,
    }
