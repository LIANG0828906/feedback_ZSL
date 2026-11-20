"""Serializable level, constraint and request models."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .canonical import content_hash
from .errors import (
    E_CONSTRAINT_INVALID,
    E_INPUT_INVALID,
    E_INPUT_TRUNCATED,
    LevelGenError,
)


def _as_int(value: Any, field_name: str, point: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise LevelGenError(
            E_INPUT_INVALID,
            f"字段 {field_name} 必须是整数",
            point=point,
        )
    return value


@dataclass(frozen=True)
class Room:
    id: int
    x: int
    y: int
    w: int
    h: int

    @classmethod
    def from_dict(cls, value: dict[str, Any], point: str = "level.rooms") -> "Room":
        if not isinstance(value, dict):
            raise LevelGenError(E_INPUT_INVALID, "房间必须是对象", point=point)
        return cls(
            id=_as_int(value.get("id"), "id", f"{point}.id"),
            x=_as_int(value.get("x"), "x", f"{point}.x"),
            y=_as_int(value.get("y"), "y", f"{point}.y"),
            w=_as_int(value.get("w"), "w", f"{point}.w"),
            h=_as_int(value.get("h"), "h", f"{point}.h"),
        )

    def to_dict(self) -> dict[str, int]:
        return {"id": self.id, "x": self.x, "y": self.y, "w": self.w, "h": self.h}

    @property
    def area(self) -> int:
        return self.w * self.h


@dataclass(frozen=True)
class Door:
    id: int
    a: int
    b: int
    dir: str = "BIDIR"

    @classmethod
    def from_dict(cls, value: dict[str, Any], point: str = "level.doors") -> "Door":
        if not isinstance(value, dict):
            raise LevelGenError(E_INPUT_INVALID, "门必须是对象", point=point)
        return cls(
            id=_as_int(value.get("id"), "id", f"{point}.id"),
            a=_as_int(value.get("a"), "a", f"{point}.a"),
            b=_as_int(value.get("b"), "b", f"{point}.b"),
            dir=str(value.get("dir", "BIDIR")),
        )

    def to_dict(self) -> dict[str, Any]:
        return {"id": self.id, "a": self.a, "b": self.b, "dir": self.dir}


@dataclass(frozen=True)
class Label:
    kind: str
    room: int
    tag: str | None = None

    @classmethod
    def from_dict(cls, value: dict[str, Any], point: str = "level.labels") -> "Label":
        if not isinstance(value, dict):
            raise LevelGenError(E_INPUT_INVALID, "标签必须是对象", point=point)
        tag = value.get("tag")
        if tag is not None and not isinstance(tag, str):
            raise LevelGenError(E_INPUT_INVALID, "tag 必须是字符串", point=f"{point}.tag")
        return cls(
            kind=str(value.get("kind", "")),
            room=_as_int(value.get("room"), "room", f"{point}.room"),
            tag=tag,
        )

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"kind": self.kind, "room": self.room}
        if self.tag is not None:
            payload["tag"] = self.tag
        return payload


@dataclass
class Level:
    grid_w: int
    grid_h: int
    rooms: list[Room] = field(default_factory=list)
    doors: list[Door] = field(default_factory=list)
    labels: list[Label] = field(default_factory=list)
    meta: dict[str, Any] = field(default_factory=lambda: {"generator": "levelgen/1.0"})

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "Level":
        if not isinstance(value, dict):
            raise LevelGenError(E_INPUT_INVALID, "关卡必须是对象", point="level")
        grid = value.get("grid")
        if not isinstance(grid, dict):
            raise LevelGenError(E_INPUT_INVALID, "关卡缺少 grid", point="level.grid")
        rooms_raw = value.get("rooms", [])
        doors_raw = value.get("doors", [])
        labels_raw = value.get("labels", [])
        if not all(isinstance(item, list) for item in (rooms_raw, doors_raw, labels_raw)):
            raise LevelGenError(E_INPUT_INVALID, "rooms/doors/labels 必须是列表", point="level")
        return cls(
            grid_w=_as_int(grid.get("w"), "grid.w", "level.grid.w"),
            grid_h=_as_int(grid.get("h"), "grid.h", "level.grid.h"),
            rooms=[
                Room.from_dict(room, f"level.rooms[{index}]")
                for index, room in enumerate(rooms_raw)
            ],
            doors=[
                Door.from_dict(door, f"level.doors[{index}]")
                for index, door in enumerate(doors_raw)
            ],
            labels=[
                Label.from_dict(label, f"level.labels[{index}]")
                for index, label in enumerate(labels_raw)
            ],
            meta=dict(value.get("meta", {"generator": "levelgen/1.0"})),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "doors": [door.to_dict() for door in self.doors],
            "grid": {"h": self.grid_h, "w": self.grid_w},
            "labels": [label.to_dict() for label in self.labels],
            "meta": dict(self.meta),
            "rooms": [room.to_dict() for room in self.rooms],
        }

    def clone(self) -> "Level":
        return Level.from_dict(self.to_dict())

    @property
    def content_hash(self) -> str:
        return content_hash(self.to_dict())

    def room_map(self) -> dict[int, Room]:
        return {room.id: room for room in self.rooms}

    def door_map(self) -> dict[int, Door]:
        return {door.id: door for door in self.doors}

    def labels_of(self, kind: str) -> list[Label]:
        return [label for label in self.labels if label.kind == kind]

    def start_room(self) -> int | None:
        labels = self.labels_of("start")
        return labels[0].room if labels else None

    def end_room(self) -> int | None:
        labels = self.labels_of("end")
        return labels[0].room if labels else None


@dataclass(frozen=True)
class Constraint:
    id: str
    kind: str
    hard: bool
    params: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_dict(
        cls,
        value: dict[str, Any],
        point: str = "request.constraints",
    ) -> "Constraint":
        if not isinstance(value, dict):
            raise LevelGenError(E_INPUT_INVALID, "约束必须是对象", point=point)
        identifier = value.get("id")
        kind = value.get("kind")
        if not isinstance(identifier, str) or not identifier:
            raise LevelGenError(
                E_CONSTRAINT_INVALID,
                "约束 id 无效",
                point=f"{point}.id",
            )
        if not isinstance(kind, str) or not kind:
            raise LevelGenError(E_INPUT_INVALID, "约束 kind 无效", point=f"{point}.kind")
        params = value.get("params", {})
        if not isinstance(params, dict):
            raise LevelGenError(E_INPUT_INVALID, "约束 params 必须是对象", point=f"{point}.params")
        hard = value.get("hard", True)
        if not isinstance(hard, bool):
            raise LevelGenError(E_INPUT_INVALID, "约束 hard 必须是布尔值", point=f"{point}.hard")
        return cls(id=identifier, kind=kind, hard=hard, params=dict(params))

    def to_dict(self) -> dict[str, Any]:
        return {
            "hard": self.hard,
            "id": self.id,
            "kind": self.kind,
            "params": dict(self.params),
        }


@dataclass
class GenerationRequest:
    seed: int
    constraints: list[Constraint]
    params: dict[str, Any]

    @classmethod
    def from_dict(cls, value: Any) -> "GenerationRequest":
        if not isinstance(value, dict):
            raise LevelGenError(E_INPUT_TRUNCATED, "输入必须是请求对象", point="request")
        required = ("seed", "params", "constraints")
        missing = [name for name in required if name not in value]
        if missing:
            raise LevelGenError(
                E_INPUT_TRUNCATED,
                f"输入被截断：缺少必需字段 {', '.join(required)}",
                point="request",
            )
        seed = value.get("seed")
        if isinstance(seed, bool) or not isinstance(seed, int):
            raise LevelGenError(E_INPUT_INVALID, "seed 必须是整数", point="request.seed")
        params = value.get("params")
        constraints_raw = value.get("constraints")
        if not isinstance(params, dict):
            raise LevelGenError(E_INPUT_INVALID, "params 必须是对象", point="request.params")
        if not isinstance(constraints_raw, list):
            raise LevelGenError(
                E_INPUT_INVALID,
                "constraints 必须是列表",
                point="request.constraints",
            )
        return cls(
            seed=seed,
            params=dict(params),
            constraints=[
                Constraint.from_dict(constraint, f"request.constraints[{index}]")
                for index, constraint in enumerate(constraints_raw)
            ],
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "constraints": [constraint.to_dict() for constraint in self.constraints],
            "params": dict(self.params),
            "seed": self.seed,
        }

    @property
    def input_hash(self) -> str:
        return content_hash(self.to_dict())
