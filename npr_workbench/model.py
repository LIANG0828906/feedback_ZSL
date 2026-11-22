"""Immutable scene, camera, parameter and frame models."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass, field
from typing import Any, Iterable

import numpy as np

from .errors import DecodeError, ParameterError

Vec3 = tuple[float, float, float]


def _vec3(value: Any, field_name: str) -> Vec3:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise ValueError(f"{field_name} must be a 3-element array")
    out = tuple(float(v) for v in value)
    if not all(math.isfinite(v) for v in out):
        raise ValueError(f"{field_name} contains non-finite values")
    return out  # type: ignore[return-value]


def _normalized(value: Vec3, field_name: str) -> Vec3:
    length = math.sqrt(sum(v * v for v in value))
    if length < 1e-12:
        raise ValueError(f"{field_name} cannot be zero")
    # Quantize to make normalization idempotent through JSON round trips.  The
    # scene content hash must not change merely because a file was reloaded.
    return tuple(round(v / length, 12) for v in value)  # type: ignore[return-value]


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    )


def sha256_json(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class Camera:
    id: str
    position: Vec3
    target: Vec3
    up: Vec3 = (0.0, 1.0, 0.0)
    fov_y: float = 45.0
    near: float = 0.05
    far: float = 100.0

    def __post_init__(self) -> None:
        object.__setattr__(self, "position", _vec3(self.position, "position"))
        object.__setattr__(self, "target", _vec3(self.target, "target"))
        object.__setattr__(self, "up", _normalized(_vec3(self.up, "up"), "up"))
        if self.position == self.target:
            raise ValueError("camera position and target must differ")
        if not math.isfinite(float(self.fov_y)) or not (1.0 <= self.fov_y <= 179.0):
            raise ValueError("fov_y must be in [1, 179]")
        if not (0.0 < self.near < self.far):
            raise ValueError("camera clipping planes require 0 < near < far")

    @property
    def forward(self) -> np.ndarray:
        vec = np.asarray(self.target, dtype=np.float64) - np.asarray(
            self.position, dtype=np.float64
        )
        return vec / np.linalg.norm(vec)

    @property
    def right(self) -> np.ndarray:
        vec = np.cross(self.forward, np.asarray(self.up, dtype=np.float64))
        norm = np.linalg.norm(vec)
        if norm < 1e-12:
            raise ValueError("camera up vector is parallel to view direction")
        return vec / norm

    @property
    def true_up(self) -> np.ndarray:
        return np.cross(self.right, self.forward)

    def with_pose(self, position: Vec3, target: Vec3, *, camera_id: str | None = None) -> "Camera":
        return Camera(
            id=camera_id or self.id,
            position=position,
            target=target,
            up=self.up,
            fov_y=self.fov_y,
            near=self.near,
            far=self.far,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "position": list(self.position),
            "target": list(self.target),
            "up": list(self.up),
            "fov_y": self.fov_y,
            "near": self.near,
            "far": self.far,
        }

    @classmethod
    def from_dict(cls, raw: Any, *, path: str = "<camera>") -> "Camera":
        if not isinstance(raw, dict):
            raise DecodeError(path, "camera must be an object")
        try:
            return cls(
                id=str(raw.get("id", "camera")),
                position=raw["position"],
                target=raw["target"],
                up=raw.get("up", [0.0, 1.0, 0.0]),
                fov_y=float(raw.get("fov_y", 45.0)),
                near=float(raw.get("near", 0.05)),
                far=float(raw.get("far", 100.0)),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise DecodeError(path, str(exc)) from exc

    def pose_hash(self) -> str:
        return sha256_json(self.to_dict())


@dataclass(frozen=True)
class Scene:
    name: str
    seed: int
    objects: tuple[dict[str, Any], ...]
    camera_poses: tuple[Camera, ...]
    lights: tuple[dict[str, Any], ...]
    background: dict[str, Any]
    schema_version: int = 1
    metadata: dict[str, Any] = field(default_factory=dict)

    def content_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "seed": self.seed,
            "objects": list(self.objects),
            "lights": list(self.lights),
            "background": self.background,
        }

    def content_hash(self) -> str:
        return sha256_json(self.content_dict())

    def to_dict(self) -> dict[str, Any]:
        return {
            **self.content_dict(),
            "name": self.name,
            "camera_poses": [camera.to_dict() for camera in self.camera_poses],
            "metadata": self.metadata,
        }

    def camera_by_id(self, camera_id: str | None = None) -> Camera:
        if not self.camera_poses:
            raise ValueError("scene has no camera poses")
        if camera_id is None:
            return self.camera_poses[0]
        for camera in self.camera_poses:
            if camera.id == camera_id:
                return camera
        raise KeyError(camera_id)


@dataclass(frozen=True)
class RenderParams:
    levels: int = 5
    density: float = 0.34
    length: float = 12.0
    preview_width: int = 120
    preview_height: int = 90
    exact_width: int = 360
    exact_height: int = 270
    tile_width: int = 40
    tile_height: int = 40
    max_workers: int = 4
    thread_pixel_budget: int = 32768
    stroke_cache_budget: int = 512
    guide_width: int = 96
    guide_height: int = 72
    stroke_reference_width: int = 320
    preview_stroke_sample_factor: int = 2
    exact_stroke_sample_factor: int = 1

    def __post_init__(self) -> None:
        self.validate()

    def validate(self) -> None:
        if isinstance(self.levels, bool) or not isinstance(self.levels, int):
            raise ParameterError("levels", self.levels, "must be an integer")
        if not (2 <= self.levels <= 16):
            raise ParameterError("levels", self.levels, "must be in [2, 16]")
        if not math.isfinite(float(self.density)):
            raise ParameterError("density", self.density, "must be finite")
        if not (0.0 <= float(self.density) <= 1.0):
            raise ParameterError("density", self.density, "must be in [0, 1]")
        if not math.isfinite(float(self.length)):
            raise ParameterError("length", self.length, "must be finite")
        if not (1.0 <= float(self.length) <= 64.0):
            raise ParameterError("length", self.length, "must be in [1, 64] reference pixels")
        if not (32 <= int(self.preview_width) <= 512):
            raise ParameterError("preview_width", self.preview_width, "must be in [32, 512]")
        if not (32 <= int(self.preview_height) <= 512):
            raise ParameterError("preview_height", self.preview_height, "must be in [32, 512]")
        if not (64 <= int(self.exact_width) <= 1024):
            raise ParameterError("exact_width", self.exact_width, "must be in [64, 1024]")
        if not (64 <= int(self.exact_height) <= 1024):
            raise ParameterError("exact_height", self.exact_height, "must be in [64, 1024]")
        if int(self.preview_width) >= int(self.exact_width) or int(
            self.preview_height
        ) >= int(self.exact_height):
            raise ParameterError(
                "preview_width",
                (self.preview_width, self.preview_height),
                "preview resolution must be lower than exact resolution",
            )
        if not (8 <= int(self.tile_width) <= 256):
            raise ParameterError("tile_width", self.tile_width, "must be in [8, 256]")
        if not (8 <= int(self.tile_height) <= 256):
            raise ParameterError("tile_height", self.tile_height, "must be in [8, 256]")
        if not (1 <= int(self.max_workers) <= 16):
            raise ParameterError("max_workers", self.max_workers, "must be in [1, 16]")
        if not (256 <= int(self.thread_pixel_budget) <= 4_194_304):
            raise ParameterError(
                "thread_pixel_budget", self.thread_pixel_budget, "must be in [256, 4194304]"
            )
        if not (8 <= int(self.stroke_cache_budget) <= 100_000):
            raise ParameterError(
                "stroke_cache_budget", self.stroke_cache_budget, "must be in [8, 100000]"
            )
        if not (32 <= int(self.guide_width) <= 256):
            raise ParameterError("guide_width", self.guide_width, "must be in [32, 256]")
        if not (32 <= int(self.guide_height) <= 256):
            raise ParameterError("guide_height", self.guide_height, "must be in [32, 256]")
        if not (64 <= int(self.stroke_reference_width) <= 2048):
            raise ParameterError(
                "stroke_reference_width", self.stroke_reference_width, "must be in [64, 2048]"
            )
        if not (1 <= int(self.preview_stroke_sample_factor) <= 8):
            raise ParameterError(
                "preview_stroke_sample_factor",
                self.preview_stroke_sample_factor,
                "must be in [1, 8]",
            )
        if not (1 <= int(self.exact_stroke_sample_factor) <= 8):
            raise ParameterError(
                "exact_stroke_sample_factor",
                self.exact_stroke_sample_factor,
                "must be in [1, 8]",
            )

    def with_controls(
        self,
        *,
        levels: int | None = None,
        density: float | None = None,
        length: float | None = None,
    ) -> "RenderParams":
        data = self.to_dict()
        if levels is not None:
            data["levels"] = levels
        if density is not None:
            data["density"] = density
        if length is not None:
            data["length"] = length
        return RenderParams(**data)

    def to_dict(self) -> dict[str, Any]:
        return {
            "levels": self.levels,
            "density": float(self.density),
            "length": float(self.length),
            "preview_width": self.preview_width,
            "preview_height": self.preview_height,
            "exact_width": self.exact_width,
            "exact_height": self.exact_height,
            "tile_width": self.tile_width,
            "tile_height": self.tile_height,
            "max_workers": self.max_workers,
            "thread_pixel_budget": self.thread_pixel_budget,
            "stroke_cache_budget": self.stroke_cache_budget,
            "guide_width": self.guide_width,
            "guide_height": self.guide_height,
            "stroke_reference_width": self.stroke_reference_width,
            "preview_stroke_sample_factor": self.preview_stroke_sample_factor,
            "exact_stroke_sample_factor": self.exact_stroke_sample_factor,
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "RenderParams":
        return cls(**raw)

    def style_hash(self) -> str:
        return sha256_json(self.to_dict())


@dataclass(frozen=True)
class Snapshot:
    version: int
    content_hash: str
    camera: Camera
    params: RenderParams

    def __post_init__(self) -> None:
        if self.version < 1:
            raise ValueError("snapshot version must be >= 1")

    def identity_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "content_hash": self.content_hash,
            "camera": self.camera.to_dict(),
            "params": self.params.to_dict(),
        }

    def identity_hash(self) -> str:
        return sha256_json(self.identity_dict())

    def to_dict(self) -> dict[str, Any]:
        return self.identity_dict()


@dataclass
class NprFrame:
    kind: str
    snapshot: Snapshot
    base_rgb: np.ndarray
    poster_rgb: np.ndarray
    levels: np.ndarray
    outline: np.ndarray
    brush_rgba: np.ndarray
    styled_rgb: np.ndarray
    coverage: np.ndarray
    duration_ms: float
    stroke_count: int
    stroke_fingerprint_root: str
    tile_commit_log: tuple[int, ...] = ()
    failed_tiles: tuple[int, ...] = ()

    @property
    def width(self) -> int:
        return int(self.styled_rgb.shape[1])

    @property
    def height(self) -> int:
        return int(self.styled_rgb.shape[0])

    @property
    def version(self) -> int:
        return self.snapshot.version

    def validate_shapes(self) -> None:
        h, w = self.styled_rgb.shape[:2]
        expected = {
            "base_rgb": (h, w, 3),
            "poster_rgb": (h, w, 3),
            "levels": (h, w),
            "outline": (h, w),
            "brush_rgba": (h, w, 4),
            "coverage": (h, w),
        }
        actual = {
            "base_rgb": self.base_rgb.shape,
            "poster_rgb": self.poster_rgb.shape,
            "levels": self.levels.shape,
            "outline": self.outline.shape,
            "brush_rgba": self.brush_rgba.shape,
            "coverage": self.coverage.shape,
        }
        for name, shape in expected.items():
            if actual[name] != shape:
                raise ValueError(
                    f"stage resolution mismatch for {name}: {actual[name]} != {shape}"
                )


@dataclass
class ExactRunResult:
    snapshot: Snapshot
    complete: bool
    paused: bool
    canceled: bool
    committed_tiles: tuple[int, ...]
    failed_tiles: tuple[int, ...]
    total_tiles: int
    frame: NprFrame | None
    checkpoint_dir: str
    commit_order: tuple[int, ...] = ()
    checkpoint_invalidated: bool = False
    invalid_reason: str | None = None


@dataclass(frozen=True)
class ConsistencyReport:
    level_histogram_deviation: float
    outline_coverage_delta: float
    stroke_coverage_delta: float
    outline_iou: float
    first_mismatch_pixel: tuple[int, int] | None
    first_mismatch_tile: int | None
    preview_version: int
    exact_version: int
    comparison_size: tuple[int, int]
    tolerances: dict[str, float]
    passed: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "level_histogram_deviation": self.level_histogram_deviation,
            "outline_coverage_delta": self.outline_coverage_delta,
            "stroke_coverage_delta": self.stroke_coverage_delta,
            "outline_iou": self.outline_iou,
            "first_mismatch_pixel": self.first_mismatch_pixel,
            "first_mismatch_tile": self.first_mismatch_tile,
            "preview_version": self.preview_version,
            "exact_version": self.exact_version,
            "comparison_size": self.comparison_size,
            "tolerances": self.tolerances,
            "passed": self.passed,
        }


def level_histogram(levels: np.ndarray, level_count: int) -> np.ndarray:
    counts = np.bincount(levels.astype(np.int64).ravel(), minlength=level_count)
    denom = max(1, int(levels.size))
    return counts[:level_count].astype(np.float64) / denom


def iter_object_kinds(objects: Iterable[dict[str, Any]]) -> list[str]:
    return [str(obj.get("kind")) for obj in objects]

