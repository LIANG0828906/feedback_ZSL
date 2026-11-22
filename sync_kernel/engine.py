from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import Any, Iterable

from .errors import E_SEMANTIC, KernelError
from .models import (
    LOGIC_HZ,
    MAX_HEADING,
    UNIT_CORE_FIELDS,
    WORLD_MAX,
    WORLD_MIN,
    AuthoritativeState,
    MatchContext,
    UnitState,
)
from .util import canonical_json_bytes, digest_json, is_int


MAX_MOVE_PER_TICK = 2_000
MAX_HEADING_DELTA = 90_000
STRIKE_COST = 1_000
STRIKE_COOLDOWN = 3
HEAL_COST = 1_500
HEAL_COOLDOWN = 5
RESOURCE_REGEN = 50
RESOURCE_MAX = 50_000
STATUS_SLOWED = 1


def deterministic_roll(seed: int, tick: int, unit_id: str, purpose: str) -> int:
    material = f"{seed}|{tick}|{unit_id}|{purpose}".encode("utf-8")
    return int.from_bytes(hashlib.sha256(material).digest()[:8], "big")


@dataclass(frozen=True)
class DeterministicRandomSource:
    seed: int

    def roll(self, tick: int, unit_id: str, purpose: str) -> int:
        return deterministic_roll(self.seed, tick, unit_id, purpose)


def normalize_heading(value: int) -> int:
    return value % MAX_HEADING


def clamp(value: int, low: int, high: int) -> int:
    return max(low, min(high, value))


def _require_int_field(
    value: dict[str, Any],
    key: str,
    path: str,
    *,
    minimum: int | None = None,
    maximum: int | None = None,
    required: bool = False,
) -> int | None:
    if key not in value:
        if required:
            raise KernelError(
                E_SEMANTIC,
                f"required input intent field {key!r} is missing",
                path=path,
            )
        return None
    number = value[key]
    if not is_int(number):
        raise KernelError(
            E_SEMANTIC,
            f"input intent field {key!r} must be an integer fixed-point value",
            path=f"{path}.{key}",
        )
    if minimum is not None and number < minimum:
        raise KernelError(
            E_SEMANTIC,
            f"input intent field {key!r} is below {minimum}",
            path=f"{path}.{key}",
        )
    if maximum is not None and number > maximum:
        raise KernelError(
            E_SEMANTIC,
            f"input intent field {key!r} is above {maximum}",
            path=f"{path}.{key}",
        )
    return number


def validate_and_index_intents(
    intents: Iterable[dict[str, Any]],
    units: dict[str, UnitState],
    *,
    tick: int,
) -> dict[str, list[dict[str, Any]]]:
    indexed: dict[str, list[dict[str, Any]]] = {unit_id: [] for unit_id in sorted(units)}
    if not isinstance(intents, list):
        raise KernelError(
            E_SEMANTIC,
            "tick intents must be a list",
            path=f"$.inputs[{tick}].intents",
        )
    for index, intent in enumerate(intents):
        path = f"$.inputs[{tick}].intents[{index}]"
        if not isinstance(intent, dict):
            raise KernelError(E_SEMANTIC, "input intent must be an object", path=path)
        unit_id = intent.get("unit_id")
        if not isinstance(unit_id, str) or not unit_id:
            raise KernelError(
                E_SEMANTIC,
                "input intent requires a non-empty unit_id",
                path=f"{path}.unit_id",
            )
        if unit_id not in units:
            raise KernelError(
                E_SEMANTIC,
                f"input intent references unknown unit {unit_id!r}",
                path=f"{path}.unit_id",
                object_id=unit_id,
            )
        dx = _require_int_field(
            intent,
            "dx",
            path,
            minimum=-MAX_MOVE_PER_TICK,
            maximum=MAX_MOVE_PER_TICK,
        )
        dy = _require_int_field(
            intent,
            "dy",
            path,
            minimum=-MAX_MOVE_PER_TICK,
            maximum=MAX_MOVE_PER_TICK,
        )
        heading_delta = _require_int_field(
            intent,
            "heading_delta",
            path,
            minimum=-MAX_HEADING_DELTA,
            maximum=MAX_HEADING_DELTA,
        )
        action = intent.get("action")
        if action not in (None, "strike", "heal"):
            raise KernelError(
                E_SEMANTIC,
                f"unsupported action {action!r}",
                path=f"{path}.action",
                object_id=unit_id,
            )
        target_id = intent.get("target_unit_id")
        if action == "strike":
            if not isinstance(target_id, str) or not target_id:
                raise KernelError(
                    E_SEMANTIC,
                    "strike action requires target_unit_id",
                    path=f"{path}.target_unit_id",
                    object_id=unit_id,
                )
            if target_id not in units:
                raise KernelError(
                    E_SEMANTIC,
                    f"strike action references unknown target {target_id!r}",
                    path=f"{path}.target_unit_id",
                    object_id=target_id,
                )
        indexed[unit_id].append(
            {
                "unit_id": unit_id,
                "player_id": str(intent.get("player_id", "")),
                "dx": dx or 0,
                "dy": dy or 0,
                "heading_delta": heading_delta or 0,
                "action": action,
                "target_unit_id": target_id,
            }
        )
    for unit_id in indexed:
        indexed[unit_id].sort(key=lambda item: (item["player_id"], item["action"] or ""))
    return indexed


def advance_state(
    state: AuthoritativeState,
    context: MatchContext,
    intents: Iterable[dict[str, Any]],
) -> AuthoritativeState:
    next_state = state.clone()
    next_tick = state.tick + 1
    indexed = validate_and_index_intents(intents, next_state.units, tick=next_tick)
    rng = DeterministicRandomSource(context.seed)

    rolls: dict[str, int] = {}
    for unit_id in sorted(next_state.units):
        rolls[unit_id] = rng.roll(next_tick, unit_id, "combat")

    for unit in next_state.units.values():
        unit.cooldown = max(0, unit.cooldown - 1)
        unit.resource = min(RESOURCE_MAX, unit.resource + RESOURCE_REGEN)

    for unit_id in sorted(next_state.units):
        unit = next_state.units[unit_id]
        roll = rolls[unit_id]
        for intent in indexed[unit_id]:
            unit.x = clamp(unit.x + int(intent["dx"]), WORLD_MIN, WORLD_MAX)
            unit.y = clamp(unit.y + int(intent["dy"]), WORLD_MIN, WORLD_MAX)
            unit.heading = normalize_heading(unit.heading + int(intent["heading_delta"]))
            action = intent["action"]
            if action == "strike":
                if unit.cooldown > 0 or unit.resource < STRIKE_COST:
                    continue
                target = next_state.units[str(intent["target_unit_id"])]
                damage = 3_000 + roll % 3_001
                target.hp = max(0, target.hp - damage)
                if roll % 7 == 0:
                    target.status_mask |= STATUS_SLOWED
                unit.resource -= STRIKE_COST
                unit.cooldown = STRIKE_COOLDOWN
            elif action == "heal":
                if unit.cooldown > 0 or unit.resource < HEAL_COST:
                    continue
                unit.hp = min(unit.max_hp, unit.hp + 5_000)
                unit.status_mask &= ~STATUS_SLOWED
                unit.resource -= HEAL_COST
                unit.cooldown = HEAL_COOLDOWN

    next_state.tick = next_tick
    next_state.sequence = next_tick
    next_state.logical_clock = next_tick * context.clock_hz
    next_state.input_offset = next_tick
    next_state.rng_progress = state.rng_progress + len(next_state.units)
    next_state.source_version = 0
    next_state.source_time_base = "internal"
    next_state.rehash()
    return next_state


class MatchEngine:
    """Authoritative deterministic simulation for one local match."""

    def __init__(
        self,
        context: MatchContext,
        initial_state: AuthoritativeState,
        inputs_by_tick: dict[int, list[dict[str, Any]]],
    ) -> None:
        self.context = context
        self.initial_state = initial_state.clone()
        self.inputs_by_tick = {
            int(tick): [dict(item) for item in intents]
            for tick, intents in sorted(inputs_by_tick.items())
        }

    def intents_for_tick(self, tick: int) -> list[dict[str, Any]]:
        return [dict(item) for item in self.inputs_by_tick.get(tick, [])]

    def advance(self, state: AuthoritativeState, tick: int | None = None) -> AuthoritativeState:
        target_tick = state.tick + 1 if tick is None else tick
        if target_tick != state.tick + 1:
            raise KernelError(
                E_SEMANTIC,
                "engine advances exactly one tick at a time",
                path="$.tick",
                details={"from": state.tick, "to": target_tick},
            )
        return advance_state(state, self.context, self.intents_for_tick(target_tick))

    def run_to(
        self,
        target_tick: int,
        *,
        start_state: AuthoritativeState | None = None,
        commit_window: int = 10,
        on_commit: Any | None = None,
    ) -> tuple[list[AuthoritativeState], dict[int, AuthoritativeState]]:
        if target_tick < 0:
            raise KernelError(E_SEMANTIC, "target tick must be non-negative", path="$.tick")
        state = (start_state or self.initial_state).clone()
        timeline = {state.tick: state.clone()}
        committed: dict[int, AuthoritativeState] = {}
        if state.tick % commit_window == 0:
            committed[state.tick] = state.clone()
            if on_commit is not None:
                on_commit(state.clone())
        while state.tick < target_tick:
            state = self.advance(state)
            timeline[state.tick] = state.clone()
            if state.tick % commit_window == 0:
                committed[state.tick] = state.clone()
                if on_commit is not None:
                    on_commit(state.clone())
        states = [timeline[tick] for tick in sorted(timeline)]
        return states, committed


def input_stream_payload(
    context: MatchContext,
    initial_state: AuthoritativeState,
    inputs_by_tick: dict[int, list[dict[str, Any]]],
) -> dict[str, Any]:
    return {
        "match_id": context.match_id,
        "seed": context.seed,
        "tick_rate": context.tick_rate,
        "initial_state": initial_state.authority_payload(),
        "inputs": [
            {"tick": tick, "intents": inputs_by_tick[tick]}
            for tick in sorted(inputs_by_tick)
        ],
    }


def compute_input_fingerprint(
    context: MatchContext,
    initial_state: AuthoritativeState,
    inputs_by_tick: dict[int, list[dict[str, Any]]],
) -> str:
    return digest_json(input_stream_payload(context, initial_state, inputs_by_tick))

