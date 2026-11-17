"""Scene JSON loading and strict structured validation."""

from __future__ import annotations

import json
import math
from pathlib import Path

from .errors import (
    BusinessError,
    DRY_FILE_MISSING,
    DUPLICATE_SOURCE_ID,
    INVALID_SCENE,
    PARAM_RANGE,
    PATH_TIME_ORDER,
    SAMPLE_RATE_MISMATCH,
    TRANSMISSION_RANGE,
)
from .wavio import read_wav

LAYER_TYPES = {"colonnade", "arch", "fence"}
SOURCE_KINDS = {"organ", "drip", "footstep"}


def _is_number(value) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _vec3(value) -> bool:
    return (
        isinstance(value, (list, tuple))
        and len(value) == 3
        and all(_is_number(component) for component in value)
    )


def load_scene(path: str | Path) -> dict:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(handle)
    except FileNotFoundError as exc:
        raise BusinessError(INVALID_SCENE, "$", f"scene file not found: {Path(path).name}") from exc
    except json.JSONDecodeError as exc:
        raise BusinessError(
            INVALID_SCENE, f"$[{exc.lineno}:{exc.colno}]", f"invalid JSON: {exc.msg}"
        ) from exc


def validate_scene(scene: dict, base_dir: str | Path | None = None) -> list[BusinessError]:
    """Validate a scene dict. When base_dir is given, dry files are also checked."""
    errors: list[BusinessError] = []

    if not isinstance(scene, dict):
        return [BusinessError(INVALID_SCENE, "$", "scene root must be an object")]

    project = scene.get("project")
    if not isinstance(project, dict):
        errors.append(BusinessError(INVALID_SCENE, "$.project", "missing project object"))
        return errors

    project_required = {
        "sample_rate": int,
        "frame_rate": (int, float),
        "block_samples": int,
        "duration": (int, float),
        "seed": int,
        "distance_ref": (int, float),
        "distance_gain_exponent": (int, float),
        "diffraction_db_per_m": (int, float),
        "max_rt60_rate_s": (int, float),
    }
    for key, expected in project_required.items():
        if key not in project:
            errors.append(BusinessError(INVALID_SCENE, f"$.project.{key}", "missing field"))
        elif not isinstance(project[key], expected) or isinstance(project[key], bool):
            errors.append(BusinessError(INVALID_SCENE, f"$.project.{key}", "wrong type"))
        elif not _is_number(project[key]):
            errors.append(BusinessError(PARAM_RANGE, f"$.project.{key}", "must be finite number"))
    if not errors:
        positive_fields = ("sample_rate", "frame_rate", "block_samples", "duration", "distance_ref")
        for key in positive_fields:
            if project[key] <= 0:
                errors.append(BusinessError(PARAM_RANGE, f"$.project.{key}", "must be > 0"))
        for key in ("distance_gain_exponent", "diffraction_db_per_m", "max_rt60_rate_s"):
            if project[key] < 0:
                errors.append(BusinessError(PARAM_RANGE, f"$.project.{key}", "must be >= 0"))

    sources = scene.get("sources")
    if not isinstance(sources, list) or not sources:
        errors.append(BusinessError(INVALID_SCENE, "$.sources", "must be a non-empty list"))
    else:
        seen_ids: set[str] = set()
        for index, source in enumerate(sources):
            path = f"$.sources[{index}]"
            if not isinstance(source, dict):
                errors.append(BusinessError(INVALID_SCENE, path, "source must be an object"))
                continue
            sid = source.get("id")
            if not isinstance(sid, str) or not sid:
                errors.append(BusinessError(INVALID_SCENE, f"{path}.id", "missing string id"))
            elif sid in seen_ids:
                errors.append(BusinessError(DUPLICATE_SOURCE_ID, f"{path}.id", f"duplicate id {sid}"))
            else:
                seen_ids.add(sid)
            if not _vec3(source.get("position")):
                errors.append(BusinessError(INVALID_SCENE, f"{path}.position", "must be [x, y, z]"))
            if not isinstance(source.get("file"), str) or not source["file"]:
                errors.append(BusinessError(INVALID_SCENE, f"{path}.file", "missing dry file path"))
            if source.get("kind") not in SOURCE_KINDS:
                errors.append(BusinessError(INVALID_SCENE, f"{path}.kind", f"must be one of {sorted(SOURCE_KINDS)}"))
            if not _is_number(source.get("gain")):
                errors.append(BusinessError(INVALID_SCENE, f"{path}.gain", "missing numeric gain"))
            elif not (-60.0 <= source["gain"] <= 24.0):
                errors.append(BusinessError(PARAM_RANGE, f"{path}.gain", "gain dB must be in [-60, 24]"))

    path_points = scene.get("path")
    if not isinstance(path_points, list) or len(path_points) < 2:
        errors.append(BusinessError(INVALID_SCENE, "$.path", "must contain at least 2 samples"))
    else:
        previous_time = None
        for index, point in enumerate(path_points):
            point_path = f"$.path[{index}]"
            if not isinstance(point, dict):
                errors.append(BusinessError(INVALID_SCENE, point_path, "path sample must be object"))
                continue
            if not _is_number(point.get("t")):
                errors.append(BusinessError(INVALID_SCENE, f"{point_path}.t", "missing numeric time"))
            elif previous_time is not None and point["t"] < previous_time:
                errors.append(
                    BusinessError(PATH_TIME_ORDER, f"{point_path}.t",
                                  f"time {point['t']} precedes {previous_time}")
                )
            if isinstance(point.get("t"), (int, float)):
                previous_time = point["t"]
            if not _vec3(point.get("position")):
                errors.append(BusinessError(INVALID_SCENE, f"{point_path}.position", "must be [x, y, z]"))
            if not _is_number(point.get("yaw")):
                errors.append(BusinessError(INVALID_SCENE, f"{point_path}.yaw", "missing numeric yaw (rad)"))

    layers = scene.get("layers")
    if not isinstance(layers, list) or not layers:
        errors.append(BusinessError(INVALID_SCENE, "$.layers", "must be a non-empty list"))
    else:
        layer_ids: set[str] = set()
        for index, layer in enumerate(layers):
            layer_path = f"$.layers[{index}]"
            if not isinstance(layer, dict):
                errors.append(BusinessError(INVALID_SCENE, layer_path, "layer must be an object"))
                continue
            lid = layer.get("id")
            if not isinstance(lid, str) or not lid:
                errors.append(BusinessError(INVALID_SCENE, f"{layer_path}.id", "missing string id"))
            elif lid in layer_ids:
                errors.append(BusinessError(DUPLICATE_SOURCE_ID, f"{layer_path}.id",
                                            f"duplicate layer id {lid}"))
            else:
                layer_ids.add(lid)
            if layer.get("type") not in LAYER_TYPES:
                errors.append(BusinessError(INVALID_SCENE, f"{layer_path}.type",
                                            f"must be one of {sorted(LAYER_TYPES)}"))
            if not _is_number(layer.get("plane")):
                errors.append(BusinessError(INVALID_SCENE, f"{layer_path}.plane", "missing numeric plane x"))
            transmission = layer.get("transmission")
            if not _is_number(transmission):
                errors.append(BusinessError(INVALID_SCENE, f"{layer_path}.transmission", "missing number"))
            elif not (0.0 < transmission <= 1.0):
                errors.append(BusinessError(TRANSMISSION_RANGE, f"{layer_path}.transmission",
                                            f"{transmission} not in (0, 1]"))
            reverb = layer.get("reverb")
            if not isinstance(reverb, dict):
                errors.append(BusinessError(INVALID_SCENE, f"{layer_path}.reverb", "missing reverb object"))
            else:
                rt60 = reverb.get("rt60")
                wet = reverb.get("wet")
                if not _is_number(rt60) or rt60 <= 0:
                    errors.append(BusinessError(PARAM_RANGE, f"{layer_path}.reverb.rt60", "must be > 0"))
                if not _is_number(wet) or not (0.0 <= wet <= 1.0):
                    errors.append(BusinessError(PARAM_RANGE, f"{layer_path}.reverb.wet", "must be in [0, 1]"))
            if not isinstance(layer.get("geometry"), dict):
                errors.append(BusinessError(INVALID_SCENE, f"{layer_path}.geometry", "missing geometry object"))

    crossfade = project.get("crossfade_width") if isinstance(project, dict) else None
    if crossfade is not None and (not _is_number(crossfade) or crossfade < 0):
        errors.append(BusinessError(PARAM_RANGE, "$.project.crossfade_width", "must be >= 0"))

    if errors or base_dir is None or not isinstance(sources, list):
        if base_dir is not None and not errors:
            errors.append(BusinessError(INVALID_SCENE, "$.sources", "no usable sources for file check"))
        return errors

    sample_rate = project["sample_rate"]
    base = Path(base_dir)
    for index, source in enumerate(sources):
        if not isinstance(source, dict) or "file" not in source:
            continue
        dry_path = base / source["file"]
        if not dry_path.is_file():
            errors.append(BusinessError(DRY_FILE_MISSING, f"$.sources[{index}].file",
                                        f"{source['file']} not found"))
            continue
        try:
            _, file_rate = read_wav(dry_path)
        except Exception as exc:  # noqa: BLE001 - surface as structured error
            errors.append(BusinessError(DRY_FILE_MISSING, f"$.sources[{index}].file",
                                        f"unreadable WAV: {exc}"))
            continue
        if file_rate != sample_rate:
            errors.append(BusinessError(
                SAMPLE_RATE_MISMATCH, f"$.sources[{index}].file",
                f"dry rate {file_rate} != project rate {sample_rate}",
            ))
    return errors

