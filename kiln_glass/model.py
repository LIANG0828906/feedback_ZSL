"""Plain data structures for the kiln glass-brick scene."""

from __future__ import annotations

from dataclasses import dataclass, field
import numpy as np


@dataclass(frozen=True)
class Brick:
    layer: int
    brick_id: int
    vertices: np.ndarray  # (V, 3) float64
    triangles: np.ndarray  # (T, 3) int64 indices into vertices
    thickness: float
    transmittance: float
    fringe_seed: int
    origin: np.ndarray  # (3,) brick local origin on the wall plane
    size: np.ndarray  # (3,) full brick size (x, y, z)

    @property
    def aabb_min(self) -> np.ndarray:
        return self.origin

    @property
    def aabb_max(self) -> np.ndarray:
        return self.origin + self.size


@dataclass(frozen=True)
class Layer:
    index: int
    plane_z: float
    grout_width: float
    brick_ids: tuple[int, ...]


@dataclass(frozen=True)
class PointLight:
    light_id: int
    position: np.ndarray
    intensity: float


@dataclass(frozen=True)
class ReceiverPlane:
    z: float
    x_min: float
    x_max: float
    y_min: float
    y_max: float

    def contains(self, point: np.ndarray) -> bool:
        return bool(
            self.x_min <= point[0] <= self.x_max
            and self.y_min <= point[1] <= self.y_max
            and abs(point[2] - self.z) < 1e-9
        )


@dataclass(frozen=True)
class Camera:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    focal_length: float
    sensor_width: float
    sensor_height: float


@dataclass(frozen=True)
class RenderConfig:
    image_width: int
    image_height: int
    tile_width: int
    tile_height: int
    sample_density: int  # soft-shadow sample count per light
    bubble_count: int  # deterministic bubble samples per brick
    background_rgb: tuple[float, float, float] = (0.015, 0.012, 0.01)


@dataclass(frozen=True)
class Scene:
    bricks: tuple[Brick, ...]
    layers: tuple[Layer, ...]
    lights: tuple[PointLight, ...]
    receiver: ReceiverPlane
    camera: Camera
    view_angles: tuple[float, ...]  # strictly increasing yaw angles (radians)
    config: RenderConfig
    raw: dict = field(default_factory=dict, compare=False)

    def brick_by_id(self, brick_id: int) -> Brick:
        for brick in self.bricks:
            if brick.brick_id == brick_id:
                return brick
        raise KeyError(brick_id)

    def layer_of(self, layer: int) -> Layer:
        for entry in self.layers:
            if entry.index == layer:
                return entry
        raise KeyError(layer)
