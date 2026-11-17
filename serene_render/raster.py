from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .scene import Layer, Scene


@dataclass
class LayerRaster:
    layer: Layer
    mask: np.ndarray
    depth: np.ndarray
    world: np.ndarray
    normal: np.ndarray
    primitive: np.ndarray


def _clip_near_triangle(
    world_vertices: np.ndarray,
    camera_vertices: np.ndarray,
    near: float,
) -> list[tuple[np.ndarray, np.ndarray]]:
    polygon_world: list[np.ndarray] = []
    polygon_camera: list[np.ndarray] = []
    for index in range(3):
        current_w = world_vertices[index]
        current_c = camera_vertices[index]
        next_w = world_vertices[(index + 1) % 3]
        next_c = camera_vertices[(index + 1) % 3]
        current_inside = current_c[2] >= near
        next_inside = next_c[2] >= near
        if current_inside:
            polygon_world.append(current_w)
            polygon_camera.append(current_c)
        if current_inside != next_inside:
            denominator = next_c[2] - current_c[2]
            if abs(float(denominator)) > 1.0e-15:
                t = (near - current_c[2]) / denominator
                polygon_world.append(current_w + t * (next_w - current_w))
                polygon_camera.append(current_c + t * (next_c - current_c))

    if len(polygon_world) < 3:
        return []
    triangles: list[tuple[np.ndarray, np.ndarray]] = []
    for index in range(1, len(polygon_world) - 1):
        triangles.append(
            (
                np.stack(
                    (polygon_world[0], polygon_world[index], polygon_world[index + 1])
                ),
                np.stack(
                    (polygon_camera[0], polygon_camera[index], polygon_camera[index + 1])
                ),
            )
        )
    return triangles


def rasterize_layers(scene: Scene) -> list[LayerRaster]:
    height, width = scene.height, scene.width
    camera = scene.camera
    outputs: list[LayerRaster] = []

    for layer in scene.layers:
        mask = np.zeros((height, width), dtype=bool)
        depth = np.full((height, width), np.inf, dtype=np.float64)
        world = np.zeros((height, width, 3), dtype=np.float64)
        normal = np.zeros((height, width, 3), dtype=np.float64)
        primitive = np.full((height, width), -1, dtype=np.int32)

        for primitive_index, triangle in enumerate(layer.triangles):
            world_vertices = layer.vertices[triangle]
            camera_vertices = camera.to_camera(world_vertices)
            clipped = _clip_near_triangle(world_vertices, camera_vertices, camera.near)
            if not clipped:
                continue
            normal_value = np.cross(
                world_vertices[1] - world_vertices[0],
                world_vertices[2] - world_vertices[0],
            )
            normal_value = normal_value / np.linalg.norm(normal_value)

            for clipped_world, clipped_camera in clipped:
                screen_x, screen_y = camera.project_camera(clipped_camera)
                min_x = max(0, int(np.floor(float(screen_x.min()))))
                max_x = min(width - 1, int(np.ceil(float(screen_x.max()))))
                min_y = max(0, int(np.floor(float(screen_y.min()))))
                max_y = min(height - 1, int(np.ceil(float(screen_y.max()))))
                if min_x > max_x or min_y > max_y:
                    continue

                area = (
                    (screen_x[1] - screen_x[0]) * (screen_y[2] - screen_y[0])
                    - (screen_y[1] - screen_y[0]) * (screen_x[2] - screen_x[0])
                )
                if abs(float(area)) <= 1.0e-12:
                    continue

                xs = np.arange(min_x, max_x + 1, dtype=np.float64)
                ys = np.arange(min_y, max_y + 1, dtype=np.float64)
                px = (xs + 0.5)[None, :]
                py = (ys + 0.5)[:, None]
                w0 = (
                    (screen_x[2] - screen_x[1]) * (py - screen_y[1])
                    - (screen_y[2] - screen_y[1]) * (px - screen_x[1])
                ) / area
                w1 = (
                    (screen_x[0] - screen_x[2]) * (py - screen_y[2])
                    - (screen_y[0] - screen_y[2]) * (px - screen_x[2])
                ) / area
                w2 = 1.0 - w0 - w1
                inside = (w0 >= -1.0e-8) & (w1 >= -1.0e-8) & (w2 >= -1.0e-8)
                if not bool(inside.any()):
                    continue

                z0, z1, z2 = clipped_camera[:, 2]
                inverse_z = w0 / z0 + w1 / z1 + w2 / z2
                valid = inside & (inverse_z > 0.0)
                if not bool(valid.any()):
                    continue
                pixel_depth = 1.0 / inverse_z
                world_x = (
                    w0 * clipped_world[0, 0] / z0
                    + w1 * clipped_world[1, 0] / z1
                    + w2 * clipped_world[2, 0] / z2
                ) / inverse_z
                world_y = (
                    w0 * clipped_world[0, 1] / z0
                    + w1 * clipped_world[1, 1] / z1
                    + w2 * clipped_world[2, 1] / z2
                ) / inverse_z
                world_z = (
                    w0 * clipped_world[0, 2] / z0
                    + w1 * clipped_world[1, 2] / z1
                    + w2 * clipped_world[2, 2] / z2
                ) / inverse_z

                region_depth = depth[min_y : max_y + 1, min_x : max_x + 1]
                region_primitive = primitive[min_y : max_y + 1, min_x : max_x + 1]
                closer = valid & (
                    (pixel_depth < region_depth - 1.0e-9)
                    | (
                        (np.abs(pixel_depth - region_depth) <= 1.0e-9)
                        & (primitive_index < region_primitive)
                    )
                    | (~np.isfinite(region_depth))
                )
                if not bool(closer.any()):
                    continue

                mask_region = mask[min_y : max_y + 1, min_x : max_x + 1]
                world_region = world[min_y : max_y + 1, min_x : max_x + 1]
                normal_region = normal[min_y : max_y + 1, min_x : max_x + 1]
                mask_region[closer] = True
                region_depth[closer] = pixel_depth[closer]
                world_region[closer, 0] = world_x[closer]
                world_region[closer, 1] = world_y[closer]
                world_region[closer, 2] = world_z[closer]
                normal_region[closer] = normal_value
                region_primitive[closer] = primitive_index

        outputs.append(
            LayerRaster(
                layer=layer,
                mask=mask,
                depth=depth,
                world=world,
                normal=normal,
                primitive=primitive,
            )
        )
    return outputs
