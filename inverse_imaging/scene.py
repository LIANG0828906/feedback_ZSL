"""Scene JSON validation and deterministic target-image loading."""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

import numpy as np

from .errors import E_SCENE_INVALID, AppError
from .params import ParameterSet, canonical_json_bytes, sha256_bytes


@dataclass(frozen=True)
class SceneSettings:
    softness: float = 0.075
    shadow_samples: int = 5
    plane_size: float = 25.0


@dataclass
class Scene:
    path: Path
    params: ParameterSet
    target: np.ndarray
    width: int
    height: int
    settings: SceneSettings
    scene_id: str
    target_path: Path
    source: dict[str, Any]

    def canonical(self) -> dict[str, Any]:
        return {
            "params": self.params.to_dict(),
            "resolution": {"width": self.width, "height": self.height},
            "settings": {
                "softness": self.settings.softness,
                "shadow_samples": self.settings.shadow_samples,
                "plane_size": self.settings.plane_size,
            },
            "target_sha256": sha256_bytes(np.ascontiguousarray(self.target).tobytes()),
            "scene_id": self.scene_id,
        }


def load_scene(path: str | Path) -> Scene:
    scene_path = Path(path).resolve()
    try:
        raw = scene_path.read_bytes()
    except OSError as exc:
        raise AppError("E_IO", f"cannot read scene: {exc}", "scene") from exc
    try:
        data = __import__("json").loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, ValueError) as exc:
        raise AppError(E_SCENE_INVALID, "scene JSON is malformed", "scene") from exc
    if not isinstance(data, dict):
        raise AppError(E_SCENE_INVALID, "scene root must be an object", "scene")
    return scene_from_dict(data, scene_path)


def scene_from_dict(data: Mapping[str, Any], scene_path: str | Path) -> Scene:
    path = Path(scene_path).resolve()
    resolution = data.get("resolution")
    if not isinstance(resolution, Mapping):
        raise AppError(E_SCENE_INVALID, "resolution object is required", "resolution")
    width = _positive_int(resolution.get("width"), "resolution.width")
    height = _positive_int(resolution.get("height"), "resolution.height")
    if width > 4096 or height > 4096:
        raise AppError(
            E_SCENE_INVALID, "resolution exceeds 4096 pixels", "resolution"
        )

    _validate_camera(data.get("camera"))
    params = ParameterSet.from_scene_dict(data)
    settings_raw = data.get("settings", {})
    if not isinstance(settings_raw, Mapping):
        raise AppError(E_SCENE_INVALID, "settings must be an object", "settings")
    settings = SceneSettings(
        softness=_nonnegative_float(
            settings_raw.get("softness", 0.075), "settings.softness"
        ),
        shadow_samples=_positive_int(
            settings_raw.get("shadow_samples", 5), "settings.shadow_samples"
        ),
        plane_size=_positive_float(
            settings_raw.get("plane_size", 25.0), "settings.plane_size"
        ),
    )
    if settings.shadow_samples > 33:
        raise AppError(
            E_SCENE_INVALID,
            "shadow_samples must be at most 33",
            "settings.shadow_samples",
        )

    target, target_path = _load_target(data, path.parent)
    expected = (height, width, 3)
    if target.shape != expected:
        raise AppError(
            E_SCENE_INVALID,
            f"target shape {target.shape} does not match resolution {expected}",
            "target",
            {"expected_shape": list(expected), "actual_shape": list(target.shape)},
        )
    if not np.all(np.isfinite(target)):
        raise AppError(E_SCENE_INVALID, "target contains non-finite values", "target")
    target = np.ascontiguousarray(target, dtype=np.float64)

    canonical_payload = {
        "params": params.to_dict(),
        "resolution": [height, width],
        "settings": settings.__dict__,
        "target_sha256": sha256_bytes(target.tobytes()),
    }
    scene_id = sha256_bytes(canonical_json_bytes(canonical_payload))
    return Scene(
        path=path,
        params=params,
        target=target,
        width=width,
        height=height,
        settings=settings,
        scene_id=scene_id,
        target_path=target_path,
        source=dict(data),
    )


def _validate_camera(camera_value: object) -> None:
    if not isinstance(camera_value, Mapping):
        raise AppError(E_SCENE_INVALID, "camera object is required", "camera")
    camera = camera_value
    if "rotation_matrix" in camera:
        matrix = camera["rotation_matrix"]
        _validate_rotation_matrix(matrix)
        if "rotation" not in camera:
            camera["rotation"] = _rotation_matrix_to_euler(matrix)
    if "extrinsics" in camera:
        extrinsics = camera["extrinsics"]
        if not isinstance(extrinsics, Mapping):
            raise AppError(
                E_SCENE_INVALID,
                "camera.extrinsics must be an object",
                "camera.extrinsics",
            )
        if "world_to_camera" in extrinsics:
            matrix = extrinsics["world_to_camera"]
            _validate_affine_invertible(matrix)
            affine = np.asarray(matrix, dtype=np.float64)
            camera_to_world = affine[:3, :3].T
            translation = affine[:3, 3]
            if "position" not in camera:
                camera["position"] = (-camera_to_world @ translation).tolist()
            if "rotation" not in camera:
                camera["rotation"] = _rotation_matrix_to_euler(camera_to_world)
    position = camera.get("position")
    if not isinstance(position, (list, tuple)) or len(position) != 3:
        raise AppError(
            E_SCENE_INVALID,
            "camera.position must contain three values",
            "camera.position",
        )
    if "rotation" not in camera:
        raise AppError(
            E_SCENE_INVALID, "camera.rotation is required", "camera.rotation"
        )
    intrinsics = camera.get("intrinsics", camera)
    if not isinstance(intrinsics, Mapping):
        raise AppError(
            E_SCENE_INVALID,
            "camera.intrinsics must be an object",
            "camera.intrinsics",
        )
    for key in ("fx", "fy", "cx", "cy"):
        value = _finite(intrinsics.get(key), f"camera.intrinsics.{key}")
        if key in ("fx", "fy") and abs(value) <= 1.0e-12:
            raise AppError(
                E_SCENE_INVALID,
                "focal length must be non-zero",
                f"camera.intrinsics.{key}",
            )


def _validate_rotation_matrix(value: object) -> None:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise AppError(
            E_SCENE_INVALID,
            "rotation_matrix must be a 3x3 matrix",
            "camera.rotation_matrix",
        )
    matrix = np.asarray(value, dtype=np.float64)
    if matrix.shape != (3, 3) or not np.all(np.isfinite(matrix)):
        raise AppError(
            E_SCENE_INVALID,
            "rotation_matrix must be finite 3x3 values",
            "camera.rotation_matrix",
        )
    determinant = float(np.linalg.det(matrix))
    if abs(determinant) <= 1.0e-8:
        raise AppError(
            E_SCENE_INVALID,
            "rotation_matrix is singular",
            "camera.rotation_matrix",
            {"determinant": determinant},
        )


def _validate_affine_invertible(value: object) -> None:
    matrix = np.asarray(value, dtype=np.float64)
    if matrix.shape != (4, 4) or not np.all(np.isfinite(matrix)):
        raise AppError(
            E_SCENE_INVALID,
            "world_to_camera must be a finite 4x4 matrix",
            "camera.extrinsics.world_to_camera",
        )
    determinant = float(np.linalg.det(matrix))
    if abs(determinant) <= 1.0e-12:
        raise AppError(
            E_SCENE_INVALID,
            "world_to_camera is not invertible",
            "camera.extrinsics.world_to_camera",
            {"determinant": determinant},
        )


def _rotation_matrix_to_euler(value: object) -> list[float]:
    matrix = np.asarray(value, dtype=np.float64)
    pitch = math.asin(max(-1.0, min(1.0, -float(matrix[2, 0]))))
    yaw = math.atan2(float(matrix[1, 0]), float(matrix[0, 0]))
    roll = math.atan2(float(matrix[2, 1]), float(matrix[2, 2]))
    return [yaw, pitch, roll]


def _load_target(data: Mapping[str, Any], base_dir: Path) -> tuple[np.ndarray, Path]:
    target_value: object | None = data.get("target")
    if target_value is None:
        target_value = data.get("target_file")
    if target_value is None:
        target_value = data.get("target_image")
    if isinstance(target_value, str):
        target_path = Path(target_value)
        if not target_path.is_absolute():
            target_path = (base_dir / target_path).resolve()
        try:
            array = np.load(target_path, allow_pickle=False)
        except (OSError, ValueError) as exc:
            raise AppError(
                "E_IO", f"cannot load target image: {exc}", "target"
            ) from exc
        return np.asarray(array, dtype=np.float64), target_path
    if isinstance(target_value, list):
        return np.asarray(target_value, dtype=np.float64), base_dir / "<inline>"
    raise AppError(
        E_SCENE_INVALID,
        "target must be a .npy path or an inline array",
        "target",
    )


def _positive_int(value: object, field_name: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise AppError(E_SCENE_INVALID, "expected a positive integer", field_name)
    return value


def _finite(value: object, field_name: str) -> float:
    if value is None:
        raise AppError(E_SCENE_INVALID, "value is required", field_name)
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise AppError(E_SCENE_INVALID, "expected a finite number", field_name) from exc
    if not math.isfinite(number):
        raise AppError(E_SCENE_INVALID, "expected a finite number", field_name)
    return number


def _positive_float(value: object, field_name: str) -> float:
    number = _finite(value, field_name)
    if number <= 0.0:
        raise AppError(E_SCENE_INVALID, "expected a positive number", field_name)
    return number


def _nonnegative_float(value: object, field_name: str) -> float:
    number = _finite(value, field_name)
    if number < 0.0:
        raise AppError(E_SCENE_INVALID, "expected a non-negative number", field_name)
    return number


def file_sha256(path: str | Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for block in iter(lambda: handle.read(65536), b""):
            digest.update(block)
    return digest.hexdigest()

