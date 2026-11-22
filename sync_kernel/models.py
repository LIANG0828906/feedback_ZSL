from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

from .errors import E_SEMANTIC, KernelError
from .util import digest_json


LOGIC_HZ = 120
WORLD_MIN = 0
WORLD_MAX = 64000
MAX_HEADING = 360_000

UNIT_CORE_FIELDS = (
    "unit_id",
    "owner_id",
    "team",
    "x",
    "y",
    "heading",
    "hp",
    "max_hp",
    "cooldown",
    "resource",
    "status_mask",
)


@dataclass
class UnitState:
    unit_id: str
    owner_id: str
    team: str
    x: int
    y: int
    heading: int
    hp: int
    max_hp: int
    cooldown: int = 0
    resource: int = 0
    status_mask: int = 0

    def clone(self) -> "UnitState":
        return UnitState(
            unit_id=self.unit_id,
            owner_id=self.owner_id,
            team=self.team,
            x=self.x,
            y=self.y,
            heading=self.heading,
            hp=self.hp,
            max_hp=self.max_hp,
            cooldown=self.cooldown,
            resource=self.resource,
            status_mask=self.status_mask,
        )

    def as_dict(self) -> dict[str, Any]:
        return {
            "unit_id": self.unit_id,
            "owner_id": self.owner_id,
            "team": self.team,
            "position": {"x": self.x, "y": self.y},
            "heading_mdeg": self.heading,
            "health": {"current": self.hp, "maximum": self.max_hp},
            "cooldown_ticks": self.cooldown,
            "resource_units": self.resource,
            "status_mask": self.status_mask,
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "UnitState":
        return cls(
            unit_id=str(value["unit_id"]),
            owner_id=str(value["owner_id"]),
            team=str(value["team"]),
            x=int(value["position"]["x"]),
            y=int(value["position"]["y"]),
            heading=int(value["heading_mdeg"]),
            hp=int(value["health"]["current"]),
            max_hp=int(value["health"]["maximum"]),
            cooldown=int(value["cooldown_ticks"]),
            resource=int(value["resource_units"]),
            status_mask=int(value["status_mask"]),
        )


@dataclass(frozen=True)
class MatchContext:
    match_id: str
    seed: int
    tick_rate: int = LOGIC_HZ
    clock_hz: int = LOGIC_HZ
    input_fingerprint: str = ""

    def as_dict(self) -> dict[str, Any]:
        return {
            "match_id": self.match_id,
            "seed": self.seed,
            "tick_rate": self.tick_rate,
            "clock_hz": self.clock_hz,
            "input_fingerprint": self.input_fingerprint,
        }


@dataclass
class AuthoritativeState:
    match_id: str
    seed: int
    tick: int
    logical_clock: int
    sequence: int
    units: dict[str, UnitState]
    input_offset: int = 0
    rng_progress: int = 0
    snapshot_hash: str = ""
    source_version: int = 0
    source_time_base: str = "internal"

    def clone(self) -> "AuthoritativeState":
        return AuthoritativeState(
            match_id=self.match_id,
            seed=self.seed,
            tick=self.tick,
            logical_clock=self.logical_clock,
            sequence=self.sequence,
            units={key: value.clone() for key, value in self.units.items()},
            input_offset=self.input_offset,
            rng_progress=self.rng_progress,
            snapshot_hash=self.snapshot_hash,
            source_version=self.source_version,
            source_time_base=self.source_time_base,
        )

    def authority_payload(self) -> dict[str, Any]:
        return {
            "match_id": self.match_id,
            "seed": self.seed,
            "tick": self.tick,
            "logical_clock": self.logical_clock,
            "sequence": self.sequence,
            "input_offset": self.input_offset,
            "rng_progress": self.rng_progress,
            "units": [
                {
                    "unit_id": unit.unit_id,
                    "owner_id": unit.owner_id,
                    "team": unit.team,
                    "x": unit.x,
                    "y": unit.y,
                    "heading": unit.heading,
                    "hp": unit.hp,
                    "max_hp": unit.max_hp,
                    "cooldown": unit.cooldown,
                    "resource": unit.resource,
                    "status_mask": unit.status_mask,
                }
                for unit in (self.units[key] for key in sorted(self.units))
            ],
        }

    def compute_hash(self) -> str:
        return digest_json(self.authority_payload())

    def rehash(self) -> "AuthoritativeState":
        self.snapshot_hash = self.compute_hash()
        return self

    def as_checkpoint_dict(self) -> dict[str, Any]:
        payload = self.authority_payload()
        payload["snapshot_hash"] = self.snapshot_hash
        payload["source_version"] = self.source_version
        payload["source_time_base"] = self.source_time_base
        return payload

    @classmethod
    def from_checkpoint_dict(cls, value: dict[str, Any]) -> "AuthoritativeState":
        units = {
            item["unit_id"]: UnitState(
                unit_id=str(item["unit_id"]),
                owner_id=str(item["owner_id"]),
                team=str(item["team"]),
                x=int(item["x"]),
                y=int(item["y"]),
                heading=int(item["heading"]),
                hp=int(item["hp"]),
                max_hp=int(item["max_hp"]),
                cooldown=int(item["cooldown"]),
                resource=int(item["resource"]),
                status_mask=int(item["status_mask"]),
            )
            for item in value["units"]
        }
        state = cls(
            match_id=str(value["match_id"]),
            seed=int(value["seed"]),
            tick=int(value["tick"]),
            logical_clock=int(value["logical_clock"]),
            sequence=int(value["sequence"]),
            units=units,
            input_offset=int(value["input_offset"]),
            rng_progress=int(value["rng_progress"]),
            snapshot_hash=str(value["snapshot_hash"]),
            source_version=int(value.get("source_version", 0)),
            source_time_base=str(value.get("source_time_base", "internal")),
        )
        return state

    def validate(self) -> None:
        if self.tick < 0 or self.sequence < 0 or self.logical_clock < 0:
            raise KernelError(
                E_SEMANTIC,
                "state tick, sequence and logical clock must be non-negative",
                path="$.state",
            )
        if not self.units:
            raise KernelError(E_SEMANTIC, "state must contain at least one unit", path="$.units")
        for unit_id, unit in self.units.items():
            if unit.unit_id != unit_id:
                raise KernelError(
                    E_SEMANTIC,
                    "unit map key does not match unit_id",
                    path=f"$.units.{unit_id}",
                    object_id=unit_id,
                )
            if unit.max_hp <= 0 or not (0 <= unit.hp <= unit.max_hp):
                raise KernelError(
                    E_SEMANTIC,
                    "health value outside [0, max_hp]",
                    path=f"$.units.{unit_id}.hp",
                    object_id=unit_id,
                )
            if unit.cooldown < 0 or unit.resource < 0:
                raise KernelError(
                    E_SEMANTIC,
                    "cooldown and resource must be non-negative",
                    path=f"$.units.{unit_id}",
                    object_id=unit_id,
                )


def build_initial_state(
    context: MatchContext,
    units: Iterable[UnitState],
) -> AuthoritativeState:
    unit_map = {unit.unit_id: unit.clone() for unit in units}
    state = AuthoritativeState(
        match_id=context.match_id,
        seed=context.seed,
        tick=0,
        logical_clock=0,
        sequence=0,
        units=unit_map,
        input_offset=0,
        rng_progress=0,
        source_version=0,
        source_time_base="initial",
    )
    state.rehash()
    return state


def first_authority_difference(
    left: AuthoritativeState,
    right: AuthoritativeState,
) -> dict[str, Any] | None:
    if left.tick != right.tick:
        return {
            "tick": min(left.tick, right.tick),
            "field": "tick",
            "left": left.tick,
            "right": right.tick,
        }
    if left.logical_clock != right.logical_clock:
        return {
            "tick": left.tick,
            "field": "logical_clock",
            "left": left.logical_clock,
            "right": right.logical_clock,
        }
    for unit_id in sorted(set(left.units) | set(right.units)):
        if unit_id not in left.units:
            return {
                "tick": left.tick,
                "field": f"units.{unit_id}",
                "left": "<missing>",
                "right": "present",
            }
        if unit_id not in right.units:
            return {
                "tick": left.tick,
                "field": f"units.{unit_id}",
                "left": "present",
                "right": "<missing>",
            }
        left_unit = left.units[unit_id]
        right_unit = right.units[unit_id]
        for field_name in UNIT_CORE_FIELDS:
            left_value = getattr(left_unit, field_name)
            right_value = getattr(right_unit, field_name)
            if left_value != right_value:
                return {
                    "tick": left.tick,
                    "field": f"units.{unit_id}.{field_name}",
                    "left": left_value,
                    "right": right_value,
                }
    return None


def state_differences(
    left: AuthoritativeState,
    right: AuthoritativeState,
) -> list[dict[str, Any]]:
    differences: list[dict[str, Any]] = []
    for unit_id in sorted(set(left.units) | set(right.units)):
        if unit_id not in left.units or unit_id not in right.units:
            differences.append(
                {
                    "unit_id": unit_id,
                    "field": "unit",
                    "predicted": unit_id in left.units,
                    "authoritative": unit_id in right.units,
                }
            )
            continue
        left_unit = left.units[unit_id]
        right_unit = right.units[unit_id]
        for field_name in UNIT_CORE_FIELDS[3:]:
            left_value = getattr(left_unit, field_name)
            right_value = getattr(right_unit, field_name)
            if left_value != right_value:
                differences.append(
                    {
                        "unit_id": unit_id,
                        "field": field_name,
                        "predicted": left_value,
                        "authoritative": right_value,
                        "delta": int(right_value) - int(left_value),
                    }
                )
    return differences

