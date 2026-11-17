from __future__ import annotations

import json
import math
import re
from pathlib import Path
from typing import Any

import numpy as np

from .errors import FrameRejected, Issue
from .io_utils import sha256_bytes
from .scene import (
    Camera,
    RenderSettings,
    Sampling,
    Scene,
    SceneLight,
    Volume,
)
from .transfer import TransferFunction, TransferSegment

FRAME_ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
INTERVAL_TOLERANCE = 1e-9


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _finite_number(value: Any) -> bool:
    return _is_number(value) and math.isfinite(float(value))


def _vec3(value: Any) -> np.ndarray | None:
    if (
        not isinstance(value, (list, tuple))
        or len(value) != 3
        or not all(_finite_number(component) for component in value)
    ):
        return None
    return np.asarray(value, dtype=np.float64)


def _pair(value: Any) -> tuple[float, float] | None:
    if (
        not isinstance(value, (list, tuple))
        or len(value) != 2
        or not all(_finite_number(component) for component in value)
    ):
        return None
    return float(value[0]), float(value[1])


def _path_label(path: Any) -> str:
    if isinstance(path, str):
        return path
    return "<value>"


def _first_indices(indices: np.ndarray, limit: int = 16) -> list[list[int]]:
    return [[int(component) for component in row] for row in indices[:limit]]


def _json_number(value: float) -> float | str:
    number = float(value)
    if math.isfinite(number):
        return number
    if math.isnan(number):
        return "NaN"
    return "Infinity" if number > 0 else "-Infinity"


def _parse_volume(
    spec: Any, scene_path: Path, issues: list[Issue]
) -> tuple[Volume | None, tuple[float, float] | None, Any, Any]:
    if not isinstance(spec, dict):
        issues.append(
            Issue(
                "VOLUME_SCHEMA_INVALID",
                "volume",
                "volume must be a JSON object",
            )
        )
        return None, None, None, None

    dims_value = spec.get("dims")
    dims: tuple[int, int, int] | None = None
    if (
        isinstance(dims_value, list)
        and len(dims_value) == 3
        and all(
            isinstance(item, int) and not isinstance(item, bool) and item > 0
            for item in dims_value
        )
    ):
        dims = (int(dims_value[0]), int(dims_value[1]), int(dims_value[2]))
        if any(item < 2 for item in dims):
            issues.append(
                Issue(
                    "VOLUME_DIMS_INVALID",
                    "volume.dims",
                    "each volume axis must contain at least two voxels",
                    {"dims": list(dims)},
                )
            )
            dims = None
        elif int(np.prod(dims)) > 100_000_000:
            issues.append(
                Issue(
                    "VOLUME_TOO_LARGE",
                    "volume.dims",
                    "volume exceeds the 100,000,000 voxel safety limit",
                    {"dims": list(dims), "voxel_count": int(np.prod(dims))},
                )
            )
            dims = None
    else:
        issues.append(
            Issue(
                "VOLUME_DIMS_INVALID",
                "volume.dims",
                "dims must be three positive integers",
                {"value": dims_value},
            )
        )

    bounds_min: np.ndarray | None = None
    bounds_max: np.ndarray | None = None
    bounds = spec.get("bounds")
    if isinstance(bounds, dict):
        bounds_min = _vec3(bounds.get("min"))
        bounds_max = _vec3(bounds.get("max"))
    if (
        bounds_min is None
        or bounds_max is None
        or not np.all(bounds_max > bounds_min)
    ):
        issues.append(
            Issue(
                "VOLUME_BOUNDS_INVALID",
                "volume.bounds",
                "bounds.min and bounds.max must be finite 3-vectors with max > min on every axis",
                {
                    "min": (
                        bounds_min.tolist() if bounds_min is not None else None
                    ),
                    "max": (
                        bounds_max.tolist() if bounds_max is not None else None
                    ),
                },
            )
        )
        bounds_min = None
        bounds_max = None

    density_range = _pair(spec.get("density_range"))
    if (
        density_range is None
        or not math.isfinite(density_range[0])
        or not math.isfinite(density_range[1])
        or density_range[0] >= density_range[1]
    ):
        issues.append(
            Issue(
                "VOLUME_DENSITY_RANGE_INVALID",
                "volume.density_range",
                "volume.density_range must be finite with min < max",
                {"value": spec.get("density_range")},
            )
        )
        density_range = None

    dtype_name = spec.get("dtype", "float32")
    if dtype_name not in {"float32", "float64"}:
        issues.append(
            Issue(
                "VOLUME_DTYPE_INVALID",
                "volume.dtype",
                "dtype must be float32 or float64",
                {"value": dtype_name},
            )
        )
        dtype = None
    else:
        dtype = np.dtype(dtype_name)

    order = spec.get("order", "C")
    if order not in {"C", "F"}:
        issues.append(
            Issue(
                "VOLUME_ORDER_INVALID",
                "volume.order",
                "order must be C or F",
                {"value": order},
            )
        )
        order = None

    if dtype is None or order is None:
        return None, density_range, bounds_min, bounds_max

    has_file = "data_file" in spec
    has_inline = "data" in spec
    if has_file == has_inline:
        issues.append(
            Issue(
                "VOLUME_DATA_INVALID",
                "volume",
                "exactly one of data_file or data must be supplied",
                {
                    "has_data_file": has_file,
                    "has_inline_data": has_inline,
                },
            )
        )
        return None, density_range, bounds_min, bounds_max

    raw_bytes: bytes | None = None
    data_path: Path | None = None
    data: np.ndarray | None = None
    expected_bytes = (
        int(np.prod(dims)) * dtype.itemsize
        if dims is not None and dtype is not None
        else None
    )

    if has_file:
        data_file = spec.get("data_file")
        if not isinstance(data_file, str) or not data_file:
            issues.append(
                Issue(
                    "VOLUME_DATA_INVALID",
                    "volume.data_file",
                    "data_file must be a non-empty string",
                )
            )
            return None, density_range, bounds_min, bounds_max
        data_path = (scene_path.parent / data_file).resolve()
        try:
            raw_bytes = data_path.read_bytes()
        except FileNotFoundError:
            issues.append(
                Issue(
                    "VOLUME_FILE_MISSING",
                    "volume.data_file",
                    "volume data file does not exist",
                    {"path": str(data_path)},
                )
            )
            return None, density_range, bounds_min, bounds_max
        except OSError as exc:
            issues.append(
                Issue(
                    "VOLUME_FILE_UNREADABLE",
                    "volume.data_file",
                    "volume data file could not be read",
                    {"path": str(data_path), "error": str(exc)},
                )
            )
            return None, density_range, bounds_min, bounds_max
    else:
        inline = spec.get("data")
        if not isinstance(inline, list):
            issues.append(
                Issue(
                    "VOLUME_DATA_INVALID",
                    "volume.data",
                    "inline data must be a one-dimensional JSON array",
                )
            )
            return None, density_range, bounds_min, bounds_max
        try:
            inline_array = np.asarray(inline, dtype=np.float64)
        except (TypeError, ValueError) as exc:
            issues.append(
                Issue(
                    "VOLUME_DATA_INVALID",
                    "volume.data",
                    "inline data contains non-numeric values",
                    {"error": str(exc)},
                )
            )
            return None, density_range, bounds_min, bounds_max
        if inline_array.ndim != 1:
            issues.append(
                Issue(
                    "VOLUME_DATA_INVALID",
                    "volume.data",
                    "inline data must be flat",
                )
            )
            return None, density_range, bounds_min, bounds_max
        if expected_bytes is not None:
            expected_values = expected_bytes // dtype.itemsize
            if inline_array.size < expected_values:
                issues.append(
                    Issue(
                        "VOLUME_TRUNCATED",
                        "volume.data",
                        "inline voxel count is smaller than dims require",
                        {
                            "expected_voxels": expected_values,
                            "actual_voxels": int(inline_array.size),
                        },
                    )
                )
                return None, density_range, bounds_min, bounds_max
            if inline_array.size > expected_values:
                issues.append(
                    Issue(
                        "VOLUME_LENGTH_MISMATCH",
                        "volume.data",
                        "inline voxel count is larger than dims require",
                        {
                            "expected_voxels": expected_values,
                            "actual_voxels": int(inline_array.size),
                        },
                    )
                )
                return None, density_range, bounds_min, bounds_max
        raw_bytes = inline_array.astype(np.dtype(dtype_name), copy=False).tobytes()

    assert raw_bytes is not None
    actual_bytes = len(raw_bytes)
    if expected_bytes is not None:
        if actual_bytes < expected_bytes:
            issues.append(
                Issue(
                    "VOLUME_TRUNCATED",
                    "volume.data_file",
                    "volume data file is shorter than dims * dtype require",
                    {
                        "path": str(data_path) if data_path else None,
                        "expected_bytes": expected_bytes,
                        "actual_bytes": actual_bytes,
                        "missing_bytes": expected_bytes - actual_bytes,
                    },
                )
            )
            return None, density_range, bounds_min, bounds_max
        if actual_bytes > expected_bytes:
            issues.append(
                Issue(
                    "VOLUME_LENGTH_MISMATCH",
                    "volume.data_file",
                    "volume data file has extra bytes beyond dims * dtype",
                    {
                        "path": str(data_path) if data_path else None,
                        "expected_bytes": expected_bytes,
                        "actual_bytes": actual_bytes,
                        "extra_bytes": actual_bytes - expected_bytes,
                    },
                )
            )
            return None, density_range, bounds_min, bounds_max

    declared_hash = spec.get("sha256")
    actual_hash = sha256_bytes(raw_bytes)
    if declared_hash is not None:
        if (
            not isinstance(declared_hash, str)
            or len(declared_hash) != 64
            or declared_hash.lower() != actual_hash
        ):
            issues.append(
                Issue(
                    "VOLUME_HASH_MISMATCH",
                    "volume.sha256",
                    "declared volume sha256 does not match the input bytes",
                    {
                        "declared": declared_hash,
                        "actual": actual_hash,
                    },
                )
            )

    if dims is None or dtype is None or order is None:
        return None, density_range, bounds_min, bounds_max

    try:
        data = np.frombuffer(raw_bytes, dtype=dtype, count=int(np.prod(dims)))
        data = data.reshape(dims, order=order)
    except ValueError as exc:
        issues.append(
            Issue(
                "VOLUME_DATA_INVALID",
                "volume.data",
                "voxel bytes cannot be reshaped using dims and order",
                {"error": str(exc), "dims": list(dims), "order": order},
            )
        )
        return None, density_range, bounds_min, bounds_max

    nonfinite = np.argwhere(~np.isfinite(data))
    if nonfinite.size:
        indices = _first_indices(nonfinite)
        issues.append(
            Issue(
                "VOLUME_NONFINITE",
                "volume.data",
                "volume contains NaN or infinite density values",
                {
                    "count": int(nonfinite.shape[0]),
                    "first_indices": indices,
                    "first_values": [
                        _json_number(data[tuple(index)])
                        for index in nonfinite[:16]
                    ],
                },
            )
        )

    if density_range is not None:
        finite_data = data[np.isfinite(data)]
        finite_min = float(finite_data.min()) if finite_data.size else None
        finite_max = float(finite_data.max()) if finite_data.size else None
        violation_mask = (data < density_range[0]) | (
            data > density_range[1]
        )
        violations = np.argwhere(violation_mask)
        if violations.size:
            issues.append(
                Issue(
                    "VOLUME_RANGE_VIOLATION",
                    "volume.data",
                    "density values fall outside volume.density_range",
                    {
                        "count": int(violations.shape[0]),
                        "density_range": list(density_range),
                        "finite_min": finite_min,
                        "finite_max": finite_max,
                        "first_indices": _first_indices(violations),
                        "first_values": [
                            _json_number(data[tuple(index)])
                            for index in violations[:16]
                        ],
                    },
                )
            )
    if nonfinite.size or (
        density_range is not None
        and np.any(
            (data[np.isfinite(data)] < density_range[0])
            | (data[np.isfinite(data)] > density_range[1])
        )
    ):
        return None, density_range, bounds_min, bounds_max

    if (
        dims is None
        or bounds_min is None
        or bounds_max is None
        or density_range is None
    ):
        return None, density_range, bounds_min, bounds_max

    volume = Volume(
        dims=dims,
        bounds_min=bounds_min,
        bounds_max=bounds_max,
        density_range=density_range,
        data=data,
        data_sha256=actual_hash,
        data_path=data_path,
    )
    return volume, density_range, bounds_min, bounds_max


def _parse_absorption_pair(value: Any) -> tuple[float, float] | None:
    if _finite_number(value):
        number = float(value)
        return number, number
    return _pair(value)


def _parse_emission_pair(value: Any) -> tuple[np.ndarray, np.ndarray] | None:
    if (
        isinstance(value, (list, tuple))
        and len(value) == 3
        and all(_finite_number(component) for component in value)
    ):
        color = np.asarray(value, dtype=np.float64)
        return color.copy(), color.copy()
    if (
        not isinstance(value, (list, tuple))
        or len(value) != 2
    ):
        return None
    start = _vec3(value[0])
    end = _vec3(value[1])
    if start is None or end is None:
        return None
    return start, end


def _parse_transfer(
    spec: Any,
    volume_density_range: tuple[float, float] | None,
    issues: list[Issue],
) -> TransferFunction | None:
    if not isinstance(spec, dict):
        issues.append(
            Issue(
                "TF_SCHEMA_INVALID",
                "transfer",
                "transfer must be a JSON object",
            )
        )
        return None

    density_range = _pair(spec.get("density_range"))
    if (
        density_range is None
        or density_range[0] >= density_range[1]
    ):
        issues.append(
            Issue(
                "TF_DENSITY_RANGE_INVALID",
                "transfer.density_range",
                "transfer.density_range must be finite with min < max",
                {"value": spec.get("density_range")},
            )
        )
        density_range = None

    if (
        density_range is not None
        and volume_density_range is not None
        and density_range != volume_density_range
    ):
        issues.append(
            Issue(
                "TF_DENSITY_RANGE_MISMATCH",
                "transfer.density_range",
                "transfer.density_range must equal volume.density_range",
                {
                    "transfer": list(density_range),
                    "volume": list(volume_density_range),
                },
            )
        )

    transition_width_value = spec.get("transition_width", 0.0)
    if not _finite_number(transition_width_value) or float(
        transition_width_value
    ) < 0.0:
        issues.append(
            Issue(
                "TF_TRANSITION_INVALID",
                "transfer.transition_width",
                "transition_width must be a finite non-negative number",
                {"value": transition_width_value},
            )
        )
        transition_width = 0.0
    else:
        transition_width = float(transition_width_value)

    segment_values = spec.get("segments")
    if not isinstance(segment_values, list) or not segment_values:
        issues.append(
            Issue(
                "TF_SCHEMA_INVALID",
                "transfer.segments",
                "segments must be a non-empty array",
            )
        )
        return None

    segments: list[TransferSegment] = []
    segments_valid = True
    for index, segment_value in enumerate(segment_values):
        if not isinstance(segment_value, dict):
            issues.append(
                Issue(
                    "TF_SCHEMA_INVALID",
                    f"transfer.segments[{index}]",
                    "each transfer segment must be an object",
                )
            )
            segments_valid = False
            continue
        interval = _pair(segment_value.get("range"))
        absorption = _parse_absorption_pair(segment_value.get("absorption"))
        emission = _parse_emission_pair(segment_value.get("emission"))
        local_valid = True
        if interval is None or interval[0] >= interval[1]:
            issues.append(
                Issue(
                    "TF_INTERVAL_INVALID",
                    f"transfer.segments[{index}].range",
                    "segment range must contain two finite numbers with start < end",
                    {"value": segment_value.get("range")},
                )
            )
            local_valid = False
        if absorption is None:
            issues.append(
                Issue(
                    "TF_ABSORPTION_INVALID",
                    f"transfer.segments[{index}].absorption",
                    "absorption must be a non-negative number or two-number pair",
                    {"value": segment_value.get("absorption")},
                )
            )
            local_valid = False
        elif absorption[0] < 0.0 or absorption[1] < 0.0:
            issues.append(
                Issue(
                    "TF_ABSORPTION_INVALID",
                    f"transfer.segments[{index}].absorption",
                    "absorption values must be non-negative",
                    {"value": segment_value.get("absorption")},
                )
            )
            local_valid = False
        if emission is None:
            issues.append(
                Issue(
                    "TF_EMISSION_INVALID",
                    f"transfer.segments[{index}].emission",
                    "emission must be an RGB color or a pair of RGB colors",
                    {"value": segment_value.get("emission")},
                )
            )
            local_valid = False
        elif np.any(emission[0] < 0.0) or np.any(emission[1] < 0.0):
            issues.append(
                Issue(
                    "TF_EMISSION_INVALID",
                    f"transfer.segments[{index}].emission",
                    "emission color components must be non-negative",
                    {"value": segment_value.get("emission")},
                )
            )
            local_valid = False
        if not local_valid:
            segments_valid = False
            continue
        assert interval is not None
        assert absorption is not None
        assert emission is not None
        segments.append(
            TransferSegment(
                index=index,
                start=interval[0],
                end=interval[1],
                absorption_start=absorption[0],
                absorption_end=absorption[1],
                emission_start=emission[0],
                emission_end=emission[1],
            )
        )

    if not segments_valid or len(segments) != len(segment_values):
        return None

    for index in range(1, len(segments)):
        previous = segments[index - 1]
        current = segments[index]
        if current.start < previous.start - INTERVAL_TOLERANCE:
            issues.append(
                Issue(
                    "TF_INTERVAL_ORDER",
                    f"transfer.segments[{index}].range",
                    "segment ranges must be supplied in increasing order",
                    {
                        "previous_start": previous.start,
                        "current_start": current.start,
                    },
                )
            )
        if current.start < previous.end - INTERVAL_TOLERANCE:
            issues.append(
                Issue(
                    "TF_INTERVAL_OVERLAP",
                    f"transfer.segments[{index}].range",
                    "adjacent transfer intervals overlap",
                    {
                        "previous_range": [previous.start, previous.end],
                        "current_range": [current.start, current.end],
                        "overlap": previous.end - current.start,
                    },
                )
            )
        elif current.start > previous.end + INTERVAL_TOLERANCE:
            issues.append(
                Issue(
                    "TF_INTERVAL_GAP",
                    f"transfer.segments[{index}].range",
                    "gap exists between adjacent transfer intervals",
                    {
                        "previous_range": [previous.start, previous.end],
                        "current_range": [current.start, current.end],
                        "gap": current.start - previous.end,
                    },
                )
            )

    if density_range is not None:
        first = segments[0]
        last = segments[-1]
        if first.start < density_range[0] - INTERVAL_TOLERANCE:
            issues.append(
                Issue(
                    "TF_RANGE_EXCEEDED",
                    "transfer.segments[0].range",
                    "first segment starts below transfer.density_range",
                    {
                        "segment_start": first.start,
                        "density_min": density_range[0],
                    },
                )
            )
        elif first.start > density_range[0] + INTERVAL_TOLERANCE:
            issues.append(
                Issue(
                    "TF_RANGE_UNCOVERED",
                    "transfer.segments[0].range",
                    "transfer function does not cover the low end of its density range",
                    {
                        "segment_start": first.start,
                        "density_min": density_range[0],
                    },
                )
            )
        if last.end > density_range[1] + INTERVAL_TOLERANCE:
            issues.append(
                Issue(
                    "TF_RANGE_EXCEEDED",
                    f"transfer.segments[{last.index}].range",
                    "last segment ends above transfer.density_range",
                    {
                        "segment_end": last.end,
                        "density_max": density_range[1],
                    },
                )
            )
        elif last.end < density_range[1] - INTERVAL_TOLERANCE:
            issues.append(
                Issue(
                    "TF_RANGE_UNCOVERED",
                    f"transfer.segments[{last.index}].range",
                    "transfer function does not cover the high end of its density range",
                    {
                        "segment_end": last.end,
                        "density_max": density_range[1],
                    },
                )
            )

    if transition_width > 0.0:
        for left, right in zip(segments[:-1], segments[1:]):
            maximum_width = 2.0 * min(left.width, right.width)
            if transition_width > maximum_width + INTERVAL_TOLERANCE:
                issues.append(
                    Issue(
                        "TF_TRANSITION_INVALID",
                        "transfer.transition_width",
                        "transition width overlaps more than one neighboring segment",
                        {
                            "transition_width": transition_width,
                            "boundary": left.end,
                            "maximum_width": maximum_width,
                        },
                    )
                )
                break

    if transition_width == 0.0:
        for left, right in zip(segments[:-1], segments[1:]):
            absorption_delta = abs(
                left.absorption_end - right.absorption_start
            )
            emission_delta = float(
                np.max(np.abs(left.emission_end - right.emission_start))
            )
            if (
                absorption_delta > INTERVAL_TOLERANCE
                or emission_delta > INTERVAL_TOLERANCE
            ):
                issues.append(
                    Issue(
                        "TF_BOUNDARY_DISCONTINUITY",
                        f"transfer.segments[{left.index}]/[{right.index}]",
                        "transfer values jump at a shared boundary without a transition width",
                        {
                            "boundary": left.end,
                            "left_absorption": left.absorption_end,
                            "right_absorption": right.absorption_start,
                            "absorption_delta": absorption_delta,
                            "left_emission": left.emission_end.tolist(),
                            "right_emission": right.emission_start.tolist(),
                            "emission_delta": emission_delta,
                        },
                    )
                )

    if density_range is None:
        return None
    return TransferFunction(
        density_min=density_range[0],
        density_max=density_range[1],
        transition_width=transition_width,
        segments=tuple(segments),
    )


def _parse_camera(spec: Any, issues: list[Issue]) -> Camera | None:
    if not isinstance(spec, dict):
        issues.append(
            Issue("CAMERA_SCHEMA_INVALID", "camera", "camera must be an object")
        )
        return None

    position = _vec3(spec.get("position"))
    if position is None:
        issues.append(
            Issue(
                "CAMERA_SCHEMA_INVALID",
                "camera.position",
                "camera.position must be a finite 3-vector",
            )
        )
        return None

    has_look_at = "look_at" in spec
    has_direction = "direction" in spec
    look_at = _vec3(spec.get("look_at")) if has_look_at else None
    declared_direction = (
        _vec3(spec.get("direction")) if has_direction else None
    )
    if has_look_at == has_direction:
        issues.append(
            Issue(
                "CAMERA_BASIS_INVALID",
                "camera",
                "exactly one of look_at or direction must be supplied",
            )
        )
        return None
    if has_look_at and look_at is None:
        issues.append(
            Issue(
                "CAMERA_BASIS_INVALID",
                "camera.look_at",
                "look_at must be a finite 3-vector",
            )
        )
        return None
    if has_direction and declared_direction is None:
        issues.append(
            Issue(
                "CAMERA_BASIS_INVALID",
                "camera.direction",
                "direction must be a finite 3-vector",
            )
        )
        return None

    if look_at is not None:
        forward = look_at - position
    else:
        assert declared_direction is not None
        forward = declared_direction
    forward_norm = float(np.linalg.norm(forward))
    if forward_norm < 1e-12:
        issues.append(
            Issue(
                "CAMERA_BASIS_INVALID",
                "camera",
                "camera forward direction must be non-zero",
            )
        )
        return None
    forward = forward / forward_norm

    up_value = _vec3(spec.get("up"))
    if up_value is None or float(np.linalg.norm(up_value)) < 1e-12:
        issues.append(
            Issue(
                "CAMERA_BASIS_INVALID",
                "camera.up",
                "camera.up must be a non-zero finite 3-vector",
            )
        )
        return None
    up_hint = up_value / np.linalg.norm(up_value)
    right = np.cross(forward, up_hint)
    right_norm = float(np.linalg.norm(right))
    if right_norm < 1e-9:
        issues.append(
            Issue(
                "CAMERA_BASIS_INVALID",
                "camera.up",
                "camera.up must not be parallel to the view direction",
            )
        )
        return None
    right /= right_norm
    up = np.cross(right, forward)
    up /= np.linalg.norm(up)

    width = spec.get("width")
    height = spec.get("height")
    if (
        not isinstance(width, int)
        or isinstance(width, bool)
        or width <= 0
        or not isinstance(height, int)
        or isinstance(height, bool)
        or height <= 0
    ):
        issues.append(
            Issue(
                "CAMERA_ASPECT_INVALID",
                "camera.width/height",
                "camera width and height must be positive integers",
                {"width": width, "height": height},
            )
        )
        return None

    fov_value = spec.get("fov_y_degrees")
    if not _finite_number(fov_value) or not 0.0 < float(fov_value) < 180.0:
        issues.append(
            Issue(
                "CAMERA_FOV_INVALID",
                "camera.fov_y_degrees",
                "vertical field of view must be finite and strictly between 0 and 180 degrees",
                {"value": fov_value},
            )
        )
        return None
    fov = float(fov_value)

    pixel_aspect = width / height
    aspect_value = spec.get("aspect_ratio", pixel_aspect)
    if not _finite_number(aspect_value) or float(aspect_value) <= 0.0:
        issues.append(
            Issue(
                "CAMERA_ASPECT_INVALID",
                "camera.aspect_ratio",
                "aspect_ratio must be finite and positive",
                {"value": aspect_value},
            )
        )
        return None
    aspect = float(aspect_value)
    if not math.isclose(
        aspect, pixel_aspect, rel_tol=1e-5, abs_tol=1e-7
    ):
        issues.append(
            Issue(
                "CAMERA_ASPECT_INVALID",
                "camera.aspect_ratio",
                "aspect_ratio must match width / height",
                {
                    "aspect_ratio": aspect,
                    "width_over_height": pixel_aspect,
                },
            )
        )
        return None

    return Camera(
        position=position,
        forward=forward,
        right=right,
        up=up,
        fov_y_degrees=fov,
        aspect_ratio=aspect,
        width=width,
        height=height,
    )


def _parse_lights(
    spec: Any,
    bounds_min: np.ndarray | None,
    bounds_max: np.ndarray | None,
    issues: list[Issue],
) -> tuple[SceneLight, ...]:
    if not isinstance(spec, list):
        issues.append(
            Issue("LIGHT_SCHEMA_INVALID", "lights", "lights must be an array")
        )
        return tuple()

    parsed: list[SceneLight] = []
    seen_ids: set[str] = set()
    for index, light_value in enumerate(spec):
        scope = f"lights[{index}]"
        if not isinstance(light_value, dict):
            issues.append(
                Issue(
                    "LIGHT_SCHEMA_INVALID",
                    scope,
                    "light must be an object",
                )
            )
            continue
        light_id = light_value.get("id")
        if not isinstance(light_id, str) or not light_id:
            issues.append(
                Issue(
                    "LIGHT_SCHEMA_INVALID",
                    f"{scope}.id",
                    "light id must be a non-empty string",
                )
            )
            continue
        if light_id in seen_ids:
            issues.append(
                Issue(
                    "LIGHT_ID_DUPLICATE",
                    f"{scope}.id",
                    "light ids must be unique",
                    {"id": light_id},
                )
            )
            continue
        seen_ids.add(light_id)

        kind = light_value.get("type")
        color = _vec3(light_value.get("color"))
        intensity = light_value.get("intensity")
        phase_g = light_value.get("phase_g", 0.0)
        min_distance = light_value.get("min_distance", 0.05)
        local_valid = True
        if kind not in {"point", "directional"}:
            issues.append(
                Issue(
                    "LIGHT_SCHEMA_INVALID",
                    f"{scope}.type",
                    "light type must be point or directional",
                    {"value": kind},
                )
            )
            local_valid = False
        if color is None or np.any(color < 0.0):
            issues.append(
                Issue(
                    "LIGHT_COLOR_INVALID",
                    f"{scope}.color",
                    "light color must be a non-negative finite RGB vector",
                )
            )
            local_valid = False
        if not _finite_number(intensity) or float(intensity) < 0.0:
            issues.append(
                Issue(
                    "LIGHT_INTENSITY_INVALID",
                    f"{scope}.intensity",
                    "light intensity must be finite and non-negative",
                    {"value": intensity},
                )
            )
            local_valid = False
        if (
            not _finite_number(phase_g)
            or not -1.0 < float(phase_g) < 1.0
        ):
            issues.append(
                Issue(
                    "LIGHT_PHASE_INVALID",
                    f"{scope}.phase_g",
                    "phase_g must be finite and strictly between -1 and 1",
                    {"value": phase_g},
                )
            )
            local_valid = False
        if not _finite_number(min_distance) or float(min_distance) <= 0.0:
            issues.append(
                Issue(
                    "LIGHT_DISTANCE_INVALID",
                    f"{scope}.min_distance",
                    "min_distance must be finite and positive",
                    {"value": min_distance},
                )
            )
            local_valid = False

        position: np.ndarray | None = None
        direction: np.ndarray | None = None
        if kind == "point":
            position = _vec3(light_value.get("position"))
            if position is None:
                issues.append(
                    Issue(
                        "LIGHT_SCHEMA_INVALID",
                        f"{scope}.position",
                        "point light position must be a finite 3-vector",
                    )
                )
                local_valid = False
            elif (
                bounds_min is not None
                and bounds_max is not None
                and np.all(position > bounds_min)
                and np.all(position < bounds_max)
            ):
                issues.append(
                    Issue(
                        "LIGHT_INSIDE_VOLUME",
                        f"{scope}.position",
                        "point light position lies strictly inside the density volume",
                        {
                            "id": light_id,
                            "position": position.tolist(),
                            "bounds_min": bounds_min.tolist(),
                            "bounds_max": bounds_max.tolist(),
                        },
                    )
                )
                local_valid = False
        elif kind == "directional":
            direction = _vec3(light_value.get("direction"))
            if direction is None or float(np.linalg.norm(direction)) < 1e-12:
                issues.append(
                    Issue(
                        "LIGHT_SCHEMA_INVALID",
                        f"{scope}.direction",
                        "directional light direction must be a non-zero finite 3-vector",
                    )
                )
                local_valid = False
            else:
                direction = direction / np.linalg.norm(direction)

        if not local_valid:
            continue
        assert color is not None
        parsed.append(
            SceneLight(
                id=light_id,
                kind=str(kind),
                color=color,
                intensity=float(intensity),
                position=position,
                direction=direction,
                phase_g=float(phase_g),
                min_distance=float(min_distance),
            )
        )

    return tuple(sorted(parsed, key=lambda light: light.id))


def _parse_sampling(spec: Any, issues: list[Issue]) -> Sampling | None:
    if not isinstance(spec, dict):
        issues.append(
            Issue(
                "SAMPLING_INVALID",
                "sampling",
                "sampling must be an object",
            )
        )
        return None
    reference_step = spec.get("reference_step")
    refinement_factor = spec.get("refinement_factor", 2.0)
    convergence_levels = spec.get("convergence_levels", 3)
    tolerance = spec.get("convergence_tolerance")
    threshold = spec.get("suspicious_opacity_jump_threshold")
    local_valid = True
    if not _finite_number(reference_step) or float(reference_step) <= 0.0:
        issues.append(
            Issue(
                "SAMPLING_INVALID",
                "sampling.reference_step",
                "reference_step must be finite and positive",
                {"value": reference_step},
            )
        )
        local_valid = False
    if (
        not _finite_number(refinement_factor)
        or float(refinement_factor) <= 1.0
    ):
        issues.append(
            Issue(
                "SAMPLING_INVALID",
                "sampling.refinement_factor",
                "refinement_factor must be finite and greater than 1",
                {"value": refinement_factor},
            )
        )
        local_valid = False
    if (
        not isinstance(convergence_levels, int)
        or isinstance(convergence_levels, bool)
        or convergence_levels < 2
    ):
        issues.append(
            Issue(
                "SAMPLING_INVALID",
                "sampling.convergence_levels",
                "convergence_levels must be an integer of at least 2",
                {"value": convergence_levels},
            )
        )
        local_valid = False
    if not _finite_number(tolerance) or float(tolerance) < 0.0:
        issues.append(
            Issue(
                "SAMPLING_INVALID",
                "sampling.convergence_tolerance",
                "convergence_tolerance must be finite and non-negative",
                {"value": tolerance},
            )
        )
        local_valid = False
    if (
        not _finite_number(threshold)
        or not 0.0 < float(threshold) <= 1.0
    ):
        issues.append(
            Issue(
                "SAMPLING_INVALID",
                "sampling.suspicious_opacity_jump_threshold",
                "suspicious_opacity_jump_threshold must be in (0, 1]",
                {"value": threshold},
            )
        )
        local_valid = False
    if not local_valid:
        return None
    return Sampling(
        reference_step=float(reference_step),
        refinement_factor=float(refinement_factor),
        convergence_levels=int(convergence_levels),
        convergence_tolerance=float(tolerance),
        suspicious_opacity_jump_threshold=float(threshold),
    )


def _parse_render(spec: Any, issues: list[Issue]) -> RenderSettings | None:
    if not isinstance(spec, dict):
        issues.append(
            Issue("RENDER_CONFIG_INVALID", "render", "render must be an object")
        )
        return None
    chunk_size = spec.get("chunk_size")
    if isinstance(chunk_size, int) and not isinstance(chunk_size, bool):
        chunk_width = chunk_height = chunk_size
    elif (
        isinstance(chunk_size, list)
        and len(chunk_size) == 2
        and all(
            isinstance(item, int)
            and not isinstance(item, bool)
            and item > 0
            for item in chunk_size
        )
    ):
        chunk_width, chunk_height = int(chunk_size[0]), int(chunk_size[1])
    else:
        issues.append(
            Issue(
                "RENDER_CONFIG_INVALID",
                "render.chunk_size",
                "chunk_size must be a positive integer or [width, height]",
                {"value": chunk_size},
            )
        )
        return None
    exposure = spec.get("exposure", 1.0)
    gamma = spec.get("gamma", 2.2)
    if not _finite_number(exposure) or float(exposure) <= 0.0:
        issues.append(
            Issue(
                "RENDER_CONFIG_INVALID",
                "render.exposure",
                "exposure must be finite and positive",
                {"value": exposure},
            )
        )
        return None
    if not _finite_number(gamma) or float(gamma) <= 0.0:
        issues.append(
            Issue(
                "RENDER_CONFIG_INVALID",
                "render.gamma",
                "gamma must be finite and positive",
                {"value": gamma},
            )
        )
        return None
    return RenderSettings(
        chunk_width=chunk_width,
        chunk_height=chunk_height,
        exposure=float(exposure),
        gamma=float(gamma),
    )


def load_validated_scene(scene_path: Path | str) -> Scene:
    path = Path(scene_path).resolve()
    fallback_frame_id = path.stem or "frame"
    try:
        scene_bytes = path.read_bytes()
    except FileNotFoundError:
        raise FrameRejected(
            fallback_frame_id,
            [
                Issue(
                    "SCENE_FILE_MISSING",
                    "scene",
                    "scene JSON file does not exist",
                    {"path": str(path)},
                )
            ],
        )
    except OSError as exc:
        raise FrameRejected(
            fallback_frame_id,
            [
                Issue(
                    "SCENE_FILE_UNREADABLE",
                    "scene",
                    "scene JSON file could not be read",
                    {"path": str(path), "error": str(exc)},
                )
            ],
        )
    scene_hash = sha256_bytes(scene_bytes)
    try:
        raw = json.loads(scene_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        details: dict[str, Any] = {"error": str(exc)}
        if isinstance(exc, json.JSONDecodeError):
            details.update({"line": exc.lineno, "column": exc.colno})
        raise FrameRejected(
            fallback_frame_id,
            [
                Issue(
                    "SCENE_JSON_INVALID",
                    "scene",
                    "scene is not valid UTF-8 JSON",
                    details,
                )
            ],
        )
    if not isinstance(raw, dict):
        raise FrameRejected(
            fallback_frame_id,
            [
                Issue(
                    "SCENE_JSON_INVALID",
                    "scene",
                    "scene root must be a JSON object",
                )
            ],
        )

    issues: list[Issue] = []
    frame_id_value = raw.get("frame_id")
    if (
        not isinstance(frame_id_value, str)
        or not FRAME_ID_PATTERN.fullmatch(frame_id_value)
    ):
        issues.append(
            Issue(
                "FRAME_ID_INVALID",
                "frame_id",
                "frame_id must match ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$",
                {"value": frame_id_value},
            )
        )
        frame_id = fallback_frame_id
    else:
        frame_id = frame_id_value

    schema_version = raw.get("schema_version")
    if schema_version != 1:
        issues.append(
            Issue(
                "SCENE_SCHEMA_UNSUPPORTED",
                "schema_version",
                "only schema_version 1 is supported",
                {"value": schema_version},
            )
        )

    volume, volume_range, bounds_min, bounds_max = _parse_volume(
        raw.get("volume"), path, issues
    )
    transfer = _parse_transfer(raw.get("transfer"), volume_range, issues)
    camera = _parse_camera(raw.get("camera"), issues)
    lights = _parse_lights(raw.get("lights"), bounds_min, bounds_max, issues)
    sampling = _parse_sampling(raw.get("sampling"), issues)
    render = _parse_render(raw.get("render"), issues)

    if issues:
        raise FrameRejected(frame_id, issues)
    assert volume is not None
    assert transfer is not None
    assert camera is not None
    assert sampling is not None
    assert render is not None
    return Scene(
        frame_id=frame_id,
        scene_path=path,
        raw_scene=raw,
        scene_json_sha256=scene_hash,
        volume=volume,
        camera=camera,
        lights=lights,
        transfer=transfer,
        sampling=sampling,
        render=render,
    )

