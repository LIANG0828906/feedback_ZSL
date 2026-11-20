"""Request preparation, structural validation and constraint checks."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .errors import (
    E_CONSTRAINT_CONFLICT,
    E_CONSTRAINT_INVALID,
    E_CONSTRAINT_LIMIT,
    E_DOOR_DANGLING,
    E_INPUT_INVALID,
    E_KEY_CYCLE,
    E_LEVEL_INVALID,
    E_ROOM_OUT_OF_BOUNDS,
    LevelGenError,
    empty_evidence,
)
from .models import Constraint, GenerationRequest, Level
from .reachability import analyze_reachability, key_dependency_cycles


MAX_CONSTRAINTS = 32
ALLOWED_CONSTRAINT_KINDS = {
    "area_target",
    "connectivity",
    "door_count_target",
    "end_locked",
    "key_chain",
    "room_count",
}
ALLOWED_LABELS = {"end", "key", "lock", "reward", "start"}
ALLOWED_DIRECTIONS = {"AB", "BA", "BIDIR"}


@dataclass
class CheckResult:
    cid: str
    ok: bool
    soft: bool
    message: str = ""
    evidence: dict[str, Any] = field(default_factory=empty_evidence)

    def to_dict(self) -> dict[str, Any]:
        return {
            "cid": self.cid,
            "evidence": {
                "doors": sorted(set(self.evidence.get("doors", []))),
                "rooms": sorted(set(self.evidence.get("rooms", []))),
            },
            "message": self.message,
            "ok": self.ok,
            "soft": self.soft,
        }


@dataclass
class PreparedRequest:
    request: GenerationRequest
    grid_w: int
    grid_h: int
    rewards: int
    max_attempts: int
    room_min_w: int
    room_max_w: int
    room_min_h: int
    room_max_h: int
    required_rooms: int
    room_hard_min: int
    room_hard_max: int
    key_order: list[str]
    room_constraints: list[Constraint]
    door_constraints: list[Constraint]
    area_constraints: list[Constraint]
    end_constraints: list[Constraint]
    connectivity_constraints: list[Constraint]
    chain_constraints: list[Constraint]


def _constraint_range(
    constraint: Constraint,
) -> tuple[int, int]:
    params = constraint.params
    if "min" in params or "max" in params:
        low = params.get("min", 0)
        high = params.get("max", 2**31 - 1)
    elif "target" in params:
        target = params.get("target")
        tolerance = params.get("tolerance", 0)
        if not isinstance(target, int) or isinstance(target, bool):
            raise LevelGenError(
                E_CONSTRAINT_INVALID,
                f"约束 {constraint.id} 的 target 必须是整数",
                point=f"request.constraints.{constraint.id}.params.target",
            )
        if not isinstance(tolerance, int) or isinstance(tolerance, bool) or tolerance < 0:
            raise LevelGenError(
                E_CONSTRAINT_INVALID,
                f"约束 {constraint.id} 的 tolerance 必须是非负整数",
                point=f"request.constraints.{constraint.id}.params.tolerance",
            )
        low = target - tolerance
        high = target + tolerance
    else:
        raise LevelGenError(
            E_CONSTRAINT_INVALID,
            f"约束 {constraint.id} 缺少 min/max 或 target/tolerance",
            point=f"request.constraints.{constraint.id}.params",
        )
    if isinstance(low, bool) or not isinstance(low, int):
        raise LevelGenError(
            E_CONSTRAINT_INVALID,
            f"约束 {constraint.id} 的下界必须是整数",
            point=f"request.constraints.{constraint.id}.params.min",
        )
    if isinstance(high, bool) or not isinstance(high, int):
        raise LevelGenError(
            E_CONSTRAINT_INVALID,
            f"约束 {constraint.id} 的上界必须是整数",
            point=f"request.constraints.{constraint.id}.params.max",
        )
    return low, high


def _read_positive_int(params: dict[str, Any], name: str, default: int, minimum: int = 1) -> int:
    value = params.get(name, default)
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise LevelGenError(
            E_INPUT_INVALID,
            f"参数 {name} 必须是大于等于 {minimum} 的整数",
            point=f"request.params.{name}",
        )
    return value


def prepare_request(request: GenerationRequest) -> PreparedRequest:
    if len(request.constraints) > MAX_CONSTRAINTS:
        raise LevelGenError(
            E_CONSTRAINT_LIMIT,
            f"约束数量 {len(request.constraints)} 超过上限 {MAX_CONSTRAINTS}",
            point="request.constraints",
        )

    seen_ids: set[str] = set()
    for index, constraint in enumerate(request.constraints):
        if constraint.id in seen_ids:
            raise LevelGenError(
                E_CONSTRAINT_INVALID,
                f"约束 id 重复: {constraint.id}",
                point=f"request.constraints[{index}].id",
            )
        seen_ids.add(constraint.id)
        if constraint.kind not in ALLOWED_CONSTRAINT_KINDS:
            raise LevelGenError(
                E_CONSTRAINT_INVALID,
                f"未知约束类型: {constraint.kind}",
                point=f"request.constraints[{index}].kind",
            )

    grid_w = _read_positive_int(request.params, "grid_w", 24, 4)
    grid_h = _read_positive_int(request.params, "grid_h", 24, 4)
    rewards = _read_positive_int(request.params, "rewards", 1, 0)
    max_attempts = _read_positive_int(request.params, "max_attempts", 64, 1)
    room_min_w = _read_positive_int(request.params, "room_min_w", 2, 1)
    room_max_w = _read_positive_int(request.params, "room_max_w", 5, room_min_w)
    room_min_h = _read_positive_int(request.params, "room_min_h", 2, 1)
    room_max_h = _read_positive_int(request.params, "room_max_h", 5, room_min_h)

    grouped: dict[str, list[Constraint]] = {
        kind: [] for kind in ALLOWED_CONSTRAINT_KINDS
    }
    for constraint in request.constraints:
        grouped[constraint.kind].append(constraint)

    room_constraints = grouped["room_count"]
    hard_room_constraints = [item for item in room_constraints if item.hard]
    room_ranges = [_constraint_range(item) for item in hard_room_constraints]
    if room_ranges:
        room_hard_min = max(item[0] for item in room_ranges)
        room_hard_max = min(item[1] for item in room_ranges)
        if room_hard_min > room_hard_max:
            identifiers = [item.id for item in hard_room_constraints]
            raise LevelGenError.conflict(
                "房间数量区间互相矛盾：约束 "
                f"{identifiers} 的交集为空（下界 {room_hard_min} > 上界 {room_hard_max}）",
                stage="static",
                constraint_ids=identifiers,
            )
    else:
        room_hard_min = 1
        room_hard_max = 2**31 - 1

    chain_constraints = grouped["key_chain"]
    if len(chain_constraints) > 1:
        raise LevelGenError(
            E_CONSTRAINT_INVALID,
            "同一请求只能定义一条钥匙链",
            point="request.constraints.key_chain",
        )
    key_order: list[str] = []
    if chain_constraints:
        raw_keys = chain_constraints[0].params.get("keys")
        if not isinstance(raw_keys, list) or not all(
            isinstance(key, str) and key for key in raw_keys
        ):
            raise LevelGenError(
                E_CONSTRAINT_INVALID,
                f"约束 {chain_constraints[0].id} 的 keys 必须是非空字符串列表",
                point=f"request.constraints.{chain_constraints[0].id}.params.keys",
            )
        if len(set(raw_keys)) != len(raw_keys):
            raise LevelGenError(
                E_CONSTRAINT_INVALID,
                f"约束 {chain_constraints[0].id} 的钥匙标签重复",
                point=f"request.constraints.{chain_constraints[0].id}.params.keys",
            )
        key_order = list(raw_keys)

    end_constraints = grouped["end_locked"]
    if end_constraints and not key_order:
        identifiers = [item.id for item in end_constraints]
        raise LevelGenError.conflict(
            f"约束 {identifiers[0]}（无锁不可达终点）需要至少一条钥匙链，"
            "但约束集合中没有 key_chain",
            stage="static",
            constraint_ids=identifiers,
        )

    required_rooms = 2 + (2 * len(key_order)) + rewards
    if room_hard_max < required_rooms:
        identifiers = [item.id for item in chain_constraints]
        identifiers.extend(item.id for item in hard_room_constraints)
        raise LevelGenError.conflict(
            f"钥匙链 {key_order} 需要至少 {required_rooms} 个房间"
            "（起点+终点+钥匙+锁+奖励），"
            f"超过房间数量上界 {room_hard_max}",
            stage="static",
            constraint_ids=identifiers or [item.id for item in chain_constraints],
        )
    if required_rooms > max(grid_w, 1) * max(grid_h, 1):
        raise LevelGenError.conflict(
            f"至少需要 {required_rooms} 个房间，但网格仅能提供 "
            f"{grid_w * grid_h} 个单元",
            stage="static",
            constraint_ids=[item.id for item in room_constraints]
            or [item.id for item in chain_constraints],
        )

    area_constraints = grouped["area_target"]
    hard_area_constraints = [
        constraint for constraint in area_constraints if constraint.hard
    ]
    hard_area_range = (
        tuple(
            (
                max(_constraint_range(constraint)[0] for constraint in hard_area_constraints),
                min(_constraint_range(constraint)[1] for constraint in hard_area_constraints),
            )
        )
        if hard_area_constraints
        else None
    )
    if hard_area_range is not None and hard_area_range[0] > hard_area_range[1]:
        identifiers = [constraint.id for constraint in hard_area_constraints]
        raise LevelGenError.conflict(
            "面积目标区间互相矛盾：约束 "
            f"{identifiers} 的硬区间交集为空"
            f"（下界 {hard_area_range[0]} > 上界 {hard_area_range[1]}）",
            stage="static",
            constraint_ids=identifiers,
        )
    for constraint in area_constraints:
        low, high = _constraint_range(constraint)
        if constraint.hard and high < required_rooms * 4:
            raise LevelGenError.conflict(
                f"约束 {constraint.id} 的面积区间 [{low}, {high}] 低于"
                f" {required_rooms} 个最小 2x2 房间所需的 {required_rooms * 4}",
                stage="static",
                constraint_ids=[constraint.id],
            )

    door_constraints = grouped["door_count_target"]
    hard_door_constraints = [
        constraint for constraint in door_constraints if constraint.hard
    ]
    hard_door_range = (
        (
            max(_constraint_range(constraint)[0] for constraint in hard_door_constraints),
            min(_constraint_range(constraint)[1] for constraint in hard_door_constraints),
        )
        if hard_door_constraints
        else None
    )
    if hard_door_range is not None and hard_door_range[0] > hard_door_range[1]:
        identifiers = [constraint.id for constraint in hard_door_constraints]
        raise LevelGenError.conflict(
            "门数量目标区间互相矛盾：约束 "
            f"{identifiers} 的硬区间交集为空"
            f"（下界 {hard_door_range[0]} > 上界 {hard_door_range[1]}）",
            stage="static",
            constraint_ids=identifiers,
        )
    for constraint in door_constraints:
        low, high = _constraint_range(constraint)
        if constraint.hard and high < required_rooms - 1:
            raise LevelGenError.conflict(
                f"约束 {constraint.id} 的门数量上界 {high} 小于连通 "
                f"{required_rooms} 个房间所需的最少 {required_rooms - 1} 扇门",
                stage="static",
                constraint_ids=[constraint.id],
            )

    return PreparedRequest(
        request=request,
        grid_w=grid_w,
        grid_h=grid_h,
        rewards=rewards,
        max_attempts=max_attempts,
        room_min_w=min(room_min_w, grid_w),
        room_max_w=min(room_max_w, grid_w),
        room_min_h=min(room_min_h, grid_h),
        room_max_h=min(room_max_h, grid_h),
        required_rooms=required_rooms,
        room_hard_min=room_hard_min,
        room_hard_max=room_hard_max,
        key_order=key_order,
        room_constraints=room_constraints,
        door_constraints=door_constraints,
        area_constraints=area_constraints,
        end_constraints=end_constraints,
        connectivity_constraints=grouped["connectivity"],
        chain_constraints=chain_constraints,
    )


def validate_level_structure(level: Level) -> None:
    if level.grid_w <= 0 or level.grid_h <= 0:
        raise LevelGenError(
            E_LEVEL_INVALID,
            "网格尺寸必须为正",
            point="imported_level.grid",
        )

    room_ids: set[int] = set()
    for index, room in enumerate(level.rooms):
        if room.id in room_ids:
            raise LevelGenError(
                E_LEVEL_INVALID,
                f"房间 id 重复: {room.id}",
                point=f"imported_level.rooms[{index}].id",
            )
        room_ids.add(room.id)
        if room.w <= 0 or room.h <= 0:
            raise LevelGenError(
                E_LEVEL_INVALID,
                f"房间 {room.id} 的宽高必须为正",
                point=f"imported_level.rooms[{index}]",
            )
        if (
            room.x < 0
            or room.y < 0
            or room.x + room.w > level.grid_w
            or room.y + room.h > level.grid_h
        ):
            raise LevelGenError(
                E_ROOM_OUT_OF_BOUNDS,
                f"房间 {room.id} 矩形 ({room.x},{room.y},{room.w},{room.h}) "
                f"越出网格 {level.grid_w}x{level.grid_h}",
                point=f"imported_level.rooms[{index}]",
            )

    for left_index, left in enumerate(level.rooms):
        for right in level.rooms[left_index + 1 :]:
            separated = (
                left.x + left.w <= right.x
                or right.x + right.w <= left.x
                or left.y + left.h <= right.y
                or right.y + right.h <= left.y
            )
            if not separated:
                raise LevelGenError(
                    E_LEVEL_INVALID,
                    f"房间 {left.id} 与 {right.id} 重叠",
                    point=f"imported_level.rooms[{left_index}]",
                )

    door_ids: set[int] = set()
    for index, door in enumerate(level.doors):
        if door.id in door_ids:
            raise LevelGenError(
                E_LEVEL_INVALID,
                f"门 id 重复: {door.id}",
                point=f"imported_level.doors[{index}].id",
            )
        door_ids.add(door.id)
        if door.a not in room_ids or door.b not in room_ids:
            missing = door.b if door.b not in room_ids else door.a
            raise LevelGenError(
                E_DOOR_DANGLING,
                f"门 {door.id} 端点 ({door.a},{door.b}) 指向不存在的房间",
                point=f"imported_level.doors[{index}]",
            )
        if door.a == door.b:
            raise LevelGenError(
                E_LEVEL_INVALID,
                f"门 {door.id} 不能连接同一房间",
                point=f"imported_level.doors[{index}]",
            )
        if door.dir not in ALLOWED_DIRECTIONS:
            raise LevelGenError(
                E_LEVEL_INVALID,
                f"门 {door.id} 的方向 {door.dir} 非法",
                point=f"imported_level.doors[{index}].dir",
            )

    start_labels = 0
    end_labels = 0
    key_tags: set[str] = set()
    lock_tags: set[str] = set()
    for index, label in enumerate(level.labels):
        if label.room not in room_ids:
            raise LevelGenError(
                E_LEVEL_INVALID,
                f"标签 {label.kind} 指向不存在的房间 {label.room}",
                point=f"imported_level.labels[{index}].room",
            )
        if label.kind not in ALLOWED_LABELS:
            raise LevelGenError(
                E_LEVEL_INVALID,
                f"未知标签类型: {label.kind}",
                point=f"imported_level.labels[{index}].kind",
            )
        if label.kind == "start":
            start_labels += 1
        elif label.kind == "end":
            end_labels += 1
        elif label.kind in ("key", "lock"):
            if not label.tag:
                raise LevelGenError(
                    E_LEVEL_INVALID,
                    f"{label.kind} 标签缺少 tag",
                    point=f"imported_level.labels[{index}].tag",
                )
            if label.kind == "key":
                if label.tag in key_tags:
                    raise LevelGenError(
                        E_LEVEL_INVALID,
                        f"钥匙标签重复: {label.tag}",
                        point=f"imported_level.labels[{index}].tag",
                    )
                key_tags.add(label.tag)
            else:
                lock_tags.add(label.tag)

    if start_labels != 1 or end_labels != 1:
        raise LevelGenError(
            E_LEVEL_INVALID,
            f"关卡必须恰有一个起点和一个终点，实际起点 {start_labels}、终点 {end_labels}",
            point="imported_level.labels",
        )
    missing_locks = key_tags - lock_tags
    if missing_locks:
        raise LevelGenError(
            E_LEVEL_INVALID,
            f"钥匙 {sorted(missing_locks)} 没有对应锁",
            point="imported_level.labels",
        )

    cycles = key_dependency_cycles(level)
    if cycles:
        cycle_text = " -> ".join(cycles[0])
        raise LevelGenError(
            E_KEY_CYCLE,
            f"钥匙锁依赖成环: {cycle_text}",
            point="imported_level.labels",
        )


def _check_connectivity(level: Level, constraint: Constraint) -> CheckResult:
    adjacency: dict[int, set[int]] = {room.id: set() for room in level.rooms}
    for door in level.doors:
        adjacency.setdefault(door.a, set()).add(door.b)
        adjacency.setdefault(door.b, set()).add(door.a)
    start = level.rooms[0].id if level.rooms else None
    visited: set[int] = set()
    if start is not None:
        stack = [start]
        visited.add(start)
        while stack:
            current = stack.pop()
            for neighbor in sorted(adjacency.get(current, set()), reverse=True):
                if neighbor not in visited:
                    visited.add(neighbor)
                    stack.append(neighbor)
    missing_rooms = sorted(set(adjacency) - visited)
    incident_doors = sorted(
        door.id
        for door in level.doors
        if door.a in missing_rooms or door.b in missing_rooms
    )
    ok = not missing_rooms and len(level.rooms) > 0
    message = "" if ok else f"图不连通，未到达房间 {missing_rooms}"
    return CheckResult(
        cid=constraint.id,
        ok=ok,
        soft=not constraint.hard,
        message=message,
        evidence={"doors": incident_doors, "rooms": missing_rooms},
    )


def _check_room_count(level: Level, constraint: Constraint) -> CheckResult:
    low, high = _constraint_range(constraint)
    count = len(level.rooms)
    ok = low <= count <= high
    message = "" if ok else f"房间数量 {count} 不在区间 [{low}, {high}]"
    return CheckResult(
        cid=constraint.id,
        ok=ok,
        soft=not constraint.hard,
        message=message,
        evidence={"rooms": [room.id for room in level.rooms], "doors": []},
    )


def _check_area(level: Level, constraint: Constraint) -> CheckResult:
    low, high = _constraint_range(constraint)
    area = sum(room.area for room in level.rooms)
    ok = low <= area <= high
    message = "" if ok else f"总面积 {area} 不在面积目标区间 [{low}, {high}]"
    return CheckResult(
        cid=constraint.id,
        ok=ok,
        soft=not constraint.hard,
        message=message,
        evidence={"rooms": [room.id for room in level.rooms], "doors": []},
    )


def _check_doors(level: Level, constraint: Constraint) -> CheckResult:
    low, high = _constraint_range(constraint)
    count = len(level.doors)
    ok = low <= count <= high
    message = "" if ok else f"门数量 {count} 不在目标区间 [{low}, {high}]"
    return CheckResult(
        cid=constraint.id,
        ok=ok,
        soft=not constraint.hard,
        message=message,
        evidence={"rooms": [], "doors": [door.id for door in level.doors]},
    )


def _reachability_evidence(
    level: Level,
    key_order: list[str],
    analysis: dict[str, Any],
) -> dict[str, list[int]]:
    rooms: set[int] = set()
    doors: set[int] = set()
    start = level.start_room()
    end = level.end_room()
    if start is not None:
        rooms.add(start)
    if end is not None:
        rooms.add(end)
    for label in level.labels:
        if label.kind in ("key", "lock") and (
            label.tag in key_order or not key_order
        ):
            rooms.add(label.room)
    full = analysis.get("full", {})
    rooms.update(full.get("room_sequence", []))
    doors.update(full.get("door_sequence", []))
    no_lock = analysis.get("no_lock", {})
    rooms.update(no_lock.get("room_sequence", []))
    doors.update(no_lock.get("door_sequence", []))
    return {"rooms": sorted(rooms), "doors": sorted(doors)}


def _check_key_chain(
    level: Level,
    constraint: Constraint,
    analysis: dict[str, Any],
) -> CheckResult:
    key_order = [
        key for key in constraint.params.get("keys", []) if isinstance(key, str)
    ]
    evidence = _reachability_evidence(level, key_order, analysis)
    if not analysis.get("key_chain_ok", False):
        failed = [
            stage["key"]
            for stage in analysis.get("stages", [])
            if not stage.get("ok", False)
        ]
        message = f"钥匙链顺序不可达，失败钥匙 {failed}"
    elif not analysis.get("end_after_all_keys", False):
        message = "取得全部钥匙后终点仍不可达"
    else:
        message = ""
    return CheckResult(
        cid=constraint.id,
        ok=not message,
        soft=not constraint.hard,
        message=message,
        evidence=evidence,
    )


def _check_end_locked(
    level: Level,
    constraint: Constraint,
    analysis: dict[str, Any],
) -> CheckResult:
    key_order = sorted(
        {
            label.tag
            for label in level.labels
            if label.kind == "key" and label.tag
        }
    )
    evidence = _reachability_evidence(level, key_order, analysis)
    no_lock = analysis.get("no_lock", {})
    problems: list[str] = []
    if no_lock.get("end_reachable", False):
        problems.append("无钥匙状态下已可到达终点")
    if not analysis.get("end_after_all_keys", False):
        problems.append("取得全部钥匙后终点不可达")
    return CheckResult(
        cid=constraint.id,
        ok=not problems,
        soft=not constraint.hard,
        message="；".join(problems),
        evidence=evidence,
    )


def evaluate_constraints(
    level: Level,
    constraints: list[Constraint],
    *,
    key_order: list[str] | None = None,
) -> list[CheckResult]:
    analysis = analyze_reachability(level, key_order)
    results: list[CheckResult] = []
    for constraint in constraints:
        if constraint.kind == "room_count":
            results.append(_check_room_count(level, constraint))
        elif constraint.kind == "connectivity":
            results.append(_check_connectivity(level, constraint))
        elif constraint.kind == "area_target":
            results.append(_check_area(level, constraint))
        elif constraint.kind == "door_count_target":
            results.append(_check_doors(level, constraint))
        elif constraint.kind == "key_chain":
            results.append(_check_key_chain(level, constraint, analysis))
        elif constraint.kind == "end_locked":
            results.append(_check_end_locked(level, constraint, analysis))
    return results


def failed_report(
    *,
    stage: str,
    attempts: int,
    checks: list[CheckResult],
    fallback_message: str,
) -> LevelGenError:
    failed = [check for check in checks if not check.ok and not check.soft]
    identifiers = [check.cid for check in failed]
    rooms: set[int] = set()
    doors: set[int] = set()
    for check in failed:
        rooms.update(check.evidence.get("rooms", []))
        doors.update(check.evidence.get("doors", []))
    messages = [check.message for check in failed if check.message]
    message = fallback_message
    if messages:
        message = "；".join(messages)
    report = {
        "attempts": attempts,
        "constraint_ids": identifiers,
        "evidence": {"doors": sorted(doors), "rooms": sorted(rooms)},
        "message": message,
        "stage": stage,
    }
    return LevelGenError(
        code=E_CONSTRAINT_CONFLICT,
        message=message,
        point="request.constraints",
        stage=stage,
        constraint_ids=identifiers,
        evidence=report["evidence"],
        report=report,
    )
