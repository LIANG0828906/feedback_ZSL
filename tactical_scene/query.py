"""Budgeted box selection and stable hover occlusion queries."""

from __future__ import annotations

import json
import math
import threading
import time
from dataclasses import dataclass, field
from enum import Enum
from typing import Iterable, Sequence

from .camera import Camera
from .errors import ErrorCode, TacticalError, scene_invalid
from .geometry import (
    AREA_EPS,
    bbox_area,
    clip_polygon_to_rect,
    clip_rect_to_viewport,
    convex_polygon_intersection,
    distance_to_polygon_boundary,
    normalize_rect,
    point_in_convex_polygon,
    polygon_area,
    polygon_bbox,
    rect_intersects,
)
from .index import BucketConfig, ScreenBucketIndex
from .projection import (
    ProjectionConfig,
    ProjectionReason,
    UnitProjection,
    project_scene,
)
from .scene import Scene


class SelectionStrategy(str, Enum):
    POLYGON_REFINE = "polygon_refine"
    FINE_BUCKET_BBOX = "fine_bucket_bbox"
    COARSE_FULL_BBOX = "coarse_full_bbox"


class DecisionReason(str, Enum):
    HIDDEN = "HIDDEN"
    CAMERA_BEHIND = "CAMERA_BEHIND"
    OUTSIDE_FRUSTUM = "OUTSIDE_FRUSTUM"
    EMPTY_QUERY_RECT = "EMPTY_QUERY_RECT"
    BBOX_NO_OVERLAP = "BBOX_NO_OVERLAP"
    POLYGON_NO_OVERLAP = "POLYGON_NO_OVERLAP"
    BELOW_THRESHOLD = "BELOW_THRESHOLD"
    INCLUDED = "INCLUDED"
    NOT_IN_SELECTION_BUCKETS = "NOT_IN_SELECTION_BUCKETS"


@dataclass(frozen=True, slots=True)
class BudgetConfig:
    """Deterministic planning inputs plus measured-budget guard rails.

    The degradation order is fixed and reported.  When the measured execution
    exceeds the budget, plans are retried in the same order at the next lower
    refinement level.  A comfortable budget therefore always chooses the same
    plan on a given machine.
    """

    budget_ms: float = 500.0
    memory_budget_bytes: int = 128 * 1024 * 1024
    selection_threshold: float = 0.0
    bucket_grids: tuple[int, ...] = (64, 32, 16, 8, 1)
    polygon_cost_us_per_candidate: float = 8.0
    bbox_cost_us_per_candidate: float = 1.2
    decision_cost_us_per_unit: float = 1.5
    index_entry_cost_us: float = 0.35
    base_cost_us: float = 80.0
    actual_degradation: bool = True
    force_strategy: str | None = None
    force_bucket_columns: int | None = None
    coverage_decimals: int = 12
    area_decimals: int = 9

    def __post_init__(self) -> None:
        if self.budget_ms < 0.0:
            raise ValueError("budget_ms must be non-negative")
        if self.memory_budget_bytes < 0:
            raise ValueError("memory_budget_bytes must be non-negative")
        if not 0.0 <= self.selection_threshold <= 1.0:
            raise ValueError("selection_threshold must be in [0, 1]")
        if not self.bucket_grids or any(size < 1 for size in self.bucket_grids):
            raise ValueError("bucket_grids must contain positive grid dimensions")
        if self.force_strategy is not None and self.force_strategy not in {
            SelectionStrategy.POLYGON_REFINE.value,
            SelectionStrategy.FINE_BUCKET_BBOX.value,
            SelectionStrategy.COARSE_FULL_BBOX.value,
        }:
            raise ValueError("unknown forced strategy")
        if self.force_bucket_columns is not None and self.force_bucket_columns < 1:
            raise ValueError("forced bucket columns must be positive")


@dataclass(frozen=True, slots=True)
class HoverConfig:
    depth_epsilon: float = 1e-4
    boundary_epsilon_px: float = 0.25
    area_samples: int = 256

    def __post_init__(self) -> None:
        if self.depth_epsilon < 0.0 or self.boundary_epsilon_px < 0.0:
            raise ValueError("occlusion epsilons must be non-negative")
        if self.area_samples < 8:
            raise ValueError("area_samples must be at least 8")


@dataclass(frozen=True, slots=True)
class StageMetrics:
    stage: str
    candidate_count: int
    elapsed_ms: float

    def to_dict(self) -> dict[str, object]:
        return {
            "stage": self.stage,
            "candidate_count": self.candidate_count,
            "elapsed_ms": self.elapsed_ms,
        }


@dataclass(frozen=True, slots=True)
class UnitDecision:
    unit_id: int
    name: str
    reason: DecisionReason
    selected: bool
    coverage: float
    intersection_area: float
    projected_area: float
    nearest_depth: float

    def to_dict(self, *, coverage_decimals: int = 12, area_decimals: int = 9) -> dict[str, object]:
        depth = None if not math.isfinite(self.nearest_depth) else round(self.nearest_depth, area_decimals)
        return {
            "unit_id": self.unit_id,
            "name": self.name,
            "reason": self.reason.value,
            "selected": self.selected,
            "coverage": round(self.coverage, coverage_decimals),
            "intersection_area": round(self.intersection_area, area_decimals),
            "projected_area": round(self.projected_area, area_decimals),
            "nearest_depth": depth,
        }


@dataclass(frozen=True, slots=True)
class SelectionResult:
    scene_version: int
    scene_hash: str
    camera_hash: str
    rect: tuple[float, float, float, float]
    threshold: float
    strategy: SelectionStrategy
    bucket_columns: int
    bucket_rows: int
    refinement_depth: int
    selected: tuple[int, ...]
    decisions: tuple[UnitDecision, ...]
    stage_counts: dict[str, int]
    stages: tuple[StageMetrics, ...]
    total_elapsed_ms: float
    budget_ms: float
    within_budget: bool
    memory_bytes: int
    memory_budget_bytes: int
    coverage_error_upper_bound: float
    degraded_stages: tuple[str, ...] = ()
    coverage_decimals: int = field(default=12, repr=False)
    area_decimals: int = field(default=9, repr=False)

    @property
    def coverage_by_unit(self) -> dict[int, float]:
        return {
            decision.unit_id: decision.coverage
            for decision in self.decisions
            if decision.selected
        }

    def canonical_dict(self, *, include_timings: bool = False) -> dict[str, object]:
        payload: dict[str, object] = {
            "kind": "selection",
            "scene_version": self.scene_version,
            "scene_hash": self.scene_hash,
            "camera_hash": self.camera_hash,
            "rect": [round(value, self.area_decimals) for value in self.rect],
            "threshold": round(self.threshold, min(self.coverage_decimals, 12)),
            "strategy": self.strategy.value,
            "bucket_grid": [self.bucket_columns, self.bucket_rows],
            "refinement_depth": self.refinement_depth,
            "selected": list(self.selected),
            "selected_coverage": [
                {
                    "unit_id": decision.unit_id,
                    "coverage": round(decision.coverage, self.coverage_decimals),
                }
                for decision in self.decisions
                if decision.selected
            ],
            "stage_candidate_counts": dict(sorted(self.stage_counts.items())),
            "coverage_error_upper_bound": self.coverage_error_upper_bound,
            "degraded_stages": list(self.degraded_stages),
            "within_budget": self.within_budget,
            "memory_bytes": self.memory_bytes,
            "units": [
                decision.to_dict(
                    coverage_decimals=self.coverage_decimals,
                    area_decimals=self.area_decimals,
                )
                for decision in self.decisions
            ],
        }
        if include_timings:
            payload["total_elapsed_ms"] = self.total_elapsed_ms
            payload["stages"] = [stage.to_dict() for stage in self.stages]
        return payload

    def to_json(self, *, include_timings: bool = False) -> str:
        return json.dumps(
            self.canonical_dict(include_timings=include_timings),
            sort_keys=True,
            separators=(",", ":"),
        )


@dataclass(frozen=True, slots=True)
class OccluderInfo:
    unit_id: int
    name: str
    nearest_depth: float
    overlap_area: float

    def to_dict(self, *, decimals: int = 9) -> dict[str, object]:
        return {
            "unit_id": self.unit_id,
            "name": self.name,
            "nearest_depth": round(self.nearest_depth, decimals),
            "overlap_area": round(self.overlap_area, decimals),
        }


@dataclass(frozen=True, slots=True)
class HoverCandidate:
    unit_id: int
    name: str
    nearest_depth: float
    farthest_depth: float
    boundary_distance: float
    point_inside: bool
    boundary_touch: bool

    def to_dict(self, *, decimals: int = 9) -> dict[str, object]:
        return {
            "unit_id": self.unit_id,
            "name": self.name,
            "nearest_depth": round(self.nearest_depth, decimals),
            "farthest_depth": round(self.farthest_depth, decimals),
            "boundary_distance": round(self.boundary_distance, decimals),
            "point_inside": self.point_inside,
            "boundary_touch": self.boundary_touch,
        }


@dataclass(frozen=True, slots=True)
class HoverResult:
    scene_version: int
    scene_hash: str
    camera_hash: str
    point: tuple[float, float]
    target_unit_id: int | None
    nearest_visible_unit_id: int | None
    nearest_visible_face_depth: float | None
    visible_screen_area: float
    visible_area_error_bound: float
    target_visible_screen_area: float
    target_projected_area: float
    candidates: tuple[HoverCandidate, ...]
    occluders: tuple[OccluderInfo, ...]
    bucket_columns: int
    bucket_rows: int
    total_elapsed_ms: float
    budget_ms: float
    within_budget: bool
    depth_epsilon: float
    boundary_epsilon_px: float
    area_samples: int
    area_decimals: int = field(default=9, repr=False)

    def canonical_dict(self, *, include_timings: bool = False) -> dict[str, object]:
        payload: dict[str, object] = {
            "kind": "hover",
            "scene_version": self.scene_version,
            "scene_hash": self.scene_hash,
            "camera_hash": self.camera_hash,
            "point": [round(value, self.area_decimals) for value in self.point],
            "target_unit_id": self.target_unit_id,
            "nearest_visible_unit_id": self.nearest_visible_unit_id,
            "nearest_visible_face_depth": (
                None
                if self.nearest_visible_face_depth is None
                else round(self.nearest_visible_face_depth, self.area_decimals)
            ),
            "visible_screen_area": round(self.visible_screen_area, self.area_decimals),
            "visible_area_error_bound": round(self.visible_area_error_bound, self.area_decimals),
            "target_visible_screen_area": round(self.target_visible_screen_area, self.area_decimals),
            "target_projected_area": round(self.target_projected_area, self.area_decimals),
            "bucket_grid": [self.bucket_columns, self.bucket_rows],
            "depth_epsilon": self.depth_epsilon,
            "boundary_epsilon_px": self.boundary_epsilon_px,
            "area_samples": self.area_samples,
            "candidates": [candidate.to_dict(decimals=self.area_decimals) for candidate in self.candidates],
            "occluders": [occluder.to_dict(decimals=self.area_decimals) for occluder in self.occluders],
        }
        if include_timings:
            payload["total_elapsed_ms"] = self.total_elapsed_ms
        return payload

    def to_json(self, *, include_timings: bool = False) -> str:
        return json.dumps(
            self.canonical_dict(include_timings=include_timings),
            sort_keys=True,
            separators=(",", ":"),
        )


@dataclass(frozen=True, slots=True)
class _Plan:
    strategy: SelectionStrategy
    columns: int
    rows: int
    refinement_depth: int
    label: str


@dataclass(slots=True)
class _Execution:
    result: SelectionResult
    attempts: list[StageMetrics]


def _path_bbox_intersection(rect_a: tuple[float, float, float, float], rect_b: tuple[float, float, float, float]) -> float:
    xmin = max(rect_a[0], rect_b[0])
    ymin = max(rect_a[1], rect_b[1])
    xmax = min(rect_a[2], rect_b[2])
    ymax = min(rect_a[3], rect_b[3])
    if xmax <= xmin or ymax <= ymin:
        return 0.0
    return (xmax - xmin) * (ymax - ymin)


def _x_interval(polygon: Sequence[Sequence[float]], y: float) -> tuple[float, float] | None:
    intersections: list[float] = []
    for index, start in enumerate(polygon):
        end = polygon[(index + 1) % len(polygon)]
        y0 = float(start[1])
        y1 = float(end[1])
        if (y0 <= y < y1) or (y1 <= y < y0):
            t = (y - y0) / (y1 - y0)
            intersections.append(float(start[0]) + t * (float(end[0]) - float(start[0])))
    if len(intersections) < 2:
        return None
    intersections.sort()
    return intersections[0], intersections[-1]


def _estimate_visible_area(
    target: Sequence[Sequence[float]],
    occluders: Sequence[Sequence[Sequence[float]]],
    samples: int,
) -> tuple[float, float]:
    target_box = polygon_bbox(target)
    if target_box is None:
        return 0.0, 0.0
    if not occluders:
        return polygon_area(target), 0.0
    ymin, ymax = target_box[1], target_box[3]
    if ymax <= ymin:
        return 0.0, 0.0
    step = (ymax - ymin) / samples
    visible = 0.0
    for sample_index in range(samples):
        y = ymin + (sample_index + 0.5) * step
        target_interval = _x_interval(target, y)
        if target_interval is None:
            continue
        intervals: list[tuple[float, float]] = []
        for polygon in occluders:
            interval = _x_interval(polygon, y)
            if interval is None:
                continue
            left = max(target_interval[0], interval[0])
            right = min(target_interval[1], interval[1])
            if right > left:
                intervals.append((left, right))
        if not intervals:
            visible += target_interval[1] - target_interval[0]
            continue
        intervals.sort()
        covered = 0.0
        current_left, current_right = intervals[0]
        for left, right in intervals[1:]:
            if left <= current_right:
                current_right = max(current_right, right)
            else:
                covered += current_right - current_left
                current_left, current_right = left, right
        covered += current_right - current_left
        visible += max(0.0, target_interval[1] - target_interval[0] - covered)
    error_bound = (target_box[2] - target_box[0]) * step
    return max(0.0, visible), max(0.0, error_bound)


class QueryEngine:
    """Queries against one immutable scene and camera.

    Index construction and QueryEngine construction never mutate the scene.
    Canonically sorted outputs make results independent of candidate insertion
    order.
    """

    def __init__(
        self,
        scene: Scene,
        camera: Camera,
        *,
        projection_config: ProjectionConfig | None = None,
        budget_config: BudgetConfig | None = None,
        hover_config: HoverConfig | None = None,
    ) -> None:
        self.scene = scene
        self.camera = camera
        self.projection_config = projection_config or ProjectionConfig()
        self.budget_config = budget_config or BudgetConfig()
        self.hover_config = hover_config or HoverConfig()
        prepare_started = time.perf_counter_ns()
        self.projections = project_scene(scene.units, camera, config=self.projection_config)
        self.prepare_elapsed_ms = (time.perf_counter_ns() - prepare_started) / 1_000_000.0
        self.projection_by_id = {projection.unit_id: projection for projection in self.projections}
        self._index_lock = threading.Lock()
        self._indices: dict[tuple[int, int], ScreenBucketIndex] = {}
        self._index_timings: dict[tuple[int, int], float] = {}

    @property
    def scene_version(self) -> int:
        return self.scene.version

    @property
    def scene_hash(self) -> str:
        return self.scene.content_hash

    def _get_index(self, columns: int, rows: int) -> tuple[ScreenBucketIndex, float, bool]:
        key = (columns, rows)
        with self._index_lock:
            existing = self._indices.get(key)
            if existing is not None:
                return existing, 0.0, True
        started = time.perf_counter_ns()
        index = ScreenBucketIndex.build(
            self.projections,
            width=self.camera.width,
            height=self.camera.height,
            config=BucketConfig(columns=columns, rows=rows),
        )
        elapsed_ms = (time.perf_counter_ns() - started) / 1_000_000.0
        with self._index_lock:
            existing = self._indices.setdefault(key, index)
            self._index_timings[key] = elapsed_ms
            return existing, elapsed_ms, existing is not index

    def _plans(self) -> tuple[_Plan, ...]:
        plans: list[_Plan] = []
        grids = tuple(sorted(set(self.budget_config.bucket_grids), reverse=True))
        for columns in grids:
            plans.append(
                _Plan(
                    SelectionStrategy.POLYGON_REFINE,
                    columns,
                    columns,
                    1,
                    f"polygon_refine/{columns}x{columns}",
                )
            )
        for columns in grids:
            plans.append(
                _Plan(
                    SelectionStrategy.FINE_BUCKET_BBOX,
                    columns,
                    columns,
                    0,
                    f"fine_bucket_bbox/{columns}x{columns}",
                )
            )
        plans.append(
            _Plan(
                SelectionStrategy.COARSE_FULL_BBOX,
                1,
                1,
                0,
                "coarse_full_bbox",
            )
        )
        return tuple(plans)

    def _estimate_index_build_ms(self, cells: int) -> float:
        # Deliberately conservative.  The two terms cover Python bucket-list
        # construction and unit registration respectively.
        return 0.0015 * self.scene.unit_count + 0.0007 * cells

    def _estimate_memory(self, *, index_cells: int | None) -> int:
        eligible_count = sum(1 for projection in self.projections if projection.eligible_for_selection)
        index_bytes = 0
        if index_cells is not None:
            index_bytes = self.scene.unit_count * 16 + index_cells * 64
        return (
            index_bytes
            + self.scene.unit_count * 192
            + eligible_count * 96
            + self.scene.unit_count * 128
        )

    def _choose_plan(
        self,
        rect: tuple[float, float, float, float],
    ) -> tuple[list[_Plan], tuple[_Plan, float, int], tuple[str, ...]]:
        if self.budget_config.force_strategy is not None:
            strategy = SelectionStrategy(self.budget_config.force_strategy)
            columns = self.budget_config.force_bucket_columns or 1
            if strategy is SelectionStrategy.COARSE_FULL_BBOX:
                columns = 1
            plan = _Plan(
                strategy=strategy,
                columns=columns,
                rows=columns,
                refinement_depth=1 if strategy is SelectionStrategy.POLYGON_REFINE else 0,
                label=f"forced/{strategy.value}/{columns}x{columns}",
            )
            return [plan], (plan, 0.0, 0), ()
        eligible_count = sum(1 for projection in self.projections if projection.eligible_for_selection)
        plans = list(self._plans())
        base_ms = self.budget_config.base_cost_us / 1000.0
        decision_ms = (
            self.scene.unit_count
            * self.budget_config.decision_cost_us_per_unit
            / 1000.0
        )
        full_estimate_ms = (
            base_ms
            + eligible_count * self.budget_config.bbox_cost_us_per_candidate / 1000.0
            + decision_ms
        )
        full_memory = self._estimate_memory(index_cells=None)
        fit_limit = self.budget_config.budget_ms * 0.95
        selected: _Plan | None = None
        selected_estimate = float("inf")
        selected_memory = 0
        cheapest_estimate = full_estimate_ms
        cheapest_memory = full_memory
        cheapest_plan = plans[-1].label
        if self.budget_config.budget_ms > 0.0:
            for strategy in (SelectionStrategy.POLYGON_REFINE, SelectionStrategy.FINE_BUCKET_BBOX):
                for columns in sorted(set(self.budget_config.bucket_grids), reverse=True):
                    plan = next(
                        item
                        for item in plans
                        if item.strategy is strategy and item.columns == columns
                    )
                    cells = columns * columns
                    build_ms = self._estimate_index_build_ms(cells)
                    per_candidate = (
                        self.budget_config.polygon_cost_us_per_candidate
                        if strategy is SelectionStrategy.POLYGON_REFINE
                        else self.budget_config.bbox_cost_us_per_candidate
                    )
                    estimate_ms = (
                        build_ms
                        + base_ms
                        + eligible_count * per_candidate / 1000.0
                        + decision_ms
                    )
                    memory_bytes = self._estimate_memory(index_cells=cells)
                    if estimate_ms < cheapest_estimate:
                        cheapest_estimate = estimate_ms
                        cheapest_memory = memory_bytes
                        cheapest_plan = plan.label
                    if (
                        estimate_ms <= fit_limit
                        and memory_bytes <= self.budget_config.memory_budget_bytes
                    ):
                        selected = plan
                        selected_estimate = estimate_ms
                        selected_memory = memory_bytes
                        break
                if selected is not None:
                    break
            if (
                selected is None
                and full_estimate_ms <= fit_limit
                and full_memory <= self.budget_config.memory_budget_bytes
            ):
                selected = plans[-1]
                selected_estimate = full_estimate_ms
                selected_memory = full_memory
        if selected is None:
            raise TacticalError(
                ErrorCode.BUDGET_EXCEEDED,
                "no selection plan fits the declared time and memory budget",
                {
                    "budget_ms": self.budget_config.budget_ms,
                    "memory_budget_bytes": self.budget_config.memory_budget_bytes,
                    "cheapest_plan": cheapest_plan,
                    "cheapest_estimated_ms": cheapest_estimate,
                    "cheapest_memory_bytes": cheapest_memory,
                    "eligible_units": eligible_count,
                },
            )
        start_index = plans.index(selected)
        skipped = tuple(plan.label for plan in plans[:start_index])
        return [selected], (selected, selected_estimate, selected_memory), skipped

    def _execute_plan(
        self,
        plan: _Plan,
        rect: tuple[float, float, float, float],
        *,
        empty_rect: bool,
        started_ns: int,
        degraded_stages: tuple[str, ...],
    ) -> SelectionResult:
        threshold = self.budget_config.selection_threshold
        coverage_decimals = self.budget_config.coverage_decimals
        area_decimals = self.budget_config.area_decimals
        stage_metrics: list[StageMetrics] = []
        eligible = [projection for projection in self.projections if projection.eligible_for_selection]
        eligible_ids = {projection.unit_id for projection in eligible}
        stage_metrics.append(
            StageMetrics("projection_prepare", len(eligible), self.prepare_elapsed_ms)
        )
        stage_metrics.append(
            StageMetrics(
                "plan_selection",
                1,
                max(0.0, (time.perf_counter_ns() - started_ns) / 1_000_000.0),
            )
        )

        if empty_rect:
            candidate_ids: tuple[int, ...] = ()
            bucket_count = 0
            bucket_ms = 0.0
            memory_bytes = 0
        elif plan.strategy is SelectionStrategy.COARSE_FULL_BBOX:
            candidate_ids = tuple(projection.unit_id for projection in eligible)
            bucket_count = len(candidate_ids)
            bucket_ms = 0.0
            memory_bytes = 0
        else:
            index, build_ms, _ = self._get_index(plan.columns, plan.rows)
            stage_metrics.append(StageMetrics("index_prepare", len(eligible), build_ms))
            started = time.perf_counter_ns()
            candidate_ids = index.query_rect(rect)
            bucket_ms = (time.perf_counter_ns() - started) / 1_000_000.0
            bucket_count = len(candidate_ids)
            memory_bytes = index.memory_bytes
        stage_metrics.append(StageMetrics("bucket_candidates", bucket_count, bucket_ms))

        bbox_started = time.perf_counter_ns()
        bbox_candidates: list[UnitProjection] = []
        for unit_id in candidate_ids:
            projection = self.projection_by_id.get(unit_id)
            if projection is None or not projection.eligible_for_selection or projection.bbox is None:
                continue
            if rect_intersects(projection.bbox, rect):
                bbox_candidates.append(projection)
        bbox_ms = (time.perf_counter_ns() - bbox_started) / 1_000_000.0
        stage_metrics.append(StageMetrics("bbox_prefilter", len(bbox_candidates), bbox_ms))

        coverage_values: dict[int, tuple[float, float, float, DecisionReason, bool]] = {}
        polygon_count = 0
        if plan.refinement_depth == 1:
            polygon_started = time.perf_counter_ns()
            for projection in bbox_candidates:
                assert projection.bbox is not None
                clipped = clip_polygon_to_rect(projection.polygon, rect)
                intersection_area = polygon_area(clipped)
                if intersection_area <= AREA_EPS or projection.projected_area <= AREA_EPS:
                    coverage_values[projection.unit_id] = (
                        0.0,
                        0.0,
                        projection.projected_area,
                        DecisionReason.POLYGON_NO_OVERLAP,
                        False,
                    )
                    continue
                coverage = min(1.0, intersection_area / projection.projected_area)
                selected = coverage > 0.0 and coverage >= threshold
                reason = DecisionReason.INCLUDED if selected else DecisionReason.BELOW_THRESHOLD
                coverage_values[projection.unit_id] = (
                    coverage,
                    intersection_area,
                    projection.projected_area,
                    reason,
                    selected,
                )
            polygon_count = sum(
                1
                for value in coverage_values.values()
                if value[0] > 0.0
            )
            polygon_ms = (time.perf_counter_ns() - polygon_started) / 1_000_000.0
            stage_metrics.append(StageMetrics("polygon_refinement", polygon_count, polygon_ms))
        else:
            bbox_started = time.perf_counter_ns()
            for projection in bbox_candidates:
                assert projection.bbox is not None
                bbox = projection.bbox
                approximate_projected_area = bbox_area(bbox)
                intersection_area = _path_bbox_intersection(rect, bbox)
                if approximate_projected_area <= AREA_EPS or intersection_area <= AREA_EPS:
                    coverage_values[projection.unit_id] = (
                        0.0,
                        0.0,
                        approximate_projected_area,
                        DecisionReason.BBOX_NO_OVERLAP,
                        False,
                    )
                    continue
                coverage = min(1.0, intersection_area / approximate_projected_area)
                selected = coverage > 0.0 and coverage >= threshold
                reason = DecisionReason.INCLUDED if selected else DecisionReason.BELOW_THRESHOLD
                coverage_values[projection.unit_id] = (
                    coverage,
                    intersection_area,
                    approximate_projected_area,
                    reason,
                    selected,
                )
            polygon_count = 0
            bbox_coverage_ms = (time.perf_counter_ns() - bbox_started) / 1_000_000.0
            stage_metrics.append(StageMetrics("bbox_coverage", len(bbox_candidates), bbox_coverage_ms))

        decisions: list[UnitDecision] = []
        for projection in self.projections:
            if projection.reason is ProjectionReason.HIDDEN:
                decisions.append(
                    UnitDecision(
                        projection.unit_id,
                        projection.name,
                        DecisionReason.HIDDEN,
                        False,
                        0.0,
                        0.0,
                        0.0,
                        math.inf,
                    )
                )
                continue
            if projection.reason is ProjectionReason.CAMERA_BEHIND:
                decisions.append(
                    UnitDecision(
                        projection.unit_id,
                        projection.name,
                        DecisionReason.CAMERA_BEHIND,
                        False,
                        0.0,
                        0.0,
                        0.0,
                        math.inf,
                    )
                )
                continue
            if projection.reason is ProjectionReason.OUTSIDE_FRUSTUM:
                decisions.append(
                    UnitDecision(
                        projection.unit_id,
                        projection.name,
                        DecisionReason.OUTSIDE_FRUSTUM,
                        False,
                        0.0,
                        0.0,
                        0.0,
                        math.inf,
                    )
                )
                continue
            if empty_rect:
                decisions.append(
                    UnitDecision(
                        projection.unit_id,
                        projection.name,
                        DecisionReason.EMPTY_QUERY_RECT,
                        False,
                        0.0,
                        0.0,
                        projection.projected_area,
                        projection.nearest_depth,
                    )
                )
                continue
            value = coverage_values.get(projection.unit_id)
            if value is not None:
                coverage, intersection_area, projected_area, reason, selected = value
                decisions.append(
                    UnitDecision(
                        projection.unit_id,
                        projection.name,
                        reason,
                        selected,
                        coverage,
                        intersection_area,
                        projected_area,
                        projection.nearest_depth,
                    )
                )
                continue
            if projection.bbox is not None and rect_intersects(projection.bbox, rect):
                reason = DecisionReason.NOT_IN_SELECTION_BUCKETS
            else:
                reason = DecisionReason.BBOX_NO_OVERLAP
            decisions.append(
                UnitDecision(
                    projection.unit_id,
                    projection.name,
                    reason,
                    False,
                    0.0,
                    0.0,
                    projection.projected_area,
                    projection.nearest_depth,
                )
            )

        selected_ids = tuple(decision.unit_id for decision in decisions if decision.selected)
        stage_counts = {
            "scene_units": self.scene.unit_count,
            "projected_eligible": len(eligible_ids),
            "bucket_candidates": bucket_count,
            "bbox_candidates": len(bbox_candidates),
            "polygon_candidates": polygon_count,
            "selected": len(selected_ids),
        }
        elapsed_ms = (time.perf_counter_ns() - started_ns) / 1_000_000.0
        within_budget = elapsed_ms <= self.budget_config.budget_ms
        if plan.refinement_depth == 1:
            error_bound = 0.0
        else:
            error_bound = 1.0
        total_memory = (
            memory_bytes
            + self.scene.unit_count * 192
            + len(eligible_ids) * 96
            + self.scene.unit_count * 128
        )
        if total_memory > self.budget_config.memory_budget_bytes:
            raise TacticalError(
                ErrorCode.BUDGET_EXCEEDED,
                "selection exceeded the declared memory quota",
                {
                    "memory_budget_bytes": self.budget_config.memory_budget_bytes,
                    "actual_memory_bytes": total_memory,
                    "strategy": plan.label,
                },
            )
        return SelectionResult(
            scene_version=self.scene.version,
            scene_hash=self.scene.content_hash,
            camera_hash=self.camera.key(),
            rect=rect,
            threshold=threshold,
            strategy=plan.strategy,
            bucket_columns=plan.columns,
            bucket_rows=plan.rows,
            refinement_depth=plan.refinement_depth,
            selected=selected_ids,
            decisions=tuple(decisions),
            stage_counts=stage_counts,
            stages=tuple(stage_metrics),
            total_elapsed_ms=elapsed_ms,
            budget_ms=self.budget_config.budget_ms,
            within_budget=within_budget,
            memory_bytes=total_memory,
            memory_budget_bytes=self.budget_config.memory_budget_bytes,
            coverage_error_upper_bound=error_bound,
            degraded_stages=degraded_stages,
            coverage_decimals=coverage_decimals,
            area_decimals=area_decimals,
        )

    def select_rect(
        self,
        x0: float,
        y0: float,
        x1: float,
        y1: float,
        *,
        expected_version: int | None = None,
        expected_hash: str | None = None,
    ) -> SelectionResult:
        if expected_version is not None and int(expected_version) != self.scene.version:
            raise TacticalError(
                ErrorCode.SCENE_STALE,
                "selection scene version does not match the published version",
                {"expected_version": int(expected_version), "actual_version": self.scene.version},
            )
        if expected_hash is not None and expected_hash != self.scene.content_hash:
            raise TacticalError(
                ErrorCode.SCENE_STALE,
                "selection scene hash does not match the published hash",
                {"expected_hash": expected_hash, "actual_hash": self.scene.content_hash},
            )
        started_ns = time.perf_counter_ns()
        normalized = normalize_rect(float(x0), float(y0), float(x1), float(y1))
        viewport_rect = clip_rect_to_viewport(normalized, self.camera.width, self.camera.height)
        empty_rect = viewport_rect is None
        effective_rect = viewport_rect or (0.0, 0.0, 0.0, 0.0)
        plans, _selected_plan, predicted_degraded = self._choose_plan(effective_rect)
        result = self._execute_plan(
            plans[0],
            effective_rect,
            empty_rect=empty_rect,
            started_ns=started_ns,
            degraded_stages=predicted_degraded,
        )
        if result.within_budget or not self.budget_config.actual_degradation:
            return result
        raise TacticalError(
            ErrorCode.BUDGET_EXCEEDED,
            "measured selection time exceeded the declared budget after degradation",
            {
                "budget_ms": self.budget_config.budget_ms,
                "elapsed_ms": result.total_elapsed_ms,
                "degraded_stages": list(result.degraded_stages),
                "stage_candidate_counts": dict(sorted(result.stage_counts.items())),
                "strategy": result.strategy.value,
                "bucket_grid": [result.bucket_columns, result.bucket_rows],
            },
        )

    def _hover_index(self) -> tuple[ScreenBucketIndex, float]:
        if self.budget_config.force_bucket_columns is not None:
            index, build_ms, _reused = self._get_index(
                self.budget_config.force_bucket_columns,
                self.budget_config.force_bucket_columns,
            )
            return index, build_ms
        if self.budget_config.budget_ms <= 0.0:
            raise TacticalError(
                ErrorCode.BUDGET_EXCEEDED,
                "hover has no positive time budget",
                {"budget_ms": self.budget_config.budget_ms},
            )
        grids = sorted(set(self.budget_config.bucket_grids), reverse=True)
        chosen = grids[-1]
        for columns in grids:
            cells = columns * columns
            estimated_ms = (
                self._estimate_index_build_ms(cells)
                + self.budget_config.base_cost_us / 1000.0
            )
            estimated_memory = (
                self.scene.unit_count * 16
                + cells * 64
                + self.scene.unit_count * 192
                + self.scene.unit_count * 128
            )
            if (
                estimated_ms <= self.budget_config.budget_ms * 0.95
                and estimated_memory <= self.budget_config.memory_budget_bytes
            ):
                chosen = columns
                break
        index, build_ms, _reused = self._get_index(chosen, chosen)
        actual_memory = (
            index.memory_bytes
            + self.scene.unit_count * 192
            + self.scene.unit_count * 128
        )
        if actual_memory > self.budget_config.memory_budget_bytes:
            raise TacticalError(
                ErrorCode.BUDGET_EXCEEDED,
                "hover index exceeded the declared memory quota",
                {
                    "memory_budget_bytes": self.budget_config.memory_budget_bytes,
                    "actual_memory_bytes": actual_memory,
                },
            )
        return index, build_ms

    def hover(
        self,
        x: float,
        y: float,
        *,
        target_unit_id: int | None = None,
        expected_version: int | None = None,
        expected_hash: str | None = None,
    ) -> HoverResult:
        if expected_version is not None and int(expected_version) != self.scene.version:
            raise TacticalError(
                ErrorCode.SCENE_STALE,
                "hover scene version does not match the published version",
                {"expected_version": int(expected_version), "actual_version": self.scene.version},
            )
        if expected_hash is not None and expected_hash != self.scene.content_hash:
            raise TacticalError(
                ErrorCode.SCENE_STALE,
                "hover scene hash does not match the published hash",
                {"expected_hash": expected_hash, "actual_hash": self.scene.content_hash},
            )
        if target_unit_id is not None and target_unit_id not in self.projection_by_id:
            raise TacticalError(
                ErrorCode.INVALID_ARGUMENT,
                "hover target unit is not in this scene",
                {"unit_id": target_unit_id},
            )
        started_ns = time.perf_counter_ns()
        point = (float(x), float(y))
        if not (0.0 <= point[0] <= self.camera.width and 0.0 <= point[1] <= self.camera.height):
            raise TacticalError(
                ErrorCode.INVALID_ARGUMENT,
                "hover point lies outside the viewport",
                {"point": list(point), "viewport": [self.camera.width, self.camera.height]},
            )
        index, _build_ms = self._hover_index()
        candidate_ids = index.query_point(point[0], point[1], neighbors=1)
        candidates: list[tuple[UnitProjection, float, bool, bool]] = []
        for unit_id in candidate_ids:
            projection = self.projection_by_id.get(unit_id)
            if projection is None or not projection.eligible_for_selection or projection.bbox is None:
                continue
            if not (
                projection.bbox[0] - self.hover_config.boundary_epsilon_px
                <= point[0]
                <= projection.bbox[2] + self.hover_config.boundary_epsilon_px
                and projection.bbox[1] - self.hover_config.boundary_epsilon_px
                <= point[1]
                <= projection.bbox[3] + self.hover_config.boundary_epsilon_px
            ):
                continue
            inside = point_in_convex_polygon(
                point,
                projection.polygon,
                eps=self.hover_config.boundary_epsilon_px,
            )
            boundary_distance = distance_to_polygon_boundary(point, projection.polygon)
            boundary_touch = boundary_distance <= self.hover_config.boundary_epsilon_px
            if not inside and not boundary_touch:
                continue
            candidates.append((projection, boundary_distance, inside, boundary_touch))

        candidates.sort(key=lambda item: (item[0].nearest_depth, item[0].unit_id))
        hover_candidates = tuple(
            HoverCandidate(
                unit_id=projection.unit_id,
                name=projection.name,
                nearest_depth=projection.nearest_depth,
                farthest_depth=projection.farthest_depth,
                boundary_distance=boundary_distance,
                point_inside=inside,
                boundary_touch=boundary_touch,
            )
            for projection, boundary_distance, inside, boundary_touch in candidates
        )
        self._raise_if_ambiguous(candidates, point, target_unit_id)

        if not candidates:
            elapsed_ms = (time.perf_counter_ns() - started_ns) / 1_000_000.0
            result = HoverResult(
                scene_version=self.scene.version,
                scene_hash=self.scene.content_hash,
                camera_hash=self.camera.key(),
                point=point,
                target_unit_id=target_unit_id,
                nearest_visible_unit_id=None,
                nearest_visible_face_depth=None,
                visible_screen_area=0.0,
                visible_area_error_bound=0.0,
                target_visible_screen_area=0.0,
                target_projected_area=0.0,
                candidates=(),
                occluders=(),
                bucket_columns=index.columns,
                bucket_rows=index.rows,
                total_elapsed_ms=elapsed_ms,
                budget_ms=self.budget_config.budget_ms,
                within_budget=elapsed_ms <= self.budget_config.budget_ms,
                depth_epsilon=self.hover_config.depth_epsilon,
                boundary_epsilon_px=self.hover_config.boundary_epsilon_px,
                area_samples=self.hover_config.area_samples,
                area_decimals=self.budget_config.area_decimals,
            )
            if not result.within_budget:
                raise TacticalError(
                    ErrorCode.BUDGET_EXCEEDED,
                    "hover query exceeded the measured time budget",
                    {"budget_ms": self.budget_config.budget_ms, "elapsed_ms": elapsed_ms},
                )
            return result

        nearest_projection = candidates[0][0]
        target_projection = (
            self.projection_by_id[target_unit_id]
            if target_unit_id is not None
            else nearest_projection
        )
        candidate_unit_ids = {projection.unit_id for projection, *_rest in candidates}
        if target_unit_id is not None and target_unit_id not in candidate_unit_ids:
            raise TacticalError(
                ErrorCode.INVALID_ARGUMENT,
                "hover target does not cover the hovered screen point",
                {
                    "unit_id": target_unit_id,
                    "point": [point[0], point[1]],
                    "point_candidates": sorted(candidate_unit_ids),
                },
            )
        occluder_projections: list[UnitProjection] = []
        if target_projection.bbox is None:
            occlusion_candidate_ids: tuple[int, ...] = ()
        else:
            occlusion_candidate_ids = index.query_rect(target_projection.bbox)
        for occluder_id in occlusion_candidate_ids:
            projection = self.projection_by_id.get(occluder_id)
            if projection is None or not projection.eligible_for_selection:
                continue
            if projection.unit_id == target_projection.unit_id:
                continue
            if projection.nearest_depth >= target_projection.nearest_depth - self.hover_config.depth_epsilon:
                continue
            overlap = convex_polygon_intersection(target_projection.polygon, projection.polygon)
            if not overlap:
                continue
            occluder_projections.append(projection)
        visible_target_area, area_error = _estimate_visible_area(
            target_projection.polygon,
            [projection.polygon for projection in occluder_projections],
            self.hover_config.area_samples,
        )
        occluders = tuple(
            OccluderInfo(
                unit_id=projection.unit_id,
                name=projection.name,
                nearest_depth=projection.nearest_depth,
                overlap_area=polygon_area(
                    convex_polygon_intersection(target_projection.polygon, projection.polygon)
                ),
            )
            for projection in sorted(
                occluder_projections,
                key=lambda item: (item.nearest_depth, item.unit_id),
            )
        )
        nearest_visible_area = (
            nearest_projection.projected_area
            if target_projection.unit_id == nearest_projection.unit_id
            else nearest_projection.projected_area
        )
        elapsed_ms = (time.perf_counter_ns() - started_ns) / 1_000_000.0
        result = HoverResult(
            scene_version=self.scene.version,
            scene_hash=self.scene.content_hash,
            camera_hash=self.camera.key(),
            point=point,
            target_unit_id=target_unit_id,
            nearest_visible_unit_id=nearest_projection.unit_id,
            nearest_visible_face_depth=nearest_projection.nearest_depth,
            visible_screen_area=nearest_visible_area,
            visible_area_error_bound=area_error,
            target_visible_screen_area=visible_target_area,
            target_projected_area=target_projection.projected_area,
            candidates=hover_candidates,
            occluders=occluders,
            bucket_columns=index.columns,
            bucket_rows=index.rows,
            total_elapsed_ms=elapsed_ms,
            budget_ms=self.budget_config.budget_ms,
            within_budget=elapsed_ms <= self.budget_config.budget_ms,
            depth_epsilon=self.hover_config.depth_epsilon,
            boundary_epsilon_px=self.hover_config.boundary_epsilon_px,
            area_samples=self.hover_config.area_samples,
            area_decimals=self.budget_config.area_decimals,
        )
        if not result.within_budget:
            raise TacticalError(
                ErrorCode.BUDGET_EXCEEDED,
                "hover query exceeded the measured time budget",
                {
                    "budget_ms": self.budget_config.budget_ms,
                    "elapsed_ms": elapsed_ms,
                    "candidate_count": len(candidates),
                },
            )
        return result

    def _raise_if_ambiguous(
        self,
        candidates: Sequence[tuple[UnitProjection, float, bool, bool]],
        point: tuple[float, float],
        target_unit_id: int | None,
    ) -> None:
        if len(candidates) < 2:
            return
        top = candidates[0][0]
        second = candidates[1][0]
        delta = abs(second.nearest_depth - top.nearest_depth)
        if delta <= self.hover_config.depth_epsilon:
            raise TacticalError(
                ErrorCode.OCCLUSION_AMBIGUOUS,
                "hover candidates cannot be depth-ordered at the declared precision",
                {
                    "point": [point[0], point[1]],
                    "depth_delta": delta,
                    "depth_epsilon": self.hover_config.depth_epsilon,
                    "candidates": [
                        {
                            "unit_id": top.unit_id,
                            "name": top.name,
                            "nearest_depth": top.nearest_depth,
                            "farthest_depth": top.farthest_depth,
                        },
                        {
                            "unit_id": second.unit_id,
                            "name": second.name,
                            "nearest_depth": second.nearest_depth,
                            "farthest_depth": second.farthest_depth,
                        },
                    ],
                },
            )
        if target_unit_id is not None:
            target_index = next(
                (
                    index
                    for index, (projection, _distance, _inside, _touch) in enumerate(candidates)
                    if projection.unit_id == target_unit_id
                ),
                None,
            )
            if target_index is not None and target_index > 0:
                front = candidates[target_index - 1][0]
                target = candidates[target_index][0]
                target_delta = abs(target.nearest_depth - front.nearest_depth)
                if target_delta <= self.hover_config.depth_epsilon:
                    raise TacticalError(
                        ErrorCode.OCCLUSION_AMBIGUOUS,
                        "hover target and the unit immediately in front are within depth precision",
                        {
                            "point": [point[0], point[1]],
                            "depth_delta": target_delta,
                            "depth_epsilon": self.hover_config.depth_epsilon,
                            "candidates": [
                                {
                                    "unit_id": front.unit_id,
                                    "name": front.name,
                                    "nearest_depth": front.nearest_depth,
                                    "farthest_depth": front.farthest_depth,
                                },
                                {
                                    "unit_id": target.unit_id,
                                    "name": target.name,
                                    "nearest_depth": target.nearest_depth,
                                    "farthest_depth": target.farthest_depth,
                                },
                            ],
                        },
                    )
