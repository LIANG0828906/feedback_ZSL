"""Formation-aware line-of-sight smoothing and waypoint extraction."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable, Mapping

from .errors import ErrorCode, WorkbenchError
from .fixed import (
    COORD_SCALE,
    interpolate_point,
    point_distance,
    point_distance_squared,
)
from .models import Coord, GridMap, Point, Scene
from .search import RawPath

SMOOTH_SAMPLES = 41


@dataclass(frozen=True)
class Polyline:
    points: tuple[Point, ...]
    lengths: tuple[int, ...]
    total_length: int

    @classmethod
    def from_points(cls, points: Iterable[Point]) -> "Polyline":
        point_list = tuple(points)
        if not point_list:
            raise ValueError("polyline requires at least one point")
        lengths = tuple(
            point_distance(point_list[index], point_list[index + 1])
            for index in range(len(point_list) - 1)
        )
        return cls(point_list, lengths, sum(lengths))

    def sample(self, unit_fraction: int) -> Point:
        """Sample by integer fraction in [0, 1000]."""

        if len(self.points) == 1 or self.total_length == 0:
            return self.points[0]
        fraction = max(0, min(1000, unit_fraction))
        target = (self.total_length * fraction) // 1000
        remaining = target
        for index, length in enumerate(self.lengths):
            if remaining <= length or index == len(self.lengths) - 1:
                return interpolate_point(
                    self.points[index],
                    self.points[index + 1],
                    remaining,
                    length,
                )
            remaining -= length
        return self.points[-1]


@dataclass(frozen=True)
class Waypoint:
    index: int
    grid: Coord
    world: Point

    def to_dict(self) -> dict[str, object]:
        return {
            "index": self.index,
            "grid": {"x": self.grid[0], "y": self.grid[1]},
            "world": {"x": self.world[0], "y": self.world[1]},
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, object]) -> "Waypoint":
        grid = payload["grid"]
        world = payload["world"]
        return cls(
            index=int(payload["index"]),
            grid=(int(grid["x"]), int(grid["y"])),  # type: ignore[index]
            world=(int(world["x"]), int(world["y"])),  # type: ignore[index]
        )


@dataclass(frozen=True)
class SmoothedPath:
    raw: RawPath
    points: tuple[Point, ...]
    waypoints: tuple[Waypoint, ...]
    baseline_cost: int
    cost: int
    slot_tolerance: int
    max_spacing: int

    @property
    def member_id(self) -> str:
        return self.raw.member_id

    @property
    def polyline(self) -> Polyline:
        return Polyline.from_points(self.points)

    def to_dict(self) -> dict[str, object]:
        return {
            "raw": self.raw.to_dict(),
            "points": [list(point) for point in self.points],
            "waypoints": [waypoint.to_dict() for waypoint in self.waypoints],
            "baseline_cost": self.baseline_cost,
            "cost": self.cost,
            "slot_tolerance": self.slot_tolerance,
            "max_spacing": self.max_spacing,
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, object]) -> "SmoothedPath":
        return cls(
            raw=RawPath.from_dict(payload["raw"]),  # type: ignore[arg-type]
            points=tuple(
                (int(point[0]), int(point[1]))
                for point in payload["points"]  # type: ignore[union-attr]
            ),
            waypoints=tuple(
                Waypoint.from_dict(item)
                for item in payload["waypoints"]  # type: ignore[union-attr]
            ),
            baseline_cost=int(payload["baseline_cost"]),
            cost=int(payload["cost"]),
            slot_tolerance=int(payload["slot_tolerance"]),
            max_spacing=int(payload["max_spacing"]),
        )


def _cells_for_line(start: Coord, end: Coord) -> tuple[Coord, ...]:
    """Bresenham line with the intermediate cells used by line-of-sight tests."""

    x0, y0 = start
    x1, y1 = end
    dx = abs(x1 - x0)
    dy = abs(y1 - y0)
    sx = 1 if x0 < x1 else -1
    sy = 1 if y0 < y1 else -1
    error = dx - dy
    cells: list[Coord] = []
    x, y = x0, y0
    while True:
        cells.append((x, y))
        if x == x1 and y == y1:
            break
        doubled = 2 * error
        if doubled > -dy:
            error -= dy
            x += sx
        if doubled < dx:
            error += dx
            y += sy
    return tuple(cells)


def line_of_sight(
    grid_map: GridMap,
    start: Coord,
    end: Coord,
    *,
    allow_corner_cutting: bool = False,
) -> bool:
    cells = _cells_for_line(start, end)
    for coord in cells:
        if not grid_map.is_passable(coord):
            return False
    if not allow_corner_cutting:
        for left, right in zip(cells, cells[1:]):
            if left[0] != right[0] and left[1] != right[1]:
                if not grid_map.is_passable((right[0], left[1])):
                    return False
                if not grid_map.is_passable((left[0], right[1])):
                    return False
    return True


def traversed_cells(path: SmoothedPath | None, points: tuple[Point, ...] | None = None) -> frozenset[Coord]:
    """All grid cells touched by the quantized smoothed polyline."""

    if points is None:
        if path is None:
            return frozenset()
        points = path.points
    result: set[Coord] = set()
    for start, end in zip(points, points[1:]):
        result.update(_cells_for_line(_grid_from_world(start), _grid_from_world(end)))
    if points:
        result.add(_grid_from_world(points[0]))
        result.add(_grid_from_world(points[-1]))
    return frozenset(result)


def _cell_point(cell: Coord) -> Point:
    return cell[0] * COORD_SCALE + COORD_SCALE // 2, cell[1] * COORD_SCALE + COORD_SCALE // 2


def _grid_from_world(point: Point) -> Coord:
    return point[0] // COORD_SCALE, point[1] // COORD_SCALE


def _relative_slot_offset(scene: Scene, member_id: str) -> Point:
    formation = scene.formation
    member = scene.member(member_id)
    parent_id = formation.parents.get(member_id)
    if parent_id is None:
        return (0, 0)
    parent = scene.member(parent_id)
    own = formation.slots[member.slot_id]
    parent_slot = formation.slots[parent.slot_id]
    return own[0] - parent_slot[0], own[1] - parent_slot[1]


def _formation_metrics(
    scene: Scene,
    member_id: str,
    polyline: Polyline,
    parent_polyline: Polyline | None,
    fraction: int,
) -> tuple[int, int]:
    if parent_polyline is None:
        return 0, 0
    parent_id = scene.formation.parents[member_id]
    if parent_id is None:
        return 0, 0
    position = polyline.sample(fraction)
    parent_position = parent_polyline.sample(fraction)
    offset = _relative_slot_offset(scene, member_id)
    expected = parent_position[0] + offset[0], parent_position[1] + offset[1]
    deviation = point_distance(position, expected)
    spacing = point_distance(position, parent_position)
    return deviation, spacing


def _candidate_paths(
    anchor_points: tuple[Point, ...],
    raw_points: tuple[Point, ...],
    current: int,
    reach: int,
    chosen: list[Point],
) -> Iterable[tuple[int, Polyline]]:
    for target in range(reach, current, -1):
        candidate = Polyline.from_points(tuple(chosen) + raw_points[target:])
        yield target, candidate


def _candidate_satisfies(
    scene: Scene,
    member_id: str,
    candidate: Polyline,
    parent_polyline: Polyline | None,
    tolerance: int,
    max_spacing: int,
) -> bool:
    if parent_polyline is None:
        return True
    for sample_index in range(SMOOTH_SAMPLES):
        fraction = (sample_index * 1000) // (SMOOTH_SAMPLES - 1)
        deviation, spacing = _formation_metrics(
            scene, member_id, candidate, parent_polyline, fraction
        )
        if deviation > tolerance or spacing > max_spacing:
            return False
    return True


def _required_anchor_indices(
    scene: Scene,
    member_id: str,
    raw_points: tuple[Point, ...],
    raw_polyline: Polyline,
    parent_polyline: Polyline | None,
    tolerance: int,
    max_spacing: int,
) -> set[int]:
    required = {0, len(raw_points) - 1}
    if parent_polyline is None:
        return required
    for index in range(1, len(raw_points) - 1):
        fraction = 0
        if raw_polyline.total_length > 0:
            prefix = Polyline.from_points(raw_points[: index + 1])
            fraction = (prefix.total_length * 1000) // raw_polyline.total_length
        deviation, spacing = _formation_metrics(
            scene, member_id, raw_polyline, parent_polyline, fraction
        )
        if deviation > tolerance or spacing > max_spacing:
            required.add(index)
    return required


def smooth_member(
    scene: Scene,
    raw_path: RawPath,
    parent_path: SmoothedPath | None,
    *,
    slot_tolerance: int | None = None,
    max_spacing: int | None = None,
    allow_corner_cutting: bool = False,
) -> SmoothedPath:
    tolerance = scene.formation.slot_tolerance if slot_tolerance is None else slot_tolerance
    spacing_limit = scene.formation.max_spacing if max_spacing is None else max_spacing
    if tolerance < 0 or tolerance > scene.formation.hard_slot_tolerance:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "slot tolerance is outside the declared hard formation bound",
            field="formation.slot_tolerance",
            member_id=raw_path.member_id,
            deviation=tolerance,
        )
    if spacing_limit <= 0:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "max spacing must be positive",
            field="formation.max_spacing",
            member_id=raw_path.member_id,
        )

    raw_points = tuple(_cell_point(cell) for cell in raw_path.cells)
    if not raw_points:
        raise WorkbenchError(
            ErrorCode.E_STATE,
            "cannot smooth an empty path",
            member_id=raw_path.member_id,
        )
    raw_polyline = Polyline.from_points(raw_points)
    parent_polyline = parent_path.polyline if parent_path is not None else None
    chosen_indices: list[int] = [0]
    chosen_points: list[Point] = [raw_points[0]]
    current = 0
    while current < len(raw_points) - 1:
        best_target: int | None = None
        best_polyline: Polyline | None = None
        for target, candidate in _candidate_paths(
            raw_points, raw_points, current, len(raw_points) - 1, chosen_points
        ):
            left_cell = _grid_from_world(raw_points[current])
            right_cell = _grid_from_world(raw_points[target])
            if not line_of_sight(
                scene.map,
                left_cell,
                right_cell,
                allow_corner_cutting=allow_corner_cutting,
            ):
                continue
            if _candidate_satisfies(
                scene,
                raw_path.member_id,
                candidate,
                parent_polyline,
                tolerance,
                spacing_limit,
            ):
                best_target = target
                best_polyline = candidate
                break
        if best_target is None:
            fallback = current + 1
            while fallback < len(raw_points) - 1:
                if line_of_sight(
                    scene.map,
                    _grid_from_world(raw_points[current]),
                    _grid_from_world(raw_points[fallback]),
                    allow_corner_cutting=allow_corner_cutting,
                ):
                    break
                fallback += 1
            best_target = fallback
            best_polyline = Polyline.from_points(
                tuple(chosen_points) + raw_points[best_target:]
            )
            if not _candidate_satisfies(
                scene,
                raw_path.member_id,
                best_polyline,
                parent_polyline,
                tolerance,
                spacing_limit,
            ):
                # Keep the raw anchor and continue. Final validation reports E_FORM
                # with the exact offending pair and deviation.
                pass
        current = best_target
        chosen_indices.append(current)
        chosen_points.append(raw_points[current])

    if chosen_indices[-1] != len(raw_points) - 1:
        chosen_indices.append(len(raw_points) - 1)
        chosen_points.append(raw_points[-1])

    points = tuple(chosen_points)
    smoothed_polyline = Polyline.from_points(points)
    if parent_polyline is not None:
        for sample_index in range(SMOOTH_SAMPLES):
            fraction = (sample_index * 1000) // (SMOOTH_SAMPLES - 1)
            deviation, spacing = _formation_metrics(
                scene,
                raw_path.member_id,
                smoothed_polyline,
                parent_polyline,
                fraction,
            )
            if deviation > tolerance or spacing > spacing_limit:
                parent_id = scene.formation.parents[raw_path.member_id]
                raise WorkbenchError(
                    ErrorCode.E_FORM,
                    (
                        f"smoothed path violates formation constraints: "
                        f"deviation={deviation}, spacing={spacing}"
                    ),
                    field="formation.slot_tolerance",
                    member_id=raw_path.member_id,
                    coord=_grid_from_world(smoothed_polyline.sample(fraction)),
                    deviation=deviation,
                    pair=(
                        raw_path.member_id,
                        parent_id if parent_id is not None else raw_path.member_id,
                    ),
                    details={"spacing": spacing, "fraction": fraction},
                )

    for cell in traversed_cells(None, points):
        if not scene.map.is_passable(cell):
            raise WorkbenchError(
                ErrorCode.E_FORM,
                "smoothed path enters an impassable cell",
                field="map.costs",
                coord=cell,
                member_id=raw_path.member_id,
            )

    waypoints = tuple(
        Waypoint(
            index=index,
            grid=_grid_from_world(point),
            world=point,
        )
        for index, point in enumerate(points)
    )
    return SmoothedPath(
        raw=raw_path,
        points=points,
        waypoints=waypoints,
        baseline_cost=raw_path.cost,
        cost=max(raw_path.cost, raw_path.cost),
        slot_tolerance=tolerance,
        max_spacing=spacing_limit,
    )


def smooth_all(
    scene: Scene,
    raw_paths: Mapping[str, RawPath],
    *,
    slot_tolerance: int | None = None,
    max_spacing: int | None = None,
    allow_corner_cutting: bool = False,
) -> dict[str, SmoothedPath]:
    result: dict[str, SmoothedPath] = {}
    for member_id in scene.formation.topo_order():
        raw_path = raw_paths.get(member_id)
        if raw_path is None:
            continue
        parent_id = scene.formation.parents.get(member_id)
        parent_path = result.get(parent_id) if parent_id is not None else None
        result[member_id] = smooth_member(
            scene,
            raw_path,
            parent_path,
            slot_tolerance=slot_tolerance,
            max_spacing=max_spacing,
            allow_corner_cutting=allow_corner_cutting,
        )
    return result


def waypoint_count_with_tolerance(
    scene: Scene,
    raw_paths: Mapping[str, RawPath],
    tolerance: int,
) -> int:
    smoothed = smooth_all(scene, raw_paths, slot_tolerance=tolerance)
    return sum(len(path.waypoints) for path in smoothed.values())
