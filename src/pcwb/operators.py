"""Pure, deterministic point-cloud edit operators."""

from __future__ import annotations

import math
import threading
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import OperationCancelled, OperatorInvalid
from .model import PointCloud, PointCloudSnapshot
from .selection import Selection, validate_selection
from .util import normalize_json_value

ProgressCallback = Callable[[str, float], None]

OPERATOR_NAMES = ("crop", "smooth", "downsample", "color_overwrite")


@dataclass(frozen=True, slots=True)
class OperationRecord:
    operator: str
    params: dict[str, Any]
    input_version: int
    input_hash: str
    output_hash: str
    input_count: int
    output_count: int
    selection_count: int

    def to_dict(self, *, version: int | None = None) -> dict[str, Any]:
        result: dict[str, Any] = {
            "operator": self.operator,
            "params": self.params,
            "input_version": self.input_version,
            "input_hash": self.input_hash,
            "output_hash": self.output_hash,
            "input_count": self.input_count,
            "output_count": self.output_count,
            "selection_count": self.selection_count,
        }
        if version is not None:
            result["version"] = version
        return result


@dataclass(frozen=True, slots=True)
class OperatorResult:
    cloud: PointCloud
    record: OperationRecord


def _check_cancel(cancel_event: threading.Event | None) -> None:
    if cancel_event is not None and cancel_event.is_set():
        raise OperationCancelled("edit cancelled")


def _emit(progress: ProgressCallback | None, phase: str, fraction: float) -> None:
    if progress is not None:
        progress(phase, float(min(max(fraction, 0.0), 1.0)))


def _unknown_parameters(params: Mapping[str, Any], allowed: set[str]) -> None:
    unknown = sorted(set(params) - allowed)
    if unknown:
        raise OperatorInvalid("unknown operator parameter", parameters=unknown)


def _positive_float(value: Any, name: str) -> float:
    if isinstance(value, bool):
        raise OperatorInvalid(f"{name} must be positive")
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise OperatorInvalid(f"{name} must be positive") from exc
    if not math.isfinite(result) or result <= 0.0:
        raise OperatorInvalid(f"{name} must be positive")
    return result


def _positive_int(value: Any, name: str, *, maximum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, (int, np.integer)):
        raise OperatorInvalid(f"{name} must be an integer")
    result = int(value)
    if result <= 0:
        raise OperatorInvalid(f"{name} must be positive")
    if maximum is not None and result > maximum:
        raise OperatorInvalid(f"{name} exceeds the supported maximum", maximum=maximum)
    return result


def _crop(
    snapshot: PointCloudSnapshot,
    selection: Selection,
    params: Mapping[str, Any],
    cancel_event: threading.Event | None,
    progress: ProgressCallback | None,
) -> PointCloud:
    _unknown_parameters(params, {"mode"})
    mode = str(params.get("mode", "keep"))
    if mode not in {"keep", "remove"}:
        raise OperatorInvalid("crop mode must be 'keep' or 'remove'")
    _check_cancel(cancel_event)
    selected = selection.index_array()
    mask = np.ones(snapshot.count, dtype=bool)
    if mode == "keep":
        mask[:] = False
        mask[selected] = True
    else:
        mask[selected] = False
    _emit(progress, "crop", 0.5)
    _check_cancel(cancel_event)
    indices = np.flatnonzero(mask)
    cloud = PointCloud.from_arrays(
        snapshot.cloud.xyz[indices], snapshot.cloud.rgb[indices]
    )
    _emit(progress, "crop", 1.0)
    return cloud


def _smooth(
    snapshot: PointCloudSnapshot,
    selection: Selection,
    params: Mapping[str, Any],
    cancel_event: threading.Event | None,
    progress: ProgressCallback | None,
) -> PointCloud:
    _unknown_parameters(params, {"radius", "iterations"})
    radius = _positive_float(params.get("radius"), "radius")
    iterations = _positive_int(params.get("iterations", 1), "iterations", maximum=100)
    _check_cancel(cancel_event)

    source_xyz = snapshot.cloud.xyz.astype(np.float64, copy=True)
    selected = selection.index_array()
    total_work = max(1, iterations * max(1, len(selected)))
    completed = 0

    for iteration in range(iterations):
        _check_cancel(cancel_event)
        minimum = source_xyz.min(axis=0) if source_xyz.size else np.zeros(3)
        cells: dict[tuple[int, int, int], list[int]] = {}
        cell_indices = np.floor((source_xyz - minimum) / radius).astype(np.int64)
        for index, cell in enumerate(map(tuple, cell_indices)):
            cells.setdefault(cell, []).append(index)

        next_xyz = source_xyz.copy()
        for local_index, point_index in enumerate(selected):
            if (local_index & 511) == 0:
                _check_cancel(cancel_event)
            cell = tuple(int(v) for v in cell_indices[point_index])
            candidates: list[int] = []
            for dx in (-1, 0, 1):
                for dy in (-1, 0, 1):
                    for dz in (-1, 0, 1):
                        candidates.extend(
                            cells.get((cell[0] + dx, cell[1] + dy, cell[2] + dz), ())
                        )
            if not candidates:
                completed += 1
                continue
            neighbor_indices = np.unique(np.asarray(candidates, dtype=np.int64))
            delta = source_xyz[neighbor_indices] - source_xyz[point_index]
            distance_squared = np.einsum("ij,ij->i", delta, delta)
            inside = distance_squared <= radius * radius
            chosen = neighbor_indices[inside]
            if chosen.size:
                next_xyz[point_index] = source_xyz[chosen].mean(axis=0)
            completed += 1
            if (completed & 1023) == 0:
                _emit(
                    progress,
                    f"smooth:{iteration + 1}/{iterations}",
                    completed / total_work,
                )
        source_xyz = next_xyz
        _emit(
            progress,
            f"smooth:{iteration + 1}/{iterations}",
            min(1.0, completed / total_work),
        )

    _check_cancel(cancel_event)
    cloud = PointCloud.from_arrays(source_xyz.astype(np.float32), snapshot.cloud.rgb)
    _emit(progress, "smooth", 1.0)
    return cloud


def _downsample(
    snapshot: PointCloudSnapshot,
    selection: Selection,
    params: Mapping[str, Any],
    cancel_event: threading.Event | None,
    progress: ProgressCallback | None,
) -> PointCloud:
    _unknown_parameters(params, {"mode", "voxel_size", "stride"})
    mode = str(params.get("mode", "voxel"))
    selected = selection.index_array()
    _check_cancel(cancel_event)

    kept_selected: list[int]
    if mode == "voxel":
        voxel_size = _positive_float(params.get("voxel_size"), "voxel_size")
        xyz = snapshot.cloud.xyz.astype(np.float64)
        origin = xyz.min(axis=0) if xyz.size else np.zeros(3)
        voxel_indices = np.floor((xyz - origin) / voxel_size).astype(np.int64)
        occupied: set[tuple[int, int, int]] = set()
        kept_selected = []
        for offset, point_index in enumerate(selected):
            if (offset & 1023) == 0:
                _check_cancel(cancel_event)
            key = tuple(int(v) for v in voxel_indices[point_index])
            if key not in occupied:
                occupied.add(key)
                kept_selected.append(int(point_index))
            if (offset & 2047) == 0:
                _emit(progress, "downsample:voxel", offset / max(1, len(selected)))
    elif mode == "stride":
        stride = _positive_int(params.get("stride"), "stride")
        kept_selected = [
            int(point_index)
            for offset, point_index in enumerate(selected)
            if offset % stride == 0
        ]
    else:
        raise OperatorInvalid("downsample mode must be 'voxel' or 'stride'")

    _check_cancel(cancel_event)
    selected_mask = np.zeros(snapshot.count, dtype=bool)
    selected_mask[selected] = True
    selected_mask[kept_selected] = False
    kept_mask = ~selected_mask
    indices = np.flatnonzero(kept_mask)
    cloud = PointCloud.from_arrays(
        snapshot.cloud.xyz[indices], snapshot.cloud.rgb[indices]
    )
    _emit(progress, "downsample", 1.0)
    return cloud


def _color_overwrite(
    snapshot: PointCloudSnapshot,
    selection: Selection,
    params: Mapping[str, Any],
    cancel_event: threading.Event | None,
    progress: ProgressCallback | None,
) -> PointCloud:
    _unknown_parameters(params, {"rgb"})
    rgb = params.get("rgb")
    if not isinstance(rgb, (list, tuple, np.ndarray)) or len(rgb) != 3:
        raise OperatorInvalid("rgb must contain exactly three channel values")
    channels: list[int] = []
    for value in rgb:
        if isinstance(value, bool) or not isinstance(value, (int, np.integer)):
            raise OperatorInvalid("rgb channels must be integers")
        channel = int(value)
        if channel < 0 or channel > 255:
            raise OperatorInvalid("rgb channels must be in 0..255")
        channels.append(channel)
    _check_cancel(cancel_event)
    colors = np.array(snapshot.cloud.rgb, copy=True)
    colors[selection.index_array()] = np.asarray(channels, dtype=np.uint8)
    _emit(progress, "color_overwrite", 1.0)
    _check_cancel(cancel_event)
    return PointCloud.from_arrays(
        snapshot.cloud.xyz, colors, bounds=snapshot.cloud.bounds
    )


_IMPLEMENTATIONS = {
    "crop": _crop,
    "smooth": _smooth,
    "downsample": _downsample,
    "color_overwrite": _color_overwrite,
}


def run_operator(
    snapshot: PointCloudSnapshot,
    selection: Selection,
    operator: str,
    params: Mapping[str, Any] | None = None,
    *,
    cancel_event: threading.Event | None = None,
    progress: ProgressCallback | None = None,
) -> OperatorResult:
    """Run one pure operator and return a new cloud plus an audit record."""

    if operator not in _IMPLEMENTATIONS:
        raise OperatorInvalid("unknown operator", operator=operator)
    validate_selection(snapshot, selection)
    if selection.is_empty:
        raise OperatorInvalid("edit requires a non-empty selection")
    normalized_params = dict(params or {})
    _check_cancel(cancel_event)
    cloud = _IMPLEMENTATIONS[operator](
        snapshot,
        selection,
        normalized_params,
        cancel_event,
        progress,
    )
    if not isinstance(cloud, PointCloud):
        raise OperatorInvalid("operator did not return a point cloud")
    output_hash = cloud.content_hash
    record = OperationRecord(
        operator=operator,
        params=_canonical_params(normalized_params),
        input_version=snapshot.version,
        input_hash=snapshot.content_hash,
        output_hash=output_hash,
        input_count=snapshot.count,
        output_count=cloud.count,
        selection_count=selection.count,
    )
    return OperatorResult(cloud=cloud, record=record)


def _canonical_params(params: Mapping[str, Any]) -> dict[str, Any]:
    try:
        return {str(key): normalize_json_value(params[key]) for key in sorted(params)}
    except ValueError as exc:
        raise OperatorInvalid("numeric parameters must be finite") from exc
