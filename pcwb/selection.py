"""Version-bound point selections.

A selection is a sorted, unique index array plus the SHA-256 of the cloud
version it was computed against. Applying an operator verifies the hash and
rejects stale selections with ``E_SELECTION_STALE``. Selections never mutate
the underlying cloud.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .cloud import PointCloud
from .errors import E_SELECTION_STALE, PointCloudError


def normalize_indices(indices: np.ndarray) -> np.ndarray:
    """Return a canonical, sorted, unique int64 index array."""
    arr = np.asarray(indices, dtype=np.int64).reshape(-1)
    return np.unique(arr)


@dataclass(frozen=True)
class Selection:
    """Immutable selection bound to a specific cloud content hash."""

    indices: np.ndarray
    cloud_hash: str
    kind: str
    params: dict

    def __post_init__(self) -> None:
        object.__setattr__(self, "indices",
                           normalize_indices(self.indices))

    @property
    def count(self) -> int:
        return int(self.indices.shape[0])

    def check(self, cloud: PointCloud) -> None:
        """Raise ``E_SELECTION_STALE`` if the selection does not bind cloud."""
        if cloud.sha256 != self.cloud_hash:
            raise PointCloudError(
                E_SELECTION_STALE,
                f"selection bound to {self.cloud_hash[:12]} but current "
                f"version is {cloud.sha256[:12]}",
            )
        if self.count and int(self.indices[-1]) >= cloud.count:
            raise PointCloudError(
                E_SELECTION_STALE,
                f"selection index {int(self.indices[-1])} out of range "
                f"for {cloud.count} points",
            )

    def bbox(self, cloud: PointCloud) -> np.ndarray:
        """Bounding box of the selected points (6-vector)."""
        self.check(cloud)
        if self.count == 0:
            return np.zeros(6, dtype=np.float64)
        pts = cloud.xyz[self.indices]
        return np.concatenate([pts.min(axis=0), pts.max(axis=0)])


def box_select(cloud: PointCloud, lower: np.ndarray,
               upper: np.ndarray) -> Selection:
    """Axis-aligned rectangle (3-D box) selection.

    Bounds are inclusive. ``lower`` / ``upper`` are xyz corners and may be
    given in either order.
    """
    lower = np.minimum(lower, upper, dtype=np.float64)
    upper = np.maximum(lower, upper, dtype=np.float64)
    inside = np.all((cloud.xyz >= lower) & (cloud.xyz <= upper), axis=1)
    indices = np.nonzero(inside)[0].astype(np.int64)
    return Selection(
        indices, cloud.sha256, "box",
        {"lower": lower.tolist(), "upper": upper.tolist()},
    )


def sphere_select(cloud: PointCloud, center: np.ndarray,
                  radius: float) -> Selection:
    """Spherical selection: points within ``radius`` of ``center``."""
    if radius < 0:
        raise ValueError("radius must be non-negative")
    center = np.asarray(center, dtype=np.float64).reshape(3)
    dist2 = np.sum((cloud.xyz - center) ** 2, axis=1)
    indices = np.nonzero(dist2 <= float(radius) ** 2)[0].astype(np.int64)
    return Selection(
        indices, cloud.sha256, "sphere",
        {"center": center.tolist(), "radius": float(radius)},
    )


def screen_rect_select(cloud: PointCloud, view_proj: np.ndarray,
                       rect_min: tuple[float, float],
                       rect_max: tuple[float, float]) -> Selection:
    """Rectangle selection in screen/clip space.

    ``view_proj`` is the camera's 4x4 view-projection matrix. ``rect_min`` /
    ``rect_max`` are NDC corners (x, y in [-1, 1]); a point is selected when
    its projected NDC position falls inside the rectangle. Points behind the
    camera (w <= 0) are never selected.
    """
    view_proj = np.asarray(view_proj, dtype=np.float64)
    n = cloud.count
    homo = np.concatenate(
        [cloud.xyz, np.ones((n, 1), dtype=np.float64)], axis=1)
    clip = homo @ view_proj.T
    w = clip[:, 3]
    valid = w > 0
    ndc = np.zeros_like(clip[:, :3])
    np.divide(clip[:, :3], w[:, None], out=ndc, where=valid[:, None])
    xmin, ymin = rect_min
    xmax, ymax = rect_max
    inside = (
        valid
        & (ndc[:, 0] >= min(xmin, xmax)) & (ndc[:, 0] <= max(xmin, xmax))
        & (ndc[:, 1] >= min(ymin, ymax)) & (ndc[:, 1] <= max(ymin, ymax))
    )
    indices = np.nonzero(inside)[0].astype(np.int64)
    return Selection(
        indices, cloud.sha256, "screen_rect",
        {"rect_min": [float(xmin), float(ymin)],
         "rect_max": [float(xmax), float(ymax)]},
    )
