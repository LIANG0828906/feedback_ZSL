"""Level, room, door, tag and constraint data structures."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Literal

from .errors import E_CONFIG_INVALID, WorkbenchError


TagKind = Literal["start", "end", "key", "lock", "reward"]
DoorDirection = Literal["both", "forward", "reverse"]
ConstraintType = Literal[
    "connectivity",
    "room_count",
    "key_chain",
    "no_lock_unreachable_end",
    "area_target",
    "door_count_target",
]

CONSTRAINT_TYPES = {
    "connectivity",
    "room_count",
    "key_chain",
    "no_lock_unreachable_end",
    "area_target",
    "door_count_target",
}


@dataclass(frozen=True)
class Room:
    id: str
    x: int
    y: int
    width: int
    height: int

    @property
    def area(self) -> int:
        return self.width * self.height

    def contains(self, other: "Room", margin: int = 0) -> bool:
        return (
            self.x - margin < other.x + other.width
            and other.x < self.x + self.width + margin
            and self.y - margin < other.y + other.height
            and other.y < self.y + self.height + margin
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "x": self.x,
            "y": self.y,
            "width": self.width,
            "height": self.height,
        }


@dataclass(frozen=True)
class Door:
    id: str
    room_a: str
    room_b: str
    direction: DoorDirection = "both"
    locked_by: str | None = None

    def allows(self, source: str, target: str) -> bool:
        if source == self.room_a and target == self.room_b:
            return self.direction in ("both", "forward")
        if source == self.room_b and target == self.room_a:
            return self.direction in ("both", "reverse")
        return False

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "id": self.id,
            "room_a": self.room_a,
            "room_b": self.room_b,
            "direction": self.direction,
        }
        if self.locked_by is not None:
            result["locked_by"] = self.locked_by
        return result


@dataclass(frozen=True)
class Tag:
    id: str
    kind: TagKind
    room_id: str
    key_id: str | None = None
    door_id: str | None = None

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "id": self.id,
            "kind": self.kind,
            "room_id": self.room_id,
        }
        if self.key_id is not None:
            result["key_id"] = self.key_id
        if self.door_id is not None:
            result["door_id"] = self.door_id
        return result


@dataclass(frozen=True)
class Level:
    grid_width: int
    grid_height: int
    rooms: tuple[Room, ...]
    doors: tuple[Door, ...]
    tags: tuple[Tag, ...]

    def room(self, room_id: str) -> Room:
        for room in self.rooms:
            if room.id == room_id:
                return room
        raise KeyError(room_id)

    def door(self, door_id: str) -> Door:
        for door in self.doors:
            if door.id == door_id:
                return door
        raise KeyError(door_id)

    def tags_at(self, room_id: str) -> tuple[Tag, ...]:
        return tuple(tag for tag in self.tags if tag.room_id == room_id)

    def tag_of_kind(self, kind: TagKind) -> tuple[Tag, ...]:
        return tuple(tag for tag in self.tags if tag.kind == kind)

    def start_room(self) -> str:
        starts = self.tag_of_kind("start")
        if len(starts) != 1:
            raise ValueError("level must have exactly one start tag")
        return starts[0].room_id

    def end_room(self) -> str:
        ends = self.tag_of_kind("end")
        if len(ends) != 1:
            raise ValueError("level must have exactly one end tag")
        return ends[0].room_id

    def to_dict(self) -> dict[str, Any]:
        return {
            "grid": {"width": self.grid_width, "height": self.grid_height},
            "rooms": [room.to_dict() for room in self.rooms],
            "doors": [door.to_dict() for door in self.doors],
            "tags": [tag.to_dict() for tag in self.tags],
        }


@dataclass(frozen=True)
class Constraint:
    id: str
    type: ConstraintType
    priority: int = 0
    hard: bool = True
    params: dict[str, Any] = field(default_factory=dict)
    decimals: int | None = None
    rounding: str | None = None

    def to_dict(self) -> dict[str, Any]:
        payload = {
            "id": self.id,
            "type": self.type,
            "priority": self.priority,
            "hard": self.hard,
            **self.params,
        }
        if self.decimals is not None:
            payload["decimals"] = self.decimals
        if self.rounding is not None:
            payload["rounding"] = self.rounding
        return payload

    @property
    def keys(self) -> tuple[str, ...]:
        return tuple(str(key) for key in self.params.get("keys", []))

    @property
    def coefficients(self) -> dict[str, float]:
        values = self.params.get("coefficients", {})
        return {str(key): float(value) for key, value in values.items()}

    @property
    def threshold(self) -> float:
        return float(self.params.get("threshold", 0.0))


@dataclass
class WorkbenchConfig:
    version: int
    seed: int
    grid_width: int
    grid_height: int
    constraints: tuple[Constraint, ...]
    decimals: int = 2
    rounding: str = "half_up"
    max_attempts: int = 64
    max_phase_retries: int = 8
    max_constraints: int = 64
    room_min_width: int = 3
    room_min_height: int = 3
    room_max_width: int = 8
    room_max_height: int = 6
    room_margin: int = 1
    rewards: int = 1
    direction_mode: str = "bidirectional"
    evaluation_mode: str = "fixed"
    uncertain_policy: str = "allow"
    raw_document: dict[str, Any] = field(default_factory=dict)
    source_hash: str = ""

    def constraints_of_type(self, kind: str, hard_only: bool = False) -> tuple[Constraint, ...]:
        return tuple(
            constraint
            for constraint in self.constraints
            if constraint.type == kind and (constraint.hard or not hard_only)
        )

    def primary_keychain(self) -> Constraint | None:
        candidates = self.constraints_of_type("key_chain")
        if not candidates:
            return None
        return sorted(candidates, key=lambda item: (-item.priority, item.id))[0]

    def room_count_bounds(self) -> tuple[int, int]:
        constraints = self.constraints_of_type("room_count")
        if not constraints:
            return (3, 12)
        minimum = max(int(item.params.get("min", 0)) for item in constraints)
        maximum = min(int(item.params.get("max", 10**9)) for item in constraints)
        return minimum, maximum

    def quantization_for(self, constraint: Constraint) -> tuple[int, str]:
        return (
            constraint.decimals if constraint.decimals is not None else self.decimals,
            constraint.rounding if constraint.rounding is not None else self.rounding,
        )

    def to_dict(self) -> dict[str, Any]:
        if self.raw_document:
            return self.raw_document
        return serialize_config(self)

    def snapshot(self) -> dict[str, Any]:
        return self.to_dict()


def serialize_config(config: WorkbenchConfig) -> dict[str, Any]:
    return {
        "version": config.version,
        "seed": config.seed,
        "grid": {"width": config.grid_width, "height": config.grid_height},
        "generation": {
            "max_attempts": config.max_attempts,
            "max_phase_retries": config.max_phase_retries,
            "room_min_width": config.room_min_width,
            "room_min_height": config.room_min_height,
            "room_max_width": config.room_max_width,
            "room_max_height": config.room_max_height,
            "room_margin": config.room_margin,
            "rewards": config.rewards,
            "direction_mode": config.direction_mode,
        },
        "evaluation": {
            "mode": config.evaluation_mode,
            "uncertain_policy": config.uncertain_policy,
        },
        "quantization": {
            "decimals": config.decimals,
            "rounding": config.rounding,
        },
        "limits": {"max_constraints": config.max_constraints},
        "constraints": [item.to_dict() for item in config.constraints],
    }


def _expect(condition: bool, message: str, location: str) -> None:
    if not condition:
        raise WorkbenchError(
            E_CONFIG_INVALID,
            message,
            details={"location": location},
        )


def label_lookup(level: Level) -> dict[str, Tag]:
    return {tag.id: tag for tag in level.tags}


def validate_level_structure(level: Level, config: WorkbenchConfig | None = None) -> list[dict[str, Any]]:
    """Structural parser used for fixtures and injected input.

    The returned list is deliberately JSON-friendly so it can travel in an error
    payload without depending on exception classes from this module.
    """

    issues: list[dict[str, Any]] = []
    room_ids = [room.id for room in level.rooms]
    if len(room_ids) != len(set(room_ids)):
        issues.append({"code": "duplicate_room_id", "location": "/rooms"})
    door_ids = [door.id for door in level.doors]
    if len(door_ids) != len(set(door_ids)):
        issues.append({"code": "duplicate_door_id", "location": "/doors"})
    tag_ids = [tag.id for tag in level.tags]
    if len(tag_ids) != len(set(tag_ids)):
        issues.append({"code": "duplicate_tag_id", "location": "/tags"})

    known_rooms = set(room_ids)
    for room_index, room in enumerate(level.rooms):
        if (
            room.x < 0
            or room.y < 0
            or room.width <= 0
            or room.height <= 0
            or room.x + room.width > level.grid_width
            or room.y + room.height > level.grid_height
        ):
            issues.append(
                {
                    "code": "room_out_of_bounds",
                    "location": f"/input_layout/rooms/{room_index}",
                    "room_id": room.id,
                }
            )
        for other in level.rooms:
            if room.id < other.id and room.contains(other, 0):
                issues.append(
                    {
                        "code": "room_overlap",
                        "location": f"/input_layout/rooms/{room_index}",
                        "room_id": room.id,
                        "other_room_id": other.id,
                    }
                )

    for door_index, door in enumerate(level.doors):
        if door.room_a not in known_rooms or door.room_b not in known_rooms:
            issues.append(
                {
                    "code": "dangling_door_endpoint",
                    "location": f"/input_layout/doors/{door_index}",
                    "door_id": door.id,
                    "missing": sorted(
                        {
                            room_id
                            for room_id in (door.room_a, door.room_b)
                            if room_id not in known_rooms
                        }
                    ),
                }
            )
        if door.room_a == door.room_b:
            issues.append(
                {
                    "code": "self_loop_door",
                    "location": f"/input_layout/doors/{door_index}",
                    "door_id": door.id,
                }
            )
        if door.direction not in ("both", "forward", "reverse"):
            issues.append(
                {
                    "code": "invalid_door_direction",
                    "location": f"/input_layout/doors/{door_index}",
                    "door_id": door.id,
                }
            )

    for tag_index, tag in enumerate(level.tags):
        if tag.room_id not in known_rooms:
            issues.append(
                {
                    "code": "dangling_tag_room",
                    "location": f"/input_layout/tags/{tag_index}",
                    "tag_id": tag.id,
                }
            )
        if tag.kind not in ("start", "end", "key", "lock", "reward"):
            issues.append(
                {
                    "code": "invalid_tag_kind",
                    "location": f"/input_layout/tags/{tag_index}",
                    "tag_id": tag.id,
                }
            )
        if tag.kind == "lock" and tag.door_id is not None and tag.door_id not in {d.id for d in level.doors}:
            issues.append(
                {
                    "code": "dangling_lock_door",
                    "location": f"/input_layout/tags/{tag_index}",
                    "tag_id": tag.id,
                }
            )
    return issues


def cycle_in_key_dependencies(dependencies: dict[str, str]) -> list[str] | None:
    """Return one cycle as a path, or None when the mapping is acyclic."""

    state: dict[str, int] = {}
    stack: list[str] = []

    def visit(node: str) -> list[str] | None:
        marker = state.get(node, 0)
        if marker == 1:
            start = stack.index(node)
            return stack[start:] + [node]
        if marker == 2:
            return None
        state[node] = 1
        stack.append(node)
        target = dependencies.get(node)
        if target is not None:
            found = visit(target)
            if found:
                return found
        stack.pop()
        state[node] = 2
        return None

    for key in sorted(dependencies):
        found = visit(key)
        if found:
            return found
    return None
