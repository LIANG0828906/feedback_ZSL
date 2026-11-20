"""Deterministic staged level generation with rollback and diagnostics."""

from __future__ import annotations

import copy
import threading
from dataclasses import dataclass, field
from typing import Any, Callable

from .errors import (
    E_CANCELLED,
    E_CONSTRAINT_CONFLICT,
    E_CONSTRAINT_UNSATISFIED,
    LevelGenError,
    empty_evidence,
)
from .models import Constraint, Door, GenerationRequest, Label, Level, Room
from .rng import DeterministicRNG
from .trace import TraceRecorder
from .validation import (
    CheckResult,
    PreparedRequest,
    evaluate_constraints,
    failed_report,
    prepare_request,
    validate_level_structure,
)


@dataclass
class GenerationResult:
    level: Level | None
    trace: dict[str, Any] | None
    error: LevelGenError | None = None
    attempts: int = 0

    @property
    def ok(self) -> bool:
        return self.level is not None and self.error is None

    @property
    def code(self) -> str | None:
        return None if self.error is None else self.error.code

    @property
    def conflict(self) -> dict[str, Any] | None:
        return None if self.error is None else self.error.report

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "attempts": self.attempts,
            "level": None if self.level is None else self.level.to_dict(),
            "ok": self.ok,
            "trace": self.trace,
        }
        if self.error is not None:
            payload["code"] = self.error.code
            payload["conflict"] = self.error.report
            payload["error"] = self.error.to_dict()
        return payload


@dataclass
class _State:
    level: Level


class _StageContext:
    def __init__(
        self,
        state: _State,
        recorder: TraceRecorder,
        stage: str,
        attempt: int,
    ) -> None:
        self.state = state
        self.recorder = recorder
        self.stage = stage
        self.attempt = attempt
        self.snapshot: Level | None = None

    def __enter__(self) -> "_StageContext":
        self.snapshot = self.state.level.clone()
        self.recorder.stage_start(
            self.stage,
            self.attempt,
            self.snapshot.content_hash,
        )
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> bool:
        if exc is None:
            self.recorder.stage_end(
                self.stage,
                self.attempt,
                self.state.level,
            )
            return False
        if self.snapshot is not None:
            self.state.level = self.snapshot
        if isinstance(exc, LevelGenError):
            self.recorder.stage_failure(self.stage, self.attempt, exc)
        else:
            wrapped = LevelGenError(
                E_CONSTRAINT_UNSATISFIED,
                f"阶段 {self.stage} 内部失败: {exc}",
                stage=self.stage,
            )
            self.recorder.stage_failure(self.stage, self.attempt, wrapped)
        self.recorder.stage_rollback(
            self.stage,
            self.attempt,
            self.state.level.content_hash,
        )
        return False


def _constraint_range(constraint: Constraint) -> tuple[int, int]:
    params = constraint.params
    if "min" in params or "max" in params:
        return int(params.get("min", 0)), int(params.get("max", 2**31 - 1))
    target = int(params.get("target", 0))
    tolerance = int(params.get("tolerance", 0))
    return target - tolerance, target + tolerance


def _intersection(constraints: list[Constraint]) -> tuple[int, int] | None:
    if not constraints:
        return None
    ranges = [_constraint_range(constraint) for constraint in constraints]
    low = max(item[0] for item in ranges)
    high = min(item[1] for item in ranges)
    if low > high:
        return None
    return low, high


def _overlaps(room: Room, existing: list[Room]) -> bool:
    for other in existing:
        if not (
            room.x + room.w <= other.x
            or other.x + other.w <= room.x
            or room.y + room.h <= other.y
            or other.y + other.h <= room.y
        ):
            return True
    return False


def _checkpoint(cancel_event: threading.Event | None) -> Callable[[], None]:
    def check() -> None:
        if cancel_event is not None and cancel_event.is_set():
            raise LevelGenError(
                E_CANCELLED,
                "生成任务已在协同检查点取消",
                point="generation.checkpoint",
            )

    return check


def _choose_room_count(
    prepared: PreparedRequest,
    rng: DeterministicRNG,
) -> int:
    all_constraints = prepared.room_constraints
    all_range = _intersection(all_constraints)
    hard_range = _intersection(
        [constraint for constraint in all_constraints if constraint.hard]
    )
    if all_range is not None:
        low, high = all_range
    elif hard_range is not None:
        low, high = hard_range
    else:
        low, high = prepared.required_rooms, prepared.required_rooms + 3
    low = max(low, prepared.required_rooms)
    capacity = max(
        1,
        (prepared.grid_w // prepared.room_min_w)
        * (prepared.grid_h // prepared.room_min_h),
    )
    high = min(high, capacity)
    if low > high:
        raise LevelGenError.conflict(
            f"房间数量下界 {low} 超过网格可容纳数量 {high}",
            stage="layout",
            constraint_ids=[constraint.id for constraint in all_constraints],
        )
    return rng.randint(low, high)


def _layout_stage(
    prepared: PreparedRequest,
    state: _State,
    recorder: TraceRecorder,
    rng: DeterministicRNG,
    attempt: int,
) -> None:
    room_count = _choose_room_count(prepared, rng)
    for room_id in range(room_count):
        width = rng.randint(prepared.room_min_w, prepared.room_max_w)
        height = rng.randint(prepared.room_min_h, prepared.room_max_h)
        max_x = prepared.grid_w - width
        max_y = prepared.grid_h - height
        placed: Room | None = None
        existing = state.level.rooms
        for _ in range(96):
            candidate = Room(
                id=room_id,
                x=rng.randint(0, max_x),
                y=rng.randint(0, max_y),
                w=width,
                h=height,
            )
            if not _overlaps(candidate, existing):
                placed = candidate
                break
        if placed is None:
            candidates = [
                Room(id=room_id, x=position_x, y=position_y, w=width, h=height)
                for position_y in range(max_y + 1)
                for position_x in range(max_x + 1)
                if not _overlaps(
                    Room(
                        id=room_id,
                        x=position_x,
                        y=position_y,
                        w=width,
                        h=height,
                    ),
                    existing,
                )
            ]
            if candidates:
                placed = rng.choice(candidates)
        if placed is None:
            identifiers = [
                constraint.id
                for constraint in prepared.room_constraints
                if constraint.hard
            ]
            identifiers.extend(
                constraint.id
                for constraint in prepared.connectivity_constraints
                if constraint.hard
            )
            identifiers = identifiers or ["builtin:layout"]
            raise LevelGenError(
                code=E_CONSTRAINT_CONFLICT,
                message=f"布局阶段无法放置房间 {room_id}（{width}x{height}）",
                point="request.constraints",
                stage="layout",
                constraint_ids=identifiers,
                evidence={
                    "doors": [],
                    "rooms": [room.id for room in state.level.rooms],
                },
                report={
                    "attempts": attempt,
                    "constraint_ids": identifiers,
                    "evidence": {
                        "doors": [],
                        "rooms": [room.id for room in state.level.rooms],
                    },
                    "message": f"布局阶段无法放置房间 {room_id}（{width}x{height}）",
                    "stage": "layout",
                },
            )
        state.level.rooms.append(placed)
        recorder.operation("layout", attempt, "place_room", placed.to_dict())


def _role_plan(
    order: list[int],
    key_order: list[str],
    rewards: int,
) -> dict[str, Any]:
    key_count = len(key_order)
    lock_positions = {2 * index for index in range(1, key_count + 1)}
    key_positions = {2 * index - 1: tag for index, tag in enumerate(key_order, start=1)}
    reward_start = 2 * key_count + 1
    reward_positions = list(range(reward_start, len(order) - 1))
    return {
        "end_position": len(order) - 1,
        "key_positions": key_positions,
        "lock_positions": lock_positions,
        "reward_count": min(rewards, len(reward_positions)),
        "reward_positions": reward_positions,
    }


def _choose_door_count(
    prepared: PreparedRequest,
    base_count: int,
    max_count: int,
    rng: DeterministicRNG,
) -> int:
    hard_constraints = [
        constraint for constraint in prepared.door_constraints if constraint.hard
    ]
    hard_range = _intersection(hard_constraints)
    all_range = _intersection(prepared.door_constraints)
    low = base_count
    high = max_count
    if hard_range is not None:
        low = max(low, hard_range[0])
        high = min(high, hard_range[1])
    if low > high:
        raise LevelGenError.conflict(
            f"门数量硬约束无法满足：可行区间 [{low}, {high}]",
            stage="doors",
            constraint_ids=[constraint.id for constraint in hard_constraints],
        )
    if all_range is not None:
        selected_low = max(low, all_range[0])
        selected_high = min(high, all_range[1])
        if selected_low <= selected_high:
            return rng.randint(selected_low, selected_high)
    soft_constraints = [
        constraint for constraint in prepared.door_constraints if not constraint.hard
    ]
    if soft_constraints:
        preferred = sum(
            int(constraint.params.get("target", base_count))
            for constraint in soft_constraints
        ) // len(soft_constraints)
        return min(max(preferred, low), high)
    return low


def _door_stage(
    prepared: PreparedRequest,
    state: _State,
    recorder: TraceRecorder,
    rng: DeterministicRNG,
    attempt: int,
) -> dict[str, Any]:
    order = [room.id for room in state.level.rooms]
    rng.shuffle(order)
    plan = _role_plan(order, prepared.key_order, prepared.rewards)
    lock_positions = plan["lock_positions"]

    doors: list[Door] = []
    for position in range(len(order) - 1):
        door = Door(
            id=len(doors),
            a=order[position],
            b=order[position + 1],
            dir="BIDIR",
        )
        doors.append(door)

    existing_pairs = {
        frozenset((door.a, door.b)) for door in doors
    }
    candidates: list[tuple[int, int]] = []
    for left in range(len(order)):
        for right in range(left + 1, len(order)):
            if frozenset((order[left], order[right])) in existing_pairs:
                continue
            if any(left < lock_position < right for lock_position in lock_positions):
                continue
            candidates.append((left, right))
    rng.shuffle(candidates)
    max_count = len(doors) + len(candidates)
    target_count = _choose_door_count(
        prepared,
        len(doors),
        max_count,
        rng,
    )
    for left, right in candidates:
        if len(doors) >= target_count:
            break
        direction_roll = rng.randint(0, 4)
        direction = "BIDIR"
        if direction_roll == 0:
            direction = "AB"
        elif direction_roll == 1:
            direction = "BA"
        door = Door(
            id=len(doors),
            a=order[left],
            b=order[right],
            dir=direction,
        )
        doors.append(door)
    state.level.doors = doors
    for door in doors:
        recorder.operation("doors", attempt, "assign_door", door.to_dict())
    return {"order": order, "plan": plan}


def _label_stage(
    prepared: PreparedRequest,
    state: _State,
    recorder: TraceRecorder,
    rng: DeterministicRNG,
    attempt: int,
    door_plan: dict[str, Any],
) -> None:
    order = door_plan["order"]
    plan = door_plan["plan"]
    labels: list[Label] = [Label(kind="start", room=order[0])]
    for position, key_tag in sorted(plan["key_positions"].items()):
        labels.append(Label(kind="key", room=order[position], tag=key_tag))
    for index, key_tag in enumerate(prepared.key_order, start=1):
        lock_position = 2 * index
        labels.append(Label(kind="lock", room=order[lock_position], tag=key_tag))

    reward_positions = list(plan["reward_positions"])
    rng.shuffle(reward_positions)
    for position in reward_positions[: plan["reward_count"]]:
        labels.append(Label(kind="reward", room=order[position]))
    labels.append(Label(kind="end", room=order[plan["end_position"]]))
    labels.sort(
        key=lambda label: (
            {"start": 0, "key": 1, "lock": 2, "reward": 3, "end": 4}.get(
                label.kind,
                9,
            ),
            label.room,
            label.tag or "",
        )
    )
    state.level.labels = labels
    for label in labels:
        recorder.operation("labels", attempt, "place_label", label.to_dict())


def _record_checks(
    recorder: TraceRecorder,
    checks: list[CheckResult],
    attempt: int,
) -> None:
    for check in checks:
        stage = "soft" if check.soft else "keychain_validation"
        recorder.check(stage, check.to_dict())


def _validate_stage(
    prepared: PreparedRequest,
    state: _State,
    recorder: TraceRecorder,
    attempt: int,
) -> None:
    validate_level_structure(state.level)
    checks = evaluate_constraints(
        state.level,
        prepared.request.constraints,
        key_order=prepared.key_order,
    )
    _record_checks(recorder, checks, attempt)
    hard_failures = [
        check for check in checks if not check.ok and not check.soft
    ]
    if hard_failures:
        report_error = failed_report(
            stage="keychain_validation",
            attempts=attempt,
            checks=checks,
            fallback_message="生成结果未满足全部硬约束",
        )
        raise report_error


def _cancelled_result(
    error: LevelGenError,
    recorder: TraceRecorder | None,
    request: GenerationRequest | None,
    attempts: int,
) -> GenerationResult:
    trace = None
    if recorder is not None and request is not None:
        trace = recorder.as_dict(request)
    return GenerationResult(level=None, trace=trace, error=error, attempts=attempts)


def generate(
    request_data: Any,
    *,
    cancel_event: threading.Event | None = None,
) -> GenerationResult:
    """Generate a level from a serializable request and return its trace."""

    checkpoint = _checkpoint(cancel_event)
    try:
        checkpoint()
        request = GenerationRequest.from_dict(copy.deepcopy(request_data))
    except LevelGenError as error:
        return GenerationResult(level=None, trace=None, error=error, attempts=0)

    recorder = TraceRecorder(request)
    try:
        prepared = prepare_request(request)
    except LevelGenError as error:
        if error.report is None:
            error.report = {
                "attempts": 0,
                "constraint_ids": list(error.constraint_ids),
                "evidence": error.evidence or empty_evidence(),
                "message": error.message,
                "stage": error.stage or "static",
            }
        recorder.conflict(error.report)
        return GenerationResult(
            level=None,
            trace=recorder.as_dict(request),
            error=error,
            attempts=0,
        )

    last_error: LevelGenError | None = None
    attempts_run = 0
    for attempt in range(prepared.max_attempts):
        checkpoint()
        attempts_run = attempt + 1
        state = _State(level=Level(grid_w=prepared.grid_w, grid_h=prepared.grid_h))
        try:
            with _StageContext(state, recorder, "layout", attempt):
                _layout_stage(
                    prepared,
                    state,
                    recorder,
                    DeterministicRNG(
                        prepared.request.seed,
                        attempt,
                        "layout",
                        recorder.rng,
                        checkpoint,
                    ),
                    attempt,
                )
            with _StageContext(state, recorder, "doors", attempt):
                door_plan = _door_stage(
                    prepared,
                    state,
                    recorder,
                    DeterministicRNG(
                        prepared.request.seed,
                        attempt,
                        "doors",
                        recorder.rng,
                        checkpoint,
                    ),
                    attempt,
                )
            with _StageContext(state, recorder, "labels", attempt):
                _label_stage(
                    prepared,
                    state,
                    recorder,
                    DeterministicRNG(
                        prepared.request.seed,
                        attempt,
                        "labels",
                        recorder.rng,
                        checkpoint,
                    ),
                    attempt,
                    door_plan,
                )
            with _StageContext(
                state,
                recorder,
                "keychain_validation",
                attempt,
            ):
                _validate_stage(prepared, state, recorder, attempt)
        except LevelGenError as error:
            last_error = error
            if error.code == E_CANCELLED:
                return _cancelled_result(
                    error,
                    recorder,
                    request,
                    attempts_run,
                )
            continue
        recorder.final(state.level)
        return GenerationResult(
            level=state.level,
            trace=recorder.as_dict(request),
            attempts=attempts_run,
        )

    if last_error is None:
        last_error = LevelGenError(
            E_CONSTRAINT_CONFLICT,
            "所有确定性尝试均失败",
            stage="keychain_validation",
        )
    report = dict(last_error.report or {})
    report.setdefault("constraint_ids", list(last_error.constraint_ids))
    report.setdefault("evidence", last_error.evidence or empty_evidence())
    report.setdefault("message", last_error.message)
    report.setdefault("stage", last_error.stage or "keychain_validation")
    report["attempts"] = attempts_run
    final_error = LevelGenError(
        code=E_CONSTRAINT_CONFLICT,
        message=last_error.message,
        point="request.constraints",
        stage=report["stage"],
        constraint_ids=list(report["constraint_ids"]),
        evidence=report["evidence"],
        report=report,
    )
    recorder.conflict(report)
    return GenerationResult(
        level=None,
        trace=recorder.as_dict(request),
        error=final_error,
        attempts=attempts_run,
    )


def generate_level(request_data: Any) -> Level:
    result = generate(request_data)
    if not result.ok or result.level is None:
        if result.error is not None:
            raise result.error
        raise LevelGenError(
            E_CONSTRAINT_UNSATISFIED,
            "生成器未返回关卡",
            point="generation",
        )
    return result.level
