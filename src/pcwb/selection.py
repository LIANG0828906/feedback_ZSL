"""Selection objects bound to one immutable cloud content hash."""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import SelectionInvalid
from .math3d import project_clip
from .model import Bounds, PointCloudSnapshot


def _normalized_float(value: Any, name: str) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise SelectionInvalid(f"{name} must be numeric") from exc
    if not np.isfinite(result):
        raise SelectionInvalid(f"{name} must be finite")
    return result


@dataclass(frozen=True, slots=True)
class Selection:
    """A sorted, unique index set tied to a source cloud hash.

    The selection stores no reference to mutable renderer buffers.  It can be
    discarded or replaced without touching the committed cloud snapshot.
    """

    version_hash: str
    indices: tuple[int, ...]
    bounds: Bounds | None
    source_count: int

    @property
    def count(self) -> int:
        return len(self.indices)

    @property
    def is_empty(self) -> bool:
        return not self.indices

    def index_array(self) -> np.ndarray:
        return np.asarray(self.indices, dtype=np.int64)

    def to_dict(self) -> dict[str, Any]:
        return {
            "version_hash": self.version_hash,
            "count": self.count,
            "bounds": None if self.bounds is None else self.bounds.to_dict(),
            "source_count": self.source_count,
        }

    @classmethod
    def empty(cls, snapshot: PointCloudSnapshot) -> Selection:
        return cls(snapshot.content_hash, (), None, snapshot.count)

    @classmethod
    def from_indices(
        cls,
        snapshot: PointCloudSnapshot,
        indices: Iterable[int] | np.ndarray,
    ) -> Selection:
        array = np.asarray(tuple(indices), dtype=np.int64)
        if array.ndim != 1:
            raise SelectionInvalid("selection indices must be one-dimensional")
        if array.size:
            if array.min() < 0 or array.max() >= snapshot.count:
                raise SelectionInvalid("selection index outside current version")
            array = np.unique(array)
        else:
            array = np.empty(0, dtype=np.int64)
        points = snapshot.cloud.xyz[array]
        bounds = None if array.size == 0 else Bounds.from_points(points)
        return cls(
            version_hash=snapshot.content_hash,
            indices=tuple(int(v) for v in array),
            bounds=bounds,
            source_count=snapshot.count,
        )


def validate_selection(snapshot: PointCloudSnapshot, selection: Selection) -> None:
    if selection.version_hash != snapshot.content_hash:
        raise SelectionInvalid(
            "selection is bound to a different point-cloud version",
            selection_hash=selection.version_hash,
            snapshot_hash=snapshot.content_hash,
        )
    if selection.source_count != snapshot.count:
        raise SelectionInvalid(
            "selection source point count does not match the snapshot"
        )
    if not selection.indices:
        return
    indices = selection.index_array()
    if indices.min() < 0 or indices.max() >= snapshot.count:
        raise SelectionInvalid("selection index outside current version")
    if np.any(np.diff(indices) <= 0):
        raise SelectionInvalid("selection indices must be sorted and unique")


def select_sphere(
    snapshot: PointCloudSnapshot,
    center: Iterable[float],
    radius: float,
) -> Selection:
    center_array = np.asarray(tuple(center), dtype=np.float64)
    if center_array.shape != (3,) or not np.isfinite(center_array).all():
        raise SelectionInvalid("sphere center must be a finite 3D point")
    radius_value = _normalized_float(radius, "radius")
    if radius_value <= 0.0:
        raise SelectionInvalid("sphere radius must be positive")
    delta = snapshot.cloud.xyz.astype(np.float64) - center_array
    distance_squared = np.einsum("ij,ij->i", delta, delta)
    indices = np.flatnonzero(distance_squared <= radius_value * radius_value)
    return Selection.from_indices(snapshot, indices)


def select_world_rectangle(
    snapshot: PointCloudSnapshot,
    minimum: Iterable[float],
    maximum: Iterable[float],
) -> Selection:
    lower = np.asarray(tuple(minimum), dtype=np.float64)
    upper = np.asarray(tuple(maximum), dtype=np.float64)
    if lower.shape != (3,) or upper.shape != (3,):
        raise SelectionInvalid("rectangle bounds must be 3D vectors")
    if not np.isfinite(lower).all() or not np.isfinite(upper).all():
        raise SelectionInvalid("rectangle bounds must be finite")
    if np.any(lower > upper):
        lower, upper = np.minimum(lower, upper), np.maximum(lower, upper)
    inside = np.all(
        (snapshot.cloud.xyz >= lower.astype(np.float32))
        & (snapshot.cloud.xyz <= upper.astype(np.float32)),
        axis=1,
    )
    return Selection.from_indices(snapshot, np.flatnonzero(inside))


def select_screen_rectangle(
    snapshot: PointCloudSnapshot,
    view_projection: np.ndarray,
    x0: float,
    y0: float,
    x1: float,
    y1: float,
    width: int,
    height: int,
) -> Selection:
    """Select points inside a screen-space rectangle.

    Coordinates are pixels with a top-left origin.  Points behind the camera
    and points clipped outside NDC depth are excluded.
    """

    if width <= 0 or height <= 0:
        raise SelectionInvalid("viewport dimensions must be positive")
    left, right = sorted((_normalized_float(x0, "x0"), _normalized_float(x1, "x1")))
    top, bottom = sorted((_normalized_float(y0, "y0"), _normalized_float(y1, "y1")))
    ndc_left = (2.0 * left / width) - 1.0
    ndc_right = (2.0 * right / width) - 1.0
    ndc_bottom = 1.0 - (2.0 * bottom / height)
    ndc_top = 1.0 - (2.0 * top / height)
    clip = project_clip(view_projection, snapshot.cloud.xyz)
    w = clip[:, 3]
    valid = w > 1e-12
    with np.errstate(divide="ignore", invalid="ignore"):
        ndc = clip[:, :3] / w[:, None]
    ndc_x = ndc[:, 0]
    ndc_y = ndc[:, 1]
    ndc_z = ndc[:, 2]
    inside = (
        valid
        & (ndc_x >= ndc_left)
        & (ndc_x <= ndc_right)
        & (ndc_y >= ndc_bottom)
        & (ndc_y <= ndc_top)
        & (ndc_z >= -1.0)
        & (ndc_z <= 1.0)
    )
    return Selection.from_indices(snapshot, np.flatnonzero(inside))
