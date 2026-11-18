from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .io_utils import sha256_bytes, stable_float
from .procedural import TextureSet
from .projection import accumulated_transmission, layer_transmission, projection_offsets
from .raster import (
    PreparedLayer,
    RasterBuffers,
    prepare_layer,
    rasterize_block,
    render_wall_block,
)
from .scene import BlockSpec, LayerSpec, SceneSpec


@dataclass(frozen=True)
class BlockResult:
    block: BlockSpec
    rgba: np.ndarray
    layer_rgba: np.ndarray
    ownership: np.ndarray
    projection_gray: dict[str, np.ndarray]
    metrics: np.ndarray


@dataclass
class RenderResult:
    rgba: np.ndarray
    layer_rgba: np.ndarray
    ownership: np.ndarray
    projection_gray: dict[str, np.ndarray]
    metrics: np.ndarray
    blocks: list[BlockResult]


class BlockRenderer:
    def __init__(self, scene: SceneSpec, textures: TextureSet) -> None:
        self.scene = scene
        self.textures = textures
        self.offsets = projection_offsets(scene.shadow.sampling_density)
        self.prepared_layers: tuple[PreparedLayer, ...] = tuple(
            prepare_layer(layer, scene.camera, scene.image, scene.light.position)
            for layer in scene.layers
        )
        self.pair_ids: list[str] = []
        for index, source in enumerate(scene.layers):
            if index + 1 < len(scene.layers):
                self.pair_ids.append(f"{source.layer}->{scene.layers[index + 1].layer}")
            self.pair_ids.append(f"{source.layer}->wall")

    def render(self, block: BlockSpec) -> BlockResult:
        scene = self.scene
        block_shape = (block.height, block.width)
        buffers = [
            rasterize_block(
                self.prepared_layers[index],
                scene.camera,
                scene.image,
                (block.x, block.y, block.width, block.height),
            )
            for index in range(len(scene.layers))
        ]
        wall_hit, wall_world, wall_normal = render_wall_block(
            scene.wall,
            scene.camera,
            scene.image,
            (block.x, block.y, block.width, block.height),
        )

        projection_gray = {
            pair_id: np.zeros(block_shape, dtype=np.uint8)
            for pair_id in self.pair_ids
        }
        layer_rgba: list[np.ndarray] = []
        layer_metric_rows: list[np.ndarray] = []

        for index, layer in enumerate(scene.layers):
            buffer = buffers[index]
            previous = list(scene.layers[:index])
            rgba, metrics, caustic_map = self._shade_layer(
                layer,
                buffer,
                previous,
                block,
            )
            layer_rgba.append(rgba)
            layer_metric_rows.append(metrics)

            if index + 1 < len(scene.layers):
                target = scene.layers[index + 1]
                target_buffer = buffers[index + 1]
                gray = projection_gray[f"{layer.layer}->{target.layer}"]
                if np.any(target_buffer.hit):
                    points = target_buffer.world[target_buffer.hit]
                    transmission, _ = layer_transmission(
                        layer,
                        self.textures.layers[layer.layer],
                        points,
                        scene.light,
                        scene.shadow,
                        self.offsets,
                    )
                    gray[target_buffer.hit] = np.rint(
                        np.clip(transmission, 0.0, 1.0) * 255.0
                    ).astype(np.uint8)

            gray_wall = projection_gray[f"{layer.layer}->wall"]
            if np.any(wall_hit):
                points = wall_world[wall_hit]
                transmission, _ = layer_transmission(
                    layer,
                    self.textures.layers[layer.layer],
                    points,
                    scene.light,
                    scene.shadow,
                    self.offsets,
                )
                gray_wall[wall_hit] = np.rint(
                    np.clip(transmission, 0.0, 1.0) * 255.0
                ).astype(np.uint8)

        wall_rgba = self._shade_wall(wall_hit, wall_world, wall_normal)
        rgba = self._composite(layer_rgba, wall_rgba, wall_hit)
        ownership = self._ownership(layer_rgba, wall_rgba, wall_hit)
        metrics = np.stack(layer_metric_rows, axis=0)
        return BlockResult(
            block=block,
            rgba=rgba,
            layer_rgba=np.stack(layer_rgba, axis=0),
            ownership=ownership,
            projection_gray=projection_gray,
            metrics=metrics,
        )

    def _shade_layer(
        self,
        layer: LayerSpec,
        buffer: RasterBuffers,
        previous_layers: list[LayerSpec],
        block: BlockSpec,
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        scene = self.scene
        height, width = block.height, block.width
        rgba = np.zeros((height, width, 4), dtype=np.uint8)
        caustic_map = np.zeros((height, width), dtype=np.float64)
        metrics = np.zeros(5, dtype=np.float64)
        if not np.any(buffer.hit):
            return rgba, metrics, caustic_map

        world = buffer.world[buffer.hit]
        uv = buffer.uv[buffer.hit]
        normal = buffer.normal[buffer.hit]
        normal /= np.maximum(np.linalg.norm(normal, axis=-1, keepdims=True), 1e-15)
        fiber, hole = self.textures.sample(layer.layer, uv)
        coverage = 1.0 - hole

        to_light = scene.light.position[None, :] - world
        distance_sq = np.maximum(np.sum(to_light * to_light, axis=-1), 1e-8)
        light_direction = to_light / np.sqrt(distance_sq)[:, None]
        cos_light = np.clip(np.abs(np.sum(normal * light_direction, axis=-1)), 0.0, 1.0)
        to_camera = scene.camera.position[None, :] - world
        camera_direction = to_camera / np.maximum(
            np.linalg.norm(to_camera, axis=-1, keepdims=True),
            1e-15,
        )
        cos_view = np.clip(np.abs(np.sum(normal * camera_direction, axis=-1)), 0.0, 1.0)
        fresnel = 0.04 + 0.96 * np.power(1.0 - cos_view, 5.0)
        irradiance = scene.light.intensity / (0.65 + distance_sq)
        fiber_modulation = 0.82 + 0.36 * fiber

        tau = hole + coverage * np.clip(
            layer.liner.transmittance * fiber_modulation,
            0.0,
            1.0,
        )
        scatter_color = layer.liner.scatter_color[None, :] * self.textures.layers[layer.layer].identity_tint[None, :]
        transmission = (
            scene.light.color[None, :]
            * irradiance[:, None]
            * tau[:, None]
            * (0.16 + 0.84 * cos_light)[:, None]
            * 0.95
        )
        reflection = (
            layer.strand_color[None, :]
            * self.textures.layers[layer.layer].identity_tint[None, :]
            * irradiance[:, None]
            * (layer.liner.reflectance * fresnel)[:, None]
            * (0.24 + 0.76 * cos_light)[:, None]
            * (0.72 + 0.28 * fiber)[:, None]
        )
        scatter = (
            scatter_color
            * irradiance[:, None]
            * (0.18 + 0.82 * cos_light)[:, None]
            * (0.72 + 0.28 * fiber)[:, None]
            * (1.0 - 0.60 * hole)[:, None]
        )

        if previous_layers:
            accumulated = accumulated_transmission(
                previous_layers,
                self.textures,
                world,
                scene.light,
                scene.shadow,
                self.offsets,
            )
            caustic_color = (
                scene.light.color[None, :]
                * scene.layers[0].liner.scatter_color[None, :]
            )
            caustic = (
                caustic_color
                * irradiance[:, None]
                * accumulated[:, None]
                * (0.22 + 0.78 * cos_light)[:, None]
                * 1.40
            )
        else:
            accumulated = np.zeros(world.shape[0], dtype=np.float64)
            caustic = np.zeros((world.shape[0], 3), dtype=np.float64)

        radiance = transmission + reflection + scatter + caustic
        mapped = 0.982 * (1.0 - np.exp(-np.clip(radiance, 0.0, 50.0)))
        mapped = np.power(np.clip(mapped, 0.0, 1.0), 1.0 / 2.2)
        alpha = coverage * 0.62 + hole * 0.72
        premultiplied = mapped * alpha[:, None]

        rgba_hit = np.empty((world.shape[0], 4), dtype=np.float64)
        rgba_hit[:, :3] = premultiplied
        rgba_hit[:, 3] = alpha
        rgba[buffer.hit] = np.rint(np.clip(rgba_hit, 0.0, 1.0) * 255.0).astype(np.uint8)
        caustic_map[buffer.hit] = accumulated

        luma_weights = np.array([0.2126, 0.7152, 0.0722], dtype=np.float64)
        metrics[0] = float(np.sum(transmission @ luma_weights))
        metrics[1] = float(np.sum(reflection @ luma_weights))
        metrics[2] = float(np.sum(scatter @ luma_weights))
        metrics[3] = float(np.sum(caustic @ luma_weights))
        metrics[4] = float(world.shape[0])
        return rgba, metrics, caustic_map

    def _shade_wall(
        self,
        wall_hit: np.ndarray,
        wall_world: np.ndarray,
        wall_normal: np.ndarray,
    ) -> np.ndarray:
        scene = self.scene
        rgba = np.zeros((*wall_hit.shape, 4), dtype=np.uint8)
        if not np.any(wall_hit):
            return rgba
        world = wall_world[wall_hit]
        normal = wall_normal[wall_hit]
        to_light = scene.light.position[None, :] - world
        distance_sq = np.maximum(np.sum(to_light * to_light, axis=-1), 1e-8)
        light_direction = to_light / np.sqrt(distance_sq)[:, None]
        cos_light = np.clip(
            np.abs(np.sum(normal * light_direction, axis=-1)),
            0.0,
            1.0,
        )
        irradiance = scene.light.intensity / (0.65 + distance_sq)
        base = (
            scene.wall.color[None, :]
            * (0.24 + 0.66 * irradiance[:, None] * cos_light[:, None])
            + scene.wall.color[None, :]
            * scene.wall.reflectance
            * (0.2 + 0.8 * cos_light)[:, None]
        )
        accumulated = accumulated_transmission(
            list(scene.layers),
            self.textures,
            world,
            scene.light,
            scene.shadow,
            self.offsets,
        )
        caustic = (
            scene.light.color[None, :]
            * scene.layers[-1].liner.scatter_color[None, :]
            * irradiance[:, None]
            * accumulated[:, None]
            * 0.90
        )
        radiance = base + caustic
        mapped = 0.982 * (1.0 - np.exp(-np.clip(radiance, 0.0, 50.0)))
        mapped = np.power(np.clip(mapped, 0.0, 1.0), 1.0 / 2.2)
        wall_pixels = np.empty((world.shape[0], 4), dtype=np.float64)
        wall_pixels[:, :3] = mapped
        wall_pixels[:, 3] = 1.0
        rgba[wall_hit] = np.rint(np.clip(wall_pixels, 0.0, 1.0) * 255.0).astype(np.uint8)
        return rgba

    def _composite(
        self,
        layer_rgba: list[np.ndarray],
        wall_rgba: np.ndarray,
        wall_hit: np.ndarray,
    ) -> np.ndarray:
        result = wall_rgba.astype(np.float64) / 255.0
        if not np.any(wall_hit):
            result[..., 3] = 0.0
        for layer in reversed(layer_rgba):
            source = layer.astype(np.float64) / 255.0
            alpha = source[..., 3:4]
            result[..., :3] = source[..., :3] + (1.0 - alpha) * result[..., :3]
            result[..., 3:4] = alpha + (1.0 - alpha) * result[..., 3:4]
        return np.rint(np.clip(result, 0.0, 1.0) * 255.0).astype(np.uint8)

    def _ownership(
        self,
        layer_rgba: list[np.ndarray],
        wall_rgba: np.ndarray,
        wall_hit: np.ndarray,
    ) -> np.ndarray:
        scene = self.scene
        height, width, _ = wall_rgba.shape
        ownership = np.zeros((height, width, 4), dtype=np.uint8)
        occlusion = np.ones((height, width), dtype=np.float64)
        layer_contributions: list[np.ndarray] = []
        layer_best_color = np.ones((height, width), dtype=np.float64)
        layer_alphas: list[np.ndarray] = []
        luma_weights = np.array([0.2126, 0.7152, 0.0722], dtype=np.float64)

        for layer in layer_rgba:
            values = layer.astype(np.float64) / 255.0
            alpha = values[..., 3]
            unpremultiplied = np.divide(
                values[..., :3],
                np.maximum(alpha[..., None], 1e-9),
            )
            contribution = alpha * occlusion * np.sum(
                unpremultiplied * luma_weights[None, None, :],
                axis=-1,
            )
            layer_contributions.append(contribution)
            layer_alphas.append(alpha)
            occlusion *= 1.0 - alpha

        wall_values = wall_rgba.astype(np.float64) / 255.0
        wall_contrib = (
            occlusion
            * wall_values[..., 3]
            * np.sum(
                wall_values[..., :3]
                * luma_weights[None, None, :],
                axis=-1,
            )
        )

        best = np.full((height, width), -1.0, dtype=np.float64)
        owner = np.zeros((height, width), dtype=np.uint8)
        active = np.zeros((height, width), dtype=np.uint8)
        front = np.zeros((height, width), dtype=np.uint8)
        for index, (layer, contribution) in enumerate(zip(scene.layers, layer_contributions)):
            choose = contribution > best
            best[choose] = contribution[choose]
            owner[choose] = min(layer.layer, 254)
            active[layer_alphas[index] > 0.02] |= np.uint8(1 << min(index, 7))
            front[front == 0] = np.where(
                layer_alphas[index][front == 0] > 0.50,
                min(layer.layer, 254),
                0,
            ).astype(np.uint8)
        choose_wall = wall_contrib > best
        owner[choose_wall] = 255
        active[wall_hit] |= np.uint8(128)
        front[front == 0] = np.where(
            wall_values[..., 3][front == 0] > 0.5,
            255,
            0,
        ).astype(np.uint8)
        owner[~wall_hit & (best < 0.0)] = 0
        ownership[..., 0] = owner
        ownership[..., 1] = active
        ownership[..., 2] = front
        ownership[..., 3] = 255
        return ownership


def render_blocks(
    scene: SceneSpec,
    textures: TextureSet,
    block_results: list[BlockResult],
) -> RenderResult:
    rgba = np.zeros((scene.image.height, scene.image.width, 4), dtype=np.uint8)
    layer_rgba = np.zeros(
        (len(scene.layers), scene.image.height, scene.image.width, 4),
        dtype=np.uint8,
    )
    ownership = np.zeros((scene.image.height, scene.image.width, 4), dtype=np.uint8)
    pair_ids = list(block_results[0].projection_gray.keys()) if block_results else []
    projection_gray = {
        pair_id: np.zeros((scene.image.height, scene.image.width), dtype=np.uint8)
        for pair_id in pair_ids
    }
    metrics = np.zeros((len(scene.layers), 5), dtype=np.float64)
    for result in block_results:
        block = result.block
        rgba[block.y : block.y + block.height, block.x : block.x + block.width] = result.rgba
        layer_rgba[
            :,
            block.y : block.y + block.height,
            block.x : block.x + block.width,
            :,
        ] = result.layer_rgba
        ownership[
            block.y : block.y + block.height,
            block.x : block.x + block.width,
        ] = result.ownership
        for pair_id, values in result.projection_gray.items():
            projection_gray[pair_id][
                block.y : block.y + block.height,
                block.x : block.x + block.width,
            ] = values
        metrics += result.metrics
    combined = np.zeros((scene.image.height, scene.image.width), dtype=np.uint8)
    for values in projection_gray.values():
        combined = np.maximum(combined, values)
    projection_gray["combined"] = combined
    return RenderResult(
        rgba=rgba,
        layer_rgba=layer_rgba,
        ownership=ownership,
        projection_gray=projection_gray,
        metrics=metrics,
        blocks=block_results,
    )


def block_color_summary(rgba: np.ndarray, ownership: np.ndarray) -> dict[str, Any]:
    return {
        "rgba_sha256": sha256_bytes(np.ascontiguousarray(rgba).tobytes()),
        "ownership_sha256": sha256_bytes(np.ascontiguousarray(ownership).tobytes()),
        "mean_rgba": [stable_float(float(value)) for value in rgba.reshape(-1, 4).mean(axis=0)],
    }
