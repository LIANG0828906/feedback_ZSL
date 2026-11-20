"""Single source of truth for locked-door/key reachability."""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass, field
from typing import Any, Iterable

from .models import Door, Level


@dataclass(frozen=True)
class ReachabilityStep:
    from_room: str
    to_room: str
    door_id: str
    unlocked_with: str | None

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "from": self.from_room,
            "to": self.to_room,
            "door_id": self.door_id,
        }
        if self.unlocked_with is not None:
            payload["unlocked_with"] = self.unlocked_with
        return payload


@dataclass
class ReachabilityResult:
    start_room: str | None
    end_room: str | None
    required_keys: tuple[str, ...]
    available_keys: tuple[str, ...]
    collected_keys: tuple[str, ...]
    reached_rooms: tuple[str, ...]
    path: tuple[str, ...]
    steps: tuple[ReachabilityStep, ...]
    unlock_order: tuple[str, ...]
    end_reachable: bool
    end_reachable_without_locks: bool
    premature_end_reachable: bool
    all_keys_collected: bool
    missing_keys: tuple[str, ...]
    ok: bool
    reason: str
    diagnostics: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "start_room": self.start_room,
            "end_room": self.end_room,
            "required_keys": list(self.required_keys),
            "available_keys": list(self.available_keys),
            "collected_keys": list(self.collected_keys),
            "reached_rooms": list(self.reached_rooms),
            "path": list(self.path),
            "steps": [step.to_dict() for step in self.steps],
            "unlock_order": list(self.unlock_order),
            "end_reachable": self.end_reachable,
            "end_reachable_without_locks": self.end_reachable_without_locks,
            "premature_end_reachable": self.premature_end_reachable,
            "all_keys_collected": self.all_keys_collected,
            "missing_keys": list(self.missing_keys),
            "ok": self.ok,
            "reason": self.reason,
            "diagnostics": self.diagnostics,
        }


def _room_keys(level: Level) -> dict[str, set[str]]:
    result: dict[str, set[str]] = {room.id: set() for room in level.rooms}
    for tag in level.tags:
        if tag.kind == "key" and tag.key_id:
            result.setdefault(tag.room_id, set()).add(tag.key_id)
    return result


def _adjacency(level: Level) -> dict[str, list[Door]]:
    result: dict[str, list[Door]] = {room.id: [] for room in level.rooms}
    for door in level.doors:
        result.setdefault(door.room_a, []).append(door)
        if door.room_b != door.room_a:
            result.setdefault(door.room_b, []).append(door)
    return result


def _shortest_state_path(
    level: Level,
    start: str,
    end: str,
    required_keys: set[str],
    initial_keys: set[str],
    require_all_keys: bool,
    allow_locked: bool = True,
) -> tuple[list[str], list[ReachabilityStep], set[str], bool] | None:
    room_keys = _room_keys(level)
    adjacency = _adjacency(level)
    initial = tuple(sorted(initial_keys | room_keys.get(start, set())))
    queue: deque[tuple[str, tuple[str, ...]]] = deque([(start, initial)])
    visited: set[tuple[str, tuple[str, ...]]] = {(start, initial)}
    parent: dict[
        tuple[str, tuple[str, ...]],
        tuple[tuple[str, tuple[str, ...]], ReachabilityStep] | None,
    ] = {(start, initial): None}

    goal: tuple[str, tuple[str, ...]] | None = None
    goal_all_keys = False
    while queue:
        room_id, key_state = queue.popleft()
        key_set = set(key_state)
        if room_id == end:
            all_keys = required_keys.issubset(key_set)
            if not require_all_keys or all_keys:
                goal = (room_id, key_state)
                goal_all_keys = all_keys
                break
        for door in adjacency.get(room_id, []):
            if not door.allows(room_id, door.room_b if door.room_a == room_id else door.room_a):
                continue
            if door.locked_by is not None:
                if not allow_locked:
                    continue
                if door.locked_by not in key_set:
                    continue
            target = door.room_b if door.room_a == room_id else door.room_a
            next_keys = tuple(sorted(key_set | room_keys.get(target, set())))
            state = (target, next_keys)
            if state in visited:
                continue
            step = ReachabilityStep(
                from_room=room_id,
                to_room=target,
                door_id=door.id,
                unlocked_with=door.locked_by if door.locked_by in key_set else None,
            )
            visited.add(state)
            parent[state] = ((room_id, key_state), step)
            queue.append(state)

    if goal is None:
        return None
    path: list[str] = []
    steps: list[ReachabilityStep] = []
    cursor: tuple[str, tuple[str, ...]] | None = goal
    while cursor is not None:
        path.append(cursor[0])
        predecessor = parent[cursor]
        if predecessor is None:
            break
        cursor, step = predecessor
        steps.append(step)
    path.reverse()
    steps.reverse()
    return path, steps, set(goal[1]), goal_all_keys


def _reachable_rooms(
    level: Level,
    start: str,
    required_keys: set[str],
    initial_keys: set[str],
    allow_locked: bool,
) -> tuple[set[str], set[str]]:
    room_keys = _room_keys(level)
    adjacency = _adjacency(level)
    keys = set(initial_keys) | room_keys.get(start, set())
    reached = {start}
    changed = True
    while changed:
        changed = False
        for room_id in list(reached):
            for door in adjacency.get(room_id, []):
                if door.locked_by is not None:
                    if not allow_locked or door.locked_by not in keys:
                        continue
                target = door.room_b if door.room_a == room_id else door.room_a
                if not door.allows(room_id, target):
                    continue
                if target not in reached:
                    reached.add(target)
                    changed = True
                    keys |= room_keys.get(target, set())
    return reached, keys


def analyze_key_chain(
    level: Level,
    required_keys: Iterable[str],
    *,
    initial_keys: Iterable[str] = (),
) -> ReachabilityResult:
    """Analyze key/lock reachability using one implementation for all callers."""

    required = tuple(sorted(set(required_keys)))
    required_set = set(required)
    initial = set(initial_keys)

    start_tags = level.tag_of_kind("start")
    end_tags = level.tag_of_kind("end")
    start = start_tags[0].room_id if len(start_tags) == 1 else None
    end = end_tags[0].room_id if len(end_tags) == 1 else None
    room_keys = _room_keys(level)
    available = set().union(*room_keys.values()) if room_keys else set()

    if start is None or end is None:
        return ReachabilityResult(
            start, end, required, tuple(sorted(available)), (), (), (), (), (), False,
            False, False, False, required, False, "start/end tag missing or duplicated",
            {"start_tags": len(start_tags), "end_tags": len(end_tags)},
        )

    lock_keys = {door.locked_by for door in level.doors if door.locked_by is not None}
    missing_key_tags = sorted(lock_keys - available)
    unrequired_locks = sorted(lock_keys - required_set)

    without_locks, _ = _reachable_rooms(level, start, required_set, initial, allow_locked=False)
    end_without_locks = end in without_locks

    full_search = _shortest_state_path(
        level,
        start,
        end,
        required_set,
        initial,
        require_all_keys=True,
        allow_locked=True,
    )
    unlocked_search = _shortest_state_path(
        level,
        start,
        end,
        required_set,
        initial,
        require_all_keys=False,
        allow_locked=True,
    )

    reached_all, collected_all = _reachable_rooms(
        level, start, required_set, initial, allow_locked=True
    )
    end_without_all = False
    if unlocked_search is not None:
        _, steps_premature, keys_premature, _ = unlocked_search
        end_without_all = not required_set.issubset(keys_premature)

    if full_search is None:
        path: tuple[str, ...] = ()
        steps: tuple[ReachabilityStep, ...] = ()
        collected: set[str] = collected_all
        unlock_order: tuple[str, ...] = ()
        end_reachable = False
    else:
        room_path, path_steps, collected, _ = full_search
        path = tuple(room_path)
        steps = tuple(path_steps)
        unlock_order = tuple(
            step.unlocked_with for step in path_steps if step.unlocked_with is not None
        )
        end_reachable = True

    missing = tuple(sorted(required_set - collected))
    all_keys_collected = required_set.issubset(collected)
    reasons: list[str] = []
    if full_search is None:
        reasons.append("end is not reachable after collecting all required keys")
    if not all_keys_collected:
        reasons.append("required keys were not all collected")
    if missing_key_tags:
        reasons.append("locked doors reference keys without key tags")
    if unrequired_locks:
        reasons.append("locked doors use keys outside the required chain")
    if end_without_all:
        reasons.append("end is reachable before every required key is collected")
    if end_without_locks:
        reasons.append("end is reachable without passing any locked door")
    ok = (
        full_search is not None
        and all_keys_collected
        and not missing_key_tags
        and not unrequired_locks
        and not end_without_all
        and not end_without_locks
    )
    return ReachabilityResult(
        start_room=start,
        end_room=end,
        required_keys=required,
        available_keys=tuple(sorted(available)),
        collected_keys=tuple(sorted(collected)),
        reached_rooms=tuple(sorted(reached_all)),
        path=path,
        steps=steps,
        unlock_order=unlock_order,
        end_reachable=end_reachable,
        end_reachable_without_locks=end_without_locks,
        premature_end_reachable=end_without_all,
        all_keys_collected=all_keys_collected,
        missing_keys=missing,
        ok=ok,
        reason="; ".join(reasons) if reasons else "key chain is valid",
        diagnostics={
            "without_lock_rooms": sorted(without_locks),
            "reachable_rooms_with_keys": sorted(reached_all),
            "missing_key_tags": missing_key_tags,
            "locks_outside_required_chain": unrequired_locks,
        },
    )


def structural_connectivity(level: Level) -> tuple[bool, tuple[str, ...]]:
    """Check undirected physical connectivity, independent of key locks."""

    if not level.rooms:
        return False, ()
    adjacency: dict[str, set[str]] = {room.id: set() for room in level.rooms}
    for door in level.doors:
        adjacency.setdefault(door.room_a, set()).add(door.room_b)
        adjacency.setdefault(door.room_b, set()).add(door.room_a)
    start = level.rooms[0].id
    reached = {start}
    queue = deque([start])
    while queue:
        current = queue.popleft()
        for target in adjacency.get(current, set()):
            if target not in reached:
                reached.add(target)
                queue.append(target)
    missing = tuple(sorted({room.id for room in level.rooms} - reached))
    return not missing, missing
