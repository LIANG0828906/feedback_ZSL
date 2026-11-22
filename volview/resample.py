"""Three-plane (axial / coronal / sagittal) resampling and window mapping.

Conventions
-----------
* World axes are ``X``, ``Y``, ``Z``; the volume array dims map to world axes
  through the manifest ``axis_order``.
* Plane normals: axial -> Z, coronal -> Y, sagittal -> X.
* In-plane image axes (row, col), fixed for every interpolation order:
  axial -> (Y, X), coronal -> (Z, X), sagittal -> (Z, Y).
* Sampling positions are always the voxel-centre world coordinates of the
  in-plane axes, so plane dimensions and sample locations are identical for
  interpolation order 0 (nearest) and 1 (linear along the plane normal).
"""

from __future__ import annotations

from typing import Dict, Tuple

import numpy as np

from .errors import ErrorCode, VolViewError
from .model import VolumeModel

PLANES = ("axial", "coronal", "sagittal")
PLANE_NORMAL: Dict[str, str] = {"axial": "Z", "coronal": "Y", "sagittal": "X"}
PLANE_INPLANE: Dict[str, Tuple[str, str]] = {
    "axial": ("Y", "X"),
    "coronal": ("Z", "X"),
    "sagittal": ("Z", "Y"),
}
INTERP_ORDERS = (0, 1)

# Display-gray tolerance for the three planes intersecting at the volume
# centre (uint8 levels).  The generator produces smooth fields so that
# 1-D normal-direction interpolation from the three directions agrees well
# within this tolerance.
INTERSECTION_TOL_GRAY = 4


def validate_interp_order(order: int) -> int:
    if order not in INTERP_ORDERS:
        raise VolViewError(ErrorCode.E_CAL, "interp_order",
                           f"interpolation order must be one of {INTERP_ORDERS}, got {order!r}")
    return order


def validate_window(model: VolumeModel, window_width: float,
                    window_level: float) -> Tuple[float, float]:
    """Window width must be positive; level must lie in the declared gray range."""
    ww = float(window_width)
    if not np.isfinite(ww) or ww <= 0.0:
        raise VolViewError(ErrorCode.E_WIN, "window_width",
                           f"window width must be positive, got {window_width!r}")
    wl = float(window_level)
    lo, hi = model.manifest.gray_range
    if not np.isfinite(wl) or not (lo <= wl <= hi):
        raise VolViewError(ErrorCode.E_WIN, "window_level",
                           f"window level {window_level!r} outside declared "
                           f"gray range [{lo}, {hi}]",
                           gray_range=[lo, hi])
    return ww, wl


def validate_plane_position(model: VolumeModel, plane: str, position: float) -> float:
    axis = PLANE_NORMAL[plane]
    pos = float(position)
    lo, hi = 0.0, model.extent(axis)
    if not np.isfinite(pos) or not (lo <= pos <= hi):
        raise VolViewError(ErrorCode.E_RANGE, "plane_position",
                           f"plane position {position!r} outside [{lo}, {hi}] "
                           f"on axis {axis}",
                           axis=axis, available=[lo, hi])
    return pos


def _slice_at_index(model: VolumeModel, plane: str, index: int,
                    row_range: Optional[Tuple[int, int]] = None) -> np.ndarray:
    """Extract the calibrated slice at integer index along the plane normal,
    returned in the (row, col) in-plane convention.

    ``row_range`` restricts the output to rows ``[start, stop)`` so chunked
    rendering never materialises more than the per-task pixel budget.
    """
    normal = PLANE_NORMAL[plane]
    dim = model.dim_of_axis[normal]
    raw = np.take(model.data, index, axis=dim)
    # Remaining array dims, in array order:
    remaining = [d for d in range(3) if d != dim]
    axes_of_remaining = [model.manifest.axis_order[d] for d in remaining]
    row_axis, col_axis = PLANE_INPLANE[plane]
    if axes_of_remaining != [row_axis, col_axis]:
        raw = np.ascontiguousarray(raw.T)
    if row_range is not None:
        raw = raw[row_range[0]:row_range[1]]
    return model.calibrated(raw)


def plane_shape(model: VolumeModel, plane: str) -> Tuple[int, int]:
    row_axis, col_axis = PLANE_INPLANE[plane]
    return (model.shape_of_axis[row_axis], model.shape_of_axis[col_axis])


def resample_plane(model: VolumeModel, plane: str, position: float,
                   order: int = 1,
                   row_range: Optional[Tuple[int, int]] = None) -> np.ndarray:
    """Calibrated gray image of ``plane`` at world ``position`` (float64 2-D)."""
    if plane not in PLANES:
        raise VolViewError(ErrorCode.E_RANGE, "plane",
                           f"unknown plane {plane!r}, expected one of {PLANES}")
    validate_interp_order(order)
    pos = validate_plane_position(model, plane, position)

    normal = PLANE_NORMAL[plane]
    n = model.shape_of_axis[normal]
    f = model.world_to_index(normal, pos)

    if order == 0:
        k = int(np.clip(int(np.floor(f + 0.5)), 0, n - 1))
        return _slice_at_index(model, plane, k, row_range)

    k0 = int(np.clip(int(np.floor(f)), 0, n - 1))
    k1 = min(k0 + 1, n - 1)
    alpha = float(np.clip(f - k0, 0.0, 1.0))
    s0 = _slice_at_index(model, plane, k0, row_range)
    if k1 == k0 or alpha == 0.0:
        return s0
    return (1.0 - alpha) * s0 + alpha * _slice_at_index(model, plane, k1, row_range)


def apply_window(calibrated: np.ndarray, window_width: float,
                 window_level: float) -> np.ndarray:
    """Map calibrated gray to display gray (uint8 0..255)."""
    lo = window_level - window_width / 2.0
    scaled = (np.asarray(calibrated, dtype=np.float64) - lo) / window_width
    return np.round(np.clip(scaled, 0.0, 1.0) * 255.0).astype(np.uint8)


def render_plane(model: VolumeModel, plane: str, position: float,
                 window_width: float, window_level: float,
                 order: int = 1,
                 row_range: Optional[Tuple[int, int]] = None) -> np.ndarray:
    """Full single-slice pipeline: resample + window mapping (uint8)."""
    ww, wl = validate_window(model, window_width, window_level)
    gray = resample_plane(model, plane, position, order, row_range)
    return apply_window(gray, ww, wl)
