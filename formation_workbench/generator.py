"""Deterministic local formation-scene generator."""

from __future__ import annotations

import random
from typing import Any

from .models import SCHEMA_VERSION


TERRAIN_TYPES: dict[str, dict[str, Any]] = {
    "plain": {"label": "Open ground", "passable": True, "color": "#8aa29e"},
    "road": {"label": "Road", "passable": True, "color": "#c7b26b"},
    "forest": {"label": "Forest", "passable": True, "color": "#4f7a52"},
    "swamp": {"label": "Swamp", "passable": True, "color": "#536b78"},
    "rock": {"label": "Rock", "passable": False, "color": "#30343b"},
}


def _empty_layer(width: int, height: int, value: Any) -> list[list[Any]]:
    return [[value for _ in range(width)] for _ in range(height)]


def generate_scene(seed: int = 167, *, width: int = 30, height: int = 22) -> dict[str, Any]:
    """Generate a deterministic tactical sample with a four-member formation tree."""

    rng = random.Random(seed)
    terrain = _empty_layer(width, height, "plain")
    costs = _empty_layer(width, height, 1)

    for x in range(width):
        terrain[0][x] = terrain[height - 1][x] = "rock"
        costs[0][x] = costs[height - 1][x] = -1
    for y in range(height):
        terrain[y][0] = terrain[y][width - 1] = "rock"
        costs[y][0] = costs[y][width - 1] = -1

    for _ in range(15):
        cx = rng.randint(5, width - 7)
        cy = rng.randint(3, height - 5)
        for dy in range(-1, 2):
            for dx in range(-2, 3):
                if rng.random() < 0.62:
                    x = cx + dx
                    y = cy + dy
                    if 1 <= x < width - 1 and 1 <= y < height - 1:
                        terrain[y][x] = "forest"
                        costs[y][x] = rng.choice((2, 3, 3, 4))

    for _ in range(9):
        cx = rng.randint(8, width - 6)
        cy = rng.randint(4, height - 6)
        for dy in range(-1, 2):
            for dx in range(-1, 2):
                if rng.random() < 0.55:
                    x = cx + dx
                    y = cy + dy
                    if 1 <= x < width - 1 and 1 <= y < height - 1:
                        terrain[y][x] = "swamp"
                        costs[y][x] = rng.choice((5, 6, 7))

    starts = {
        "alpha": (4, 3),
        "bravo": (8, 3),
        "charlie": (4, 5),
        "delta": (8, 5),
    }
    goals = {
        "alpha": (22, 15),
        "bravo": (26, 15),
        "charlie": (22, 17),
        "delta": (26, 17),
    }
    protected: set[tuple[int, int]] = set()
    for coord in list(starts.values()) + list(goals.values()):
        x, y = coord
        for dy in range(-2, 3):
            for dx in range(-2, 3):
                protected.add((x + dx, y + dy))

    def bresenham(start: tuple[int, int], end: tuple[int, int]) -> list[tuple[int, int]]:
        x0, y0 = start
        x1, y1 = end
        dx = abs(x1 - x0)
        dy = abs(y1 - y0)
        sx = 1 if x0 < x1 else -1
        sy = 1 if y0 < y1 else -1
        error = dx - dy
        cells: list[tuple[int, int]] = []
        while True:
            cells.append((x0, y0))
            if x0 == x1 and y0 == y1:
                return cells
            doubled = 2 * error
            if doubled > -dy:
                error -= dy
                x0 += sx
            if doubled < dx:
                error += dx
                y0 += sy

    for member_id in starts:
        for x, y in bresenham(starts[member_id], goals[member_id]):
            for dy in range(-1, 2):
                for dx in range(-1, 2):
                    protected.add((x + dx, y + dy))

    for x, y in protected:
        if 1 <= x < width - 1 and 1 <= y < height - 1:
            terrain[y][x] = "road" if (x + y) % 5 == 0 else "plain"
            costs[y][x] = 1

    blockade_cells = [(13, 2), (14, 2), (15, 2), (13, 3), (14, 3), (15, 3)]
    for x, y in blockade_cells:
        terrain[y][x] = "forest"
        costs[y][x] = 3

    corridor_closure = [(12, 8), (13, 9), (14, 9), (15, 10)]
    for x, y in corridor_closure:
        terrain[y][x] = "swamp"
        costs[y][x] = 5

    payload: dict[str, Any] = {
        "schema_version": SCHEMA_VERSION,
        "name": f"Tactical Formation {seed}",
        "seed": seed,
        "map": {
            "width": width,
            "height": height,
            "costs": costs,
            "terrain": terrain,
            "terrain_types": TERRAIN_TYPES,
            "blockades": [
                {
                    "blockade_id": "blk-forest-north",
                    "label": "Northern forest exclusion",
                    "terrain": "forest",
                    "cells": [list(cell) for cell in blockade_cells],
                    "active": True,
                },
                {
                    "blockade_id": "blk-crossing",
                    "label": "Central corridor closure (initially open)",
                    "terrain": "swamp",
                    "cells": [list(cell) for cell in corridor_closure],
                    "active": False,
                },
            ],
        },
        "formation": {
            "version": "wedge-tree-v1",
            "leader": "alpha",
            "slots": {
                "alpha": {"x": -2000, "y": -1000},
                "bravo": {"x": 2000, "y": -1000},
                "charlie": {"x": -2000, "y": 1000},
                "delta": {"x": 2000, "y": 1000},
            },
            "parents": {
                "alpha": None,
                "bravo": "alpha",
                "charlie": "alpha",
                "delta": "bravo",
            },
            "slot_tolerance": 2200,
            "max_spacing": 7000,
            "hard_slot_tolerance": 9000,
        },
        "members": [
            {
                "member_id": "alpha",
                "start": {"x": starts["alpha"][0], "y": starts["alpha"][1]},
                "goal": {"x": goals["alpha"][0], "y": goals["alpha"][1]},
                "slot_id": "alpha",
                "step": 1700,
                "turn_rate": 12000,
            },
            {
                "member_id": "bravo",
                "start": {"x": starts["bravo"][0], "y": starts["bravo"][1]},
                "goal": {"x": goals["bravo"][0], "y": goals["bravo"][1]},
                "slot_id": "bravo",
                "step": 1700,
                "turn_rate": 12000,
            },
            {
                "member_id": "charlie",
                "start": {"x": starts["charlie"][0], "y": starts["charlie"][1]},
                "goal": {"x": goals["charlie"][0], "y": goals["charlie"][1]},
                "slot_id": "charlie",
                "step": 1700,
                "turn_rate": 12000,
            },
            {
                "member_id": "delta",
                "start": {"x": starts["delta"][0], "y": starts["delta"][1]},
                "goal": {"x": goals["delta"][0], "y": goals["delta"][1]},
                "slot_id": "delta",
                "step": 1700,
                "turn_rate": 12000,
            },
        ],
        "search": {
            "heuristic_weight": 1.0,
            "neighborhood": 8,
            "max_expansions": 1_000_000,
            "allow_corner_cutting": False,
        },
        "simulation": {
            "max_ticks": 500,
            "interval_ticks": 25,
            "slowdown_divisor": 2,
            "max_parallel_workers": 4,
            "per_worker_buffer": 4,
        },
    }
    return payload
