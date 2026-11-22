"""Slab (thick-slice) equivalence: weighted averaging of adjacent slices.

For a plane at world position ``p`` along its normal axis and a requested
thickness ``t`` (mm), every voxel-centre slice whose centre falls inside
``[p - t/2, p + t/2]`` contributes with a triangular weight
``w_k = max(0, 1 - 2*|p_k - p| / t)``; weights are renormalised to sum to 1.
The weight set therefore depends on both the thickness and the axis spacing.

* ``t == 0`` selects the single-slice path (regular resampling).
* The window is clipped at the volume boundary: only existing slices are
  used and weights are renormalised, so edges are never dropped and no
  zero padding is introduced.
* ``t`` larger than the full axis extent raises ``E_THICK`` carrying the
  available range.
"""

from __future__ import annotations

from typing import Tuple

import numpy as np

from .errors import ErrorCode, VolViewError
from .model import VolumeModel
from .resample import (PLANE_NORMAL, _slice_at_index, apply_window,
                       resample_plane, validate_interp_order,
                       validate_plane_position, validate_window)


def max_thickness(model: VolumeModel, plane: str) -> float:
    """Maximum usable slab thickness = full axis extent (mm)."""
    return model.extent(PLANE_NORMAL[plane])


def validate_thickness(model: VolumeModel, plane: str, thickness: float) -> float:
    t = float(thickness)
    hi = max_thickness(model, plane)
    if not np.isfinite(t) or t < 0.0 or t > hi:
        raise VolViewError(ErrorCode.E_THICK, "thickness",
                           f"thickness {thickness!r} outside available range "
                           f"[0.0, {hi}]",
                           available=[0.0, hi])
    return t


def slab_weights(model: VolumeModel, plane: str, position: float,
                 thickness: float) -> Tuple[np.ndarray, np.ndarray]:
    """Return (slice_indices, normalised_weights) for the slab."""
    normal = PLANE_NORMAL[plane]
    spacing = model.manifest.spacing[normal]
    n = model.shape_of_axis[normal]
    half = thickness / 2.0
    centres = np.arange(n, dtype=np.float64) * spacing
    inside = np.abs(centres - position) <= half + 1e-9
    indices = np.nonzero(inside)[0]
    if indices.size == 0:  # thickness smaller than spacing: nearest slice
        indices = np.array([int(np.argmin(np.abs(centres - position)))])
    weights = np.maximum(0.0, 1.0 - 2.0 * np.abs(centres[indices] - position) / thickness)
    total = float(weights.sum())
    if total <= 0.0:  # degenerate: single slice exactly at the edge
        weights = np.ones_like(weights)
        total = float(weights.sum())
    return indices, weights / total


def resample_slab(model: VolumeModel, plane: str, position: float,
                  thickness: float, order: int = 1,
                  row_range: Optional[Tuple[int, int]] = None) -> np.ndarray:
    """Calibrated equivalent thick-slice image (float64 2-D)."""
    validate_interp_order(order)
    pos = validate_plane_position(model, plane, position)
    t = validate_thickness(model, plane, thickness)
    if t == 0.0:
        return resample_plane(model, plane, pos, order, row_range)
    indices, weights = slab_weights(model, plane, pos, t)
    acc = None
    for index, weight in zip(indices.tolist(), weights.tolist()):
        contribution = _slice_at_index(model, plane, index, row_range) * weight
        acc = contribution if acc is None else acc + contribution
    return acc


def render_slab(model: VolumeModel, plane: str, position: float,
                thickness: float, window_width: float, window_level: float,
                order: int = 1,
                row_range: Optional[Tuple[int, int]] = None) -> np.ndarray:
    """Full slab pipeline: weighted average + window mapping (uint8)."""
    ww, wl = validate_window(model, window_width, window_level)
    gray = resample_slab(model, plane, position, thickness, order, row_range)
    return apply_window(gray, ww, wl)


def high_frequency_energy(image: np.ndarray) -> float:
    """Sum of squared first differences (used to check smoothing monotonicity)."""
    img = np.asarray(image, dtype=np.float64)
    energy = 0.0
    if img.shape[0] > 1:
        energy += float(np.sum(np.diff(img, axis=0) ** 2))
    if img.shape[1] > 1:
        energy += float(np.sum(np.diff(img, axis=1) ** 2))
    return energy
