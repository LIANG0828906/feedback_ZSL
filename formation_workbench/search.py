"""Deterministic member-layered fixed-point A* search."""

from __future__ import annotations

import heapq
from dataclasses import dataclass, field
from typing import Callable, Iterable

from .errors import ErrorCode, WorkbenchError
from .fixed import (
    COORD_SCALE,
    DIAGONAL_SCALE,
    MAX_PLANNER_COST,
    WEIGHT_SCALE,
    checked_add,
)
from .models import Coord, GridMap, SearchConfig, Scene

Heuristic = Callable[[Coord, Coord, GridMap, int], int]


@dataclass(frozen=True)
class PathDependencies:
    cells: tuple[Coord, ...]
    terrain_classes: tuple[str, ...]
    blockade_ids: tuple[str, ...]
    map_hash: str
    search_cells: tuple[Coord, ...] = ()

    def to_dict(self) -> dict[str, object]:
        return {
            "cells": [list(cell) for cell in self.cells],
            "terrain_classes": list(self.terrain_classes),
            "blockade_ids": list(self.blockade_ids),
            "map_hash": self.map_hash,
            "search_cells": [list(cell) for cell in self.search_cells],
        }

    @classmethod
    def from_dict(cls, payload: dict[str, object]) -> "PathDependencies":
        return cls(
            cells=tuple((int(cell[0]), int(cell[1])) for cell in payload["cells"]),
            terrain_classes=tuple(str(item) for item in payload["terrain_classes"]),
            blockade_ids=tuple(str(item) for item in payload["blockade_ids"]),
            map_hash=str(payload["map_hash"]),
            search_cells=tuple(
                (int(cell[0]), int(cell[1]))
                for cell in payload.get("search_cells", ())
            ),
        )


@dataclass(frozen=True)
class RawPath:
    member_id: str
    cells: tuple[Coord, ...]
    cost: int
    dependencies: PathDependencies
    expansions: int
    heuristic_checks: int = 0
    algorithm_version: str = "astar-fixed-1"

    def to_dict(self) -> dict[str, object]:
        return {
            "member_id": self.member_id,
            "cells": [list(cell) for cell in self.cells],
            "cost": self.cost,
            "dependencies": self.dependencies.to_dict(),
            "expansions": self.expansions,
            "heuristic_checks": self.heuristic_checks,
            "algorithm_version": self.algorithm_version,
        }

    @classmethod
    def from_dict(cls, payload: dict[str, object]) -> "RawPath":
        return cls(
            member_id=str(payload["member_id"]),
            cells=tuple((int(cell[0]), int(cell[1])) for cell in payload["cells"]),
            cost=int(payload["cost"]),
            dependencies=PathDependencies.from_dict(payload["dependencies"]),
            expansions=int(payload["expansions"]),
            heuristic_checks=int(payload.get("heuristic_checks", 0)),
            algorithm_version=str(payload["algorithm_version"]),
        )


def _move_cost(cell_cost: int, diagonal: bool) -> int:
    if cell_cost < 0:
        raise ValueError("cannot enter an impassable cell")
    if diagonal:
        return (cell_cost * DIAGONAL_SCALE + COORD_SCALE - 1) // COORD_SCALE
    return cell_cost * COORD_SCALE


def neighbors(
    grid_map: GridMap,
    coord: Coord,
    neighborhood: int,
    allow_corner_cutting: bool,
) -> tuple[tuple[Coord, int], ...]:
    x, y = coord
    if neighborhood == 4:
        offsets = ((1, 0), (0, 1), (-1, 0), (0, -1))
    elif neighborhood == 8:
        offsets = (
            (1, 0),
            (0, 1),
            (-1, 0),
            (0, -1),
            (1, 1),
            (1, -1),
            (-1, 1),
            (-1, -1),
        )
    else:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "neighborhood must be 4 or 8",
            field="search.neighborhood",
        )
    result: list[tuple[Coord, int]] = []
    for dx, dy in offsets:
        next_coord = (x + dx, y + dy)
        if not grid_map.is_passable(next_coord):
            continue
        diagonal = dx != 0 and dy != 0
        if diagonal and not allow_corner_cutting:
            if not grid_map.is_passable((x + dx, y)) or not grid_map.is_passable((x, y + dy)):
                continue
        result.append((next_coord, _move_cost(grid_map.cost_at(next_coord), diagonal)))
    return tuple(result)


def potential_neighbor_coords(coord: Coord, neighborhood: int) -> tuple[Coord, ...]:
    x, y = coord
    if neighborhood == 4:
        offsets = ((1, 0), (0, 1), (-1, 0), (0, -1))
    elif neighborhood == 8:
        offsets = (
            (1, 0),
            (0, 1),
            (-1, 0),
            (0, -1),
            (1, 1),
            (1, -1),
            (-1, 1),
            (-1, -1),
        )
    else:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "neighborhood must be 4 or 8",
            field="search.neighborhood",
        )
    return tuple((x + dx, y + dy) for dx, dy in offsets)


def _base_heuristic(
    coord: Coord,
    goal: Coord,
    grid_map: GridMap,
    neighborhood: int,
) -> int:
    dx = abs(goal[0] - coord[0])
    dy = abs(goal[1] - coord[1])
    min_cost = grid_map.minimum_passable_cost()
    if neighborhood == 8:
        diagonal_steps = min(dx, dy)
        straight_steps = max(dx, dy) - diagonal_steps
        scaled_distance = (
            straight_steps * COORD_SCALE + diagonal_steps * DIAGONAL_SCALE
        )
    else:
        scaled_distance = (dx + dy) * COORD_SCALE
    return (scaled_distance * min_cost) // COORD_SCALE


def weighted_heuristic(
    coord: Coord,
    goal: Coord,
    grid_map: GridMap,
    neighborhood: int,
    weight: int,
) -> int:
    base = _base_heuristic(coord, goal, grid_map, neighborhood)
    result = (base * weight) // WEIGHT_SCALE
    if result > MAX_PLANNER_COST:
        raise OverflowError("heuristic exceeds fixed-point overflow bound")
    return result


def _reverse_exact_distances(
    grid_map: GridMap,
    goal: Coord,
    config: SearchConfig,
) -> dict[Coord, int]:
    """Exact remaining cost to goal under the same movement model."""

    distances: dict[Coord, int] = {goal: 0}
    heap: list[tuple[int, int, int]] = [(0, goal[1], goal[0])]
    while heap:
        distance, y, x = heapq.heappop(heap)
        current = (x, y)
        if distances.get(current) != distance:
            continue
        for next_coord, _ in neighbors(
            grid_map, current, config.neighborhood, config.allow_corner_cutting
        ):
            step_cost = _move_cost(
                grid_map.cost_at(current),
                next_coord[0] != current[0] and next_coord[1] != current[1],
            )
            candidate = checked_add(distance, step_cost, label="reverse distance")
            if candidate < distances.get(next_coord, MAX_PLANNER_COST + 1):
                distances[next_coord] = candidate
                heapq.heappush(heap, (candidate, next_coord[1], next_coord[0]))
    return distances


def first_heuristic_overestimate(
    member_id: str,
    grid_map: GridMap,
    goal: Coord,
    config: SearchConfig,
    heuristic: Heuristic,
) -> tuple[Coord, int, int] | None:
    exact = _reverse_exact_distances(grid_map, goal, config)
    for y in range(grid_map.height):
        for x in range(grid_map.width):
            coord = (x, y)
            if coord not in exact:
                continue
            value = heuristic(coord, goal, grid_map, config.neighborhood)
            if value > exact[coord]:
                return coord, value, exact[coord]
    return None


def _terrain_dependencies(grid_map: GridMap, cells: Iterable[Coord]) -> tuple[tuple[str, ...], tuple[str, ...]]:
    terrain: set[str] = set()
    blockades: set[str] = set()
    for coord in cells:
        terrain.add(grid_map.terrain_at(coord))
        blockades.update(grid_map.blockade_ids_at(coord))
    return tuple(sorted(terrain)), tuple(sorted(blockades))


def plan_member(
    scene: Scene,
    member_id: str,
    *,
    config: SearchConfig | None = None,
    heuristic: Heuristic | None = None,
) -> RawPath:
    """Run member-layered A*. The result is independent of call order and thread id."""

    search_config = config or scene.search
    member = scene.member(member_id)
    grid_map = scene.map
    start = member.start
    goal = member.goal
    if not grid_map.in_bounds(start) or not grid_map.in_bounds(goal):
        raise WorkbenchError(
            ErrorCode.E_RANGE,
            "member endpoint is outside map bounds",
            field="members.start",
            coord=start,
            member_id=member_id,
        )
    if not grid_map.is_passable(start) or not grid_map.is_passable(goal):
        raise WorkbenchError(
            ErrorCode.E_RANGE,
            "member endpoint is impassable",
            field="members.start",
            coord=start if not grid_map.is_passable(start) else goal,
            member_id=member_id,
        )

    def h(coord: Coord) -> int:
        if heuristic is not None:
            return heuristic(coord, goal, grid_map, search_config.neighborhood)
        try:
            return weighted_heuristic(
                coord,
                goal,
                grid_map,
                search_config.neighborhood,
                search_config.heuristic_weight,
            )
        except OverflowError as exc:
            raise WorkbenchError(
                ErrorCode.E_HEUR,
                str(exc),
                field="heuristic",
                coord=coord,
                member_id=member_id,
            ) from exc

    if search_config.verify_heuristic:
        overestimate = first_heuristic_overestimate(
            member_id, grid_map, goal, search_config, lambda c, g, m, n: h(c)
        )
        if overestimate is not None:
            coord, h_value, exact = overestimate
            raise WorkbenchError(
                ErrorCode.E_HEUR,
                f"heuristic overestimates at grid {coord}: h={h_value}, exact={exact}",
                field="heuristic",
                coord=coord,
                member_id=member_id,
                details={"heuristic": h_value, "exact_remaining": exact},
            )

    start_h = h(start)
    best_cost: dict[Coord, int] = {start: 0}
    parent: dict[Coord, Coord] = {}
    closed: set[Coord] = set()
    heap: list[tuple[int, int, int, int]] = [(start_h, start_h, start[1], start[0])]
    expansions = 0
    heuristic_checks = 0
    search_cells: set[Coord] = {start}

    while heap:
        _, _, y, x = heapq.heappop(heap)
        current = (x, y)
        if current in closed:
            continue
        closed.add(current)
        expansions += 1
        if expansions > search_config.max_expansions:
            raise WorkbenchError(
                ErrorCode.E_HEUR,
                "search expansion limit exceeded",
                field="search.max_expansions",
                coord=current,
                member_id=member_id,
            )
        if current == goal:
            break

        current_cost = best_cost[current]
        for potential in potential_neighbor_coords(
            current, search_config.neighborhood
        ):
            if grid_map.in_bounds(potential):
                search_cells.add(potential)
        for next_coord, step_cost in neighbors(
            grid_map,
            current,
            search_config.neighborhood,
            search_config.allow_corner_cutting,
        ):
            if next_coord in closed:
                continue
            try:
                candidate = checked_add(current_cost, step_cost, label="path cost")
            except OverflowError as exc:
                raise WorkbenchError(
                    ErrorCode.E_HEUR,
                    str(exc),
                    field="search.cost",
                    coord=next_coord,
                    member_id=member_id,
                ) from exc
            previous = best_cost.get(next_coord)
            predecessor_improves = (
                previous is not None
                and candidate == previous
                and current < parent.get(next_coord, (10**9, 10**9))
            )
            if previous is None or candidate < previous or predecessor_improves:
                best_cost[next_coord] = candidate
                parent[next_coord] = current
                estimate = h(next_coord)
                heuristic_checks += 1
                try:
                    total = checked_add(candidate, estimate, label="f score")
                except OverflowError as exc:
                    raise WorkbenchError(
                        ErrorCode.E_HEUR,
                        str(exc),
                        field="search.cost",
                        coord=next_coord,
                        member_id=member_id,
                    ) from exc
                heapq.heappush(heap, (total, estimate, next_coord[1], next_coord[0]))

    if goal not in best_cost:
        raise WorkbenchError(
            ErrorCode.E_NO_PATH,
            f"no passable path from {start} to {goal}",
            field="members.goal",
            coord=goal,
            member_id=member_id,
        )

    cells_reversed: list[Coord] = [goal]
    while cells_reversed[-1] != start:
        cells_reversed.append(parent[cells_reversed[-1]])
    cells = tuple(reversed(cells_reversed))
    terrain_classes, blockade_ids = _terrain_dependencies(grid_map, cells)
    return RawPath(
        member_id=member_id,
        cells=cells,
        cost=best_cost[goal],
        dependencies=PathDependencies(
            cells=cells,
            terrain_classes=terrain_classes,
            blockade_ids=blockade_ids,
            map_hash=grid_map.content_hash(),
            search_cells=tuple(sorted(search_cells)),
        ),
        expansions=expansions,
        heuristic_checks=heuristic_checks,
    )
