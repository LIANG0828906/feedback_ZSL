"""Fault injection channel that validates damaged inputs before output."""

from __future__ import annotations

import copy
from collections import deque
from dataclasses import dataclass, field
from typing import Any

from .errors import (
    E_LEVEL_INVALID,
    LevelGenError,
)
from .generator import generate
from .models import Door, Label, Level, Room
from .reachability import key_dependency_cycles
from .validation import validate_level_structure


INJECTION_TYPES = (
    "constraint_limit",
    "dangling_door",
    "key_lock_cycle",
    "room_out_of_bounds",
    "truncated_input",
)


@dataclass
class InjectionResult:
    code: str
    message: str
    point: str
    injections_applied: list[dict[str, Any]] = field(default_factory=list)
    trace: dict[str, Any] | None = None
    report: dict[str, Any] | None = None
    level_written: bool = False

    @property
    def ok(self) -> bool:
        return False

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "injections_applied": list(self.injections_applied),
            "level_written": False,
            "message": self.message,
            "point": self.point,
        }
        if self.report is not None:
            payload["report"] = self.report
        return payload


def _request_injection(
    request: dict[str, Any],
    injection_type: str,
) -> tuple[dict[str, Any], dict[str, Any]]:
    mutated = copy.deepcopy(request)
    if injection_type == "truncated_input":
        mutated = {"seed": request.get("seed", 0)}
        return mutated, {
            "detail": "仅保留字段 ['seed']，其余字段被截断丢弃",
            "point": "request",
            "type": injection_type,
        }
    if injection_type == "constraint_limit":
        constraints = list(mutated.get("constraints", []))
        while len(constraints) <= 32:
            constraints.append(
                {
                    "hard": True,
                    "id": f"inject_limit_{len(constraints)}",
                    "kind": "room_count",
                    "params": {"min": 1, "max": 100},
                }
            )
        mutated["constraints"] = constraints
        return mutated, {
            "detail": f"约束数量被撑到 {len(constraints)} 条",
            "point": "request.constraints",
            "type": injection_type,
        }
    return mutated, {}


def _distance_map(level: Level) -> dict[int, int]:
    start = level.start_room()
    if start is None:
        return {}
    adjacency: dict[int, set[int]] = {room.id: set() for room in level.rooms}
    for door in level.doors:
        adjacency.setdefault(door.a, set()).add(door.b)
        adjacency.setdefault(door.b, set()).add(door.a)
    distances = {start: 0}
    queue: deque[int] = deque([start])
    while queue:
        current = queue.popleft()
        for neighbor in sorted(adjacency.get(current, set())):
            if neighbor not in distances:
                distances[neighbor] = distances[current] + 1
                queue.append(neighbor)
    return distances


def _inject_key_cycle(level: Level) -> tuple[Level, dict[str, Any]]:
    key_tags = [
        label.tag
        for label in level.labels
        if label.kind == "key" and label.tag
    ]
    if len(key_tags) < 2:
        raise LevelGenError(
            E_LEVEL_INVALID,
            "钥匙锁环注入至少需要两把钥匙",
            point="injection.key_lock_cycle",
        )
    first_key, second_key = key_tags[0], key_tags[1]
    distances = _distance_map(level)
    late_rooms = sorted(
        (room.id for room in level.rooms),
        key=lambda room_id: (distances.get(room_id, -1), room_id),
        reverse=True,
    )
    if len(late_rooms) < 1:
        raise LevelGenError(
            E_LEVEL_INVALID,
            "关卡没有可用于环注入的房间",
            point="injection.key_lock_cycle",
        )

    candidates: list[tuple[int, int]] = []
    for first_room in late_rooms[:4]:
        for second_room in late_rooms[:4]:
            candidates.append((first_room, second_room))
    for room_id in late_rooms[:4]:
        candidates.append((room_id, room_id))

    for first_room, second_room in candidates:
        mutated = level.clone()
        mutated.labels = [
            label
            for label in mutated.labels
            if not (
                label.kind == "key"
                and label.tag in (first_key, second_key)
            )
        ]
        mutated.labels.extend(
            [
                Label(kind="key", room=first_room, tag=first_key),
                Label(kind="key", room=second_room, tag=second_key),
            ]
        )
        if key_dependency_cycles(mutated):
            return mutated, {
                "detail": f"{first_key} 与 {second_key} 互相锁闭",
                "point": "imported_level.labels",
                "type": "key_lock_cycle",
            }
    raise LevelGenError(
        E_LEVEL_INVALID,
        "无法构造钥匙锁依赖环",
        point="injection.key_lock_cycle",
    )


def _level_injection(
    level: Level,
    injection_type: str,
) -> tuple[Level, dict[str, Any]]:
    mutated = level.clone()
    if injection_type == "room_out_of_bounds":
        if not mutated.rooms:
            raise LevelGenError(
                E_LEVEL_INVALID,
                "关卡没有房间可用于越界注入",
                point="injection.room_out_of_bounds",
            )
        original = mutated.rooms[0]
        mutated.rooms[0] = Room(
            id=original.id,
            x=mutated.grid_w,
            y=original.y,
            w=original.w,
            h=original.h,
        )
        return mutated, {
            "point": "imported_level.rooms[0].x",
            "type": injection_type,
        }
    if injection_type == "dangling_door":
        if not mutated.doors:
            raise LevelGenError(
                E_LEVEL_INVALID,
                "关卡没有门可用于悬空端点注入",
                point="injection.dangling_door",
            )
        original = mutated.doors[0]
        mutated.doors[0] = Door(
            id=original.id,
            a=original.a,
            b=max(room.id for room in mutated.rooms) + 9999,
            dir=original.dir,
        )
        return mutated, {
            "point": "imported_level.doors[0].b",
            "type": injection_type,
        }
    if injection_type == "key_lock_cycle":
        return _inject_key_cycle(mutated)
    return mutated, {}


def run_injection(
    request: dict[str, Any],
    injection_type: str,
) -> InjectionResult:
    if injection_type not in INJECTION_TYPES:
        raise LevelGenError(
            E_LEVEL_INVALID,
            f"未知注入类型: {injection_type}",
            point="injection.type",
        )

    if injection_type in ("truncated_input", "constraint_limit"):
        mutated_request, applied = _request_injection(request, injection_type)
        result = generate(mutated_request)
        error = result.error
        if error is None:
            raise LevelGenError(
                E_LEVEL_INVALID,
                f"注入 {injection_type} 未被拦截",
                point="injection",
            )
        return InjectionResult(
            code=error.code,
            message=error.message,
            point=error.point or applied["point"],
            injections_applied=[applied],
            trace=result.trace,
            report=error.report,
        )

    base = generate(request)
    if not base.ok or base.level is None:
        error = base.error or LevelGenError(
            E_LEVEL_INVALID,
            "基础关卡生成失败，无法执行注入",
            point="injection",
        )
        raise error
    mutated, applied = _level_injection(base.level, injection_type)
    try:
        validate_level_structure(mutated)
    except LevelGenError as error:
        return InjectionResult(
            code=error.code,
            message=error.message,
            point=error.point or applied["point"],
            injections_applied=[applied],
            trace=base.trace,
            report=error.report,
        )
    raise LevelGenError(
        E_LEVEL_INVALID,
        f"注入 {injection_type} 未被结构校验拦截",
        point="injection",
    )
