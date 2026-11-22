"""Immutable domain models and version constants."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field, replace
from typing import Any

from .fixed import COORD_SCALE

ALGORITHM_VERSION = "formation-core-1.0.0"
FIXED_POINT_VERSION = "coord1000-angle1000-cost-i63-v1"
SCHEMA_VERSION = 1

Coord = tuple[int, int]
Point = tuple[int, int]


@dataclass(frozen=True)
class TerrainType:
    name: str
    label: str
    passable: bool
    color: str


@dataclass(frozen=True)
class Blockade:
    blockade_id: str
    terrain: str
    cells: tuple[Coord, ...]
    active: bool
    label: str = ""


@dataclass(frozen=True)
class GridMap:
    width: int
    height: int
    costs: tuple[tuple[int, ...], ...]
    terrain: tuple[tuple[str, ...], ...]
    terrain_types: dict[str, TerrainType]
    blockades: tuple[Blockade, ...] = ()

    def in_bounds(self, coord: Coord) -> bool:
        x, y = coord
        return 0 <= x < self.width and 0 <= y < self.height

    def blockade_cells(self) -> frozenset[Coord]:
        result: set[Coord] = set()
        for blockade in self.blockades:
            if blockade.active:
                result.update(blockade.cells)
        return frozenset(result)

    def blockade_ids_at(self, coord: Coord) -> tuple[str, ...]:
        return tuple(
            sorted(
                blockade.blockade_id
                for blockade in self.blockades
                if blockade.active and coord in blockade.cells
            )
        )

    def is_passable(self, coord: Coord) -> bool:
        if not self.in_bounds(coord):
            return False
        x, y = coord
        if self.costs[y][x] < 0:
            return False
        if coord in self.blockade_cells():
            return False
        terrain = self.terrain_types.get(self.terrain[y][x])
        return terrain is not None and terrain.passable

    def cost_at(self, coord: Coord) -> int:
        x, y = coord
        return self.costs[y][x]

    def terrain_at(self, coord: Coord) -> str:
        x, y = coord
        return self.terrain[y][x]

    def with_cost(self, coord: Coord, cost: int) -> "GridMap":
        x, y = coord
        rows = [list(row) for row in self.costs]
        rows[y][x] = cost
        terrain_rows = [list(row) for row in self.terrain]
        current_terrain = self.terrain_types.get(self.terrain[y][x])
        if cost < 0 and current_terrain is not None and current_terrain.passable:
            replacement = next(
                (name for name, item in sorted(self.terrain_types.items()) if not item.passable),
                self.terrain[y][x],
            )
            terrain_rows[y][x] = replacement
        elif cost >= 0 and current_terrain is not None and not current_terrain.passable:
            replacement = next(
                (name for name, item in sorted(self.terrain_types.items()) if item.passable),
                self.terrain[y][x],
            )
            terrain_rows[y][x] = replacement
        return replace(
            self,
            costs=tuple(tuple(row) for row in rows),
            terrain=tuple(tuple(row) for row in terrain_rows),
        )

    def with_blockade_active(self, blockade_id: str, active: bool) -> "GridMap":
        blockades = tuple(
            replace(blockade, active=active) if blockade.blockade_id == blockade_id else blockade
            for blockade in self.blockades
        )
        return replace(self, blockades=blockades)

    def content_payload(self) -> dict[str, Any]:
        return {
            "width": self.width,
            "height": self.height,
            "costs": [list(row) for row in self.costs],
            "terrain": [list(row) for row in self.terrain],
            "terrain_types": {
                key: {
                    "label": value.label,
                    "passable": value.passable,
                    "color": value.color,
                }
                for key, value in sorted(self.terrain_types.items())
            },
            "blockades": [
                {
                    "blockade_id": item.blockade_id,
                    "terrain": item.terrain,
                    "cells": [list(cell) for cell in item.cells],
                    "active": item.active,
                    "label": item.label,
                }
                for item in sorted(self.blockades, key=lambda entry: entry.blockade_id)
            ],
        }

    def content_hash(self) -> str:
        canonical = json.dumps(
            self.content_payload(),
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
        )
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()

    def minimum_passable_cost(self) -> int:
        values = [
            self.costs[y][x]
            for y in range(self.height)
            for x in range(self.width)
            if self.costs[y][x] >= 0 and self.is_passable((x, y))
        ]
        return min(values) if values else 1


@dataclass(frozen=True)
class FormationSpec:
    version: str
    leader: str
    slots: dict[str, Point]
    parents: dict[str, str | None]
    slot_tolerance: int
    max_spacing: int
    hard_slot_tolerance: int

    def children(self, member_id: str) -> tuple[str, ...]:
        return tuple(
            sorted(child for child, parent in self.parents.items() if parent == member_id)
        )

    def descendants(self, member_id: str) -> tuple[str, ...]:
        result: list[str] = []
        stack = list(reversed(self.children(member_id)))
        while stack:
            child = stack.pop()
            result.append(child)
            stack.extend(reversed(self.children(child)))
        return tuple(result)

    def closure(self, member_ids: set[str] | frozenset[str]) -> tuple[str, ...]:
        result = set(member_ids)
        for member_id in member_ids:
            result.update(self.descendants(member_id))
        return tuple(sorted(result))

    def topo_order(self) -> tuple[str, ...]:
        order: list[str] = []
        queue = [self.leader]
        while queue:
            current = queue.pop(0)
            if current in order:
                continue
            order.append(current)
            queue.extend(self.children(current))
        return tuple(order)

    def version_payload(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "leader": self.leader,
            "slots": {key: list(value) for key, value in sorted(self.slots.items())},
            "parents": {key: value for key, value in sorted(self.parents.items())},
            "slot_tolerance": self.slot_tolerance,
            "max_spacing": self.max_spacing,
            "hard_slot_tolerance": self.hard_slot_tolerance,
        }

    def version_hash(self) -> str:
        canonical = json.dumps(
            self.version_payload(), sort_keys=True, separators=(",", ":")
        )
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class MemberSpec:
    member_id: str
    start: Coord
    goal: Coord
    slot_id: str
    step: int
    turn_rate: int


@dataclass(frozen=True)
class SearchConfig:
    heuristic_weight: int = 1000
    neighborhood: int = 8
    max_expansions: int = 1_000_000
    allow_corner_cutting: bool = False
    verify_heuristic: bool = True

    def version_payload(self) -> dict[str, Any]:
        return {
            "heuristic_weight": self.heuristic_weight,
            "neighborhood": self.neighborhood,
            "max_expansions": self.max_expansions,
            "allow_corner_cutting": self.allow_corner_cutting,
            "verify_heuristic": self.verify_heuristic,
        }

    def version_hash(self) -> str:
        canonical = json.dumps(
            self.version_payload(), sort_keys=True, separators=(",", ":")
        )
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class SimulationConfig:
    max_ticks: int = 600
    interval_ticks: int = 20
    slowdown_divisor: int = 2
    max_parallel_workers: int = 4
    per_worker_buffer: int = 4

    def version_payload(self) -> dict[str, Any]:
        return {
            "max_ticks": self.max_ticks,
            "interval_ticks": self.interval_ticks,
            "slowdown_divisor": self.slowdown_divisor,
            "max_parallel_workers": self.max_parallel_workers,
            "per_worker_buffer": self.per_worker_buffer,
        }

    def version_hash(self) -> str:
        canonical = json.dumps(
            self.version_payload(), sort_keys=True, separators=(",", ":")
        )
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class Scene:
    schema_version: int
    name: str
    seed: int
    map: GridMap
    formation: FormationSpec
    members: tuple[MemberSpec, ...]
    search: SearchConfig
    simulation: SimulationConfig

    def member(self, member_id: str) -> MemberSpec:
        for member in self.members:
            if member.member_id == member_id:
                return member
        raise KeyError(member_id)

    @property
    def member_ids(self) -> tuple[str, ...]:
        return tuple(member.member_id for member in self.members)

    def with_map(self, grid_map: GridMap) -> "Scene":
        return replace(self, map=grid_map)

    def content_payload(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "name": self.name,
            "seed": self.seed,
            "map": self.map.content_payload(),
            "formation": self.formation.version_payload(),
            "members": [
                {
                    "member_id": member.member_id,
                    "start": list(member.start),
                    "goal": list(member.goal),
                    "slot_id": member.slot_id,
                    "step": member.step,
                    "turn_rate": member.turn_rate,
                }
                for member in sorted(self.members, key=lambda item: item.member_id)
            ],
            "search": self.search.version_payload(),
            "simulation": self.simulation.version_payload(),
        }

    def content_hash(self) -> str:
        canonical = json.dumps(
            self.content_payload(),
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
        )
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def world_point(coord: Coord) -> Point:
    return coord[0] * COORD_SCALE + COORD_SCALE // 2, coord[1] * COORD_SCALE + COORD_SCALE // 2
