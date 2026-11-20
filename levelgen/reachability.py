"""The single reachability implementation used by generation and checks."""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass
from typing import Any

from .models import Door, Level


@dataclass(frozen=True)
class TraversalResult:
    reachable_rooms: list[int]
    room_sequence: list[int]
    door_sequence: list[int]
    unlock_order: list[str]
    collected_keys: list[str]
    end_reachable: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "collected_keys": list(self.collected_keys),
            "door_sequence": list(self.door_sequence),
            "end_reachable": self.end_reachable,
            "reachable_rooms": list(self.reachable_rooms),
            "room_sequence": list(self.room_sequence),
            "unlock_order": list(self.unlock_order),
        }


def _adjacency(level: Level) -> dict[int, list[tuple[int, Door]]]:
    adjacency: dict[int, list[tuple[int, Door]]] = {
        room.id: [] for room in level.rooms
    }
    for door in sorted(level.doors, key=lambda item: item.id):
        if door.a in adjacency and door.b in adjacency:
            if door.dir in ("BIDIR", "AB"):
                adjacency[door.a].append((door.b, door))
            if door.dir in ("BIDIR", "BA"):
                adjacency[door.b].append((door.a, door))
    return adjacency


def _room_locks(level: Level) -> dict[int, str]:
    return {
        label.room: label.tag or ""
        for label in level.labels
        if label.kind == "lock"
    }


def traverse(
    level: Level,
    *,
    accepted_keys: set[str] | None,
    start_room: int,
    unlock_locks: bool,
    target_room: int | None = None,
) -> TraversalResult:
    """Traverse directed doors while applying the same room-lock rule everywhere."""

    adjacency = _adjacency(level)
    locks = _room_locks(level)
    keys_by_room: dict[int, list[str]] = {}
    for label in level.labels:
        if label.kind == "key" and label.tag:
            keys_by_room.setdefault(label.room, []).append(label.tag)

    allowed = set(accepted_keys or set())
    visited: set[int] = {start_room}
    parent_room: dict[int, int] = {}
    parent_door: dict[int, int] = {}
    queue: deque[int] = deque([start_room])
    unlock_order: list[str] = []
    collected_keys: list[str] = []

    while True:
        changed = False
        while queue:
            current = queue.popleft()
            for neighbor, door in adjacency.get(current, []):
                if neighbor in visited:
                    continue
                lock_tag = locks.get(neighbor)
                if lock_tag and unlock_locks:
                    if lock_tag not in allowed:
                        continue
                    if lock_tag not in unlock_order:
                        unlock_order.append(lock_tag)
                elif lock_tag and not unlock_locks:
                    continue
                visited.add(neighbor)
                parent_room[neighbor] = current
                parent_door[neighbor] = door.id
                for key_tag in sorted(keys_by_room.get(neighbor, [])):
                    if key_tag not in collected_keys:
                        collected_keys.append(key_tag)
                queue.append(neighbor)
                changed = True
        if not changed:
            break

    destination = target_room
    end_reachable = destination is not None and destination in visited
    room_sequence: list[int] = []
    door_sequence: list[int] = []
    if end_reachable and destination is not None:
        cursor = destination
        room_sequence.append(cursor)
        while cursor != start_room:
            door_sequence.append(parent_door[cursor])
            cursor = parent_room[cursor]
            room_sequence.append(cursor)
        room_sequence.reverse()
        door_sequence.reverse()

    return TraversalResult(
        reachable_rooms=sorted(visited),
        room_sequence=room_sequence,
        door_sequence=door_sequence,
        unlock_order=unlock_order,
        collected_keys=collected_keys,
        end_reachable=end_reachable,
    )


def analyze_reachability(
    level: Level,
    key_order: list[str] | None = None,
) -> dict[str, Any]:
    """Build an auditable unlock schedule from the common traversal engine."""

    start_room = level.start_room()
    end_room = level.end_room()
    if start_room is None or end_room is None:
        return {
            "end_after_all_keys": False,
            "full": TraversalResult([], [], [], [], [], False).to_dict(),
            "key_chain_ok": False,
            "no_lock": TraversalResult([], [], [], [], [], False).to_dict(),
            "stages": [],
        }

    ordered_keys = list(key_order or [])
    if not ordered_keys:
        ordered_keys = sorted(
            {
                label.tag
                for label in level.labels
                if label.kind == "key" and label.tag
            }
        )

    no_lock = traverse(
        level,
        accepted_keys=set(),
        start_room=start_room,
        unlock_locks=False,
        target_room=end_room,
    )

    stages: list[dict[str, Any]] = []
    accepted: set[str] = set()
    key_chain_ok = True
    for key_tag in ordered_keys:
        key_rooms = {
            label.room
            for label in level.labels
            if label.kind == "key" and label.tag == key_tag
        }
        stage_result = traverse(
            level,
            accepted_keys=set(accepted),
            start_room=start_room,
            unlock_locks=True,
            target_room=end_room,
        )
        key_reachable = any(room in stage_result.reachable_rooms for room in key_rooms)
        leaked_keys = [
            later_key
            for later_key in ordered_keys[len(stages) + 1 :]
            if any(
                label.room in stage_result.reachable_rooms
                for label in level.labels
                if label.kind == "key" and label.tag == later_key
            )
        ]
        stage_ok = key_reachable and not leaked_keys
        key_chain_ok = key_chain_ok and stage_ok
        stages.append(
            {
                "available_keys": sorted(accepted),
                "key": key_tag,
                "key_rooms": sorted(key_rooms),
                "leaked_later_keys": leaked_keys,
                "ok": stage_ok,
                "result": stage_result.to_dict(),
            }
        )
        accepted.add(key_tag)

    full = traverse(
        level,
        accepted_keys=set(ordered_keys),
        start_room=start_room,
        unlock_locks=True,
        target_room=end_room,
    )
    end_after_all_keys = (
        full.end_reachable
        and set(ordered_keys).issubset(set(full.collected_keys))
    )
    return {
        "end_after_all_keys": end_after_all_keys,
        "full": full.to_dict(),
        "key_chain_ok": key_chain_ok,
        "no_lock": no_lock.to_dict(),
        "stages": stages,
    }


def key_dependency_cycles(level: Level) -> list[list[str]]:
    """Find key dependency cycles from lock rooms crossed on shortest paths."""

    start_room = level.start_room()
    if start_room is None:
        return []

    key_rooms: dict[str, list[int]] = {}
    lock_tags = {
        label.room: label.tag or ""
        for label in level.labels
        if label.kind == "lock"
    }
    for label in level.labels:
        if label.kind == "key" and label.tag:
            key_rooms.setdefault(label.tag, []).append(label.room)

    dependencies: dict[str, set[str]] = {key: set() for key in key_rooms}
    for key_tag, rooms in key_rooms.items():
        for room_id in rooms:
            result = traverse(
                level,
                accepted_keys=set(key_rooms),
                start_room=start_room,
                unlock_locks=True,
                target_room=room_id,
            )
            path_locks = {
                lock_tags[path_room]
                for path_room in result.room_sequence
                if path_room in lock_tags and lock_tags[path_room]
            }
            dependencies[key_tag].update(
                lock_tag for lock_tag in path_locks if lock_tag in key_rooms
            )

    cycles: list[list[str]] = []
    state: dict[str, int] = {key: 0 for key in dependencies}
    stack: list[str] = []

    def visit(node: str) -> None:
        state[node] = 1
        stack.append(node)
        for dependency in sorted(dependencies.get(node, set())):
            if state.get(dependency, 0) == 0:
                visit(dependency)
            elif state.get(dependency) == 1:
                cycle_start = stack.index(dependency)
                cycle = stack[cycle_start:] + [dependency]
                if cycle not in cycles:
                    cycles.append(cycle)
        stack.pop()
        state[node] = 2

    for key_tag in sorted(dependencies):
        if state[key_tag] == 0:
            visit(key_tag)
    cycles.sort(key=lambda cycle: (-len(cycle), cycle))
    return cycles
