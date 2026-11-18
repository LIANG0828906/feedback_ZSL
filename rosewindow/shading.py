"""Per-chunk glass transmission, enamel reflection, and stone shading."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .geometry import (
    KIND_GLASS,
    KIND_LEAD,
    KIND_STONE,
    KIND_WALL,
    HitBuffers,
)
from .scene import GlassPiece, Scene
from .shadow import ShadowResult
from .texture import TextureBank, bilinear_sample


@dataclass(frozen=True)
class ChunkShadeResult:
    rgb: np.ndarray
    owner: np.ndarray
    transmission_sums: np.ndarray
    reflection_sums: np.ndarray
    pixel_counts: np.ndarray
    max_linear_value: float


def _tone_encode(linear: np.ndarray, scene: Scene) -> np.ndarray:
    linear = np.clip(linear, 0.0, None)
    toned = 1.0 - np.exp(-linear * scene.render.exposure)
    gamma = np.power(np.clip(toned, 0.0, 1.0), 1.0 / scene.render.gamma)
    return np.floor(gamma * 255.0 + 0.5).astype(np.uint8)


def _compress_highlights(linear: np.ndarray) -> np.ndarray:
    """Keep <=1 by scaling all channels together, preserving the hue."""
    peak = linear.max(axis=1, keepdims=True)
    scale = np.minimum(1.0, 1.0 / np.maximum(peak, 1e-12))
    return linear * scale


def _piece_uv(
    piece: GlassPiece, positions: np.ndarray
) -> tuple[np.ndarray, np.ndarray]:
    min_x, min_y, max_x, max_y = piece.bounds_xy
    width = max(max_x - min_x, 1e-12)
    height = max(max_y - min_y, 1e-12)
    u = (positions[:, 0] - min_x) / width
    v = (positions[:, 1] - min_y) / height
    return u, v


def _hash_to_unit(value: np.ndarray | int) -> np.ndarray:
    array = np.asarray(value, dtype=np.uint64)
    mixed = (array ^ (array >> np.uint64(33))) * np.uint64(0xFF51AFD7ED558CCD)
    mixed = (mixed ^ (mixed >> np.uint64(33))) * np.uint64(0xC4CEB9FE1A85EC53)
    mixed = mixed ^ (mixed >> np.uint64(33))
    return (mixed >> np.uint64(40)).astype(np.float64) / float(1 << 24)


def _shade_piece(
    scene: Scene,
    piece: GlassPiece,
    texture_bank: TextureBank,
    positions: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, float]:
    texture = texture_bank.get(piece.serial)
    u, v = _piece_uv(piece, positions)
    transmission_texture = bilinear_sample(texture.transmission, u, v)[:, None]
    reflection_texture = bilinear_sample(texture.reflection, u, v)[:, None]

    normal = piece.normal
    light_direction = scene.light.direction
    cosine_incidence = min(1.0, abs(float(np.dot(normal, light_direction))))
    view = scene.camera.position[None, :] - positions
    view_length = np.linalg.norm(view, axis=1)
    view = view / np.maximum(view_length[:, None], 1e-12)
    cosine_view = np.clip(np.abs(view @ normal), 0.0, 1.0)

    transmission_color = piece.transmission_color[None, :]
    ambient = scene.render.ambient_transmission * (0.62 + 0.38 * transmission_texture)
    direct = (
        scene.light.intensity
        * cosine_incidence
        * scene.light.color[None, :]
    )
    transmitted_linear = (
        transmission_color
        * piece.transmittance
        * transmission_texture
        * (ambient + direct)
    )
    transmitted_linear = np.clip(transmitted_linear, 0.0, None)

    f0 = np.clip(
        piece.enamel_reflectance * reflection_texture, 0.0, 0.999
    )
    fresnel = f0 + (1.0 - f0) * np.power(
        1.0 - cosine_view[:, None], scene.reflection.fresnel_power
    )
    fresnel = np.clip(fresnel, 0.0, 0.95)
    environment = (
        scene.reflection.environment_color[None, :]
        * scene.reflection.intensity
        * (0.78 + 0.32 * reflection_texture)
    )
    transmission_contribution = transmitted_linear * (1.0 - fresnel)
    reflection_contribution = environment * fresnel
    linear = _compress_highlights(
        transmission_contribution + reflection_contribution
    )
    return (
        linear,
        transmission_contribution.sum(axis=1),
        reflection_contribution.sum(axis=1),
        float(fresnel.mean()),
    )


def _stone_color_at(
    scene: Scene, positions: np.ndarray
) -> tuple[np.ndarray, np.ndarray]:
    ix = np.floor(
        (positions[:, 0] - (scene.wall.center_x - scene.wall.width * 0.5))
        / scene.wall.block_width
    ).astype(np.int64)
    iy = np.floor(
        (positions[:, 1] - (scene.wall.center_y - scene.wall.height * 0.5))
        / scene.wall.block_height
    ).astype(np.int64)
    block_hash = (
        (ix.astype(np.uint64) * np.uint64(0x9E3779B1))
        ^ (iy.astype(np.uint64) * np.uint64(0x85EBCA77))
        ^ np.uint64(0xC2B2AE3D)
    )
    variation = 0.82 + 0.30 * _hash_to_unit(block_hash)
    local_x = (
        (positions[:, 0] - (scene.wall.center_x - scene.wall.width * 0.5))
        / scene.wall.block_width
    ) - ix
    local_y = (
        (positions[:, 1] - (scene.wall.center_y - scene.wall.height * 0.5))
        / scene.wall.block_height
    ) - iy
    mortar_distance = np.minimum(
        np.minimum(local_x, 1.0 - local_x) * scene.wall.block_width,
        np.minimum(local_y, 1.0 - local_y) * scene.wall.block_height,
    )
    mortar = np.clip((0.010 - mortar_distance) / 0.010, 0.0, 1.0)
    color = scene.wall.color[None, :] * variation[:, None]
    color = color * (1.0 - 0.42 * mortar[:, None])
    return color, mortar


def _shade_stone(
    scene: Scene,
    positions: np.ndarray,
    shadow: ShadowResult,
    *,
    opposite_wall: bool,
) -> np.ndarray:
    color, _mortar = _stone_color_at(scene, positions)
    normal = np.asarray([0.0, 0.0, -1.0], dtype=np.float64)
    diffuse = abs(float(np.dot(normal, scene.light.direction)))
    illumination = scene.render.wall_ambient + scene.render.wall_direct * diffuse
    linear = color * illumination
    if opposite_wall and len(positions):
        coverage = shadow.sample(positions[:, 0], positions[:, 1])
        linear = linear * (1.0 - 0.82 * coverage[:, None])
    return linear


def render_chunk(
    scene: Scene,
    texture_bank: TextureBank,
    buffers: HitBuffers,
    shadow: ShadowResult,
    x0: int,
    y0: int,
    x1: int,
    y1: int,
) -> ChunkShadeResult:
    height = y1 - y0
    width = x1 - x0
    linear = np.zeros((height, width, 3), dtype=np.float64)
    kinds = buffers.kind[y0:y1, x0:x1]
    owners = buffers.owner[y0:y1, x0:x1]
    world = buffers.world[y0:y1, x0:x1]
    piece_count = len(scene.pieces)
    transmission_sums = np.zeros(piece_count, dtype=np.float64)
    reflection_sums = np.zeros(piece_count, dtype=np.float64)
    pixel_counts = np.zeros(piece_count, dtype=np.int64)
    max_linear_value = 0.0

    for piece in scene.pieces:
        mask = (kinds == KIND_GLASS) & (owners == piece.serial)
        if not np.any(mask):
            continue
        positions = world[mask]
        piece_linear, trans_values, refl_values, _fresnel = _shade_piece(
            scene, piece, texture_bank, positions
        )
        piece_linear = np.clip(piece_linear, 0.0, None)
        linear[mask] = piece_linear
        piece_index = piece.serial - 1
        transmission_sums[piece_index] = float(trans_values.sum())
        reflection_sums[piece_index] = float(refl_values.sum())
        pixel_counts[piece_index] = int(piece_linear.shape[0])
        max_linear_value = max(max_linear_value, float(piece_linear.max()))

    wall_mask = kinds == KIND_WALL
    if np.any(wall_mask):
        wall_linear = _shade_stone(
            scene, world[wall_mask], shadow, opposite_wall=True
        )
        linear[wall_mask] = wall_linear
        max_linear_value = max(max_linear_value, float(wall_linear.max()))

    stone_mask = kinds == KIND_STONE
    if np.any(stone_mask):
        stone_linear = _shade_stone(
            scene, world[stone_mask], shadow, opposite_wall=False
        )
        linear[stone_mask] = stone_linear
        max_linear_value = max(max_linear_value, float(stone_linear.max()))

    lead_mask = kinds == KIND_LEAD
    if np.any(lead_mask):
        lead_linear = np.asarray([0.018, 0.0155, 0.013], dtype=np.float64)
        linear[lead_mask] = lead_linear
        max_linear_value = max(max_linear_value, float(lead_linear.max()))

    background_mask = kinds == 0
    if np.any(background_mask):
        background = np.asarray([0.012, 0.014, 0.013], dtype=np.float64)
        linear[background_mask] = background

    rgb = _tone_encode(linear, scene)
    owner = np.where(kinds == KIND_GLASS, owners, 0).astype(np.int32)
    return ChunkShadeResult(
        rgb=rgb,
        owner=owner,
        transmission_sums=transmission_sums,
        reflection_sums=reflection_sums,
        pixel_counts=pixel_counts,
        max_linear_value=max_linear_value,
    )
