from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .transfer import TransferFunction


@dataclass(frozen=True)
class Volume:
    dims: tuple[int, int, int]
    bounds_min: np.ndarray
    bounds_max: np.ndarray
    density_range: tuple[float, float]
    data: np.ndarray
    data_sha256: str
    data_path: Path | None


@dataclass(frozen=True)
class Camera:
    position: np.ndarray
    forward: np.ndarray
    right: np.ndarray
    up: np.ndarray
    fov_y_degrees: float
    aspect_ratio: float
    width: int
    height: int


@dataclass(frozen=True)
class SceneLight:
    id: str
    kind: str
    color: np.ndarray
    intensity: float
    position: np.ndarray | None
    direction: np.ndarray | None
    phase_g: float
    min_distance: float


@dataclass(frozen=True)
class Sampling:
    reference_step: float
    refinement_factor: float
    convergence_levels: int
    convergence_tolerance: float
    suspicious_opacity_jump_threshold: float


@dataclass(frozen=True)
class RenderSettings:
    chunk_width: int
    chunk_height: int
    exposure: float
    gamma: float


@dataclass(frozen=True)
class Scene:
    frame_id: str
    scene_path: Path
    raw_scene: dict[str, Any]
    scene_json_sha256: str
    volume: Volume
    camera: Camera
    lights: tuple[SceneLight, ...]
    transfer: TransferFunction
    sampling: Sampling
    render: RenderSettings

    def input_fingerprint(self, renderer_version: str) -> str:
        from .io_utils import canonical_json_bytes, sha256_bytes

        payload = {
            "renderer_version": renderer_version,
            "scene_json": self.raw_scene,
            "volume_sha256": self.volume.data_sha256,
        }
        return sha256_bytes(canonical_json_bytes(payload))

    def validate_aspect(self) -> bool:
        return math.isclose(
            self.camera.aspect_ratio,
            self.camera.width / self.camera.height,
            rel_tol=1e-5,
            abs_tol=1e-7,
        )

