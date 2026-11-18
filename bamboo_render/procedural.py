from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from .io_utils import sha256_bytes, stable_float
from .scene import LayerSpec, SceneSpec

MASK_64 = np.uint64(0xFFFFFFFFFFFFFFFF)


@dataclass(frozen=True)
class LayerTexture:
    layer: int
    fiber_u8: np.ndarray
    hole_u8: np.ndarray
    fiber_sha256: str
    hole_sha256: str
    identity_tint: np.ndarray

    @property
    def height(self) -> int:
        return int(self.fiber_u8.shape[0])

    @property
    def width(self) -> int:
        return int(self.fiber_u8.shape[1])


@dataclass(frozen=True)
class TextureSet:
    layers: dict[int, LayerTexture]
    seed: int

    def sample(self, layer: int, uvs: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        texture = self.layers[layer]
        fiber = _sample_bilinear(texture.fiber_u8, uvs).astype(np.float64) / 255.0
        hole = _sample_bilinear(texture.hole_u8, uvs).astype(np.float64) / 255.0
        return fiber, hole

    def sample_world(self, layer_spec: LayerSpec, points: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        uv = layer_spec.world_to_uv(points)
        return self.sample(layer_spec.layer, uv)


def generate_textures(scene: SceneSpec) -> TextureSet:
    textures = {
        layer.layer: generate_layer_texture(layer, scene.seed)
        for layer in scene.layers
    }
    return TextureSet(layers=textures, seed=scene.seed)


def generate_layer_texture(layer: LayerSpec, seed: int) -> LayerTexture:
    width, height = layer.texture_resolution
    fiber = _fiber_table(layer, seed)
    hole = _hole_table(layer, seed)
    tint_hash = _hash01(
        np.asarray([0], dtype=np.uint64),
        np.asarray([0], dtype=np.uint64),
        seed,
        layer.layer,
        0x9E3779B97F4A7C15,
    )[0]
    tint = np.array(
        [
            0.96 + 0.08 * tint_hash,
            0.96 + 0.08 * _hash01_scalar(seed, layer.layer, 0x243F6A8885A308D3),
            0.96 + 0.08 * _hash01_scalar(seed, layer.layer, 0x13198A2E03707344),
        ],
        dtype=np.float64,
    )
    return LayerTexture(
        layer=layer.layer,
        fiber_u8=fiber,
        hole_u8=hole,
        fiber_sha256=sha256_bytes(np.ascontiguousarray(fiber).tobytes()),
        hole_sha256=sha256_bytes(np.ascontiguousarray(hole).tobytes()),
        identity_tint=tint,
    )


def save_textures(
    textures: TextureSet,
    scene: SceneSpec,
    output_dir: Path,
) -> dict[str, Any]:
    texture_dir = output_dir / "textures"
    texture_dir.mkdir(parents=True, exist_ok=True)
    layer_reports: list[dict[str, Any]] = []
    for layer in scene.layers:
        texture = textures.layers[layer.layer]
        stem = f"layer_{layer.layer:02d}"
        fiber_path = texture_dir / f"{stem}_fiber.npy"
        hole_path = texture_dir / f"{stem}_hole_mask.npy"
        np.save(fiber_path, texture.fiber_u8, allow_pickle=False)
        np.save(hole_path, texture.hole_u8, allow_pickle=False)
        Image.fromarray(texture.fiber_u8, mode="L").save(
            texture_dir / f"{stem}_fiber.png",
            format="PNG",
            compress_level=6,
        )
        Image.fromarray(texture.hole_u8, mode="L").save(
            texture_dir / f"{stem}_hole_mask.png",
            format="PNG",
            compress_level=6,
        )
        layer_reports.append(
            {
                "layer": layer.layer,
                "name": layer.name,
                "resolution": [texture.width, texture.height],
                "fiber_sha256": texture.fiber_sha256,
                "hole_mask_sha256": texture.hole_sha256,
                "fiber_mean_u8": stable_float(float(texture.fiber_u8.mean())),
                "hole_mean_u8": stable_float(float(texture.hole_u8.mean())),
                "hole_fraction": stable_float(float(np.mean(texture.hole_u8 >= 128))),
                "identity_tint_rgb": [stable_float(value) for value in texture.identity_tint],
                "fiber_file": f"textures/{stem}_fiber.npy",
                "hole_mask_file": f"textures/{stem}_hole_mask.npy",
            }
        )
    return {
        "schema_version": 1,
        "seed": scene.seed,
        "layers": layer_reports,
        "generation": "splitmix64-hash + seeded value noise + analytic trihex weave",
    }


def _fiber_table(layer: LayerSpec, seed: int) -> np.ndarray:
    width, height = layer.texture_resolution
    y, x = np.mgrid[0:height, 0:width]
    xf = (x.astype(np.float64) + 0.5) / width
    yf = (y.astype(np.float64) + 0.5) / height
    phase = _hash01_scalar(seed, layer.layer, 0xA4093822299F31D0) * (2.0 * np.pi)

    broad = _value_noise(width, height, seed, layer.layer, cells_x=max(4, width // 28), cells_y=max(4, height // 28), salt=1)
    medium = _value_noise(width, height, seed, layer.layer, cells_x=max(6, width // 14), cells_y=max(6, height // 14), salt=2)
    fine = _value_noise(width, height, seed, layer.layer, cells_x=max(8, width // 7), cells_y=max(8, height // 7), salt=3)
    mottle = np.clip(0.50 * broad + 0.32 * medium + 0.18 * fine, 0.0, 1.0)
    grain = 0.5 + 0.5 * np.sin(
        2.0 * np.pi * (12.0 * xf + 0.65 * yf + 1.75 * broad) + phase
    )
    cross_grain = 0.5 + 0.5 * np.sin(
        2.0 * np.pi * (-3.0 * xf + 19.0 * yf + 0.9 * medium) + phase * 0.37
    )
    pattern = 0.58 * mottle + 0.27 * grain + 0.15 * cross_grain
    contrast = layer.liner.fiber_contrast
    centered = 0.5 + (pattern - 0.5) * (0.65 + 1.15 * contrast)
    layer_bias = (_hash01_scalar(seed, layer.layer, 0x082EFA98EC4E6C89) - 0.5) * 0.035
    result = np.clip(centered + layer_bias, 0.0, 1.0)
    return np.rint(result * 255.0).astype(np.uint8)


def _hole_table(layer: LayerSpec, seed: int) -> np.ndarray:
    width, height = layer.texture_resolution
    y, x = np.mgrid[0:height, 0:width]
    u = (x.astype(np.float64) + 0.5) / width
    v = (y.astype(np.float64) + 0.5) / height
    world_width = max(float(layer.xy_max[0] - layer.xy_min[0]), 1e-9)
    world_height = max(float(layer.xy_max[1] - layer.xy_min[1]), 1e-9)
    px = u * world_width
    py = v * world_height

    wobble_x = _value_noise(
        width,
        height,
        seed,
        layer.layer,
        cells_x=max(3, width // 32),
        cells_y=max(3, height // 32),
        salt=11,
    ) - 0.5
    wobble_y = _value_noise(
        width,
        height,
        seed,
        layer.layer,
        cells_x=max(3, width // 32),
        cells_y=max(3, height // 32),
        salt=12,
    ) - 0.5
    wobble = layer.weave_pitch * 0.018
    px = px + wobble * wobble_x
    py = py + wobble * wobble_y

    sqrt3 = np.sqrt(3.0)
    normals = (
        (1.0, 0.0),
        (0.5, sqrt3 / 2.0),
        (-0.5, sqrt3 / 2.0),
    )
    k = 2.0 * np.pi / layer.weave_pitch
    distances: list[np.ndarray] = []
    for family, (nx, ny) in enumerate(normals):
        offset = _hash01_scalar(seed, layer.layer, 1000 + family)
        phase = k * (px * nx + py * ny) + 2.0 * np.pi * offset
        cosine = np.cos(phase)
        distance = (1.0 - cosine) / k
        distances.append(distance)
    nearest = np.minimum(np.minimum(distances[0], distances[1]), distances[2])
    radius = layer.strand_diameter * 0.5
    pixel_world = min(world_width / width, world_height / height)
    aa = max(pixel_world * 0.58, 1e-9)
    coverage = 1.0 - _smoothstep(radius - aa, radius + aa, nearest)
    hole = np.clip(1.0 - coverage, 0.0, 1.0)
    return np.rint(hole * 255.0).astype(np.uint8)


def _value_noise(
    width: int,
    height: int,
    seed: int,
    layer: int,
    cells_x: int,
    cells_y: int,
    salt: int,
) -> np.ndarray:
    cells_x = max(2, int(cells_x))
    cells_y = max(2, int(cells_y))
    gx = (np.arange(width, dtype=np.float64) + 0.5) * cells_x / width
    gy = (np.arange(height, dtype=np.float64) + 0.5) * cells_y / height
    x0 = np.floor(gx).astype(np.int64)
    y0 = np.floor(gy).astype(np.int64)
    fx = _smoothstep(0.0, 1.0, gx - x0)
    fy = _smoothstep(0.0, 1.0, gy - y0)
    x0u = x0.astype(np.uint64)
    y0u = y0.astype(np.uint64)
    x1u = (x0 + 1).astype(np.uint64)
    y1u = (y0 + 1).astype(np.uint64)
    xx0, yy0 = np.meshgrid(x0u, y0u)
    xx1, yy1 = np.meshgrid(x1u, y1u)
    v00 = _hash01(xx0, yy0, seed, layer, salt)
    v10 = _hash01(xx1, yy0, seed, layer, salt)
    v01 = _hash01(xx0, yy1, seed, layer, salt)
    v11 = _hash01(xx1, yy1, seed, layer, salt)
    fx_grid = np.broadcast_to(fx[None, :], (height, width))
    fy_grid = np.broadcast_to(fy[:, None], (height, width))
    top = v00 * (1.0 - fx_grid) + v10 * fx_grid
    bottom = v01 * (1.0 - fx_grid) + v11 * fx_grid
    return top * (1.0 - fy_grid) + bottom * fy_grid


def _hash01(
    x: np.ndarray,
    y: np.ndarray,
    seed: int,
    layer: int,
    salt: int,
) -> np.ndarray:
    with np.errstate(over="ignore"):
        value = x.astype(np.uint64, copy=False) * np.uint64(0x9E3779B97F4A7C15)
        value = value ^ (y.astype(np.uint64, copy=False) * np.uint64(0xC2B2AE3D27D4EB4F))
        value = value ^ np.uint64(seed & 0xFFFFFFFFFFFFFFFF)
        value = value ^ (np.uint64(layer & 0xFFFFFFFF) * np.uint64(0xD6E8FEB86659FD93))
        value = value ^ np.uint64(salt & 0xFFFFFFFFFFFFFFFF)
        value = _mix64(value)
    return (value >> np.uint64(11)).astype(np.float64) * (1.0 / 9007199254740992.0)


def _hash01_scalar(seed: int, layer: int, salt: int) -> float:
    x = np.asarray([0], dtype=np.uint64)
    y = np.asarray([0], dtype=np.uint64)
    return float(_hash01(x, y, seed, layer, salt)[0])


def _mix64(value: np.ndarray) -> np.ndarray:
    z = value.astype(np.uint64, copy=True)
    z = (z ^ (z >> np.uint64(30))) * np.uint64(0xBF58476D1CE4E5B9)
    z = (z ^ (z >> np.uint64(27))) * np.uint64(0x94D049BB133111EB)
    return z ^ (z >> np.uint64(31))


def _smoothstep(edge0: float, edge1: float, value: np.ndarray) -> np.ndarray:
    if edge1 <= edge0:
        return (value >= edge1).astype(np.float64)
    t = np.clip((value - edge0) / (edge1 - edge0), 0.0, 1.0)
    return t * t * (3.0 - 2.0 * t)


def _sample_bilinear(table: np.ndarray, uvs: np.ndarray) -> np.ndarray:
    uv = np.asarray(uvs, dtype=np.float64).reshape(-1, 2)
    height, width = table.shape
    x = np.clip(uv[:, 0], 0.0, 1.0) * (width - 1)
    y = np.clip(uv[:, 1], 0.0, 1.0) * (height - 1)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    x1 = np.minimum(x0 + 1, width - 1)
    y1 = np.minimum(y0 + 1, height - 1)
    fx = (x - x0).astype(np.float64)
    fy = (y - y0).astype(np.float64)
    v00 = table[y0, x0].astype(np.float64)
    v10 = table[y0, x1].astype(np.float64)
    v01 = table[y1, x0].astype(np.float64)
    v11 = table[y1, x1].astype(np.float64)
    top = v00 * (1.0 - fx) + v10 * fx
    bottom = v01 * (1.0 - fx) + v11 * fx
    return top * (1.0 - fy) + bottom * fy
