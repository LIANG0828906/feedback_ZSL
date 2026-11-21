"""Frustum classification and deterministic screen-space projection."""

from __future__ import annotations

import math
from dataclasses import dataclass
from enum import Enum
from typing import Sequence

import numpy as np

from .camera import Camera
from .geometry import (
    AREA_EPS,
    GEOMETRY_EPS,
    clip_polygon_to_rect,
    convex_hull,
    polygon_area,
    polygon_bbox,
    capsule_surface_samples,
)
from .scene import AABBShape, SceneUnit


class ProjectionReason(str, Enum):
    HIDDEN = "HIDDEN"
    CAMERA_BEHIND = "CAMERA_BEHIND"
    OUTSIDE_FRUSTUM = "OUTSIDE_FRUSTUM"
    VISIBLE_INSIDE = "VISIBLE_INSIDE"
    VISIBLE_CROSSES_FRUSTUM = "VISIBLE_CROSSES_FRUSTUM"


@dataclass(frozen=True, slots=True)
class ProjectionConfig:
    capsule_axis_samples: int = 3
    capsule_radial_samples: int = 10
    depth_quantum: float = 1e-9

    def __post_init__(self) -> None:
        if self.capsule_axis_samples < 2 or self.capsule_radial_samples < 6:
            raise ValueError("capsule tessellation is too coarse")


@dataclass(frozen=True, slots=True)
class UnitProjection:
    unit_id: int
    name: str
    reason: ProjectionReason
    polygon: tuple[tuple[float, float], ...]
    bbox: tuple[float, float, float, float] | None
    nearest_depth: float
    farthest_depth: float
    projected_area: float
    clipped: bool

    @property
    def eligible_for_selection(self) -> bool:
        return self.reason in (
            ProjectionReason.VISIBLE_INSIDE,
            ProjectionReason.VISIBLE_CROSSES_FRUSTUM,
        )

    @property
    def behind_camera(self) -> bool:
        return self.reason is ProjectionReason.CAMERA_BEHIND

    def canonical(self) -> dict[str, object]:
        return {
            "unit_id": self.unit_id,
            "name": self.name,
            "reason": self.reason.value,
            "polygon": [[round(x, 9), round(y, 9)] for x, y in self.polygon],
            "bbox": None if self.bbox is None else [round(v, 9) for v in self.bbox],
            "nearest_depth": round(self.nearest_depth, 9),
            "farthest_depth": round(self.farthest_depth, 9),
            "projected_area": round(self.projected_area, 9),
            "clipped": self.clipped,
        }


def _transform_points(matrix: np.ndarray, points: np.ndarray) -> np.ndarray:
    homogeneous = np.concatenate([points, np.ones((points.shape[0], 1))], axis=1)
    return (matrix @ homogeneous.T).T


def _visible_camera_points(
    camera_points: np.ndarray,
    edges: Sequence[tuple[int, int]],
    *,
    near: float,
    far: float,
) -> tuple[np.ndarray, bool, bool, bool]:
    """Clip a convex point/edge soup against the near and far planes.

    Returns points, whether the near plane cut the primitive, whether the far
    plane cut it, and whether any original point was behind the camera.
    """

    z = camera_points[:, 2]
    near_signed = -z - near
    far_signed = z + far
    inside = (near_signed >= -1e-12) & (far_signed >= -1e-12)
    any_behind = bool(np.any(near_signed < 0.0))
    near_cut = False
    far_cut = False
    output: list[np.ndarray] = [camera_points[index] for index in np.nonzero(inside)[0]]

    for start_index, end_index in edges:
        start_near = float(near_signed[start_index])
        end_near = float(near_signed[end_index])
        if start_near * end_near < 0.0:
            denominator = start_near - end_near
            if abs(denominator) > GEOMETRY_EPS:
                t = start_near / denominator
                t = max(0.0, min(1.0, t))
                output.append(
                    camera_points[start_index]
                    + t * (camera_points[end_index] - camera_points[start_index])
                )
                near_cut = True
        start_far = float(far_signed[start_index])
        end_far = float(far_signed[end_index])
        if start_far * end_far < 0.0:
            denominator = start_far - end_far
            if abs(denominator) > GEOMETRY_EPS:
                t = start_far / denominator
                t = max(0.0, min(1.0, t))
                output.append(
                    camera_points[start_index]
                    + t * (camera_points[end_index] - camera_points[start_index])
                )
                far_cut = True

    any_inside = bool(np.any(inside))
    if any_behind and any_inside:
        near_cut = True
    if bool(np.any(far_signed < 0.0)) and any_inside:
        far_cut = True
    if not output:
        return np.empty((0, 3), dtype=np.float64), near_cut, far_cut, any_behind
    clipped = np.asarray(output, dtype=np.float64)
    valid = (clipped[:, 2] <= -near + 1e-9) & (clipped[:, 2] >= -far - 1e-9)
    return clipped[valid], near_cut, far_cut, any_behind


def _to_screen(camera_points: np.ndarray, camera: Camera) -> np.ndarray:
    homogeneous = np.concatenate(
        [camera_points, np.ones((camera_points.shape[0], 1))],
        axis=1,
    )
    clip = (camera.projection_array @ homogeneous.T).T
    w = clip[:, 3]
    valid = np.isfinite(w) & (w > 1e-12)
    if not np.any(valid):
        return np.empty((0, 2), dtype=np.float64)
    clip = clip[valid]
    w = clip[:, 3]
    ndc_x = clip[:, 0] / w
    ndc_y = clip[:, 1] / w
    screen_x = (ndc_x * 0.5 + 0.5) * camera.width
    screen_y = (1.0 - (ndc_y * 0.5 + 0.5)) * camera.height
    valid_screen = np.isfinite(screen_x) & np.isfinite(screen_y)
    return np.column_stack([screen_x[valid_screen], screen_y[valid_screen]])


def _project_primitive(
    unit: SceneUnit,
    camera: Camera,
    config: ProjectionConfig,
) -> UnitProjection:
    if not unit.visible:
        return UnitProjection(
            unit_id=unit.unit_id,
            name=unit.name,
            reason=ProjectionReason.HIDDEN,
            polygon=(),
            bbox=None,
            nearest_depth=math.inf,
            farthest_depth=math.inf,
            projected_area=0.0,
            clipped=False,
        )

    if isinstance(unit.shape, AABBShape):
        local_points = unit.shape.corners
        edges = unit.shape.edges
    else:
        local_points, edges_list = capsule_surface_samples(
            unit.shape.a,
            unit.shape.b,
            unit.shape.radius,
            axis_samples=config.capsule_axis_samples,
            radial_samples=config.capsule_radial_samples,
        )
        edges = tuple(edges_list)

    world_h = _transform_points(unit.transform_array, local_points)
    camera_h = (camera.view_array @ world_h.T).T
    camera_points = camera_h[:, :3]
    if camera_points.shape[0] == 0:
        raise ValueError("primitive generated no samples")
    z = camera_points[:, 2]
    if bool(np.all(z > -camera.near + 1e-12)):
        return UnitProjection(
            unit_id=unit.unit_id,
            name=unit.name,
            reason=ProjectionReason.CAMERA_BEHIND,
            polygon=(),
            bbox=None,
            nearest_depth=math.inf,
            farthest_depth=math.inf,
            projected_area=0.0,
            clipped=False,
        )
    if bool(np.all(z < -camera.far - 1e-12)):
        return UnitProjection(
            unit_id=unit.unit_id,
            name=unit.name,
            reason=ProjectionReason.OUTSIDE_FRUSTUM,
            polygon=(),
            bbox=None,
            nearest_depth=math.inf,
            farthest_depth=math.inf,
            projected_area=0.0,
            clipped=False,
        )

    visible_points, near_cut, far_cut, any_behind = _visible_camera_points(
        camera_points,
        edges,
        near=camera.near,
        far=camera.far,
    )
    if visible_points.shape[0] < 3:
        reason = (
            ProjectionReason.CAMERA_BEHIND
            if any_behind and not near_cut
            else ProjectionReason.OUTSIDE_FRUSTUM
        )
        return UnitProjection(
            unit_id=unit.unit_id,
            name=unit.name,
            reason=reason,
            polygon=(),
            bbox=None,
            nearest_depth=math.inf,
            farthest_depth=math.inf,
            projected_area=0.0,
            clipped=near_cut or far_cut,
        )

    screen_points = _to_screen(visible_points, camera)
    if screen_points.shape[0] < 3:
        return UnitProjection(
            unit_id=unit.unit_id,
            name=unit.name,
            reason=ProjectionReason.OUTSIDE_FRUSTUM,
            polygon=(),
            bbox=None,
            nearest_depth=math.inf,
            farthest_depth=math.inf,
            projected_area=0.0,
            clipped=near_cut or far_cut,
        )

    hull = convex_hull(screen_points)
    if len(hull) < 3:
        return UnitProjection(
            unit_id=unit.unit_id,
            name=unit.name,
            reason=ProjectionReason.OUTSIDE_FRUSTUM,
            polygon=(),
            bbox=None,
            nearest_depth=math.inf,
            farthest_depth=math.inf,
            projected_area=0.0,
            clipped=near_cut or far_cut,
        )
    raw_area = polygon_area(hull)
    clipped_polygon = clip_polygon_to_rect(hull, (0.0, 0.0, float(camera.width), float(camera.height)))
    if not clipped_polygon or polygon_area(clipped_polygon) <= AREA_EPS:
        return UnitProjection(
            unit_id=unit.unit_id,
            name=unit.name,
            reason=ProjectionReason.OUTSIDE_FRUSTUM,
            polygon=(),
            bbox=None,
            nearest_depth=math.inf,
            farthest_depth=math.inf,
            projected_area=0.0,
            clipped=True,
        )
    clipped_area = polygon_area(clipped_polygon)
    side_cut = False
    for x, y in hull:
        if x < -1e-7 or x > camera.width + 1e-7 or y < -1e-7 or y > camera.height + 1e-7:
            side_cut = True
            break
    if not side_cut and raw_area > clipped_area + max(1e-7, raw_area * 1e-10):
        side_cut = True
    clipped = near_cut or far_cut or side_cut
    reason = (
        ProjectionReason.VISIBLE_CROSSES_FRUSTUM
        if clipped
        else ProjectionReason.VISIBLE_INSIDE
    )
    nearest_depth = max(0.0, float(-np.max(visible_points[:, 2])))
    farthest_depth = max(0.0, float(-np.min(visible_points[:, 2])))
    order = sorted((float(x), float(y)) for x, y in clipped_polygon)
    # Keep clipped polygon winding but canonicalize its starting vertex so
    # serialization is independent of clipping implementation details.
    start = order[0] if order else None
    if start is not None and start in [(float(x), float(y)) for x, y in clipped_polygon]:
        index = [(float(x), float(y)) for x, y in clipped_polygon].index(start)
        clipped_polygon = clipped_polygon[index:] + clipped_polygon[:index]
    polygon_tuple = tuple((float(x), float(y)) for x, y in clipped_polygon)
    return UnitProjection(
        unit_id=unit.unit_id,
        name=unit.name,
        reason=reason,
        polygon=polygon_tuple,
        bbox=polygon_bbox(polygon_tuple),
        nearest_depth=nearest_depth,
        farthest_depth=farthest_depth,
        projected_area=clipped_area,
        clipped=clipped,
    )


def project_scene(
    units: Sequence[SceneUnit],
    camera: Camera,
    *,
    config: ProjectionConfig | None = None,
) -> tuple[UnitProjection, ...]:
    effective = config or ProjectionConfig()
    projections = [_project_primitive(unit, camera, effective) for unit in units]
    projections.sort(key=lambda projection: projection.unit_id)
    return tuple(projections)
