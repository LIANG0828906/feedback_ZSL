"""Tick-level deterministic formation replay."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Mapping

from .errors import ErrorCode, WorkbenchError
from .fixed import (
    ANGLE_SCALE,
    COORD_SCALE,
    angle_difference,
    interpolate_point,
    point_distance,
    point_segment_distance,
    rotate_towards,
    segment_heading,
)
from .models import MemberSpec, Point, Scene, SimulationConfig
from .smoothing import Polyline, SmoothedPath, _grid_from_world, _relative_slot_offset

MAX_STEP = 20 * COORD_SCALE
MAX_TURN = 180 * ANGLE_SCALE


@dataclass(frozen=True)
class TickRecord:
    tick: int
    member_id: str
    position: Point
    heading: int
    segment_index: int
    segment_progress: int
    total_progress: int
    slot_deviation: int
    parent_spacing: int
    slowdown_reason: str | None
    completed: bool

    def to_dict(self) -> dict[str, object]:
        return {
            "tick": self.tick,
            "member_id": self.member_id,
            "position": list(self.position),
            "heading": self.heading,
            "segment_index": self.segment_index,
            "segment_progress": self.segment_progress,
            "total_progress": self.total_progress,
            "slot_deviation": self.slot_deviation,
            "parent_spacing": self.parent_spacing,
            "slowdown_reason": self.slowdown_reason,
            "completed": self.completed,
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "TickRecord":
        return cls(
            tick=int(payload["tick"]),
            member_id=str(payload["member_id"]),
            position=(int(payload["position"][0]), int(payload["position"][1])),
            heading=int(payload["heading"]),
            segment_index=int(payload["segment_index"]),
            segment_progress=int(payload["segment_progress"]),
            total_progress=int(payload["total_progress"]),
            slot_deviation=int(payload["slot_deviation"]),
            parent_spacing=int(payload["parent_spacing"]),
            slowdown_reason=(
                None
                if payload.get("slowdown_reason") is None
                else str(payload["slowdown_reason"])
            ),
            completed=bool(payload["completed"]),
        )


@dataclass(frozen=True)
class MemberTrace:
    member_id: str
    ticks: tuple[TickRecord, ...]
    completed_tick: int
    path_length: int
    simulation_version: str = "tick-fixed-1"

    def to_dict(self) -> dict[str, object]:
        return {
            "member_id": self.member_id,
            "ticks": [tick.to_dict() for tick in self.ticks],
            "completed_tick": self.completed_tick,
            "path_length": self.path_length,
            "simulation_version": self.simulation_version,
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "MemberTrace":
        return cls(
            member_id=str(payload["member_id"]),
            ticks=tuple(TickRecord.from_dict(item) for item in payload["ticks"]),
            completed_tick=int(payload["completed_tick"]),
            path_length=int(payload["path_length"]),
            simulation_version=str(payload["simulation_version"]),
        )


@dataclass(frozen=True)
class SimulationInterval:
    member_id: str
    interval_index: int
    tick_start: int
    tick_end: int
    trace_digest: str

    def to_dict(self) -> dict[str, object]:
        return {
            "member_id": self.member_id,
            "interval_index": self.interval_index,
            "tick_start": self.tick_start,
            "tick_end": self.tick_end,
            "trace_digest": self.trace_digest,
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "SimulationInterval":
        return cls(
            member_id=str(payload["member_id"]),
            interval_index=int(payload["interval_index"]),
            tick_start=int(payload["tick_start"]),
            tick_end=int(payload["tick_end"]),
            trace_digest=str(payload["trace_digest"]),
        )


def _validate_member_parameters(member: MemberSpec) -> None:
    if not (1 <= member.step <= MAX_STEP):
        raise WorkbenchError(
            ErrorCode.E_PARA,
            f"step must be in [1, {MAX_STEP}]",
            field="members.step",
            member_id=member.member_id,
            details={"step": member.step},
        )
    if not (1 <= member.turn_rate <= MAX_TURN):
        raise WorkbenchError(
            ErrorCode.E_PARA,
            f"turn_rate must be in [1, {MAX_TURN}]",
            field="members.turn_rate",
            member_id=member.member_id,
            details={"turn_rate": member.turn_rate},
        )


def _formation_at_tick(
    scene: Scene,
    member_id: str,
    position: Point,
    parent_trace: "MemberTrace | None",
    tick: int,
) -> tuple[int, int]:
    if parent_trace is None or not parent_trace.ticks:
        return 0, 0
    parent_record = parent_trace.ticks[min(tick, len(parent_trace.ticks) - 1)]
    offset = _relative_slot_offset(scene, member_id)
    expected = (
        parent_record.position[0] + offset[0],
        parent_record.position[1] + offset[1],
    )
    return (
        point_distance(position, expected),
        point_distance(position, parent_record.position),
    )


def _advance(
    scene: Scene,
    member: MemberSpec,
    path: SmoothedPath,
    parent_trace: "MemberTrace | None",
    config: SimulationConfig,
) -> MemberTrace:
    _validate_member_parameters(member)
    polyline = path.polyline
    if not polyline.points:
        raise WorkbenchError(
            ErrorCode.E_ADV,
            "path must contain at least one point",
            item="path_points",
            member_id=member.member_id,
            tick=0,
        )

    points = polyline.points
    segment_index = 0
    segment_progress = 0
    total_progress = 0
    position = points[0]
    heading = segment_heading(points[0], points[1]) if len(points) > 1 else 0
    completed_tick: int | None = None
    ticks: list[TickRecord] = []

    def record(
        tick: int,
        *,
        slowdown_reason: str | None,
        completed: bool,
        deviation: int,
        spacing: int,
    ) -> None:
        ticks.append(
            TickRecord(
                tick=tick,
                member_id=member.member_id,
                position=position,
                heading=heading,
                segment_index=segment_index,
                segment_progress=segment_progress,
                total_progress=total_progress,
                slot_deviation=deviation,
                parent_spacing=spacing,
                slowdown_reason=slowdown_reason,
                completed=completed,
            )
        )

    initial_deviation, initial_spacing = _formation_at_tick(
        scene, member.member_id, position, parent_trace, 0
    )
    if initial_deviation > scene.formation.hard_slot_tolerance:
        raise WorkbenchError(
            ErrorCode.E_ADV,
            "formation collapsed at tick 0",
            item="slot_deviation",
            member_id=member.member_id,
            tick=0,
            deviation=initial_deviation,
            pair=(
                member.member_id,
                scene.formation.parents.get(member.member_id) or member.member_id,
            ),
        )
    record(
        0,
        slowdown_reason=None,
        completed=len(points) == 1,
        deviation=initial_deviation,
        spacing=initial_spacing,
    )

    if len(points) == 1:
        completed_tick = 0
        for tick in range(1, config.max_ticks + 1):
            deviation, spacing = _formation_at_tick(
                scene, member.member_id, position, parent_trace, tick
            )
            if deviation > scene.formation.hard_slot_tolerance:
                raise WorkbenchError(
                    ErrorCode.E_ADV,
                    "formation collapsed for a stationary member",
                    item="slot_deviation",
                    member_id=member.member_id,
                    tick=tick,
                    deviation=deviation,
                    pair=(
                        member.member_id,
                        scene.formation.parents.get(member.member_id)
                        or member.member_id,
                    ),
                    details={"partial_ticks": [item.to_dict() for item in ticks[-8:]]},
                )
            reason = None
            if deviation > scene.formation.slot_tolerance:
                reason = f"formation_stop:deviation={deviation}"
            record(
                tick,
                slowdown_reason=reason,
                completed=True,
                deviation=deviation,
                spacing=spacing,
            )
        return MemberTrace(
            member_id=member.member_id,
            ticks=tuple(ticks),
            completed_tick=completed_tick,
            path_length=polyline.total_length,
        )

    for tick in range(1, config.max_ticks + 1):
        if completed_tick is not None:
            record(
                tick,
                slowdown_reason=None,
                completed=True,
                deviation=ticks[-1].slot_deviation,
                spacing=ticks[-1].parent_spacing,
            )
            continue

        current_deviation, current_spacing = _formation_at_tick(
            scene, member.member_id, position, parent_trace, tick - 1
        )
        if current_deviation > scene.formation.hard_slot_tolerance:
            raise WorkbenchError(
                ErrorCode.E_ADV,
                "formation collapsed",
                item="slot_deviation",
                member_id=member.member_id,
                tick=tick,
                deviation=current_deviation,
                pair=(
                    member.member_id,
                    scene.formation.parents.get(member.member_id) or member.member_id,
                ),
                details={
                    "partial_ticks": [item.to_dict() for item in ticks[-8:]],
                },
            )
        spacing_hard_bound = scene.formation.max_spacing + scene.formation.hard_slot_tolerance
        if current_spacing > spacing_hard_bound:
            raise WorkbenchError(
                ErrorCode.E_ADV,
                "formation parent spacing collapsed",
                item="parent_spacing",
                member_id=member.member_id,
                tick=tick,
                deviation=current_spacing - scene.formation.max_spacing,
                pair=(
                    member.member_id,
                    scene.formation.parents.get(member.member_id) or member.member_id,
                ),
                details={
                    "spacing": current_spacing,
                    "partial_ticks": [item.to_dict() for item in ticks[-8:]],
                },
            )

        slowdown_reason: str | None = None
        effective_step = member.step
        if (
            current_deviation > scene.formation.slot_tolerance
            or current_spacing > scene.formation.max_spacing
        ):
            effective_step = max(1, member.step // config.slowdown_divisor)
            slowdown_reason = (
                f"formation_slowdown:deviation={current_deviation},"
                f"spacing={current_spacing}"
            )

        at_vertex = segment_progress >= polyline.lengths[segment_index]
        if at_vertex:
            if segment_index >= len(polyline.lengths) - 1:
                completed_tick = tick
                record(
                    tick,
                    slowdown_reason=slowdown_reason,
                    completed=True,
                    deviation=current_deviation,
                    spacing=current_spacing,
                )
                continue
            next_heading = segment_heading(
                points[segment_index + 1], points[segment_index + 2]
            )
            previous_heading = heading
            heading, applied = rotate_towards(
                heading, next_heading, member.turn_rate
            )
            if abs(applied) > member.turn_rate:
                raise WorkbenchError(
                    ErrorCode.E_ADV,
                    "heading change exceeded turn rate",
                    item="heading_delta",
                    member_id=member.member_id,
                    tick=tick,
                    deviation=abs(applied),
                    details={"previous": previous_heading, "new": heading},
                )
            remaining_turn = angle_difference(next_heading, heading)
            if remaining_turn != 0:
                if slowdown_reason is None:
                    slowdown_reason = "turn_rate_limit"
            else:
                next_segment = segment_index + 1
                if next_segment > segment_index + 1:
                    raise WorkbenchError(
                        ErrorCode.E_ADV,
                        "path segment jump detected",
                        item="segment_index",
                        member_id=member.member_id,
                        tick=tick,
                        deviation=next_segment - segment_index,
                    )
                segment_index = next_segment
                segment_progress = 0
        else:
            target_heading = segment_heading(
                points[segment_index], points[segment_index + 1]
            )
            previous_heading = heading
            heading, applied = rotate_towards(
                heading, target_heading, member.turn_rate
            )
            if abs(applied) > member.turn_rate:
                raise WorkbenchError(
                    ErrorCode.E_ADV,
                    "heading change exceeded turn rate",
                    item="heading_delta",
                    member_id=member.member_id,
                    tick=tick,
                    deviation=abs(applied),
                    details={"previous": previous_heading, "new": heading},
                )
            if angle_difference(target_heading, heading) == 0:
                segment_length = polyline.lengths[segment_index]
                move = min(effective_step, segment_length - segment_progress)
                new_progress = segment_progress + move
                new_position = interpolate_point(
                    points[segment_index],
                    points[segment_index + 1],
                    new_progress,
                    segment_length,
                )
                distance_to_line = point_segment_distance(
                    new_position,
                    points[segment_index],
                    points[segment_index + 1],
                )
                if distance_to_line > 2:
                    raise WorkbenchError(
                        ErrorCode.E_ADV,
                        "advanced position left the active path segment",
                        item="position",
                        member_id=member.member_id,
                        tick=tick,
                        deviation=distance_to_line,
                        details={
                            "position": list(new_position),
                            "segment_index": segment_index,
                        },
                    )
                position = new_position
                segment_progress = new_progress
                total_progress += move

        deviation, spacing = _formation_at_tick(
            scene, member.member_id, position, parent_trace, tick
        )
        if deviation > scene.formation.hard_slot_tolerance:
            raise WorkbenchError(
                ErrorCode.E_ADV,
                "formation collapsed after advance",
                item="slot_deviation",
                member_id=member.member_id,
                tick=tick,
                deviation=deviation,
                pair=(
                    member.member_id,
                    scene.formation.parents.get(member.member_id) or member.member_id,
                ),
                details={
                    "position": list(position),
                    "grid": list(_grid_from_world(position)),
                    "attempt_slowdown_reason": slowdown_reason,
                    "partial_ticks": [item.to_dict() for item in ticks[-8:]],
                },
            )
        if spacing_hard_bound < spacing:
            raise WorkbenchError(
                ErrorCode.E_ADV,
                "formation spacing collapsed after advance",
                item="parent_spacing",
                member_id=member.member_id,
                tick=tick,
                deviation=spacing - scene.formation.max_spacing,
                pair=(
                    member.member_id,
                    scene.formation.parents.get(member.member_id) or member.member_id,
                ),
                details={
                    "partial_ticks": [item.to_dict() for item in ticks[-8:]],
                },
            )

        at_goal = (
            segment_index == len(polyline.lengths) - 1
            and segment_progress >= polyline.lengths[-1]
        )
        if at_goal and completed_tick is None:
            completed_tick = tick
        record(
            tick,
            slowdown_reason=slowdown_reason,
            completed=at_goal,
            deviation=deviation,
            spacing=spacing,
        )

    if completed_tick is None:
        raise WorkbenchError(
            ErrorCode.E_ADV,
            "member did not reach its path endpoint within max_ticks",
            item="completed_tick",
            member_id=member.member_id,
            tick=config.max_ticks,
        )
    return MemberTrace(
        member_id=member.member_id,
        ticks=tuple(ticks),
        completed_tick=completed_tick,
        path_length=polyline.total_length,
    )


def simulate_member(
    scene: Scene,
    member_id: str,
    paths: Mapping[str, SmoothedPath],
    *,
    config: SimulationConfig | None = None,
    parent_trace: MemberTrace | None = None,
) -> MemberTrace:
    simulation_config = config or scene.simulation
    member = scene.member(member_id)
    path = paths.get(member_id)
    if path is None:
        raise WorkbenchError(
            ErrorCode.E_STATE,
            "smoothed path is unavailable",
            member_id=member_id,
        )
    parent_id = scene.formation.parents.get(member_id)
    if parent_id is not None and parent_trace is None:
        if parent_id not in paths:
            raise WorkbenchError(
                ErrorCode.E_STATE,
                f"parent path '{parent_id}' is unavailable for formation replay",
                member_id=member_id,
            )
        parent_trace = simulate_member(
            scene, parent_id, paths, config=simulation_config
        )
    trace = _advance(scene, member, path, parent_trace, simulation_config)
    validate_member_trace(scene, member_id, path, trace)
    return trace


def validate_member_trace(
    scene: Scene,
    member_id: str,
    path: SmoothedPath,
    trace: MemberTrace,
) -> None:
    """Reject discontinuous, off-polyline or instant-turn traces."""

    member = scene.member(member_id)
    polyline = path.polyline
    previous_segment = 0
    previous_heading = trace.ticks[0].heading if trace.ticks else 0
    for record in trace.ticks:
        if len(polyline.lengths) > 0:
            if not (0 <= record.segment_index < len(polyline.lengths)):
                raise WorkbenchError(
                    ErrorCode.E_ADV,
                    "trace segment index is outside the path",
                    item="segment_index",
                    member_id=member_id,
                    tick=record.tick,
                    deviation=record.segment_index,
                )
            if (
                record.segment_index < previous_segment
                or record.segment_index > previous_segment + 1
            ):
                raise WorkbenchError(
                    ErrorCode.E_ADV,
                    "trace skipped or reversed a path segment",
                    item="segment_index",
                    member_id=member_id,
                    tick=record.tick,
                    deviation=record.segment_index - previous_segment,
                )
            previous_segment = record.segment_index
            distance = point_segment_distance(
                record.position,
                polyline.points[record.segment_index],
                polyline.points[record.segment_index + 1],
            )
            if distance > 2:
                raise WorkbenchError(
                    ErrorCode.E_ADV,
                    "trace position left the active path segment",
                    item="position",
                    member_id=member_id,
                    tick=record.tick,
                    deviation=distance,
                    coord=record.position,
                )
        heading_delta = abs(angle_difference(record.heading, previous_heading))
        if heading_delta > member.turn_rate:
            raise WorkbenchError(
                ErrorCode.E_ADV,
                "trace performed an instant turn",
                item="heading_delta",
                member_id=member_id,
                tick=record.tick,
                deviation=heading_delta,
                coord=record.position,
            )
        previous_heading = record.heading


def simulate_all(
    scene: Scene,
    paths: Mapping[str, SmoothedPath],
    *,
    config: SimulationConfig | None = None,
) -> dict[str, MemberTrace]:
    simulation_config = config or scene.simulation
    result: dict[str, MemberTrace] = {}
    for member_id in scene.formation.topo_order():
        parent_id = scene.formation.parents.get(member_id)
        result[member_id] = simulate_member(
            scene,
            member_id,
            paths,
            config=simulation_config,
            parent_trace=result.get(parent_id) if parent_id is not None else None,
        )
    return result


def interval_digest(trace: MemberTrace, tick_start: int, tick_end: int) -> str:
    import hashlib
    import json

    payload = [
        trace.ticks[index].to_dict()
        for index in range(tick_start, min(tick_end + 1, len(trace.ticks)))
    ]
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def split_intervals(
    trace: MemberTrace, interval_ticks: int
) -> tuple[SimulationInterval, ...]:
    if interval_ticks <= 0:
        raise ValueError("interval_ticks must be positive")
    intervals: list[SimulationInterval] = []
    for start in range(0, len(trace.ticks), interval_ticks):
        end = min(start + interval_ticks - 1, len(trace.ticks) - 1)
        intervals.append(
            SimulationInterval(
                member_id=trace.member_id,
                interval_index=start // interval_ticks,
                tick_start=start,
                tick_end=end,
                trace_digest=interval_digest(trace, start, end),
            )
        )
    return tuple(intervals)
