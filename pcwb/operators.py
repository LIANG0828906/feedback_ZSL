"""Pure, byte-deterministic point-cloud edit operators.

Every operator:

* takes an immutable :class:`~pcwb.cloud.PointCloud` plus a version-bound
  :class:`~pcwb.selection.Selection` and JSON-safe parameters;
* never mutates its inputs;
* performs all arithmetic in float64 and casts to float32 only at
  serialization time, iterates arrays in index order and emits points in a
  canonical order, so identical (input bytes, parameters) produce identical
  output bytes;
* returns an :class:`ApplyResult` holding the new arrays and a serializable
  operation record (operator name, parameters, input version/hash).
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .cloud import PointCloud
from .selection import Selection, normalize_indices


@dataclass(frozen=True)
class ApplyResult:
    """Operator output: new cloud arrays plus the operation record."""

    xyz: np.ndarray
    rgb: np.ndarray
    record: dict


def _record(name: str, params: dict, cloud: PointCloud,
            selected: int) -> dict:
    return {
        "operator": name,
        "params": _canonical(params),
        "input_version": cloud.version,
        "input_hash": cloud.sha256,
        "input_count": cloud.count,
        "selected_count": int(selected),
    }


def _canonical(value):
    """Coerce numpy scalars/arrays into JSON-safe canonical Python values."""
    if isinstance(value, dict):
        return {str(k): _canonical(value[k]) for k in sorted(value)}
    if isinstance(value, (list, tuple)):
        return [_canonical(v) for v in value]
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, np.ndarray):
        return [_canonical(v) for v in value.tolist()]
    return value


def _check(cloud: PointCloud, selection: Selection) -> np.ndarray:
    selection.check(cloud)
    return normalize_indices(selection.indices)


def crop(cloud: PointCloud, selection: Selection,
         mode: str = "delete") -> ApplyResult:
    """Crop the selection.

    ``mode="delete"`` removes selected points; ``mode="keep"`` keeps only the
    selected points. Point order relative to the survivor set is preserved.
    """
    if mode not in ("delete", "keep"):
        raise ValueError(f"crop mode must be 'delete' or 'keep', got {mode!r}")
    indices = _check(cloud, selection)
    mask = np.zeros(cloud.count, dtype=bool)
    mask[indices] = True
    keep = mask if mode == "keep" else ~mask
    new_xyz = cloud.xyz[keep].copy()
    new_rgb = cloud.rgb[keep].copy()
    record = _record("crop", {"mode": mode}, cloud, indices.shape[0])
    record["output_count"] = int(new_xyz.shape[0])
    return ApplyResult(new_xyz, new_rgb, record)


def color_cover(cloud: PointCloud, selection: Selection,
                rgb=(255, 0, 0)) -> ApplyResult:
    """Overwrite the color of every selected point with a constant RGB."""
    color = np.asarray(rgb, dtype=np.uint8).reshape(3)
    indices = _check(cloud, selection)
    new_rgb = cloud.rgb.copy()
    new_rgb[indices] = color
    params = {"rgb": [int(color[0]), int(color[1]), int(color[2])]}
    record = _record("color_cover", params, cloud, indices.shape[0])
    record["output_count"] = cloud.count
    return ApplyResult(cloud.xyz.copy(), new_rgb, record)


def _voxel_table(xyz: np.ndarray, cell: float):
    """Build a deterministic voxel -> point-index table.

    Returns ``(keys, point_order, group_offsets)`` where points sharing a
    voxel key are contiguous in ``point_order`` and each group is itself
    sorted by original point index.
    """
    origin = xyz.min(axis=0)
    coords = np.floor((xyz - origin) / cell).astype(np.int64)
    # lexsort on (index, kz, ky, kx) yields unique-keys-first ordering while
    # keeping point indices ascending inside each group.
    order = np.lexsort(
        (np.arange(xyz.shape[0]), coords[:, 2], coords[:, 1], coords[:, 0]))
    sorted_coords = coords[order]
    key_change = np.empty(xyz.shape[0], dtype=bool)
    key_change[0] = True
    if xyz.shape[0] > 1:
        key_change[1:] = np.any(
            sorted_coords[1:] != sorted_coords[:-1], axis=1)
    offsets = np.nonzero(key_change)[0]
    keys = sorted_coords[offsets]
    return keys, order, offsets


def smooth(cloud: PointCloud, selection: Selection, radius: float = 1.0,
           iterations: int = 1, smooth_color: bool = True,
           cancel_check=None, progress_cb=None) -> ApplyResult:
    """Fixed-radius neighborhood weighted average on the selection.

    Each selected point is replaced by the uniform weighted average of every
    point (selected or not) within ``radius``. Neighbor lookup uses a uniform
    voxel hash table of cell size ``radius`` and only the 27 neighboring
    voxels are scanned. Iterations run in index order; colors are rounded to
    the nearest byte. Non-selected points are copied through unchanged.
    """
    if radius <= 0:
        raise ValueError("smooth radius must be positive")
    if iterations < 1:
        raise ValueError("smooth iterations must be >= 1")
    indices = _check(cloud, selection)
    new_xyz = cloud.xyz.copy()
    new_rgb = cloud.rgb.copy()
    radius2 = float(radius) ** 2

    work_xyz = cloud.xyz.copy()
    work_rgb = cloud.rgb.astype(np.float64).copy()
    total_steps = int(iterations) * max(1, indices.shape[0])
    done_steps = 0
    for _ in range(int(iterations)):
        keys, order, offsets = _voxel_table(work_xyz, float(radius))
        origin = work_xyz.min(axis=0)
        key_to_group = {
            tuple(keys[g].tolist()): g for g in range(keys.shape[0])
        }
        for pos, idx in enumerate(indices):
            if cancel_check is not None and pos % 256 == 0:
                cancel_check()
            if progress_cb is not None and pos % 512 == 0:
                progress_cb(min(0.95, done_steps / max(1, total_steps)),
                            "smoothing neighborhoods")
            center = work_xyz[idx]
            center_cell = np.floor((center - origin) / radius).astype(np.int64)
            neighbor_idx = []
            for dz in (-1, 0, 1):
                for dy in (-1, 0, 1):
                    for dx in (-1, 0, 1):
                        group = key_to_group.get(
                            tuple((center_cell + [dx, dy, dz]).tolist()))
                        if group is None:
                            continue
                        start = offsets[group]
                        end = (offsets[group + 1]
                               if group + 1 < offsets.shape[0]
                               else order.shape[0])
                        neighbor_idx.append(order[start:end])
            if not neighbor_idx:
                continue
            neigh = np.concatenate(neighbor_idx)
            neigh.sort()
            delta = work_xyz[neigh] - center
            within = np.einsum("ij,ij->i", delta, delta) <= radius2
            neigh = neigh[within]
            if neigh.shape[0] == 0:
                continue
            new_xyz[idx] = work_xyz[neigh].mean(axis=0)
            if smooth_color:
                new_rgb[idx] = np.rint(
                    work_rgb[neigh].mean(axis=0)).astype(np.uint8)
            done_steps += 1
        work_xyz = new_xyz.copy()
        work_rgb = new_rgb.astype(np.float64)

    params = {"radius": float(radius), "iterations": int(iterations),
              "smooth_color": bool(smooth_color)}
    record = _record("smooth", params, cloud, indices.shape[0])
    record["output_count"] = cloud.count
    return ApplyResult(new_xyz, new_rgb, record)


def downsample(cloud: PointCloud, selection: Selection,
               voxel_size: float = 1.0) -> ApplyResult:
    """Voxel-grid centroid downsampling restricted to the selection.

    Points inside each occupied voxel (among selected points) are replaced by
    their centroid and average color; non-selected points pass through
    unchanged. Output points are sorted by (z, y, x) centroid so the result is
    canonical regardless of input index order.
    """
    if voxel_size <= 0:
        raise ValueError("downsample voxel_size must be positive")
    indices = _check(cloud, selection)
    selected_set = np.zeros(cloud.count, dtype=bool)
    selected_set[indices] = True
    other_xyz = cloud.xyz[~selected_set]
    other_rgb = cloud.rgb[~selected_set]

    if indices.shape[0] == 0:
        new_xyz, new_rgb = other_xyz, other_rgb
    else:
        sel_xyz = cloud.xyz[indices]
        sel_rgb = cloud.rgb[indices].astype(np.float64)
        origin = sel_xyz.min(axis=0)
        coords = np.floor((sel_xyz - origin) / voxel_size).astype(np.int64)
        order = np.lexsort(
            (np.arange(sel_xyz.shape[0]),
             coords[:, 2], coords[:, 1], coords[:, 0]))
        sorted_coords = coords[order]
        key_change = np.ones(sel_xyz.shape[0], dtype=bool)
        if sel_xyz.shape[0] > 1:
            key_change[1:] = np.any(
                sorted_coords[1:] != sorted_coords[:-1], axis=1)
        group_ids = np.cumsum(key_change) - 1
        group_count = int(group_ids[-1]) + 1
        centroid_xyz = np.empty((group_count, 3), dtype=np.float64)
        centroid_rgb = np.empty((group_count, 3), dtype=np.float64)
        for g in range(group_count):
            members = order[group_ids == g]
            centroid_xyz[g] = sel_xyz[members].mean(axis=0)
            centroid_rgb[g] = sel_rgb[members].mean(axis=0)
        centroid_rgb = np.rint(centroid_rgb).astype(np.uint8)

        new_xyz = np.concatenate([other_xyz, centroid_xyz])
        new_rgb = np.concatenate([other_rgb, centroid_rgb])
        sort_idx = np.lexsort(
            (new_xyz[:, 0], new_xyz[:, 1], new_xyz[:, 2]))
        new_xyz = np.ascontiguousarray(new_xyz[sort_idx])
        new_rgb = np.ascontiguousarray(new_rgb[sort_idx])

    params = {"voxel_size": float(voxel_size)}
    record = _record("downsample", params, cloud, indices.shape[0])
    record["output_count"] = int(new_xyz.shape[0])
    return ApplyResult(new_xyz, new_rgb, record)


OPERATORS = {
    "crop": crop,
    "color_cover": color_cover,
    "smooth": smooth,
    "downsample": downsample,
}


def apply_operator(name: str, cloud: PointCloud, selection: Selection,
                   params: dict | None = None) -> ApplyResult:
    """Dispatch a named operator with the given JSON-safe parameters."""
    try:
        func = OPERATORS[name]
    except KeyError as exc:
        raise ValueError(f"unknown operator {name!r}") from exc
    return func(cloud, selection, **(params or {}))
