"""Scene JSON validation, hashing and disk I/O."""

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

from .errors import DecodeError
from .model import Camera, Scene, _normalized, _vec3


def _decode_vec3(raw: Any, path: str, field_name: str) -> list[float]:
    try:
        return list(_vec3(raw, field_name))
    except (TypeError, ValueError) as exc:
        raise DecodeError(path, str(exc), field=field_name) from exc


def _validate_color(raw: Any, path: str, field_name: str) -> list[float]:
    color = _decode_vec3(raw, path, field_name)
    if any(value < 0.0 or value > 1.0 for value in color):
        raise DecodeError(path, "color channels must be in [0, 1]", field=field_name)
    return color


def _validate_object(raw: Any, path: str, index: int) -> dict[str, Any]:
    field_prefix = f"objects[{index}]"
    if not isinstance(raw, dict):
        raise DecodeError(path, "object must be an object", field=field_prefix)
    kind = raw.get("kind")
    if kind not in {"sphere", "plane", "box"}:
        raise DecodeError(
            path,
            f"unsupported geometry kind {kind!r}; expected sphere, plane or box",
            field=f"{field_prefix}.kind",
        )
    out: dict[str, Any] = {
        "kind": kind,
        "name": str(raw.get("name", f"{kind}-{index}")),
        "color": _validate_color(raw.get("color"), path, f"{field_prefix}.color"),
    }
    if kind == "sphere":
        out["center"] = _decode_vec3(raw.get("center"), path, f"{field_prefix}.center")
        radius = float(raw.get("radius", 0.0))
        if not math.isfinite(radius) or radius <= 0.0:
            raise DecodeError(path, "radius must be finite and > 0", field=f"{field_prefix}.radius")
        out["radius"] = radius
    elif kind == "plane":
        out["point"] = _decode_vec3(raw.get("point"), path, f"{field_prefix}.point")
        try:
            out["normal"] = list(_normalized(raw.get("normal"), f"{field_prefix}.normal"))
        except (TypeError, ValueError) as exc:
            raise DecodeError(path, str(exc), field=f"{field_prefix}.normal") from exc
    else:
        out["center"] = _decode_vec3(raw.get("center"), path, f"{field_prefix}.center")
        half = _decode_vec3(raw.get("half_extents"), path, f"{field_prefix}.half_extents")
        if any(value <= 0.0 for value in half):
            raise DecodeError(
                path, "half_extents must all be > 0", field=f"{field_prefix}.half_extents"
            )
        out["half_extents"] = half
    roughness = float(raw.get("roughness", 0.6))
    if not (0.0 <= roughness <= 1.0):
        raise DecodeError(path, "roughness must be in [0, 1]", field=f"{field_prefix}.roughness")
    out["roughness"] = roughness
    specular = float(raw.get("specular", 0.3))
    if not (0.0 <= specular <= 1.0):
        raise DecodeError(path, "specular must be in [0, 1]", field=f"{field_prefix}.specular")
    out["specular"] = specular
    return out


def _validate_light(raw: Any, path: str, index: int) -> dict[str, Any]:
    prefix = f"lights[{index}]"
    if not isinstance(raw, dict):
        raise DecodeError(path, "light must be an object", field=prefix)
    try:
        direction = list(_normalized(raw.get("direction"), f"{prefix}.direction"))
    except (TypeError, ValueError) as exc:
        raise DecodeError(path, str(exc), field=f"{prefix}.direction") from exc
    color = _validate_color(raw.get("color", [1, 1, 1]), path, f"{prefix}.color")
    intensity = float(raw.get("intensity", 1.0))
    if not math.isfinite(intensity) or intensity < 0.0:
        raise DecodeError(path, "intensity must be finite and >= 0", field=f"{prefix}.intensity")
    return {"direction": direction, "color": color, "intensity": intensity}


def validate_scene_data(raw: Any, *, path: str = "<memory>") -> Scene:
    if not isinstance(raw, dict):
        raise DecodeError(path, "scene root must be an object")
    try:
        schema_version = int(raw.get("schema_version", raw.get("schema", 1)))
    except (TypeError, ValueError) as exc:
        raise DecodeError(path, "schema_version must be an integer", cause=str(exc)) from exc
    if schema_version != 1:
        raise DecodeError(path, f"unsupported schema_version {schema_version}", field="schema_version")

    objects_raw = raw.get("objects")
    if not isinstance(objects_raw, list) or not objects_raw:
        raise DecodeError(path, "objects must be a non-empty array", field="objects")
    objects = tuple(_validate_object(obj, path, i) for i, obj in enumerate(objects_raw))

    cameras_raw = raw.get("camera_poses")
    if not isinstance(cameras_raw, list) or not cameras_raw:
        raise DecodeError(
            path, "camera_poses must be a non-empty array", field="camera_poses"
        )
    cameras = tuple(
        Camera.from_dict(camera, path=f"{path}:camera_poses[{i}]")
        for i, camera in enumerate(cameras_raw)
    )

    lights_raw = raw.get("lights", [{"direction": [-0.4, 0.8, 0.45], "color": [1, 1, 1]}])
    if not isinstance(lights_raw, list) or not lights_raw:
        raise DecodeError(path, "lights must be a non-empty array", field="lights")
    lights = tuple(_validate_light(light, path, i) for i, light in enumerate(lights_raw))

    background = raw.get(
        "background",
        {"top": [0.12, 0.20, 0.29], "bottom": [0.42, 0.48, 0.50]},
    )
    if not isinstance(background, dict):
        raise DecodeError(path, "background must be an object", field="background")
    background = {
        "top": _validate_color(background.get("top"), path, "background.top"),
        "bottom": _validate_color(background.get("bottom"), path, "background.bottom"),
    }
    seed = int(raw.get("seed", 0))
    metadata = raw.get("metadata", {})
    if not isinstance(metadata, dict):
        raise DecodeError(path, "metadata must be an object", field="metadata")
    return Scene(
        name=str(raw.get("name", "untitled")),
        seed=seed,
        objects=objects,
        camera_poses=cameras,
        lights=lights,
        background=background,
        schema_version=schema_version,
        metadata=metadata,
    )


def load_scene(path: str | Path) -> Scene:
    file_path = Path(path)
    try:
        text = file_path.read_text(encoding="utf-8")
    except OSError as exc:
        raise DecodeError(str(file_path), "cannot read file", cause=str(exc)) from exc
    try:
        raw = json.loads(text)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise DecodeError(str(file_path), "invalid JSON", cause=str(exc)) from exc
    return validate_scene_data(raw, path=str(file_path))


def save_scene(scene: Scene, path: str | Path) -> str:
    file_path = Path(path)
    file_path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(scene.to_dict(), indent=2, sort_keys=True, ensure_ascii=True) + "\n"
    file_path.write_text(payload, encoding="utf-8")
    return scene.content_hash()


def scene_hash_from_file(path: str | Path) -> str:
    return load_scene(path).content_hash()

