"""Strict and deterministic scene validation."""

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any, Iterable

from .errors import ErrorCode, ValidationIssue, WorkbenchError, first_issue_error
from .fixed import (
    ANGLE_SCALE,
    COORD_SCALE,
    MAX_CELL_COST,
    MAX_WORLD_EXTENT,
    WEIGHT_SCALE,
)
from .models import (
    SCHEMA_VERSION,
    Blockade,
    FormationSpec,
    GridMap,
    MemberSpec,
    Scene,
    SearchConfig,
    SimulationConfig,
    TerrainType,
)


class _Collector:
    def __init__(self) -> None:
        self.issues: list[ValidationIssue] = []

    def add(
        self,
        code: ErrorCode,
        field: str,
        message: str,
        *,
        coord: tuple[int, int] | None = None,
        member_id: str | None = None,
    ) -> None:
        self.issues.append(
            ValidationIssue(
                code=code,
                field=field,
                message=message,
                coord=coord,
                member_id=member_id,
            )
        )

    def require(self, payload: dict[str, Any], key: str, field: str) -> Any:
        if key not in payload:
            self.add(ErrorCode.E_SCHEMA, field, f"missing required field '{field}'")
            return None
        return payload[key]


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _is_finite_number(value: Any) -> bool:
    return (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(float(value))
    )


def _parse_terrain_types(
    raw: Any, collector: _Collector
) -> dict[str, TerrainType]:
    if not isinstance(raw, dict) or not raw:
        collector.add(
            ErrorCode.E_SCHEMA,
            "map.terrain_types",
            "map.terrain_types must be a non-empty object",
        )
        return {}
    result: dict[str, TerrainType] = {}
    for name, entry in raw.items():
        field = f"map.terrain_types.{name}"
        if not isinstance(entry, dict):
            collector.add(ErrorCode.E_SCHEMA, field, "terrain type must be an object")
            continue
        passable = entry.get("passable")
        if not isinstance(passable, bool):
            collector.add(ErrorCode.E_SCHEMA, f"{field}.passable", "passable must be boolean")
            passable = True
        color = entry.get("color", "#7b8794")
        if not isinstance(color, str):
            collector.add(ErrorCode.E_SCHEMA, f"{field}.color", "color must be a string")
            color = "#7b8794"
        label = entry.get("label", name)
        if not isinstance(label, str):
            collector.add(ErrorCode.E_SCHEMA, f"{field}.label", "label must be a string")
            label = name
        result[name] = TerrainType(
            name=name,
            label=label,
            passable=passable,
            color=color,
        )
    return result


def _parse_matrix(
    raw: Any,
    width: int,
    height: int,
    field: str,
    collector: _Collector,
) -> tuple[tuple[Any, ...], ...]:
    if not isinstance(raw, list):
        collector.add(ErrorCode.E_SCHEMA, field, f"{field} must be a matrix")
        return tuple()
    if len(raw) != height:
        collector.add(
            ErrorCode.E_RANGE,
            field,
            f"{field} has {len(raw)} rows, expected {height}",
        )
    rows: list[tuple[Any, ...]] = []
    for y, row in enumerate(raw):
        if not isinstance(row, list):
            collector.add(
                ErrorCode.E_SCHEMA,
                f"{field}[{y}]",
                "matrix row must be an array",
                coord=(0, y),
            )
            continue
        if len(row) != width:
            collector.add(
                ErrorCode.E_RANGE,
                f"{field}[{y}]",
                f"row has {len(row)} columns, expected {width}",
                coord=(0, y),
            )
        values = []
        for x, value in enumerate(row):
            if x >= width:
                break
            values.append(value)
            if len(values) == width:
                break
        while len(values) < width:
            values.append(None)
        rows.append(tuple(values))
    while len(rows) < height:
        rows.append(tuple(None for _ in range(width)))
    return tuple(rows)


def _parse_map(raw: Any, collector: _Collector) -> GridMap | None:
    if not isinstance(raw, dict):
        collector.add(ErrorCode.E_SCHEMA, "map", "map must be an object")
        return None

    width = collector.require(raw, "width", "map.width")
    height = collector.require(raw, "height", "map.height")
    if not _is_int(width) or not _is_int(height):
        collector.add(ErrorCode.E_SCHEMA, "map.width", "map dimensions must be integers")
        return None
    if not (2 <= width <= 512 and 2 <= height <= 512):
        collector.add(
            ErrorCode.E_RANGE,
            "map.width",
            "map dimensions must be in [2, 512]",
        )
        return None

    terrain_types = _parse_terrain_types(
        collector.require(raw, "terrain_types", "map.terrain_types"), collector
    )
    raw_costs = collector.require(raw, "costs", "map.costs")
    raw_terrain = collector.require(raw, "terrain", "map.terrain")
    cost_matrix = _parse_matrix(raw_costs, width, height, "map.costs", collector)
    terrain_matrix = _parse_matrix(raw_terrain, width, height, "map.terrain", collector)

    costs: list[list[int]] = [[-1 for _ in range(width)] for _ in range(height)]
    terrain: list[list[str]] = [["void" for _ in range(width)] for _ in range(height)]
    for y in range(height):
        for x in range(width):
            cost = cost_matrix[y][x] if y < len(cost_matrix) else None
            terrain_name = terrain_matrix[y][x] if y < len(terrain_matrix) else None
            coord = (x, y)
            if not _is_int(cost):
                collector.add(
                    ErrorCode.E_RANGE if cost is not None else ErrorCode.E_SCHEMA,
                    f"map.costs[{y}][{x}]",
                    "cell cost must be an integer",
                    coord=coord,
                )
            elif cost < -1 or cost > MAX_CELL_COST:
                collector.add(
                    ErrorCode.E_RANGE,
                    f"map.costs[{y}][{x}]",
                    f"cell cost must be -1 or in [0, {MAX_CELL_COST}]",
                    coord=coord,
                )
            else:
                costs[y][x] = cost

            if not isinstance(terrain_name, str) or not terrain_name:
                collector.add(
                    ErrorCode.E_SCHEMA,
                    f"map.terrain[{y}][{x}]",
                    "cell terrain classification must be a non-empty string",
                    coord=coord,
                )
                continue
            terrain[y][x] = terrain_name
            terrain_entry = terrain_types.get(terrain_name)
            if terrain_entry is None:
                collector.add(
                    ErrorCode.E_REF,
                    f"map.terrain[{y}][{x}]",
                    f"unknown terrain classification '{terrain_name}'",
                    coord=coord,
                )
                continue
            if _is_int(cost):
                if cost == -1 and terrain_entry.passable:
                    collector.add(
                        ErrorCode.E_SCHEMA,
                        f"map.terrain[{y}][{x}]",
                        "impassable cell must reference a non-passable terrain class",
                        coord=coord,
                    )
                if cost >= 0 and not terrain_entry.passable:
                    collector.add(
                        ErrorCode.E_SCHEMA,
                        f"map.costs[{y}][{x}]",
                        "passable cell must reference a passable terrain class",
                        coord=coord,
                    )

    raw_blockades = collector.require(raw, "blockades", "map.blockades")
    blockades: list[Blockade] = []
    if not isinstance(raw_blockades, list):
        collector.add(ErrorCode.E_SCHEMA, "map.blockades", "blockades must be an array")
        raw_blockades = []
    seen_blockade_ids: set[str] = set()
    for index, item in enumerate(raw_blockades):
        field = f"map.blockades[{index}]"
        if not isinstance(item, dict):
            collector.add(ErrorCode.E_SCHEMA, field, "blockade must be an object")
            continue
        blockade_id = item.get("blockade_id")
        if not isinstance(blockade_id, str) or not blockade_id:
            collector.add(
                ErrorCode.E_SCHEMA, f"{field}.blockade_id", "blockade_id must be a string"
            )
            continue
        if blockade_id in seen_blockade_ids:
            collector.add(
                ErrorCode.E_SCHEMA,
                f"{field}.blockade_id",
                f"duplicate blockade id '{blockade_id}'",
            )
        seen_blockade_ids.add(blockade_id)
        terrain_name = item.get("terrain")
        if "terrain" not in item:
            collector.add(
                ErrorCode.E_SCHEMA,
                f"{field}.terrain",
                "missing required field 'terrain'",
            )
            terrain_name = next(iter(terrain_types), "void")
        elif not isinstance(terrain_name, str) or terrain_name not in terrain_types:
            collector.add(
                ErrorCode.E_REF,
                f"{field}.terrain",
                f"blockade references missing terrain '{terrain_name}'",
            )
            terrain_name = next(iter(terrain_types), "void")
        cells_value = item.get("cells")
        cells: list[tuple[int, int]] = []
        if not isinstance(cells_value, list):
            collector.add(ErrorCode.E_SCHEMA, f"{field}.cells", "cells must be an array")
        else:
            for cell_index, cell in enumerate(cells_value):
                if (
                    not isinstance(cell, list)
                    or len(cell) != 2
                    or not all(_is_int(value) for value in cell)
                ):
                    collector.add(
                        ErrorCode.E_SCHEMA,
                        f"{field}.cells[{cell_index}]",
                        "cell must be [x, y]",
                    )
                    continue
                x, y = cell
                if not (0 <= x < width and 0 <= y < height):
                    collector.add(
                        ErrorCode.E_RANGE,
                        f"{field}.cells[{cell_index}]",
                        "blockade cell is outside map bounds",
                        coord=(x, y),
                    )
                    continue
                cells.append((x, y))
        active = item.get("active", True)
        if not isinstance(active, bool):
            collector.add(
                ErrorCode.E_SCHEMA, f"{field}.active", "active must be boolean"
            )
            active = True
        label = item.get("label", blockade_id)
        if not isinstance(label, str):
            collector.add(ErrorCode.E_SCHEMA, f"{field}.label", "label must be a string")
            label = blockade_id
        blockades.append(
            Blockade(
                blockade_id=blockade_id,
                terrain=terrain_name,
                cells=tuple(cells),
                active=active,
                label=label,
            )
        )

    return GridMap(
        width=width,
        height=height,
        costs=tuple(tuple(row) for row in costs),
        terrain=tuple(tuple(row) for row in terrain),
        terrain_types=terrain_types,
        blockades=tuple(sorted(blockades, key=lambda entry: entry.blockade_id)),
    )


def _parse_members(
    raw: Any,
    grid_map: GridMap,
    slots: dict[str, tuple[int, int]],
    collector: _Collector,
) -> tuple[MemberSpec, ...]:
    if not isinstance(raw, list) or not raw:
        collector.add(ErrorCode.E_SCHEMA, "members", "members must be a non-empty array")
        return tuple()
    members: list[MemberSpec] = []
    seen_ids: set[str] = set()
    for index, item in enumerate(raw):
        field = f"members[{index}]"
        if not isinstance(item, dict):
            collector.add(ErrorCode.E_SCHEMA, field, "member must be an object")
            continue
        member_id = item.get("member_id")
        if not isinstance(member_id, str) or not member_id:
            collector.add(
                ErrorCode.E_SCHEMA, f"{field}.member_id", "member_id must be a string"
            )
            continue
        if member_id in seen_ids:
            collector.add(
                ErrorCode.E_SCHEMA,
                f"{field}.member_id",
                f"duplicate member id '{member_id}'",
                member_id=member_id,
            )
        seen_ids.add(member_id)

        coords: dict[str, tuple[int, int]] = {}
        for key in ("start", "goal"):
            raw_coord = item.get(key)
            if (
                not isinstance(raw_coord, dict)
                or not _is_int(raw_coord.get("x"))
                or not _is_int(raw_coord.get("y"))
            ):
                collector.add(
                    ErrorCode.E_SCHEMA,
                    f"{field}.{key}",
                    f"{key} must contain integer x and y",
                    member_id=member_id,
                )
                continue
            coord = (raw_coord["x"], raw_coord["y"])
            coords[key] = coord
            if not grid_map.in_bounds(coord):
                collector.add(
                    ErrorCode.E_RANGE,
                    f"{field}.{key}",
                    f"{key} is outside map bounds",
                    coord=coord,
                    member_id=member_id,
                )
            elif not grid_map.is_passable(coord):
                collector.add(
                    ErrorCode.E_RANGE,
                    f"{field}.{key}",
                    f"{key} is not passable",
                    coord=coord,
                    member_id=member_id,
                )

        slot_id = item.get("slot_id")
        if not isinstance(slot_id, str):
            collector.add(
                ErrorCode.E_SCHEMA, f"{field}.slot_id", "slot_id must be a string"
            )
        elif slot_id not in slots:
            collector.add(
                ErrorCode.E_REF,
                f"{field}.slot_id",
                f"slot reference '{slot_id}' does not exist",
                member_id=member_id,
            )

        step = item.get("step")
        if not _is_int(step):
            collector.add(
                ErrorCode.E_SCHEMA,
                f"{field}.step",
                "step must be a fixed-point integer",
                member_id=member_id,
            )
            step = 1
        elif not (1 <= step <= 20 * COORD_SCALE):
            collector.add(
                ErrorCode.E_RANGE,
                f"{field}.step",
                f"step must be in [1, {20 * COORD_SCALE}]",
                member_id=member_id,
            )
            step = 1

        turn_rate = item.get("turn_rate")
        if not _is_int(turn_rate):
            collector.add(
                ErrorCode.E_SCHEMA,
                f"{field}.turn_rate",
                "turn_rate must be a fixed-point integer",
                member_id=member_id,
            )
            turn_rate = 1
        elif not (1 <= turn_rate <= 180 * ANGLE_SCALE):
            collector.add(
                ErrorCode.E_RANGE,
                f"{field}.turn_rate",
                f"turn_rate must be in [1, {180 * ANGLE_SCALE}] millidegrees/tick",
                member_id=member_id,
            )
            turn_rate = 1

        if "start" in coords and "goal" in coords:
            members.append(
                MemberSpec(
                    member_id=member_id,
                    start=coords["start"],
                    goal=coords["goal"],
                    slot_id=slot_id if isinstance(slot_id, str) else "",
                    step=step,
                    turn_rate=turn_rate,
                )
            )
    return tuple(sorted(members, key=lambda member: member.member_id))


def _parse_formation(
    raw: Any,
    members: tuple[MemberSpec, ...],
    collector: _Collector,
) -> FormationSpec | None:
    if not isinstance(raw, dict):
        collector.add(ErrorCode.E_SCHEMA, "formation", "formation must be an object")
        return None

    version = raw.get("version")
    if not isinstance(version, str) or not version:
        collector.add(
            ErrorCode.E_SCHEMA, "formation.version", "formation version must be a string"
        )
        version = "invalid"

    slots_raw = collector.require(raw, "slots", "formation.slots")
    slots: dict[str, tuple[int, int]] = {}
    if not isinstance(slots_raw, dict):
        collector.add(ErrorCode.E_SCHEMA, "formation.slots", "slots must be an object")
    else:
        for slot_id, value in slots_raw.items():
            field = f"formation.slots.{slot_id}"
            if not isinstance(value, dict):
                collector.add(ErrorCode.E_SCHEMA, field, "slot offset must be an object")
                continue
            if "x" not in value or "y" not in value:
                collector.add(
                    ErrorCode.E_SCHEMA,
                    field,
                    "slot offset requires x and y",
                )
                continue
            offset_x = value.get("x")
            offset_y = value.get("y")
            if not _is_finite_number(offset_x) or not _is_finite_number(offset_y):
                collector.add(
                    ErrorCode.E_RANGE,
                    f"{field}.x",
                    "slot offsets must be finite numbers",
                )
                continue
            if abs(float(offset_x)) > MAX_WORLD_EXTENT or abs(float(offset_y)) > MAX_WORLD_EXTENT:
                collector.add(
                    ErrorCode.E_RANGE,
                    f"{field}.x",
                    "slot offset magnitude is outside the supported world range",
                )
                continue
            if float(offset_x) != int(offset_x) or float(offset_y) != int(offset_y):
                collector.add(
                    ErrorCode.E_RANGE,
                    field,
                    "slot offsets must be whole fixed-point units",
                )
                continue
            slots[str(slot_id)] = (int(offset_x), int(offset_y))

    leader = raw.get("leader")
    if not isinstance(leader, str) or not leader:
        collector.add(ErrorCode.E_SCHEMA, "formation.leader", "leader must be a member id")
        leader = members[0].member_id if members else ""
    member_ids = {member.member_id for member in members}
    if leader not in member_ids:
        collector.add(
            ErrorCode.E_REF,
            "formation.leader",
            f"leader reference '{leader}' does not exist",
        )

    parents_raw = collector.require(raw, "parents", "formation.parents")
    parents: dict[str, str | None] = {}
    if not isinstance(parents_raw, dict):
        collector.add(
            ErrorCode.E_SCHEMA, "formation.parents", "parents must be an object"
        )
    else:
        for member_id in sorted(member_ids):
            value = parents_raw.get(member_id)
            if value is not None and not isinstance(value, str):
                collector.add(
                    ErrorCode.E_SCHEMA,
                    f"formation.parents.{member_id}",
                    "parent must be a member id or null",
                    member_id=member_id,
                )
                value = None
            if isinstance(value, str) and value not in member_ids:
                collector.add(
                    ErrorCode.E_REF,
                    f"formation.parents.{member_id}",
                    f"parent reference '{value}' does not exist",
                    member_id=member_id,
                )
                value = None
            parents[member_id] = value

    slot_tolerance = raw.get("slot_tolerance")
    if not _is_int(slot_tolerance):
        collector.add(
            ErrorCode.E_SCHEMA,
            "formation.slot_tolerance",
            "slot_tolerance must be a fixed-point integer",
        )
        slot_tolerance = 0
    elif not (0 <= slot_tolerance <= 100 * COORD_SCALE):
        collector.add(
            ErrorCode.E_RANGE,
            "formation.slot_tolerance",
            "slot_tolerance is outside [0, 100000]",
        )
        slot_tolerance = 0

    max_spacing = raw.get("max_spacing")
    if not _is_int(max_spacing):
        collector.add(
            ErrorCode.E_SCHEMA,
            "formation.max_spacing",
            "max_spacing must be a fixed-point integer",
        )
        max_spacing = 1
    elif not (1 <= max_spacing <= 1000 * COORD_SCALE):
        collector.add(
            ErrorCode.E_RANGE,
            "formation.max_spacing",
            "max_spacing is outside supported range",
        )
        max_spacing = 1

    hard_tolerance = raw.get("hard_slot_tolerance", max_spacing)
    if not _is_int(hard_tolerance):
        collector.add(
            ErrorCode.E_SCHEMA,
            "formation.hard_slot_tolerance",
            "hard_slot_tolerance must be a fixed-point integer",
        )
        hard_tolerance = max_spacing
    elif not (slot_tolerance <= hard_tolerance <= 1000 * COORD_SCALE):
        collector.add(
            ErrorCode.E_RANGE,
            "formation.hard_slot_tolerance",
            "hard_slot_tolerance must be >= slot_tolerance and within supported range",
        )
        hard_tolerance = max_spacing

    if leader in parents:
        if parents[leader] is not None:
            collector.add(
                ErrorCode.E_FORM if hasattr(ErrorCode, "E_FORM") else ErrorCode.E_SCHEMA,
                f"formation.parents.{leader}",
                "leader must not have a parent",
                member_id=leader,
            )
        parents[leader] = None

    if member_ids and leader in member_ids:
        disconnected: list[str] = []
        for member_id in sorted(member_ids):
            visited: set[str] = set()
            current: str | None = member_id
            while current is not None:
                if current in visited:
                    collector.add(
                        ErrorCode.E_SCHEMA,
                        "formation.parents",
                        f"formation parent graph contains a cycle at '{current}'",
                        member_id=member_id,
                    )
                    current = None
                    break
                visited.add(current)
                current = parents.get(current)
            if leader not in visited:
                disconnected.append(member_id)
        if disconnected:
            collector.add(
                ErrorCode.E_SCHEMA,
                "formation.parents",
                f"members not connected to leader: {', '.join(disconnected)}",
            )

    if collector.issues:
        return None
    return FormationSpec(
        version=version,
        leader=leader,
        slots=slots,
        parents=parents,
        slot_tolerance=slot_tolerance,
        max_spacing=max_spacing,
        hard_slot_tolerance=hard_tolerance,
    )


def _parse_search(raw: Any, collector: _Collector) -> SearchConfig:
    if raw is None:
        return SearchConfig()
    if not isinstance(raw, dict):
        collector.add(ErrorCode.E_SCHEMA, "search", "search must be an object")
        return SearchConfig()
    weight = raw.get("heuristic_weight", 1.0)
    if not _is_finite_number(weight) or float(weight) <= 0:
        collector.add(
            ErrorCode.E_PARA,
            "search.heuristic_weight",
            "heuristic_weight must be a positive finite number",
        )
        weight = 1.0
    weight_fixed = int(round(float(weight) * WEIGHT_SCALE))
    neighborhood = raw.get("neighborhood", 8)
    if not _is_int(neighborhood):
        collector.add(
            ErrorCode.E_SCHEMA, "search.neighborhood", "neighborhood must be an integer"
        )
        neighborhood = 8
    elif neighborhood not in (4, 8):
        collector.add(
            ErrorCode.E_PARA, "search.neighborhood", "neighborhood must be 4 or 8"
        )
        neighborhood = 8
    max_expansions = raw.get("max_expansions", 1_000_000)
    if not _is_int(max_expansions):
        collector.add(
            ErrorCode.E_SCHEMA,
            "search.max_expansions",
            "max_expansions must be an integer",
        )
        max_expansions = 1_000_000
    elif not (1 <= max_expansions <= 100_000_000):
        collector.add(
            ErrorCode.E_PARA,
            "search.max_expansions",
            "max_expansions is outside supported bounds",
        )
        max_expansions = 1_000_000
    allow_corner_cutting = raw.get("allow_corner_cutting", False)
    if not isinstance(allow_corner_cutting, bool):
        collector.add(
            ErrorCode.E_SCHEMA,
            "search.allow_corner_cutting",
            "allow_corner_cutting must be boolean",
        )
        allow_corner_cutting = False
    return SearchConfig(
        heuristic_weight=weight_fixed,
        neighborhood=neighborhood,
        max_expansions=max_expansions,
        allow_corner_cutting=allow_corner_cutting,
    )


def _parse_simulation(raw: Any, collector: _Collector) -> SimulationConfig:
    if raw is None:
        return SimulationConfig()
    if not isinstance(raw, dict):
        collector.add(
            ErrorCode.E_SCHEMA, "simulation", "simulation must be an object"
        )
        return SimulationConfig()
    max_ticks = raw.get("max_ticks", 600)
    if not _is_int(max_ticks):
        collector.add(
            ErrorCode.E_SCHEMA, "simulation.max_ticks", "max_ticks must be an integer"
        )
        max_ticks = 600
    elif not (1 <= max_ticks <= 100_000):
        collector.add(
            ErrorCode.E_PARA,
            "simulation.max_ticks",
            "max_ticks must be in [1, 100000]",
        )
        max_ticks = 600
    interval_ticks = raw.get("interval_ticks", 20)
    if not _is_int(interval_ticks):
        collector.add(
            ErrorCode.E_SCHEMA,
            "simulation.interval_ticks",
            "interval_ticks must be an integer",
        )
        interval_ticks = 20
    elif not (1 <= interval_ticks <= 10_000):
        collector.add(
            ErrorCode.E_PARA,
            "simulation.interval_ticks",
            "interval_ticks must be in [1, 10000]",
        )
        interval_ticks = 20
    divisor = raw.get("slowdown_divisor", 2)
    if not _is_int(divisor):
        collector.add(
            ErrorCode.E_SCHEMA,
            "simulation.slowdown_divisor",
            "slowdown_divisor must be an integer",
        )
        divisor = 2
    elif not (2 <= divisor <= 16):
        collector.add(
            ErrorCode.E_PARA,
            "simulation.slowdown_divisor",
            "slowdown_divisor must be in [2, 16]",
        )
        divisor = 2
    workers = raw.get("max_parallel_workers", 4)
    if not _is_int(workers) or not (1 <= workers <= 32):
        collector.add(
            ErrorCode.E_PARA,
            "simulation.max_parallel_workers",
            "max_parallel_workers must be in [1, 32]",
        )
        workers = 4
    buffer_size = raw.get("per_worker_buffer", 4)
    if not _is_int(buffer_size) or not (1 <= buffer_size <= 4096):
        collector.add(
            ErrorCode.E_PARA,
            "simulation.per_worker_buffer",
            "per_worker_buffer must be in [1, 4096]",
        )
        buffer_size = 4
    return SimulationConfig(
        max_ticks=max_ticks,
        interval_ticks=interval_ticks,
        slowdown_divisor=divisor,
        max_parallel_workers=workers,
        per_worker_buffer=buffer_size,
    )


def validate_scene(payload: Any) -> Scene:
    """Validate a scene payload, raising a structured error on any issue."""

    collector = _Collector()
    if not isinstance(payload, dict):
        raise first_issue_error(
            [
                ValidationIssue(
                    ErrorCode.E_SCHEMA, "scene", "scene payload must be an object"
                )
            ]
        )

    schema_version = payload.get("schema_version", SCHEMA_VERSION)
    if not _is_int(schema_version) or schema_version != SCHEMA_VERSION:
        collector.add(
            ErrorCode.E_SCHEMA,
            "schema_version",
            f"unsupported schema_version; expected {SCHEMA_VERSION}",
        )

    name = payload.get("name", "untitled")
    if not isinstance(name, str):
        collector.add(ErrorCode.E_SCHEMA, "name", "name must be a string")
        name = "untitled"
    seed = payload.get("seed", 0)
    if not _is_int(seed):
        collector.add(ErrorCode.E_SCHEMA, "seed", "seed must be an integer")
        seed = 0

    grid_map = _parse_map(collector.require(payload, "map", "map"), collector)
    raw_members = collector.require(payload, "members", "members")
    raw_formation = collector.require(payload, "formation", "formation")

    slots: dict[str, tuple[int, int]] = {}
    if isinstance(raw_formation, dict) and isinstance(raw_formation.get("slots"), dict):
        for key, value in raw_formation["slots"].items():
            if isinstance(value, dict) and "x" in value and "y" in value:
                if _is_finite_number(value.get("x")) and _is_finite_number(value.get("y")):
                    slots[str(key)] = (int(value["x"]), int(value["y"]))
                else:
                    # Preserve the key so member slot checks do not mask the
                    # more specific finite-number error emitted by formation parsing.
                    slots[str(key)] = (0, 0)

    members = (
        _parse_members(raw_members, grid_map, slots, collector)
        if grid_map is not None
        else tuple()
    )
    formation = _parse_formation(raw_formation, members, collector)
    search = _parse_search(payload.get("search"), collector)
    simulation = _parse_simulation(payload.get("simulation"), collector)

    if collector.issues or grid_map is None or formation is None:
        raise first_issue_error(collector.issues)

    used_slots = {member.slot_id for member in members}
    for slot_id in sorted(used_slots):
        if slot_id not in formation.slots:
            collector.add(
                ErrorCode.E_REF,
                "members.slot_id",
                f"slot reference '{slot_id}' does not exist",
            )
    if collector.issues:
        raise first_issue_error(collector.issues)

    return Scene(
        schema_version=SCHEMA_VERSION,
        name=name,
        seed=seed,
        map=grid_map,
        formation=formation,
        members=members,
        search=search,
        simulation=simulation,
    )


def load_scene(path: str | Path) -> Scene:
    source = Path(path)
    try:
        with source.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except FileNotFoundError as exc:
        raise WorkbenchError(
            ErrorCode.E_IO, f"scene file not found: {source}", field="path"
        ) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise WorkbenchError(
            ErrorCode.E_IO,
            f"cannot read scene '{source}': {exc}",
            field="path",
        ) from exc
    return validate_scene(payload)
