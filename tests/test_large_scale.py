from __future__ import annotations

import time

import numpy as np

from tactical_scene.camera import Camera
from tactical_scene.query import BudgetConfig, QueryEngine
from tactical_scene.scene import AABBShape, CapsuleShape, Scene, SceneUnit


def test_large_scene_selection_is_version_bound_and_deterministic():
    rng = np.random.default_rng(123)
    units = []
    for index in range(6000):
        x = float(rng.uniform(-90, 90))
        y = float(rng.uniform(-55, 55))
        z = float(rng.uniform(-5, 5))
        matrix = np.eye(4, dtype=np.float64)
        matrix[:3, 3] = (x, y, z)
        if index % 4:
            shape = AABBShape((-1.0, -1.0, -1.0), (1.0, 1.0, 1.0))
        else:
            shape = CapsuleShape((0, 0, -1.5), (0, 0, 1.5), 0.8)
        units.append(
            SceneUnit(
                index + 1,
                f"u{index + 1}",
                shape,
                tuple(float(value) for value in matrix.reshape(-1)),
            )
        )
    scene = Scene(1, tuple(units))
    camera = Camera.look_at(
        (0, 0, 180),
        (0, 0, 0),
        (0, 1, 0),
        width=1600,
        height=900,
        fov_y_degrees=50,
        near=0.5,
        far=5000,
    )
    budget = BudgetConfig(
        budget_ms=5000.0,
        memory_budget_bytes=256 * 1024 * 1024,
        bucket_grids=(64, 32, 16, 8, 1),
    )
    engine = QueryEngine(scene, camera, budget_config=budget)
    started = time.perf_counter()
    first = engine.select_rect(100, 140, 1320, 720)
    elapsed = time.perf_counter() - started
    second = engine.select_rect(100, 140, 1320, 720)
    assert first.scene_version == scene.version
    assert first.scene_hash == scene.content_hash
    assert first.selected == tuple(sorted(first.selected))
    assert first.canonical_dict() == second.canonical_dict()
    assert elapsed < 20.0
    assert first.stage_counts["scene_units"] == 6000
    assert first.stage_counts["selected"] == len(first.selected)
