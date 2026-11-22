"""Distance annotations with dual (world / voxel) readouts and accuracy bounds.

* Endpoints are stored in world coordinates (mm).  Voxel coordinates are
  derived per axis as ``index = world / spacing`` and both readouts are
  always returned together so they can be cross-checked.
* Endpoint placement snaps in-plane coordinates to the sampling grid:
  order 0 snaps to voxel centres (quantum = spacing), order 1 to the
  half-voxel grid (quantum = spacing / 2).  The plane-normal coordinate is
  taken from the plane position itself.
* The accuracy bound is derived *only* from the observed spacing, the
  interpolation order and the endpoint grid positions -- never from the
  ground truth: each endpoint carries a snap error of at most
  ``sqrt(3) * quantum / 2``, so the length error of a two-point annotation
  is bounded by ``sqrt(3) * quantum`` with
  ``quantum = max_spacing * (1 if order == 0 else 0.5)``.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import List, Optional, Sequence

from .errors import ErrorCode, VolViewError
from .model import WORLD_AXES, VolumeModel
from .resample import PLANE_INPLANE, PLANE_NORMAL

PLANE_MEMBERSHIP_TOL = 1e-6  # mm


def snap_quantum(model: VolumeModel, order: int) -> float:
    """Grid quantum (mm) used when snapping endpoints for ``order``."""
    max_spacing = max(model.manifest.spacing[a] for a in WORLD_AXES)
    return max_spacing if order == 0 else max_spacing / 2.0


def accuracy_bound(model: VolumeModel, order: int) -> float:
    """Upper bound of the length error of a two-point annotation (mm).

    Derived exclusively from the declared spacing and the interpolation
    order (through the snap quantum); the ground truth is never consulted.
    """
    return math.sqrt(3.0) * snap_quantum(model, order)


def _snap(value: float, quantum: float) -> float:
    return round(value / quantum) * quantum


@dataclass
class Annotation:
    id: int
    plane: str
    plane_position: float
    endpoints_world: List[List[float]]  # [[x,y,z], [x,y,z]]
    endpoints_voxel: List[List[float]]
    world_length: float
    voxel_length: float
    true_length: Optional[float] = None  # analytic truth, when checking accuracy

    def reading(self) -> dict:
        return {
            "id": self.id,
            "plane": self.plane,
            "endpoints_world": [list(p) for p in self.endpoints_world],
            "endpoints_voxel": [list(p) for p in self.endpoints_voxel],
            "world_length": self.world_length,
            "voxel_length": self.voxel_length,
        }

    def cross_check(self, model: VolumeModel, tol: float = 1e-9) -> bool:
        """Verify the voxel readout is consistent with the world readout."""
        delta_world = [self.endpoints_world[1][i] - self.endpoints_world[0][i]
                       for i in range(3)]
        spacings = [model.manifest.spacing[a] for a in WORLD_AXES]
        world_len = math.sqrt(sum(d * d for d in delta_world))
        voxel_len = math.sqrt(sum((d / s) ** 2 for d, s in zip(delta_world, spacings)))
        scale = max(1.0, abs(self.world_length), abs(self.voxel_length))
        return (abs(world_len - self.world_length) <= tol * scale
                and abs(voxel_len - self.voxel_length) <= tol * scale)


def _validate_endpoint(model: VolumeModel, point: Sequence[float],
                       endpoint_index: int, annotation_id: int) -> None:
    if len(point) != 3:
        raise VolViewError(ErrorCode.E_RANGE,
                           f"annotation[{annotation_id}].endpoint[{endpoint_index}]",
                           "endpoint must have 3 world coordinates",
                           annotation_id=annotation_id, endpoint=endpoint_index)
    for axis, value in zip(WORLD_AXES, point):
        v = float(value)
        lo, hi = 0.0, model.extent(axis)
        if not math.isfinite(v) or not (lo - 1e-9 <= v <= hi + 1e-9):
            raise VolViewError(
                ErrorCode.E_RANGE, f"annotation[{annotation_id}].endpoint[{endpoint_index}]",
                f"endpoint {endpoint_index} coordinate {value!r} on axis {axis} "
                f"outside volume range [{lo}, {hi}]",
                annotation_id=annotation_id, endpoint=endpoint_index,
                axis=axis, available=[lo, hi])


def make_annotation(model: VolumeModel, annotation_id: int, plane: str,
                    plane_position: float, p0: Sequence[float],
                    p1: Sequence[float], order: int,
                    true_length: Optional[float] = None) -> Annotation:
    """Validate, snap and measure a two-point annotation on ``plane``."""
    _validate_endpoint(model, p0, 0, annotation_id)
    _validate_endpoint(model, p1, 1, annotation_id)

    normal = PLANE_NORMAL[plane]
    normal_idx = WORLD_AXES.index(normal)
    for endpoint_index, point in enumerate((p0, p1)):
        if abs(float(point[normal_idx]) - plane_position) > PLANE_MEMBERSHIP_TOL:
            raise VolViewError(
                ErrorCode.E_RANGE, f"annotation[{annotation_id}].endpoint[{endpoint_index}]",
                f"endpoint {endpoint_index} does not lie in plane {plane} at "
                f"position {plane_position} (normal coordinate "
                f"{point[normal_idx]!r})",
                annotation_id=annotation_id, endpoint=endpoint_index,
                plane=plane)

    quantum = snap_quantum(model, order)
    snapped: List[List[float]] = []
    for point in (p0, p1):
        coords = [float(_snap(float(c), quantum)) for c in point]
        coords[normal_idx] = float(plane_position)  # in-plane by construction
        snapped.append(coords)

    spacings = [model.manifest.spacing[a] for a in WORLD_AXES]
    endpoints_voxel = [[c / s for c, s in zip(coords, spacings)] for coords in snapped]
    delta = [snapped[1][i] - snapped[0][i] for i in range(3)]
    world_length = math.sqrt(sum(d * d for d in delta))
    voxel_length = math.sqrt(sum((d / s) ** 2 for d, s in zip(delta, spacings)))

    annotation = Annotation(
        id=annotation_id, plane=plane, plane_position=float(plane_position),
        endpoints_world=snapped, endpoints_voxel=endpoints_voxel,
        world_length=world_length, voxel_length=voxel_length,
        true_length=true_length)

    if true_length is not None:
        deviation = abs(world_length - float(true_length))
        bound = accuracy_bound(model, order)
        if deviation > bound:
            raise VolViewError(
                ErrorCode.E_ACC, f"annotation[{annotation_id}]",
                f"measured {world_length:.6f} mm deviates {deviation:.6f} mm "
                f"from truth {float(true_length):.6f} mm, bound {bound:.6f} mm",
                annotation_id=annotation_id, measured=world_length,
                deviation=deviation, bound=bound)
    return annotation


def project_to_plane(model: VolumeModel, annotation: Annotation,
                     plane: str, plane_position: float) -> dict:
    """Re-intersect an annotation with another plane.

    World coordinates are preserved; the projection onto the target plane is
    returned as fractional pixel coordinates together with a flag telling
    whether the segment intersects the plane.
    """
    row_axis, col_axis = PLANE_INPLANE[plane]
    normal_idx = WORLD_AXES.index(PLANE_NORMAL[plane])
    row_idx, col_idx = WORLD_AXES.index(row_axis), WORLD_AXES.index(col_axis)

    def to_pixel(point: Sequence[float]) -> List[float]:
        return [
            point[row_idx] / model.manifest.spacing[row_axis],
            point[col_idx] / model.manifest.spacing[col_axis],
        ]

    p0, p1 = annotation.endpoints_world
    d0 = p0[normal_idx] - plane_position
    d1 = p1[normal_idx] - plane_position
    intersects = (d0 == 0.0) or (d1 == 0.0) or (d0 * d1 < 0.0)
    return {
        "annotation_id": annotation.id,
        "plane": plane,
        "pixels": [to_pixel(p0), to_pixel(p1)],
        "intersects": bool(intersects),
        "endpoints_world": [list(p0), list(p1)],
    }
