from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .raster import LayerRaster, rasterize_layers
from .scene import Layer, Scene
from .shadows import combined_shadow, projection_report
from .shading import (
    ground_shadow_and_color,
    shade_mullion,
    shade_silk,
    tone_map_premultiplied,
    unit_vectors,
)
from .texture import WeaveTexture, generate_weave_texture


@dataclass(frozen=True)
class TileResult:
    x: int
    y: int
    width: int
    height: int
    rgba: np.ndarray
    layer_map: np.ndarray


class Renderer:
    def __init__(self, scene: Scene):
        self.scene = scene
        self.rasters: list[LayerRaster] = rasterize_layers(scene)
        self.textures: dict[int, WeaveTexture] = {}
        for layer in scene.silk_layers:
            seed = int(layer.material["pattern_seed"])
            resolution = tuple(int(value) for value in layer.material["texture_resolution"])
            self.textures[layer.layer_index] = generate_weave_texture(seed, resolution)
        self.projection = projection_report(scene)

    def _ground_intersections(
        self, xs: np.ndarray, ys: np.ndarray
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        scene = self.scene
        rays = scene.camera.world_ray_grid(xs, ys)
        origin_y = float(scene.camera.position[1])
        denominator = rays[..., 1]
        valid = np.abs(denominator) > 1.0e-12
        t = np.full(denominator.shape, np.inf, dtype=np.float64)
        t[valid] = (scene.ground.y - origin_y) / denominator[valid]
        hits = valid & (t > 0.0)
        points = scene.camera.position[None, None, :] + t[..., None] * rays
        xmin, xmax, zmin, zmax = scene.ground.bounds
        inside = (
            hits
            & (points[..., 0] >= xmin)
            & (points[..., 0] <= xmax)
            & (points[..., 2] >= zmin)
            & (points[..., 2] <= zmax)
        )
        if not scene.ground.render:
            inside &= False
        return inside, points, rays

    def _shade_silk_layer(
        self,
        raster: LayerRaster,
        world: np.ndarray,
        normal: np.ndarray,
    ) -> tuple[np.ndarray, np.ndarray, int]:
        scene = self.scene
        layer = raster.layer
        points = np.asarray(world, dtype=np.float64).reshape(-1, 3)
        view_direction = unit_vectors(scene.camera.position[None, :] - points)
        shadow, evaluations = combined_shadow(scene, points)
        shade = shade_silk(
            scene,
            layer,
            self.textures[layer.layer_index],
            points,
            normal,
            view_direction,
            shadow,
        )
        return shade.premultiplied, shade.alpha, evaluations

    def render_tile(self, x: int, y: int, width: int, height: int) -> TileResult:
        scene = self.scene
        xs = np.arange(x, x + width, dtype=np.int64)
        ys = np.arange(y, y + height, dtype=np.int64)
        ground_mask, ground_points, _ = self._ground_intersections(xs, ys)

        premultiplied = np.zeros((height, width, 3), dtype=np.float64)
        alpha = np.zeros((height, width), dtype=np.float64)
        layer_map = np.full((height, width), -1, dtype=np.int32)

        if bool(ground_mask.any()):
            ground_points_flat = ground_points[ground_mask]
            shadow, _ = combined_shadow(scene, ground_points_flat, ground=True)
            ground_color = ground_shadow_and_color(scene, ground_points_flat, shadow)
            premultiplied[ground_mask] = ground_color
            alpha[ground_mask] = 1.0
            layer_map[ground_mask] = -2

        for raster in self.rasters:
            layer = raster.layer
            region = raster.mask[y : y + height, x : x + width]
            if not bool(region.any()):
                continue
            world = raster.world[y : y + height, x : x + width][region]
            normal = raster.normal[y : y + height, x : x + width][region]

            if layer.is_silk:
                source, source_alpha, _ = self._shade_silk_layer(
                    raster, world, normal
                )
            else:
                source, source_alpha = shade_mullion(scene, layer, normal)

            destination = premultiplied[region]
            destination_alpha = alpha[region]
            premultiplied[region] = (
                source + (1.0 - source_alpha[:, None]) * destination
            )
            alpha[region] = source_alpha + (1.0 - source_alpha) * destination_alpha
            visible = source_alpha > 1.0e-5
            if bool(visible.any()):
                region_flat = region.copy()
                flat_positions = np.flatnonzero(region_flat.ravel())
                flat_positions = flat_positions[visible]
                layer_map_flat = layer_map.ravel()
                layer_map_flat[flat_positions] = layer.layer_index

        rgba = tone_map_premultiplied(premultiplied, alpha, scene.exposure)
        return TileResult(
            x=x,
            y=y,
            width=width,
            height=height,
            rgba=rgba,
            layer_map=layer_map,
        )

    def tile_coordinates(self) -> list[tuple[int, int, int, int]]:
        scene = self.scene
        coordinates: list[tuple[int, int, int, int]] = []
        for y in range(0, scene.height, scene.tile_size):
            for x in range(0, scene.width, scene.tile_size):
                width = min(scene.tile_size, scene.width - x)
                height = min(scene.tile_size, scene.height - y)
                coordinates.append((x, y, width, height))
        return coordinates

    def _points_and_view(
        self, raster: LayerRaster
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        points = raster.world[raster.mask]
        normals = raster.normal[raster.mask]
        view_direction = unit_vectors(self.scene.camera.position[None, :] - points)
        return points, normals, view_direction

    def layer_statistics(self) -> tuple[list[dict[str, Any]], int, int]:
        scene = self.scene
        layer_stats: list[dict[str, Any]] = []
        silk_shadow_evaluations = 0
        ground_visible_count = 0

        xs_all = np.arange(scene.width, dtype=np.int64)
        ys_all = np.arange(scene.height, dtype=np.int64)
        ground_mask, ground_points, _ = self._ground_intersections(xs_all, ys_all)
        ground_visible_count = int(ground_mask.sum())

        for raster in self.rasters:
            layer = raster.layer
            visible_fragments = int(raster.mask.sum())
            if layer.is_silk:
                points, normals, view = self._points_and_view(raster)
                shadow, evaluations = combined_shadow(scene, points)
                silk_shadow_evaluations += evaluations
                shade = shade_silk(
                    scene,
                    layer,
                    self.textures[layer.layer_index],
                    points,
                    normals,
                    view,
                    shadow,
                )
                transmitted = float(np.sum(shade.transmitted_luminance))
                reflected = float(np.sum(shade.reflected_luminance))
                total = transmitted + reflected
                if total > 1.0e-15:
                    transmission_fraction = transmitted / total
                    reflection_fraction = reflected / total
                else:
                    transmission_fraction = 0.0
                    reflection_fraction = 0.0
                layer_stats.append(
                    {
                        "layer_index": layer.layer_index,
                        "id": layer.layer_id,
                        "kind": layer.kind,
                        "visible_fragments": visible_fragments,
                        "average_transmittance": (
                            float(np.mean(shade.effective_transmittance))
                            if visible_fragments
                            else 0.0
                        ),
                        "transmission_fraction": transmission_fraction,
                        "reflection_fraction": reflection_fraction,
                    }
                )
            else:
                layer_stats.append(
                    {
                        "layer_index": layer.layer_index,
                        "id": layer.layer_id,
                        "kind": layer.kind,
                        "visible_fragments": visible_fragments,
                        "average_transmittance": 0.0,
                        "transmission_fraction": 0.0,
                        "reflection_fraction": 1.0 if visible_fragments else 0.0,
                    }
                )

        ground_shadow_evaluations = ground_visible_count * len(scene.mullion_layers)
        return (
            layer_stats,
            silk_shadow_evaluations,
            ground_shadow_evaluations,
        )
