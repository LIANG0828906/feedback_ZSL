"""Seeded, order-independent paper fibers and irregular cutout masks."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass

import numpy as np

from .scene import Layer, Scene, TextureConfig


def derive_seed(texture_seed: int, layer_index: int, channel: str) -> int:
    payload = f"{texture_seed}:{layer_index}:{channel}".encode("utf-8")
    return int.from_bytes(hashlib.sha256(payload).digest()[:8], "little")


def _smoothstep(value: np.ndarray) -> np.ndarray:
    return value * value * (3.0 - 2.0 * value)


def _value_noise(
    rng: np.random.Generator,
    height: int,
    width: int,
    cell: int,
) -> np.ndarray:
    grid_h = height // cell + 3
    grid_w = width // cell + 3
    lattice = rng.random((grid_h, grid_w), dtype=np.float64)
    y = np.arange(height, dtype=np.float64) / float(cell)
    x = np.arange(width, dtype=np.float64) / float(cell)
    y0 = np.floor(y).astype(np.int64)
    x0 = np.floor(x).astype(np.int64)
    fy = _smoothstep(y - y0)
    fx = _smoothstep(x - x0)
    yy0, xx0 = np.meshgrid(y0, x0, indexing="ij")
    fy_grid, fx_grid = np.meshgrid(fy, fx, indexing="ij")
    n00 = lattice[yy0, xx0]
    n01 = lattice[yy0, xx0 + 1]
    n10 = lattice[yy0 + 1, xx0]
    n11 = lattice[yy0 + 1, xx0 + 1]
    nx0 = n00 * (1.0 - fx_grid) + n01 * fx_grid
    nx1 = n10 * (1.0 - fx_grid) + n11 * fx_grid
    return nx0 * (1.0 - fy_grid) + nx1 * fy_grid


def _fiber_table(
    texture_seed: int,
    layer_index: int,
    texture: TextureConfig,
) -> np.ndarray:
    rng = np.random.default_rng(derive_seed(texture_seed, layer_index, "fiber"))
    height, width = texture.height, texture.width
    broad = _value_noise(rng, height, width, max(18, width // 9))
    medium = _value_noise(rng, height, width, max(7, width // 31))
    fine = _value_noise(rng, height, width, max(3, width // 83))
    y, x = np.mgrid[0:height, 0:width]
    phase = rng.random() * np.pi * 2.0
    streak = 0.5 + 0.5 * np.sin(
        x * 0.21 + y * 0.047 + phase + (medium - 0.5) * 4.2
    )
    mottled = 0.52 * broad + 0.31 * medium + 0.12 * fine + 0.05 * streak
    centered = (mottled - 0.5) * (1.55 + 2.2 * texture.fiber_contrast) * 2.1
    fibers = np.clip(0.5 + centered, 0.0, 1.0)
    return fibers.astype(np.float64)


def _organic_motif(
    x: np.ndarray,
    y: np.ndarray,
    cx: float,
    cy: float,
    rx: float,
    ry: float,
    phase: float,
    lobes: int,
) -> np.ndarray:
    dx = (x - cx) / rx
    dy = (y - cy) / ry
    radius = np.sqrt(dx * dx + dy * dy)
    angle = np.arctan2(dy, dx)
    boundary = (
        1.0
        + 0.16 * np.sin(lobes * angle + phase)
        + 0.08 * np.sin((lobes + 3) * angle - phase * 1.7)
    )
    return radius < boundary


def _hole_table(
    texture_seed: int,
    layer_index: int,
    texture: TextureConfig,
) -> np.ndarray:
    height, width = texture.height, texture.width
    rng = np.random.default_rng(derive_seed(texture_seed, layer_index, "holes"))
    coarse = _value_noise(rng, height, width, max(12, width // 13))
    medium = _value_noise(rng, height, width, max(6, width // 31))
    field = 0.72 * coarse + 0.28 * medium
    threshold = float(np.quantile(field, 1.0 - texture.hole_coverage))
    holes = field > threshold

    y, x = np.mgrid[0:height, 0:width]
    u = (x + 0.5) / width
    v = (y + 0.5) / height

    # A central flower-like cutout anchors projection probes and is always present.
    holes |= _organic_motif(
        u,
        v,
        cx=0.46 + (rng.random() - 0.5) * 0.035,
        cy=0.54 + (rng.random() - 0.5) * 0.035,
        rx=0.108 + (rng.random() - 0.5) * 0.018,
        ry=0.094 + (rng.random() - 0.5) * 0.018,
        phase=rng.random() * np.pi * 2.0,
        lobes=7 + 2 * int(rng.integers(0, 3)),
    )

    for _ in range(7):
        center_x = 0.12 + 0.76 * float(rng.random())
        center_y = 0.12 + 0.76 * float(rng.random())
        radius_x = 0.025 + 0.055 * float(rng.random())
        radius_y = 0.021 + 0.048 * float(rng.random())
        holes |= _organic_motif(
            u,
            v,
            center_x,
            center_y,
            radius_x,
            radius_y,
            float(rng.random() * np.pi * 2.0),
            int(rng.integers(5, 11)),
        )

    for _ in range(18):
        center_x = 0.04 + 0.92 * float(rng.random())
        center_y = 0.04 + 0.92 * float(rng.random())
        radius = 0.0025 + 0.0065 * float(rng.random())
        dy = (v - center_y) / radius
        dx = (x.astype(np.float64) / width - center_x) / radius
        holes |= dx * dx + dy * dy < 1.0

    return holes.astype(np.uint8)


@dataclass(frozen=True)
class LayerTexture:
    layer_index: int
    fibers: np.ndarray
    holes: np.ndarray
    fiber_sha256: str
    hole_sha256: str
    hole_count: int
    hole_fraction: float


@dataclass(frozen=True)
class SceneTextures:
    layers: tuple[LayerTexture, ...]

    def for_layer(self, layer_index: int) -> LayerTexture:
        for texture in self.layers:
            if texture.layer_index == layer_index:
                return texture
        raise KeyError(layer_index)


def _float_table_digest(table: np.ndarray) -> str:
    payload = np.asarray(table, dtype="<f4").tobytes(order="C")
    return hashlib.sha256(payload).hexdigest()


def _mask_digest(mask: np.ndarray) -> str:
    packed = np.packbits(np.asarray(mask, dtype=np.uint8).ravel(order="C"))
    return hashlib.sha256(packed.tobytes()).hexdigest()


def count_hole_components(mask: np.ndarray) -> int:
    """Count four-connected cutout regions without external image libraries."""

    holes = np.asarray(mask, dtype=bool)
    height, width = holes.shape
    visited = np.zeros_like(holes, dtype=bool)
    count = 0
    for start_y in range(height):
        for start_x in range(width):
            if not holes[start_y, start_x] or visited[start_y, start_x]:
                continue
            count += 1
            stack = [(start_y, start_x)]
            visited[start_y, start_x] = True
            while stack:
                y, x = stack.pop()
                for ny, nx in ((y - 1, x), (y + 1, x), (y, x - 1), (y, x + 1)):
                    if (
                        0 <= ny < height
                        and 0 <= nx < width
                        and holes[ny, nx]
                        and not visited[ny, nx]
                    ):
                        visited[ny, nx] = True
                        stack.append((ny, nx))
    return count


def generate_textures(scene: Scene) -> SceneTextures:
    textures: list[LayerTexture] = []
    for layer in scene.layers_inner_to_outer:
        fibers = _fiber_table(layer.texture_seed, layer.layer_index, scene.texture)
        holes = _hole_table(layer.texture_seed, layer.layer_index, scene.texture)
        textures.append(
            LayerTexture(
                layer_index=layer.layer_index,
                fibers=fibers,
                holes=holes,
                fiber_sha256=_float_table_digest(fibers),
                hole_sha256=_mask_digest(holes),
                hole_count=count_hole_components(holes),
                hole_fraction=float(np.mean(holes, dtype=np.float64)),
            )
        )
    return SceneTextures(layers=tuple(textures))


def bilinear_sample(field: np.ndarray, u: np.ndarray, v: np.ndarray) -> np.ndarray:
    """Sample a [v, u] table, returning 0 outside its domain."""

    height, width = field.shape
    u = np.asarray(u, dtype=np.float64)
    v = np.asarray(v, dtype=np.float64)
    inside = (u >= 0.0) & (u <= 1.0) & (v >= 0.0) & (v <= 1.0)
    uc = np.clip(u, 0.0, 1.0)
    vc = np.clip(v, 0.0, 1.0)
    x = uc * (width - 1)
    y = vc * (height - 1)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    x1 = np.minimum(x0 + 1, width - 1)
    y1 = np.minimum(y0 + 1, height - 1)
    tx = x - x0
    ty = y - y0
    top = field[y0, x0] * (1.0 - tx) + field[y0, x1] * tx
    bottom = field[y1, x0] * (1.0 - tx) + field[y1, x1] * tx
    return (top * (1.0 - ty) + bottom * ty) * inside


def sample_layer_uv(
    layer: Layer, x_mm: np.ndarray, y_mm: np.ndarray
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    xmin, xmax, ymin, ymax = layer.mesh.bounds
    u = (np.asarray(x_mm) - xmin) / max(xmax - xmin, 1e-12)
    v = (np.asarray(y_mm) - ymin) / max(ymax - ymin, 1e-12)
    inside = (u >= 0.0) & (u <= 1.0) & (v >= 0.0) & (v <= 1.0)
    return u, v, inside
