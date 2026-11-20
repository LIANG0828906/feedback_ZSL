"""Neighbour interpolation and visibility-aware leak compensation."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .errors import ErrorCode, ProbeError
from .grid import ProbeGrid


LEAK_VISIBILITY_THRESHOLD = 0.5


def occlusion_compensation_blend(
    visibility_mean: np.ndarray, strength: float
) -> np.ndarray:
    """Blend only strongly occluded probes; low visibility marks a leak risk.

    The threshold is explicit so a strength edit invalidates only probes whose
    compensation term is non-zero, plus their one-hop compensation dependents.
    """

    visibility = np.asarray(visibility_mean, dtype=np.float64)
    if not np.isfinite(visibility).all():
        raise ProbeError(ErrorCode.INTERP_FAILED, "visibility summary contains non-finite values")
    if not np.isfinite(strength) or strength < 0.0 or strength > 1.0:
        raise ProbeError(ErrorCode.INTERP_FAILED, "compensation strength must be in [0, 1]")
    leak = np.clip(
        (LEAK_VISIBILITY_THRESHOLD - visibility) / LEAK_VISIBILITY_THRESHOLD,
        0.0,
        1.0,
    )
    return np.clip(float(strength) * leak, 0.0, 1.0)


def _validate_coefficients(coefficients: np.ndarray) -> np.ndarray:
    coeffs = np.asarray(coefficients, dtype=np.float64)
    if coeffs.ndim != 3 or coeffs.shape[2] != 3:
        raise ProbeError(ErrorCode.INTERP_FAILED, "coefficients must have shape (N, K, 3)")
    if not np.isfinite(coeffs).all():
        raise ProbeError(ErrorCode.INTERP_FAILED, "coefficients contain non-finite values")
    return coeffs


def inverse_distance_neighbour_coefficients(
    coefficients: np.ndarray,
    grid: ProbeGrid,
    probe_ids: np.ndarray | list[int] | None = None,
    batch_size: int = 1024,
) -> np.ndarray:
    """Multi-point inverse-distance interpolation over the 26-neighbour lattice."""

    coeffs = _validate_coefficients(coefficients)
    if coeffs.shape[0] != grid.count:
        raise ProbeError(
            ErrorCode.INTERP_FAILED,
            f"coefficient count {coeffs.shape[0]} does not match grid count {grid.count}",
        )
    ids = (
        np.arange(grid.count, dtype=np.int64)
        if probe_ids is None
        else np.asarray(probe_ids, dtype=np.int64)
    )
    if ids.ndim != 1:
        raise ProbeError(ErrorCode.INTERP_FAILED, "probe_ids must be one-dimensional")
    if np.any(ids < 0) or np.any(ids >= grid.count):
        bad = int(ids[(ids < 0) | (ids >= grid.count)][0])
        raise ProbeError(ErrorCode.INTERP_FAILED, "probe id is outside the grid", probe_id=bad)

    output = np.empty((ids.size, coeffs.shape[1], 3), dtype=np.float64)
    positions = grid.centers
    for batch_start in range(0, ids.size, batch_size):
        batch_ids = ids[batch_start : batch_start + batch_size]
        for local_index, probe_id in enumerate(batch_ids):
            neighbours = grid.adjacency[int(probe_id)]
            if neighbours.size == 0:
                if grid.count == 1:
                    output[batch_start + local_index] = coeffs[int(probe_id)]
                    continue
                raise ProbeError(
                    ErrorCode.INTERP_FAILED,
                    "probe has no neighbours in the interpolation structure",
                    probe_id=int(probe_id),
                )
            difference = positions[neighbours] - positions[int(probe_id)]
            squared_distance = np.einsum("nd,nd->n", difference, difference)
            if not np.isfinite(squared_distance).all() or np.any(squared_distance <= 0.0):
                raise ProbeError(
                    ErrorCode.INTERP_FAILED,
                    "probe has a degenerate neighbour distance",
                    probe_id=int(probe_id),
                )
            weights = 1.0 / np.maximum(squared_distance, 1e-18)
            weights /= weights.sum()
            output[batch_start + local_index] = np.einsum(
                "n,nkc->kc", weights, coeffs[neighbours], optimize=True
            )
    return output


@dataclass(frozen=True)
class AnchorStructure:
    anchor_mask: np.ndarray
    anchor_ids: np.ndarray
    stencil_probe_ids: np.ndarray
    stencil_weights: np.ndarray
    unsampled_ids: np.ndarray


def _axis_anchor_brackets(
    dimensions: int, stride: int
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    anchor_indices = np.arange(0, dimensions, stride, dtype=np.int64)
    if anchor_indices[-1] != dimensions - 1:
        anchor_indices = np.append(anchor_indices, dimensions - 1)
    coordinates = np.arange(dimensions, dtype=np.int64)
    upper_positions = np.searchsorted(anchor_indices, coordinates, side="right")
    if anchor_indices.size == 1:
        lower_positions = np.zeros(dimensions, dtype=np.int64)
        upper_positions = np.zeros(dimensions, dtype=np.int64)
        fractions = np.zeros(dimensions, dtype=np.float64)
        return anchor_indices[lower_positions], anchor_indices[upper_positions], fractions
    upper_positions = np.clip(upper_positions, 1, anchor_indices.size - 1)
    lower_positions = upper_positions - 1
    lower_coordinates = anchor_indices[lower_positions]
    upper_coordinates = anchor_indices[upper_positions]
    fractions = (coordinates - lower_coordinates) / (
        upper_coordinates - lower_coordinates
    ).astype(np.float64)
    return lower_coordinates, upper_coordinates, fractions


def lattice_anchor_structure(grid: ProbeGrid, stride: int = 2) -> AnchorStructure:
    """Build deterministic sparse anchors and an 8-corner reconstruction stencil."""

    if isinstance(stride, bool) or not isinstance(stride, int) or stride < 2:
        raise ProbeError(ErrorCode.INTERP_FAILED, "anchor stride must be an integer >= 2")
    brackets = [_axis_anchor_brackets(grid.dims[axis], stride) for axis in range(3)]
    anchor_axes = [
        np.unique(np.concatenate([brackets[axis][0], brackets[axis][1]]))
        for axis in range(3)
    ]
    coordinates = np.stack(
        [
            np.repeat(np.arange(grid.dims[0]), grid.dims[1] * grid.dims[2]),
            np.tile(np.repeat(np.arange(grid.dims[1]), grid.dims[2]), grid.dims[0]),
            np.tile(np.arange(grid.dims[2]), grid.dims[0] * grid.dims[1]),
        ],
        axis=1,
    )
    anchor_mask = np.ones(grid.count, dtype=bool)
    for axis in range(3):
        anchor_mask &= np.isin(coordinates[:, axis], anchor_axes[axis])
    anchor_ids = np.flatnonzero(anchor_mask).astype(np.int64)
    unsampled_ids = np.flatnonzero(~anchor_mask).astype(np.int64)
    if unsampled_ids.size == 0:
        stencil_ids = np.arange(grid.count, dtype=np.int64)[:, None]
        stencil_weights = np.ones((grid.count, 1), dtype=np.float64)
        return AnchorStructure(
            anchor_mask, anchor_ids, stencil_ids, stencil_weights, unsampled_ids
        )

    lower_indices: list[np.ndarray] = []
    upper_indices: list[np.ndarray] = []
    fractions: list[np.ndarray] = []
    for axis in range(3):
        lower_coordinates, upper_coordinates, axis_fractions = brackets[axis]
        lower_indices.append(lower_coordinates)
        upper_indices.append(upper_coordinates)
        fractions.append(axis_fractions)

    corner_ids = np.empty((unsampled_ids.size, 8), dtype=np.int64)
    corner_weights = np.empty((unsampled_ids.size, 8), dtype=np.float64)
    cursor = 0
    for dx in (0, 1):
        ix = (
            lower_indices[0][coordinates[unsampled_ids, 0]]
            if dx == 0
            else upper_indices[0][coordinates[unsampled_ids, 0]]
        )
        wx = (
            1.0 - fractions[0][coordinates[unsampled_ids, 0]]
            if dx == 0
            else fractions[0][coordinates[unsampled_ids, 0]]
        )
        for dy in (0, 1):
            iy = (
                lower_indices[1][coordinates[unsampled_ids, 1]]
                if dy == 0
                else upper_indices[1][coordinates[unsampled_ids, 1]]
            )
            wy = (
                1.0 - fractions[1][coordinates[unsampled_ids, 1]]
                if dy == 0
                else fractions[1][coordinates[unsampled_ids, 1]]
            )
            for dz in (0, 1):
                iz = (
                    lower_indices[2][coordinates[unsampled_ids, 2]]
                    if dz == 0
                    else upper_indices[2][coordinates[unsampled_ids, 2]]
                )
                wz = (
                    1.0 - fractions[2][coordinates[unsampled_ids, 2]]
                    if dz == 0
                    else fractions[2][coordinates[unsampled_ids, 2]]
                )
                corner_ids[:, cursor] = (
                    ix * grid.dims[1] + iy
                ) * grid.dims[2] + iz
                corner_weights[:, cursor] = wx * wy * wz
                cursor += 1
    stencil_ids = np.empty((grid.count, 8), dtype=np.int64)
    stencil_weights = np.zeros((grid.count, 8), dtype=np.float64)
    stencil_ids[anchor_ids, 0] = anchor_ids
    stencil_weights[anchor_ids, 0] = 1.0
    stencil_ids[unsampled_ids] = corner_ids
    stencil_weights[unsampled_ids] = corner_weights
    return AnchorStructure(
        anchor_mask=anchor_mask,
        anchor_ids=anchor_ids,
        stencil_probe_ids=stencil_ids,
        stencil_weights=stencil_weights,
        unsampled_ids=unsampled_ids,
    )


def interpolate_from_stencil(
    values: np.ndarray,
    stencil_ids: np.ndarray,
    stencil_weights: np.ndarray,
    target_ids: np.ndarray,
) -> np.ndarray:
    """Interpolate a per-probe array using an existing anchor stencil."""

    source = np.asarray(values)
    ids = np.asarray(stencil_ids, dtype=np.int64)[target_ids]
    weights = np.asarray(stencil_weights, dtype=np.float64)[target_ids]
    gathered = source[ids]
    if source.ndim == 1:
        return np.einsum("nk,nk->n", weights, gathered, optimize=True)
    return np.einsum("nk,nk...->n...", weights, gathered, optimize=True)


def apply_occlusion_compensation(
    raw_coefficients: np.ndarray,
    visibility_mean: np.ndarray,
    grid: ProbeGrid,
    strength: float,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Blend occluded probes toward their local multi-point reconstruction.

    Returns (compensated coefficients, blend weights, neighbour coefficients).
    Blend weights are zero for probes whose result is independent of a strength
    change, allowing exact cache reuse in incremental bakes.
    """

    coeffs = _validate_coefficients(raw_coefficients)
    visibility = np.asarray(visibility_mean, dtype=np.float64)
    if visibility.shape != (grid.count,) or not np.isfinite(visibility).all():
        raise ProbeError(ErrorCode.INTERP_FAILED, "visibility summary must have shape (N,)")
    neighbour_coeffs = inverse_distance_neighbour_coefficients(coeffs, grid)
    blend = occlusion_compensation_blend(visibility, strength)
    compensated = (
        coeffs * (1.0 - blend[:, None, None])
        + neighbour_coeffs * blend[:, None, None]
    )
    compensated.setflags(write=False)
    blend.setflags(write=False)
    neighbour_coeffs.setflags(write=False)
    return compensated, blend, neighbour_coeffs


@dataclass(frozen=True)
class TrilinearQuery:
    coefficients: np.ndarray
    probe_ids: np.ndarray
    weights: np.ndarray


def trilinear_query(
    grid: ProbeGrid,
    coefficients: np.ndarray,
    position: np.ndarray | tuple[float, float, float],
) -> TrilinearQuery:
    coeffs = _validate_coefficients(coefficients)
    if coeffs.shape[0] != grid.count:
        raise ProbeError(ErrorCode.INTERP_FAILED, "coefficient count does not match the grid")
    point = np.asarray(position, dtype=np.float64)
    if point.shape != (3,) or not np.isfinite(point).all():
        raise ProbeError(ErrorCode.INTERP_FAILED, "query position must be a finite vec3")
    origin = np.asarray(grid.origin, dtype=np.float64)
    dimensions = np.asarray(grid.dims, dtype=np.int64)
    relative = (point - origin) / grid.spacing
    maximum = (dimensions - 1).astype(np.float64)
    tolerance = 1e-9
    if np.any(relative < -tolerance) or np.any(relative > maximum + tolerance):
        nearest = int(np.argmin(np.linalg.norm(grid.centers - point[None, :], axis=1)))
        raise ProbeError(
            ErrorCode.INTERP_FAILED,
            "query position lies outside the probe lattice",
            probe_id=nearest,
        )
    relative = np.clip(relative, 0.0, maximum)
    lower = np.floor(relative).astype(np.int64)
    upper = np.minimum(lower + 1, dimensions - 1)
    fractions = relative - lower.astype(np.float64)

    probe_ids: list[int] = []
    weights: list[float] = []
    for dx in (0, 1):
        ix = int(lower[0] if dx == 0 else upper[0])
        wx = 1.0 - fractions[0] if dx == 0 else fractions[0]
        for dy in (0, 1):
            iy = int(lower[1] if dy == 0 else upper[1])
            wy = 1.0 - fractions[1] if dy == 0 else fractions[1]
            for dz in (0, 1):
                iz = int(lower[2] if dz == 0 else upper[2])
                wz = 1.0 - fractions[2] if dz == 0 else fractions[2]
                weight = float(wx * wy * wz)
                if weight <= 0.0:
                    continue
                probe_id = grid.index(ix, iy, iz)
                if probe_id < 0:
                    raise ProbeError(
                        ErrorCode.INTERP_FAILED,
                        "trilinear cell references a missing probe",
                        probe_id=grid.nearest_probe(point),
                    )
                probe_ids.append(probe_id)
                weights.append(weight)

    if not probe_ids:
        raise ProbeError(
            ErrorCode.INTERP_FAILED,
            "empty trilinear stencil",
            probe_id=grid.nearest_probe(point),
        )
    weight_array = np.asarray(weights, dtype=np.float64)
    weight_sum = float(weight_array.sum())
    if not np.isfinite(weight_sum) or weight_sum <= 0.0:
        raise ProbeError(
            ErrorCode.INTERP_FAILED,
            "invalid trilinear weights",
            probe_id=grid.nearest_probe(point),
        )
    weight_array /= weight_sum
    id_array = np.asarray(probe_ids, dtype=np.int64)
    interpolated = np.einsum("n,nkc->kc", weight_array, coeffs[id_array], optimize=True)
    id_array.setflags(write=False)
    weight_array.setflags(write=False)
    interpolated.setflags(write=False)
    return TrilinearQuery(interpolated, id_array, weight_array)

