from __future__ import annotations

import math

import numpy as np
import pytest

from tactical_scene.camera import Camera
from tactical_scene.errors import ErrorCode
from tactical_scene.index import ScreenBucketIndex
from tactical_scene.projection import ProjectionReason
from tactical_scene.query import BudgetConfig, DecisionReason, QueryEngine, SelectionStrategy
from tactical_scene.scene import AABBShape, Scene, SceneUnit


def _camera() -> Camera:
    return Camera.orthographic(
        left=0.0,
        right=100.0,
        bottom=0.0,
        top=100.0,
        width=100,
        height=100,
        near=0.1,
        far=1000.0,
    )


def _transform(x: float, y: float, z: float) -> tuple[float, ...]:
    matrix = np.eye(4, dtype=np.float64)
    matrix[:3, 3] = (x, y, z)
    return tuple(float(value) for value in matrix.reshape(-1))


def _box(unit_id: int, x: float, y: float, z: float, half: float = 2.0, **kwargs) -> SceneUnit:
    shape = AABBShape((-half, -half, -half), (half, half, half))
    return SceneUnit(unit_id, f"u{unit_id}", shape, _transform(x, y, z), **kwargs)


def _big_budget() -> BudgetConfig:
    return BudgetConfig(budget_ms=1e12, memory_budget_bytes=2**40, bucket_grids=(64,))


def test_projection_bbox_area_and_depth():
    scene = Scene(1, (_box(1, 20, 20, 100),))
    engine = QueryEngine(scene, _camera(), budget_config=_big_budget())
    projection = engine.projections[0]
    assert projection.reason is ProjectionReason.VISIBLE_INSIDE
    assert projection.bbox == pytest.approx((18.0, 78.0, 22.0, 82.0))
    assert projection.projected_area == pytest.approx(16.0)
    assert projection.nearest_depth == pytest.approx(898.0)


def test_exact_coverage_and_threshold():
    scene = Scene(1, (_box(1, 20, 20, 100),))
    engine = QueryEngine(scene, _camera(), budget_config=_big_budget())
    result = engine.select_rect(18, 78, 20, 80)
    assert result.strategy is SelectionStrategy.POLYGON_REFINE
    assert result.selected == (1,)
    assert result.coverage_by_unit[1] == 0.25
    assert result.coverage_error_upper_bound == 0.0

    strict = QueryEngine(
        scene,
        _camera(),
        budget_config=BudgetConfig(
            budget_ms=1e12,
            memory_budget_bytes=2**40,
            selection_threshold=0.3,
            bucket_grids=(64,),
        ),
    )
    filtered = strict.select_rect(18, 78, 20, 80)
    assert filtered.selected == ()
    assert filtered.decisions[0].reason is DecisionReason.BELOW_THRESHOLD


def test_camera_behind_and_far_units_are_not_candidates():
    scene = Scene(
        1,
        (
            _box(1, 20, 20, 100),
            _box(2, 20, 20, 1010),  # behind the camera at z=1000
            _box(3, 20, 20, -10),  # beyond the far plane
        ),
    )
    engine = QueryEngine(scene, _camera(), budget_config=_big_budget())
    reasons = {projection.unit_id: projection.reason for projection in engine.projections}
    assert reasons[1] is ProjectionReason.VISIBLE_INSIDE
    assert reasons[2] is ProjectionReason.CAMERA_BEHIND
    assert reasons[3] is ProjectionReason.OUTSIDE_FRUSTUM
    result = engine.select_rect(0, 0, 100, 100)
    assert result.selected == (1,)
    decisions = {decision.unit_id: decision for decision in result.decisions}
    assert decisions[2].reason is DecisionReason.CAMERA_BEHIND
    assert decisions[3].reason is DecisionReason.OUTSIDE_FRUSTUM
    assert result.stage_counts["bucket_candidates"] == 1


def test_crossing_side_uses_clipped_polygon_area():
    scene = Scene(1, (_box(1, 100, 20, 100, half=10.0),))
    engine = QueryEngine(scene, _camera(), budget_config=_big_budget())
    projection = engine.projections[0]
    assert projection.reason is ProjectionReason.VISIBLE_CROSSES_FRUSTUM
    assert projection.bbox == (90.0, 70.0, 100.0, 90.0)
    assert projection.projected_area == 200.0
    result = engine.select_rect(90, 70, 100, 90)
    assert result.coverage_by_unit[1] == 1.0


def test_crossing_near_plane_stays_visible():
    scene = Scene(1, (_box(1, 20, 20, 1000.0, half=3.0),))
    engine = QueryEngine(scene, _camera(), budget_config=_big_budget())
    projection = engine.projections[0]
    assert projection.reason is ProjectionReason.VISIBLE_CROSSES_FRUSTUM
    assert projection.polygon
    assert projection.projected_area > 0.0


def test_candidate_order_does_not_change_conclusions(monkeypatch):
    units = tuple(_box(index + 1, 10 + index * 3, 20 + index * 2, 100 + index) for index in range(80))
    scene = Scene(1, units)
    engine = QueryEngine(scene, _camera(), budget_config=_big_budget())
    expected = engine.select_rect(0, 0, 80, 100)

    original = ScreenBucketIndex.query_rect

    def reversed_query(self, rect):
        return tuple(reversed(original(self, rect)))

    monkeypatch.setattr(ScreenBucketIndex, "query_rect", reversed_query)
    reordered = QueryEngine(scene, _camera(), budget_config=_big_budget()).select_rect(0, 0, 80, 100)
    assert reordered.canonical_dict() == expected.canonical_dict()


def test_rotated_corner_is_bbox_false_positive_but_not_polygon_hit():
    camera = Camera.orthographic(
        left=-50.0,
        right=50.0,
        bottom=-50.0,
        top=50.0,
        width=100,
        height=100,
    )
    angle = math.radians(35.0)
    rotation = np.asarray(
        [
            [math.cos(angle), -math.sin(angle), 0.0, 0.0],
            [math.sin(angle), math.cos(angle), 0.0, 0.0],
            [0.0, 0.0, 1.0, 0.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
        dtype=np.float64,
    )
    rotation[2, 3] = 100.0
    unit = SceneUnit(
        1,
        "diamond",
        AABBShape((-10, -10, -1), (10, 10, 1)),
        tuple(float(value) for value in rotation.reshape(-1)),
    )
    scene = Scene(1, (unit,))
    bbox_engine = QueryEngine(
        scene,
        camera,
        budget_config=BudgetConfig(
            budget_ms=1e12,
            memory_budget_bytes=2**40,
            force_strategy=SelectionStrategy.COARSE_FULL_BBOX.value,
            actual_degradation=False,
        ),
    )
    polygon_engine = QueryEngine(
        scene,
        camera,
        budget_config=BudgetConfig(
            budget_ms=1e12,
            memory_budget_bytes=2**40,
            force_strategy=SelectionStrategy.POLYGON_REFINE.value,
            force_bucket_columns=64,
            actual_degradation=False,
        ),
    )
    bbox = bbox_engine.projections[0].bbox
    assert bbox is not None
    query_rect = (bbox[0], bbox[1], bbox[0] + 0.35, bbox[1] + 0.35)
    bbox_result = bbox_engine.select_rect(*query_rect)
    polygon_result = polygon_engine.select_rect(*query_rect)
    assert bbox_result.selected == (1,)
    assert bbox_result.coverage_error_upper_bound == 1.0
    assert polygon_result.selected == ()
    assert polygon_result.decisions[0].reason is DecisionReason.POLYGON_NO_OVERLAP


def test_budget_forces_declared_degradation_and_zero_budget_errors():
    units = tuple(_box(index + 1, (index % 40) * 2.0 + 1, (index // 40) * 4.0 + 1, 100) for index in range(2000))
    scene = Scene(1, units)
    camera = _camera()
    result = QueryEngine(
        scene,
        camera,
        budget_config=BudgetConfig(
            budget_ms=15.0,
            memory_budget_bytes=2**40,
            bucket_grids=(64, 32, 16, 8, 1),
        ),
    ).select_rect(0, 0, 100, 100)
    assert result.strategy in (SelectionStrategy.FINE_BUCKET_BBOX, SelectionStrategy.COARSE_FULL_BBOX)
    assert result.degraded_stages
    assert result.coverage_error_upper_bound == 1.0

    import pytest

    with pytest.raises(Exception) as exc_info:
        QueryEngine(
            scene,
            camera,
            budget_config=BudgetConfig(budget_ms=0.0),
        ).select_rect(0, 0, 100, 100)
    assert getattr(exc_info.value, "code", None) == ErrorCode.BUDGET_EXCEEDED
