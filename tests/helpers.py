from __future__ import annotations

from formation_workbench.models import (
    SCHEMA_VERSION,
    FormationSpec,
    GridMap,
    MemberSpec,
    Scene,
    SearchConfig,
    SimulationConfig,
    TerrainType,
)


def lane_scene() -> Scene:
    width, height = 12, 8
    costs = [[-1 for _ in range(width)] for _ in range(height)]
    terrain = [["rock" for _ in range(width)] for _ in range(height)]
    for y in (2, 4, 6):
        for x in range(1, width - 1):
            costs[y][x] = 1
            terrain[y][x] = "plain"
    grid_map = GridMap(
        width=width,
        height=height,
        costs=tuple(tuple(row) for row in costs),
        terrain=tuple(tuple(row) for row in terrain),
        terrain_types={
            "plain": TerrainType("plain", "Lane", True, "#8aa29e"),
            "rock": TerrainType("rock", "Rock", False, "#30343b"),
        },
    )
    formation = FormationSpec(
        version="lane-tree-v1",
        leader="alpha",
        slots={
            "alpha": (0, 0),
            "bravo": (0, 2000),
            "charlie": (0, 4000),
        },
        parents={"alpha": None, "bravo": "alpha", "charlie": "alpha"},
        slot_tolerance=300,
        max_spacing=6000,
        hard_slot_tolerance=2000,
    )
    members = (
        MemberSpec("alpha", (2, 2), (9, 2), "alpha", 1000, 120000),
        MemberSpec("bravo", (2, 4), (9, 4), "bravo", 1000, 120000),
        MemberSpec("charlie", (2, 6), (9, 6), "charlie", 1000, 120000),
    )
    return Scene(
        schema_version=SCHEMA_VERSION,
        name="Lane isolation fixture",
        seed=7,
        map=grid_map,
        formation=formation,
        members=members,
        search=SearchConfig(),
        simulation=SimulationConfig(
            max_ticks=50,
            interval_ticks=5,
            slowdown_divisor=2,
            max_parallel_workers=2,
            per_worker_buffer=2,
        ),
    )
