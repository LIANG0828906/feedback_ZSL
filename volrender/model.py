"""Scene data model and deterministic geometry helpers."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Iterable

import numpy as np


MEDIUM_BOX = 0
MEDIUM_SPHERE = 1


@dataclass
class Camera:
    """Pinhole camera.

    ``intrinsics`` is a row-major 3x3 matrix. ``world_to_camera`` maps a
    homogeneous world point to camera space using ``[R | t; 0 1]``.
    """

    intrinsics: np.ndarray
    world_to_camera: np.ndarray

    def __post_init__(self) -> None:
        self.intrinsics = np.asarray(self.intrinsics, dtype=np.float64).reshape(3, 3)
        self.world_to_camera = np.asarray(
            self.world_to_camera, dtype=np.float64
        ).reshape(4, 4)

    @property
    def camera_to_world(self) -> np.ndarray:
        return np.linalg.inv(self.world_to_camera)

    @property
    def origin(self) -> np.ndarray:
        return self.camera_to_world[:3, 3].copy()

    @property
    def rotation(self) -> np.ndarray:
        return self.world_to_camera[:3, :3].copy()

    def ray_for_pixel(self, pixel_x: int, pixel_y: int) -> tuple[np.ndarray, np.ndarray]:
        uv = np.array([pixel_x + 0.5, pixel_y + 0.5, 1.0], dtype=np.float64)
        camera_dir = np.linalg.inv(self.intrinsics) @ uv
        camera_dir /= np.linalg.norm(camera_dir)
        world_dir = self.camera_to_world[:3, :3] @ camera_dir
        world_dir /= np.linalg.norm(world_dir)
        return self.origin, world_dir

    def project(self, point: np.ndarray) -> tuple[float, float, float] | None:
        """Return pixel x/y and camera depth, or None if behind the camera."""

        homogeneous = np.ones(4, dtype=np.float64)
        homogeneous[:3] = point
        camera_point = self.world_to_camera @ homogeneous
        depth = float(camera_point[2])
        if not np.isfinite(depth) or depth <= 1e-9:
            return None
        image_point = self.intrinsics @ (camera_point[:3] / depth)
        return float(image_point[0]), float(image_point[1]), depth


@dataclass
class DirectionalLight:
    # Direction pointing from the surface toward the light.
    direction: np.ndarray
    radiance: np.ndarray

    def __post_init__(self) -> None:
        self.direction = np.asarray(self.direction, dtype=np.float64).reshape(3)
        self.radiance = np.asarray(self.radiance, dtype=np.float64).reshape(3)

    @property
    def unit_direction(self) -> np.ndarray:
        value = self.direction / np.linalg.norm(self.direction)
        return value.astype(np.float64, copy=False)


@dataclass
class Medium:
    id: int
    kind: int
    density_field_id: int
    sigma_s: np.ndarray
    sigma_a: np.ndarray
    g: float
    density_scale: float
    shape_a: np.ndarray
    shape_b: np.ndarray
    source_offset: int = -1
    _bounds_cache: tuple[np.ndarray, np.ndarray] | None = field(
        default=None, init=False, repr=False
    )

    def __post_init__(self) -> None:
        self.sigma_s = np.asarray(self.sigma_s, dtype=np.float64).reshape(3)
        self.sigma_a = np.asarray(self.sigma_a, dtype=np.float64).reshape(3)
        self.shape_a = np.asarray(self.shape_a, dtype=np.float64).reshape(3)
        self.shape_b = np.asarray(self.shape_b, dtype=np.float64).reshape(4)

    @property
    def kind_name(self) -> str:
        return "box" if self.kind == MEDIUM_BOX else "sphere"

    @property
    def bounds(self) -> tuple[np.ndarray, np.ndarray]:
        if self._bounds_cache is not None:
            return self._bounds_cache
        if self.kind == MEDIUM_BOX:
            result = (self.shape_a.copy(), self.shape_b[:3].copy())
            self._bounds_cache = result
            return result
        center = self.shape_a
        radius = float(self.shape_b[0])
        result = (center - radius, center + radius)
        self._bounds_cache = result
        return result

    @property
    def center(self) -> np.ndarray:
        if self.kind == MEDIUM_SPHERE:
            return self.shape_a.copy()
        lo, hi = self.bounds
        return (lo + hi) * 0.5


@dataclass
class DensityField:
    id: int
    dims: tuple[int, int, int]
    chunk_shape: tuple[int, int, int]
    chunks: dict[tuple[int, int, int], np.ndarray]
    source_offset: int = -1
    chunk_offsets: dict[tuple[int, int, int], int] = field(default_factory=dict)
    _grid: np.ndarray | None = field(default=None, init=False, repr=False)

    def __post_init__(self) -> None:
        self.dims = tuple(int(v) for v in self.dims)
        self.chunk_shape = tuple(int(v) for v in self.chunk_shape)
        normalised: dict[tuple[int, int, int], np.ndarray] = {}
        for key, value in self.chunks.items():
            normalised[tuple(int(v) for v in key)] = np.asarray(
                value, dtype=np.float64
            )
        self.chunks = normalised

    def chunk_extent(self, key: tuple[int, int, int]) -> tuple[int, int, int]:
        start = (
            key[0] * self.chunk_shape[0],
            key[1] * self.chunk_shape[1],
            key[2] * self.chunk_shape[2],
        )
        return (
            min(self.chunk_shape[0], self.dims[0] - start[0]),
            min(self.chunk_shape[1], self.dims[1] - start[1]),
            min(self.chunk_shape[2], self.dims[2] - start[2]),
        )

    def chunk_count(self) -> tuple[int, int, int]:
        return tuple(
            (self.dims[i] + self.chunk_shape[i] - 1) // self.chunk_shape[i]
            for i in range(3)
        )

    def chunk_keys(self) -> Iterable[tuple[int, int, int]]:
        nx, ny, nz = self.chunk_count()
        for cz in range(nz):
            for cy in range(ny):
                for cx in range(nx):
                    yield (cx, cy, cz)

    def materialize(self) -> np.ndarray:
        if self._grid is not None:
            return self._grid
        grid = np.zeros(
            (self.dims[2], self.dims[1], self.dims[0]), dtype=np.float64
        )
        cx, cy, cz = self.chunk_shape
        for key, chunk in self.chunks.items():
            sx = key[0] * cx
            sy = key[1] * cy
            sz = key[2] * cz
            nz, ny, nx = chunk.shape
            grid[sz : sz + nz, sy : sy + ny, sx : sx + nx] = chunk
        self._grid = grid
        return grid

    def set_chunk(
        self, key: tuple[int, int, int], value: np.ndarray
    ) -> None:
        """Replace a chunk and invalidate the materialized interpolation grid."""

        key = tuple(int(v) for v in key)
        self.chunks[key] = np.asarray(value, dtype=np.float64)
        self._grid = None


@dataclass
class Scene:
    width: int
    height: int
    camera: Camera
    light: DirectionalLight
    media: list[Medium]
    density_fields: dict[int, DensityField]
    declared_media_count: int = -1
    declared_density_count: int = -1
    source_size: int = 0

    def __post_init__(self) -> None:
        if self.declared_media_count < 0:
            self.declared_media_count = len(self.media)
        if self.declared_density_count < 0:
            self.declared_density_count = len(self.density_fields)

    @property
    def scene_bounds(self) -> tuple[np.ndarray, np.ndarray] | None:
        if not self.media:
            return None
        lows = np.stack([m.bounds[0] for m in self.media])
        highs = np.stack([m.bounds[1] for m in self.media])
        return np.min(lows, axis=0), np.max(highs, axis=0)

    def media_by_id(self) -> dict[int, Medium]:
        return {medium.id: medium for medium in self.media}

    def density_by_id(self) -> dict[int, DensityField]:
        return dict(self.density_fields)


def density_sample(
    field: DensityField,
    world_point: np.ndarray,
    bounds_lo: np.ndarray,
    bounds_hi: np.ndarray,
) -> tuple[float, set[tuple[int, int, int]]]:
    """Trilinearly sample a chunked density field mapped to a medium AABB.

    Return the scalar and every chunk needed for the interpolation stencil.
    The scalar is zero outside the medium bounds.
    """

    span_x = float(bounds_hi[0] - bounds_lo[0])
    span_y = float(bounds_hi[1] - bounds_lo[1])
    span_z = float(bounds_hi[2] - bounds_lo[2])
    if span_x <= 0.0 or span_y <= 0.0 or span_z <= 0.0:
        return 0.0, set()
    u = (float(world_point[0]) - float(bounds_lo[0])) / span_x
    v = (float(world_point[1]) - float(bounds_lo[1])) / span_y
    w = (float(world_point[2]) - float(bounds_lo[2])) / span_z
    if (
        u < -1e-12
        or u > 1.0 + 1e-12
        or v < -1e-12
        or v > 1.0 + 1e-12
        or w < -1e-12
        or w > 1.0 + 1e-12
    ):
        return 0.0, set()
    u = min(max(u, 0.0), 1.0)
    v = min(max(v, 0.0), 1.0)
    w = min(max(w, 0.0), 1.0)
    nx, ny, nz = field.dims
    gx = u * max(nx - 1, 0)
    gy = v * max(ny - 1, 0)
    gz = w * max(nz - 1, 0)
    ix0 = int(gx)
    iy0 = int(gy)
    iz0 = int(gz)
    ix1 = min(ix0 + 1, nx - 1)
    iy1 = min(iy0 + 1, ny - 1)
    iz1 = min(iz0 + 1, nz - 1)
    fx = gx - ix0
    fy = gy - iy0
    fz = gz - iz0
    cx, cy, cz = field.chunk_shape
    touched = {
        (ix // cx, iy // cy, iz // cz)
        for ix in (ix0, ix1)
        for iy in (iy0, iy1)
        for iz in (iz0, iz1)
    }

    grid = field.materialize()
    c000 = float(grid[iz0, iy0, ix0])
    c100 = float(grid[iz0, iy0, ix1])
    c010 = float(grid[iz0, iy1, ix0])
    c110 = float(grid[iz0, iy1, ix1])
    c001 = float(grid[iz1, iy0, ix0])
    c101 = float(grid[iz1, iy0, ix1])
    c011 = float(grid[iz1, iy1, ix0])
    c111 = float(grid[iz1, iy1, ix1])
    c00 = c000 * (1.0 - fx) + c100 * fx
    c10 = c010 * (1.0 - fx) + c110 * fx
    c01 = c001 * (1.0 - fx) + c101 * fx
    c11 = c011 * (1.0 - fx) + c111 * fx
    c0 = c00 * (1.0 - fy) + c10 * fy
    c1 = c01 * (1.0 - fy) + c11 * fy
    return float(c0 * (1.0 - fz) + c1 * fz), touched
