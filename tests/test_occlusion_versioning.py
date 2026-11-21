from __future__ import annotations

import json
import threading
import time
from dataclasses import replace

import numpy as np
import pytest

from tactical_scene.camera import Camera
from tactical_scene.errors import ErrorCode, TacticalError
from tactical_scene.query import BudgetConfig, HoverConfig, QueryEngine
from tactical_scene.scene import AABBShape, Scene, SceneUnit, write_scene
from tactical_scene.versioning import (
    SceneEngine,
    SceneRepository,
    load_published_scene,
    upsert_manifest_entry,
)


def _camera() -> Camera:
    return Camera.orthographic(
        left=-50,
        right=50,
        bottom=-50,
        top=50,
        width=100,
        height=100,
        near=0.1,
        far=1000,
    )


def _transform(x: float, y: float, z: float) -> tuple[float, ...]:
    matrix = np.eye(4, dtype=np.float64)
    matrix[:3, 3] = (x, y, z)
    return tuple(float(value) for value in matrix.reshape(-1))


def _stack_unit(unit_id: int, z: float, *, half: float = 6.0) -> SceneUnit:
    return SceneUnit(
        unit_id,
        f"u{unit_id}",
        AABBShape((-half, -half, -half), (half, half, half)),
        _transform(0, 0, z),
    )


def _budget(**kwargs) -> BudgetConfig:
    values = dict(budget_ms=1e12, memory_budget_bytes=2**40, bucket_grids=(64,))
    values.update(kwargs)
    return BudgetConfig(**values)


def test_hover_returns_nearest_and_occluders_for_target():
    scene = Scene(1, (_stack_unit(1, 90), _stack_unit(2, 100), _stack_unit(3, 110)))
    engine = QueryEngine(scene, _camera(), budget_config=_budget())
    result = engine.hover(50, 50, target_unit_id=1)
    assert result.nearest_visible_unit_id == 3
    assert result.nearest_visible_face_depth == 884.0
    assert [occluder.unit_id for occluder in result.occluders] == [3, 2]
    assert result.target_visible_screen_area == pytest.approx(0.0)


def test_hover_ambiguous_depth_is_reported():
    scene = Scene(
        1,
        (_stack_unit(1, 100.0), _stack_unit(2, 100.0000005)),
    )
    engine = QueryEngine(
        scene,
        _camera(),
        budget_config=_budget(),
        hover_config=HoverConfig(depth_epsilon=1e-4),
    )
    with pytest.raises(TacticalError) as exc_info:
        engine.hover(50, 50)
    assert exc_info.value.code == ErrorCode.OCCLUSION_AMBIGUOUS
    assert exc_info.value.details["depth_delta"] <= 1e-4
    assert {candidate["unit_id"] for candidate in exc_info.value.details["candidates"]} == {1, 2}


def test_hover_depth_order_is_stable_when_panning():
    scene = Scene(1, (_stack_unit(1, 90), _stack_unit(2, 110)))
    first_camera = _camera()
    second_camera = Camera.orthographic(
        left=-45,
        right=55,
        bottom=-45,
        top=55,
        width=100,
        height=100,
        near=0.1,
        far=1000,
    )
    first = QueryEngine(scene, first_camera, budget_config=_budget()).hover(50, 50)
    second = QueryEngine(scene, second_camera, budget_config=_budget()).hover(45, 55)
    assert first.nearest_visible_unit_id == second.nearest_visible_unit_id == 2


def test_stale_handle_and_ongoing_snapshot_isolation():
    scene = Scene(1, (_stack_unit(1, 100),))
    repository = SceneRepository(scene)
    handle = repository.acquire_handle()
    ongoing = QueryEngine(repository.resolve_handle(handle), _camera(), budget_config=_budget())
    updated_unit = replace(scene.units[0], transform=_transform(5, 0, 100))
    published = repository.publish(scene.with_updates([updated_unit]))
    assert published.version == 2
    with pytest.raises(TacticalError) as exc_info:
        repository.resolve_handle(handle)
    assert exc_info.value.code == ErrorCode.STALE_HANDLE
    result = ongoing.select_rect(0, 0, 100, 100)
    assert result.scene_version == 1
    assert result.scene_hash == scene.content_hash


def test_manifest_mismatch_is_stale(tmp_path):
    scene = Scene(1, (_stack_unit(1, 100),))
    scene_path = tmp_path / "scene.bfscene"
    manifest_path = tmp_path / "manifest.json"
    write_scene(scene_path, scene)
    upsert_manifest_entry(manifest_path, scene)
    payload = json.loads(manifest_path.read_text(encoding="utf-8"))
    payload["entries"][0]["content_hash"] = "f" * 64
    manifest_path.write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(TacticalError) as exc_info:
        load_published_scene(scene_path, manifest_path)
    assert exc_info.value.code == ErrorCode.SCENE_STALE


def test_background_edit_is_exclusive_and_queries_see_published_versions():
    scene = Scene(1, (_stack_unit(1, 100),))
    engine = SceneEngine(scene, budget_config=_budget())
    old_handle = engine.acquire_handle()

    def mutate(base: Scene):
        time.sleep(0.15)
        unit = replace(base.units[0], transform=_transform(8, 0, 100))
        return (unit,)

    future = engine.submit_edit(mutate)
    with pytest.raises(TacticalError) as busy:
        engine.submit_edit(lambda base: tuple(base.units))
    assert busy.value.code == ErrorCode.BUSY
    during = engine.select_rect(_camera(), 0, 0, 100, 100)
    assert during.scene_version == 1
    new_scene = future.result(timeout=5)
    assert new_scene.version == 2
    after = engine.select_rect(_camera(), 0, 0, 100, 100)
    assert after.scene_version == 2
    with pytest.raises(TacticalError) as stale:
        engine.repository.resolve_handle(old_handle)
    assert stale.value.code == ErrorCode.STALE_HANDLE
    assert engine.repository.max_publish_block_ms < 1000.0
    engine.shutdown()


def test_publish_rejects_non_monotonic_versions():
    scene = Scene(1, (_stack_unit(1, 100),))
    repository = SceneRepository(scene)
    with pytest.raises(TacticalError) as exc_info:
        repository.publish(scene)
    assert exc_info.value.code == ErrorCode.SCENE_STALE
