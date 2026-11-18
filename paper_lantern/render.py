from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .geometry import PlanarMapper, build_mappers, ray_mesh_uv_hit
from .scene import Layer, Scene
from .texture import Texture, generate_texture, sample_bilinear


@dataclass
class ProjectedTriangle:
    screen: np.ndarray
    view_z: np.ndarray
    uv: np.ndarray
    normal: np.ndarray
    triangle_id: int


@dataclass
class TileResult:
    rgba: np.ndarray
    depth: np.ndarray
    layer_rgba: np.ndarray
    visible_fragments: np.ndarray
    paper_fragments: np.ndarray
    effective_transmission_sum: np.ndarray
    cutout_fragments: np.ndarray
    shadow_sample_rays: int
    shadow_occluder_tests: int


def _clip_polygon_near(
    view_points: np.ndarray,
    uvs: np.ndarray,
    near: float,
) -> tuple[np.ndarray, np.ndarray]:
    output_points: list[np.ndarray] = []
    output_uvs: list[np.ndarray] = []
    count = len(view_points)
    for index in range(count):
        current = view_points[index]
        following = view_points[(index + 1) % count]
        current_uv = uvs[index]
        following_uv = uvs[(index + 1) % count]
        current_inside = current[2] >= near
        following_inside = following[2] >= near
        if current_inside:
            output_points.append(current)
            output_uvs.append(current_uv)
        if current_inside != following_inside:
            alpha = (near - current[2]) / (following[2] - current[2])
            output_points.append(current + alpha * (following - current))
            output_uvs.append(current_uv + alpha * (following_uv - current_uv))
    if not output_points:
        return np.empty((0, 3)), np.empty((0, 2))
    return np.asarray(output_points), np.asarray(output_uvs)


class RenderEngine:
    def __init__(self, scene: Scene):
        self.scene = scene
        self.camera = scene.camera
        self.image = scene.image
        self.tan_half = math.tan(math.radians(scene.camera.fov_y_deg) * 0.5)
        self.aspect = scene.image.width / scene.image.height
        self.textures: tuple[Texture, ...] = tuple(
            generate_texture(scene.seed, layer.layer_index, layer.texture_resolution)
            for layer in scene.layers
        )
        self.texture_by_layer = {
            layer.layer_index: texture for layer, texture in zip(scene.layers, self.textures)
        }
        self.mappers: tuple[PlanarMapper | None, ...] = build_mappers(scene.layers)
        self.projected: tuple[tuple[ProjectedTriangle, ...], ...] = tuple(
            self._project_layer(layer) for layer in scene.layers
        )
        self._build_sample_disk()

    def _build_sample_disk(self) -> None:
        count = self.scene.shadow.samples
        index = np.arange(count, dtype=np.float64)
        golden_angle = math.pi * (3.0 - math.sqrt(5.0))
        radius = np.sqrt((index + 0.5) / count)
        angle = index * golden_angle
        if count == 1:
            radius[:] = 0.0
            angle[:] = 0.0
        self.sample_cos = np.cos(angle)
        self.sample_sin = np.sin(angle)
        self.sample_radius = radius

    def _project_layer(self, layer: Layer) -> tuple[ProjectedTriangle, ...]:
        camera = self.camera
        forward = camera.forward
        right = camera.right
        true_up = camera.true_up
        triangles: list[ProjectedTriangle] = []
        for triangle_id, (triangle, triangle_uv) in enumerate(
            zip(layer.mesh.triangles, layer.mesh.uvs[layer.mesh.triangles])
        ):
            world = layer.mesh.vertices[triangle]
            relative = world - camera.position
            view = np.stack(
                (
                    relative @ right,
                    relative @ true_up,
                    relative @ forward,
                ),
                axis=1,
            )
            clipped_points, clipped_uvs = _clip_polygon_near(
                view, triangle_uv, camera.near
            )
            if len(clipped_points) < 3:
                continue
            projected_points = np.stack(
                (
                    (
                        clipped_points[:, 0]
                        / (clipped_points[:, 2] * self.tan_half * self.aspect)
                        * 0.5
                        + 0.5
                    )
                    * self.image.width,
                    (
                        0.5
                        - clipped_points[:, 1]
                        / (clipped_points[:, 2] * self.tan_half)
                        * 0.5
                    )
                    * self.image.height,
                ),
                axis=1,
            )
            normal = layer.mesh.face_normals[triangle_id]
            for fan_index in range(1, len(clipped_points) - 1):
                indices = (0, fan_index, fan_index + 1)
                triangles.append(
                    ProjectedTriangle(
                        screen=projected_points[list(indices)],
                        view_z=clipped_points[list(indices), 2],
                        uv=clipped_uvs[list(indices)],
                        normal=normal,
                        triangle_id=triangle_id,
                    )
                )
        return tuple(triangles)

    def _pixel_ray_directions(
        self, x_start: int, x_end: int, y_start: int, y_end: int
    ) -> np.ndarray:
        x = np.arange(x_start, x_end, dtype=np.float64) + 0.5
        y = np.arange(y_start, y_end, dtype=np.float64) + 0.5
        ndc_x = (x / self.image.width) * 2.0 - 1.0
        ndc_y = 1.0 - (y / self.image.height) * 2.0
        grid_x, grid_y = np.meshgrid(ndc_x, ndc_y)
        directions = (
            self.camera.forward[None, None, :]
            + (grid_x * self.tan_half * self.aspect)[..., None]
            * self.camera.right[None, None, :]
            + (grid_y * self.tan_half)[..., None] * self.camera.true_up[None, None, :]
        )
        return directions

    def _raster_layer(
        self,
        layer_position: int,
        x_start: int,
        x_end: int,
        y_start: int,
        y_end: int,
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
        height = y_end - y_start
        width = x_end - x_start
        depth_buffer = np.full((height, width), np.inf, dtype=np.float64)
        uv_buffer = np.zeros((height, width, 2), dtype=np.float64)
        normal_buffer = np.zeros((height, width, 3), dtype=np.float64)
        triangle_buffer = np.full((height, width), -1, dtype=np.int64)
        for triangle in self.projected[layer_position]:
            screen = triangle.screen
            min_x = int(np.floor(np.min(screen[:, 0])))
            max_x = int(np.ceil(np.max(screen[:, 0])))
            min_y = int(np.floor(np.min(screen[:, 1])))
            max_y = int(np.ceil(np.max(screen[:, 1])))
            if max_x <= x_start or min_x >= x_end or max_y <= y_start or min_y >= y_end:
                continue
            bbox_x0 = max(min_x, x_start)
            bbox_x1 = min(max_x, x_end)
            bbox_y0 = max(min_y, y_start)
            bbox_y1 = min(max_y, y_end)
            if bbox_x0 >= bbox_x1 or bbox_y0 >= bbox_y1:
                continue
            px = np.arange(bbox_x0, bbox_x1, dtype=np.float64) + 0.5
            py = np.arange(bbox_y0, bbox_y1, dtype=np.float64) + 0.5
            grid_x, grid_y = np.meshgrid(px, py)
            x0, y0 = screen[0]
            x1, y1 = screen[1]
            x2, y2 = screen[2]
            area = (x1 - x0) * (y2 - y0) - (y1 - y0) * (x2 - x0)
            if abs(area) <= 1e-15:
                continue
            b0 = ((x1 - grid_x) * (y2 - grid_y) - (y1 - grid_y) * (x2 - grid_x)) / area
            b1 = ((x2 - grid_x) * (y0 - grid_y) - (y2 - grid_y) * (x0 - grid_x)) / area
            b2 = 1.0 - b0 - b1
            inside = (b0 >= -1e-9) & (b1 >= -1e-9) & (b2 >= -1e-9)
            if not np.any(inside):
                continue
            z = triangle.view_z
            inverse_z = b0 / z[0] + b1 / z[1] + b2 / z[2]
            valid = inside & (inverse_z > 0.0)
            valid &= (1.0 / np.maximum(inverse_z, 1e-15)) <= self.camera.far
            if not np.any(valid):
                continue
            view_z = np.zeros_like(inverse_z)
            view_z[valid] = 1.0 / inverse_z[valid]
            uv = np.zeros(grid_x.shape + (2,), dtype=np.float64)
            for channel in range(2):
                perspective_weight = (
                    b0 * triangle.uv[0, channel] / z[0]
                    + b1 * triangle.uv[1, channel] / z[1]
                    + b2 * triangle.uv[2, channel] / z[2]
                )
                uv[..., channel] = perspective_weight / inverse_z
            local_y0 = bbox_y0 - y_start
            local_y1 = bbox_y1 - y_start
            local_x0 = bbox_x0 - x_start
            local_x1 = bbox_x1 - x_start
            current_depth = depth_buffer[local_y0:local_y1, local_x0:local_x1]
            tie = np.abs(view_z - current_depth) <= 1e-10
            current_triangle = triangle_buffer[
                local_y0:local_y1, local_x0:local_x1
            ]
            update = valid & (
                (view_z < current_depth - 1e-10)
                | (tie & (triangle.triangle_id < current_triangle))
            )
            if not np.any(update):
                continue
            current_depth[update] = view_z[update]
            uv_buffer[local_y0:local_y1, local_x0:local_x1][update] = uv[update]
            normal_buffer[local_y0:local_y1, local_x0:local_x1][update] = triangle.normal
            current_triangle[update] = triangle.triangle_id
        covered = np.isfinite(depth_buffer)
        return depth_buffer, uv_buffer, normal_buffer, covered

    def _visibility(
        self,
        points: np.ndarray,
        layer_position: int,
    ) -> tuple[np.ndarray, int, int]:
        if layer_position == 0:
            return np.ones(len(points), dtype=np.float64), 0, 0
        light = self.scene.light
        to_light = light.position[None, :] - points
        distances = np.linalg.norm(to_light, axis=1)
        central_direction = to_light / np.maximum(distances[:, None], 1e-15)
        reference = np.asarray([0.0, 1.0, 0.0])
        reference_b = np.asarray([1.0, 0.0, 0.0])
        tangent_one = np.cross(central_direction, reference)
        bad = np.linalg.norm(tangent_one, axis=1) <= 1e-9
        if np.any(bad):
            tangent_one[bad] = np.cross(central_direction[bad], reference_b)
        tangent_one = tangent_one / np.maximum(
            np.linalg.norm(tangent_one, axis=1)[:, None], 1e-15
        )
        tangent_two = np.cross(central_direction, tangent_one)
        tangent_two = tangent_two / np.maximum(
            np.linalg.norm(tangent_two, axis=1)[:, None], 1e-15
        )
        disk_radius = math.tan(self.scene.shadow.softness) * distances
        offset = disk_radius[:, None, None] * (
            self.sample_cos[None, :, None] * tangent_one[:, None, :]
            + self.sample_sin[None, :, None] * tangent_two[:, None, :]
        )
        sampled_lights = light.position[None, None, :] + offset
        directions = points[:, None, :] - sampled_lights
        origins = np.broadcast_to(
            sampled_lights, (len(points), self.scene.shadow.samples, 3)
        ).reshape(-1, 3)
        flat_directions = directions.reshape(-1, 3)
        transmission = np.ones(len(origins), dtype=np.float64)
        occluder_tests = 0
        for inner_position in range(layer_position):
            inner_layer = self.scene.layers[inner_position]
            mapper = self.mappers[inner_position]
            if mapper is not None:
                ray_t, uv, inside = mapper.ray_plane_uv(origins, flat_directions)
                hit = inside & (ray_t > 1e-7) & (ray_t < 1.0 - 1e-7)
            else:
                hit, uv = ray_mesh_uv_hit(origins, flat_directions, inner_layer.mesh)
            texture = self.texture_by_layer[inner_layer.layer_index]
            sampled = sample_bilinear(texture.table, uv[:, 0], uv[:, 1])
            paper = sampled[:, 0]
            effective_transmission = (
                inner_layer.transmittance * sampled[:, 1]
            )
            effective_transmission = np.clip(effective_transmission, 0.0, 1.0)
            hit_factor = np.ones_like(transmission)
            hit_factor[hit] = 1.0 - paper[hit] * (1.0 - effective_transmission[hit])
            transmission *= hit_factor
            occluder_tests += len(origins)
        visibility = transmission.reshape(
            len(points), self.scene.shadow.samples
        ).mean(axis=1)
        return visibility, len(origins), occluder_tests

    def visibility_at(self, points: np.ndarray, layer_position: int) -> np.ndarray:
        """Expose projected-light visibility for deterministic verification."""
        points = np.asarray(points, dtype=np.float64)
        visibility, _, _ = self._visibility(points, layer_position)
        return visibility

    def render_tile(self, block_index: int, column: int, row: int) -> TileResult:
        x_start = column * self.image.tile_width
        y_start = row * self.image.tile_height
        x_end = min(x_start + self.image.tile_width, self.image.width)
        y_end = min(y_start + self.image.tile_height, self.image.height)
        width = x_end - x_start
        height = y_end - y_start
        directions = self._pixel_ray_directions(x_start, x_end, y_start, y_end)
        layer_count = len(self.scene.layers)
        layer_rgba = np.zeros((layer_count, height, width, 4), dtype=np.float64)
        layer_depth = np.zeros((layer_count, height, width), dtype=np.float64)
        visible_fragments = np.zeros(layer_count, dtype=np.int64)
        paper_fragments = np.zeros(layer_count, dtype=np.int64)
        transmission_sum = np.zeros(layer_count, dtype=np.float64)
        cutout_fragments = np.zeros(layer_count, dtype=np.int64)
        shadow_sample_rays = 0
        shadow_occluder_tests = 0

        for layer_position, layer in enumerate(self.scene.layers):
            depth, uv, normal, covered = self._raster_layer(
                layer_position, x_start, x_end, y_start, y_end
            )
            visible_fragments[layer_position] = int(np.count_nonzero(covered))
            if not np.any(covered):
                continue
            cover_y, cover_x = np.nonzero(covered)
            pixel_uv = uv[cover_y, cover_x]
            texture = self.texture_by_layer[layer.layer_index]
            sampled = sample_bilinear(texture.table, pixel_uv[:, 0], pixel_uv[:, 1])
            paper_mask = np.clip(sampled[:, 0], 0.0, 1.0)
            paper = paper_mask > 1e-6
            paper_count = int(np.count_nonzero(paper))
            paper_fragments[layer_position] = paper_count
            cutout_fragments[layer_position] = int(len(paper_mask) - paper_count)
            if paper_count == 0:
                continue
            paper_y = cover_y[paper]
            paper_x = cover_x[paper]
            paper_mask_values = paper_mask[paper]
            paper_uv = pixel_uv[paper]
            paper_sample = sampled[paper]
            pixel_z = depth[paper_y, paper_x]
            points = self.camera.position[None, :] + directions[paper_y, paper_x] * pixel_z[
                :, None
            ]
            normals = normal[paper_y, paper_x]
            normals = normals / np.maximum(
                np.linalg.norm(normals, axis=1)[:, None], 1e-15
            )
            to_light = self.scene.light.position[None, :] - points
            distance_squared = np.sum(to_light * to_light, axis=1)
            distance = np.sqrt(distance_squared)
            light_direction = to_light / np.maximum(distance[:, None], 1e-15)
            incident = np.abs(np.einsum("ij,ij->i", normals, light_direction))
            angle_factor = 0.22 + 0.78 * incident
            visibility, sample_rays, occluder_tests = self._visibility(
                points, layer_position
            )
            shadow_sample_rays += sample_rays
            shadow_occluder_tests += occluder_tests
            projected_factor = 0.12 + 0.88 * visibility
            falloff = 1.0 / np.maximum(distance_squared, 0.025)
            irradiance = (
                self.scene.light.color[None, :]
                * self.scene.light.intensity
                * falloff[:, None]
            )
            effective_transmission = (
                layer.transmittance * paper_sample[:, 1]
            )
            effective_transmission = np.clip(effective_transmission, 0.0, 1.0)
            transmitted = (
                irradiance
                * effective_transmission[:, None]
                * angle_factor[:, None]
                * projected_factor[:, None]
            )
            scattered = (
                irradiance
                * layer.scatter_color[None, :]
                * paper_sample[:, 2][:, None]
                * (0.16 + 0.84 * angle_factor)[:, None]
                * (0.30 + 0.70 * visibility)[:, None]
            )
            radiance = np.maximum(transmitted + scattered, 0.0)
            toned = 1.0 - np.exp(-radiance)
            opacity = np.clip(
                paper_mask_values * (1.0 - np.clip(effective_transmission, 0.0, 1.0)),
                0.0,
                1.0,
            )
            premultiplied = toned * opacity[:, None]
            layer_rgba[layer_position, paper_y, paper_x, :3] = premultiplied
            layer_rgba[layer_position, paper_y, paper_x, 3] = opacity
            layer_depth[layer_position, paper_y, paper_x] = pixel_z
            transmission_sum[layer_position] = float(
                np.sum(effective_transmission * paper_mask_values)
            )

        output_rgba = np.zeros((height, width, 4), dtype=np.float64)
        output_depth = np.zeros((height, width), dtype=np.float64)
        for layer_position in range(layer_count):
            source = layer_rgba[layer_position]
            alpha = source[..., 3]
            one_minus = 1.0 - alpha
            output_rgba[..., :3] = source[..., :3] + output_rgba[..., :3] * one_minus[
                ..., None
            ]
            output_rgba[..., 3] = alpha + output_rgba[..., 3] * one_minus
            source_depth = layer_depth[layer_position]
            output_depth = source_depth * alpha + output_depth * one_minus
        output_rgba[..., :3] = np.clip(output_rgba[..., :3], 0.0, 1.0)
        output_rgba[..., 3] = np.clip(output_rgba[..., 3], 0.0, 1.0)
        output_depth = np.maximum(output_depth, 0.0)
        return TileResult(
            rgba=output_rgba,
            depth=output_depth,
            layer_rgba=np.clip(layer_rgba, 0.0, 1.0),
            visible_fragments=visible_fragments,
            paper_fragments=paper_fragments,
            effective_transmission_sum=transmission_sum,
            cutout_fragments=cutout_fragments,
            shadow_sample_rays=shadow_sample_rays,
            shadow_occluder_tests=shadow_occluder_tests,
        )

