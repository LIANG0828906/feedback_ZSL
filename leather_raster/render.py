from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .projection import ProjectionData
from .scene import Camera, Layer, Scene
from .texture import fiber_texture
from .util import quantize_energy


@dataclass(frozen=True)
class PreparedTriangle:
    serial: int
    original_triangle: int
    screen: np.ndarray
    world: np.ndarray
    depth: np.ndarray


@dataclass(frozen=True)
class BlockSpec:
    block_id: str
    row: int
    col: int
    x0: int
    y0: int
    x1: int
    y1: int

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    @property
    def height(self) -> int:
        return self.y1 - self.y0


@dataclass
class BlockResult:
    spec: BlockSpec
    rgba: np.ndarray
    layer_map: np.ndarray
    rgba_sha256: str = ""
    layer_sha256: str = ""
    color_summary: dict[str, Any] | None = None
    stats: dict[str, Any] | None = None


def block_specs(scene: Scene) -> list[BlockSpec]:
    specs: list[BlockSpec] = []
    rows = (scene.height + scene.render.block_height - 1) // scene.render.block_height
    cols = (scene.width + scene.render.block_width - 1) // scene.render.block_width
    for row in range(rows):
        for col in range(cols):
            y0 = row * scene.render.block_height
            x0 = col * scene.render.block_width
            y1 = min(scene.height, y0 + scene.render.block_height)
            x1 = min(scene.width, x0 + scene.render.block_width)
            specs.append(
                BlockSpec(
                    block_id=f"r{row:03d}_c{col:03d}",
                    row=row,
                    col=col,
                    x0=x0,
                    y0=y0,
                    x1=x1,
                    y1=y1,
                )
            )
    return specs


def _camera_coordinates(points: np.ndarray, camera: Camera) -> np.ndarray:
    relative = points - camera.position
    return np.stack(
        [
            relative @ camera.right,
            relative @ camera.camera_up,
            relative @ camera.forward,
        ],
        axis=-1,
    )


def _clip_polygon(
    camera_points: np.ndarray,
    world_points: np.ndarray,
    near: float,
    far: float,
) -> tuple[list[np.ndarray], list[np.ndarray]]:
    def clip_halfspace(
        source_camera: list[np.ndarray],
        source_world: list[np.ndarray],
        limit: float,
        keep_greater: bool,
    ) -> tuple[list[np.ndarray], list[np.ndarray]]:
        result_camera: list[np.ndarray] = []
        result_world: list[np.ndarray] = []
        if not source_camera:
            return result_camera, result_world
        count = len(source_camera)
        for index in range(count):
            current = source_camera[index]
            current_world = source_world[index]
            previous = source_camera[index - 1]
            previous_world = source_world[index - 1]
            current_z = float(current[2])
            previous_z = float(previous[2])
            current_inside = current_z >= limit if keep_greater else current_z <= limit
            previous_inside = previous_z >= limit if keep_greater else previous_z <= limit
            if current_inside != previous_inside:
                denominator = previous_z - current_z
                if abs(denominator) > 1e-20:
                    t = (previous_z - limit) / denominator
                    result_camera.append(previous + t * (current - previous))
                    result_world.append(previous_world + t * (current_world - previous_world))
            if current_inside:
                result_camera.append(current)
                result_world.append(current_world)
        return result_camera, result_world

    clipped_camera, clipped_world = clip_halfspace(
        list(camera_points),
        list(world_points),
        near,
        True,
    )
    return clip_halfspace(clipped_camera, clipped_world, far, False)


def _prepare_layer_triangles(scene: Scene, layer: Layer) -> list[PreparedTriangle]:
    camera = scene.camera
    width2 = scene.width * scene.render.supersampling
    height2 = scene.height * scene.render.supersampling
    aspect = width2 / max(height2, 1)
    serial = 0
    prepared: list[PreparedTriangle] = []
    solid_indices = np.nonzero(layer.triangle_cutout_ids == 0)[0]
    for original_triangle in solid_indices:
        triangle = layer.triangles[int(original_triangle)]
        world = layer.vertices[triangle]
        camera_points = _camera_coordinates(world, camera)
        clipped_camera, clipped_world = _clip_polygon(
            camera_points,
            world,
            camera.near,
            camera.far,
        )
        if len(clipped_camera) < 3:
            continue
        for fan_index in range(1, len(clipped_camera) - 1):
            camera_triangle = np.asarray(
                [clipped_camera[0], clipped_camera[fan_index], clipped_camera[fan_index + 1]],
                dtype=np.float64,
            )
            world_triangle = np.asarray(
                [clipped_world[0], clipped_world[fan_index], clipped_world[fan_index + 1]],
                dtype=np.float64,
            )
            depth = camera_triangle[:, 2]
            if bool(np.any(depth <= camera.near)):
                continue
            x_ndc = (camera_triangle[:, 0] / depth) / (aspect * camera.tan_half_fov)
            y_ndc = (camera_triangle[:, 1] / depth) / camera.tan_half_fov
            screen = np.stack(
                [
                    (x_ndc + 1.0) * 0.5 * width2,
                    (1.0 - y_ndc) * 0.5 * height2,
                ],
                axis=-1,
            )
            prepared.append(
                PreparedTriangle(
                    serial=serial,
                    original_triangle=int(original_triangle),
                    screen=screen,
                    world=world_triangle,
                    depth=depth,
                )
            )
            serial += 1
    return prepared


def _barycentric(
    triangle: np.ndarray,
    x: np.ndarray,
    y: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    a = triangle[0]
    b = triangle[1]
    c = triangle[2]
    denominator = (b[1] - c[1]) * (a[0] - c[0]) + (c[0] - b[0]) * (a[1] - c[1])
    if abs(denominator) <= 1e-15:
        zeros = np.zeros_like(x)
        return zeros, zeros, zeros, np.zeros_like(x, dtype=bool)
    w0 = ((b[1] - c[1]) * (x - c[0]) + (c[0] - b[0]) * (y - c[1])) / denominator
    w1 = ((c[1] - a[1]) * (x - c[0]) + (a[0] - c[0]) * (y - c[1])) / denominator
    w2 = 1.0 - w0 - w1
    inside = (w0 >= -1e-9) & (w1 >= -1e-9) & (w2 >= -1e-9)
    return w0, w1, w2, inside


def _rasterize_layer(
    prepared: list[PreparedTriangle],
    spec: BlockSpec,
    supersampling: int,
) -> tuple[np.ndarray, np.ndarray]:
    height = spec.height * supersampling
    width = spec.width * supersampling
    gx0 = spec.x0 * supersampling
    gy0 = spec.y0 * supersampling
    gx1 = spec.x1 * supersampling
    gy1 = spec.y1 * supersampling
    zbuffer = np.full((height, width), np.inf, dtype=np.float64)
    serial_buffer = np.full((height, width), -1, dtype=np.int32)
    barycentric = np.zeros((height, width, 3), dtype=np.float64)
    for triangle in prepared:
        min_x = max(gx0, int(np.floor(float(np.min(triangle.screen[:, 0])))) - 1)
        max_x = min(gx1, int(np.ceil(float(np.max(triangle.screen[:, 0])))) + 2)
        min_y = max(gy0, int(np.floor(float(np.min(triangle.screen[:, 1])))) - 1)
        max_y = min(gy1, int(np.ceil(float(np.max(triangle.screen[:, 1])))) + 2)
        if max_x <= min_x or max_y <= min_y:
            continue
        xs = np.arange(min_x, max_x, dtype=np.float64) + 0.5
        ys = np.arange(min_y, max_y, dtype=np.float64) + 0.5
        xx, yy = np.meshgrid(xs, ys)
        w0, w1, w2, inside = _barycentric(triangle.screen, xx, yy)
        if not bool(np.any(inside)):
            continue
        inverse_depth = (
            w0 / triangle.depth[0] + w1 / triangle.depth[1] + w2 / triangle.depth[2]
        )
        depth = 1.0 / np.maximum(inverse_depth, 1e-30)
        local_y0 = min_y - gy0
        local_y1 = max_y - gy0
        local_x0 = min_x - gx0
        local_x1 = max_x - gx0
        old_depth = zbuffer[local_y0:local_y1, local_x0:local_x1]
        old_serial = serial_buffer[local_y0:local_y1, local_x0:local_x1]
        better = inside & (
            (depth < old_depth - 1e-10)
            | ((np.abs(depth - old_depth) <= 1e-10) & (triangle.serial < old_serial))
        )
        if not bool(np.any(better)):
            continue
        old_depth[better] = depth[better]
        old_serial[better] = triangle.serial
        bary_target = barycentric[local_y0:local_y1, local_x0:local_x1]
        bary_target[better, 0] = w0[better]
        bary_target[better, 1] = w1[better]
        bary_target[better, 2] = w2[better]
    return serial_buffer, barycentric


def _linear_to_srgb(value: np.ndarray) -> np.ndarray:
    clipped = np.clip(value, 0.0, 1.0)
    return np.where(
        clipped <= 0.0031308,
        12.92 * clipped,
        1.055 * np.power(clipped, 1.0 / 2.4) - 0.055,
    )


def _tone_map(value: np.ndarray, exposure: float) -> np.ndarray:
    exposed = np.maximum(value * exposure, 0.0)
    mapped = exposed / (1.0 + exposed)
    return _linear_to_srgb(mapped)


def _shade_layer(
    scene: Scene,
    layer: Layer,
    world: np.ndarray,
    normals: np.ndarray,
    projection_rgb: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    camera = scene.camera
    view_vector = world - camera.position[None, :]
    view_length = np.maximum(np.linalg.norm(view_vector, axis=1), 1e-30)
    view = view_vector / view_length[:, None]
    normal = normals.copy()
    flip = np.einsum("ij,ij->i", normal, view) > 0.0
    normal[flip] *= -1.0

    u, v = layer.surface_uv(world)
    texture = fiber_texture(u, v, layer.fiber_seed)
    color_modulation = np.clip(
        layer.tint[None, :] * (0.76 + 0.38 * texture[:, None]),
        0.0,
        1.0,
    )
    cos_view = np.abs(np.einsum("ij,ij->i", normal, view))
    cos_view = np.clip(cos_view, 0.12, 1.0)
    optical_stretch = 1.0 / cos_view - 1.0
    transmittance = np.clip(
        layer.transmittance
        * (0.70 + 0.52 * texture)
        * np.exp(-3.0 * layer.thickness * optical_stretch),
        0.0,
        1.0,
    )
    alpha = np.clip(1.0 - 0.62 * transmittance, 0.20, 0.98)

    ambient = color_modulation * (scene.render.ambient * (0.30 + 0.70 * cos_view[:, None]))
    reflection = ambient.copy()
    transmission = np.zeros_like(reflection)
    for light in scene.lights:
        light_vector = light.position[None, :] - world
        distance = np.maximum(np.linalg.norm(light_vector, axis=1), 1e-12)
        light_direction = light_vector / distance[:, None]
        attenuation = light.intensity / (1.0 + 0.55 * distance**2)
        ndotl = np.einsum("ij,ij->i", normal, light_direction)
        wrapped = np.clip((ndotl + 0.34) / 1.34, 0.0, 1.0)
        diffuse = (
            color_modulation
            * light.color[None, :]
            * attenuation[:, None]
            * wrapped[:, None]
            * 0.54
        )
        transmitted = (
            color_modulation
            * light.color[None, :]
            * attenuation[:, None]
            * transmittance[:, None]
            * (0.28 + 0.72 * np.abs(ndotl))[:, None]
            * 0.68
        )
        reflection += diffuse
        transmission += transmitted
    projection = np.maximum(projection_rgb, 0.0)
    linear = reflection + transmission + projection
    display = _tone_map(linear, scene.render.exposure)
    return display, alpha, reflection, transmission, projection


def _screen_coverage(
    scene: Scene,
    spec: BlockSpec,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    supersampling = scene.render.supersampling
    height = spec.height * supersampling
    width = spec.width * supersampling
    gx0 = spec.x0 * supersampling
    gy0 = spec.y0 * supersampling
    xs = np.arange(gx0, gx0 + width, dtype=np.float64) + 0.5
    ys = np.arange(gy0, gy0 + height, dtype=np.float64) + 0.5
    width2 = scene.width * supersampling
    height2 = scene.height * supersampling
    aspect = width2 / max(height2, 1)
    ndc_x = ((xs / width2) * 2.0 - 1.0) * aspect * scene.camera.tan_half_fov
    ndc_y = (1.0 - (ys / height2) * 2.0) * scene.camera.tan_half_fov
    xx, yy = np.meshgrid(ndc_x, ndc_y)
    direction = (
        scene.camera.forward[None, None, :]
        + xx[..., None] * scene.camera.right[None, None, :]
        + yy[..., None] * scene.camera.camera_up[None, None, :]
    )
    direction = direction / np.maximum(
        np.linalg.norm(direction, axis=2, keepdims=True),
        1e-30,
    )
    screen = scene.screen
    denominator = direction @ screen.normal
    numerator = float(np.dot(screen.origin - scene.camera.position, screen.normal))
    safe = np.where(np.abs(denominator) > 1e-12, denominator, np.nan)
    distance = numerator / safe
    valid = np.isfinite(distance) & (distance > scene.camera.near) & (distance <= scene.camera.far)
    origin_grid = np.broadcast_to(scene.camera.position, direction.shape)
    hit_points = origin_grid + distance[..., None] * direction
    relative = hit_points - screen.origin
    u = (relative @ screen.axis_u) / screen.width
    v = (relative @ screen.axis_v) / screen.height
    inside = valid & (u >= 0.0) & (u <= 1.0) & (v >= 0.0) & (v <= 1.0)
    return inside, u, v, distance


def render_block(
    scene: Scene,
    spec: BlockSpec,
    prepared_layers: list[list[PreparedTriangle]],
    projection: ProjectionData,
) -> BlockResult:
    supersampling = scene.render.supersampling
    height2 = spec.height * supersampling
    width2 = spec.width * supersampling
    gx0 = spec.x0 * supersampling
    gy0 = spec.y0 * supersampling
    layer_count = len(scene.layers)

    top_layer = np.full((height2, width2), 255, dtype=np.uint8)
    composited_rgb = np.zeros((height2, width2, 3), dtype=np.float64)
    composited_alpha = np.zeros((height2, width2), dtype=np.float64)
    layer_samples = [0 for _ in range(layer_count)]
    transmission_energy = [0 for _ in range(layer_count)]
    reflection_energy = [0 for _ in range(layer_count)]
    projection_energy = [0 for _ in range(layer_count)]
    screen_projection_energy = 0

    for layer_position, layer in enumerate(scene.layers):
        serial_buffer, barycentric = _rasterize_layer(
            prepared_layers[layer_position],
            spec,
            supersampling,
        )
        hit = serial_buffer >= 0
        layer_samples[layer_position] = int(np.count_nonzero(hit))
        if not bool(np.any(hit)):
            continue

        layer_rgb = np.zeros((height2, width2, 3), dtype=np.float64)
        layer_alpha = np.zeros((height2, width2), dtype=np.float64)
        trans_energy_value = 0.0
        reflect_energy_value = 0.0
        projection_energy_value = 0.0
        receiver_map = projection.receiver_map(f"layer:{layer.layer_index}")

        unique_serials = np.unique(serial_buffer[hit])
        prepared_lookup = {triangle.serial: triangle for triangle in prepared_layers[layer_position]}
        for serial in unique_serials:
            serial = int(serial)
            mask = serial_buffer == serial
            triangle = prepared_lookup[serial]
            weights_screen = barycentric[mask]
            inverse_depth = (
                weights_screen[:, 0] / triangle.depth[0]
                + weights_screen[:, 1] / triangle.depth[1]
                + weights_screen[:, 2] / triangle.depth[2]
            )
            perspective_weights = weights_screen / triangle.depth[None, :]
            perspective_weights = perspective_weights / inverse_depth[:, None]
            world = perspective_weights @ triangle.world
            normal = np.broadcast_to(
                layer.face_normals[triangle.original_triangle],
                world.shape,
            ).copy()

            if receiver_map is not None:
                ys, xs = np.nonzero(mask)
                projection_linear = receiver_map[
                    ys + gy0,
                    xs + gx0,
                ].astype(np.float64)
            else:
                projection_linear = np.zeros((world.shape[0], 3), dtype=np.float64)

            display, alpha, reflection, transmission, layer_projection = _shade_layer(
                scene,
                layer,
                world,
                normal,
                projection_linear,
            )
            ys, xs = np.nonzero(mask)
            layer_rgb[ys, xs] = display
            layer_alpha[ys, xs] = alpha
            trans_energy_value += quantize_energy(float(np.sum(transmission)))
            reflect_energy_value += quantize_energy(float(np.sum(reflection)))
            projection_energy_value += quantize_energy(float(np.sum(layer_projection)))

        transmission_energy[layer_position] = int(trans_energy_value)
        reflection_energy[layer_position] = int(reflect_energy_value)
        projection_energy[layer_position] = int(projection_energy_value)

        front_layer_value = top_layer == 255
        top_layer[hit & front_layer_value] = layer.layer_index
        premultiplied = layer_rgb * layer_alpha[..., None]
        remaining = 1.0 - composited_alpha
        composited_rgb += premultiplied * remaining[..., None]
        composited_alpha += layer_alpha * remaining

    screen_inside, screen_u, screen_v, _ = _screen_coverage(scene, spec)
    screen_alpha = np.zeros((height2, width2), dtype=np.float64)
    screen_rgb = np.zeros((height2, width2, 3), dtype=np.float64)
    if bool(np.any(screen_inside)):
        uv_u = np.clip(screen_u, 0.0, 1.0)
        uv_v = np.clip(screen_v, 0.0, 1.0)
        cloth_texture = fiber_texture(
            uv_u * 1.7 + 0.3 * uv_v,
            uv_v * 1.3 - 0.2 * uv_u,
            scene.screen.fiber_seed,
        )
        cloth = (
            scene.screen.tint[None, None, :]
            * (0.34 + 0.24 * cloth_texture[..., None])
            + scene.render.ambient * 0.18
        )
        screen_projection = np.zeros_like(cloth)
        screen_map = projection.receiver_map("screen")
        if screen_map is not None:
            ys, xs = np.nonzero(screen_inside)
            screen_projection[ys, xs] = screen_map[ys + gy0, xs + gx0]
            screen_projection_energy += quantize_energy(float(np.sum(screen_map[ys + gy0, xs + gx0])))
        screen_linear = cloth + np.maximum(screen_projection, 0.0)
        screen_rgb = _tone_map(screen_linear, scene.render.exposure)
        screen_alpha = screen_inside.astype(np.float64)

    remaining = 1.0 - composited_alpha
    composited_rgb += screen_rgb * screen_alpha[..., None] * remaining[..., None]
    composited_alpha += screen_alpha * remaining

    background = scene.render.background
    remaining = 1.0 - composited_alpha
    composited_rgb += background[None, None, :3] * background[3] * remaining[..., None]
    composited_alpha += background[3] * remaining

    rgba_samples = np.concatenate(
        [composited_rgb, composited_alpha[..., None]],
        axis=2,
    )
    rgba_samples = np.clip(rgba_samples, 0.0, 1.0)
    rgba_downsampled = rgba_samples.reshape(
        spec.height,
        supersampling,
        spec.width,
        supersampling,
        4,
    ).mean(axis=(1, 3))
    rgba = np.floor(rgba_downsampled * 255.0 + 0.5).astype(np.uint8)

    top_reshaped = top_layer.reshape(
        spec.height,
        supersampling,
        spec.width,
        supersampling,
    ).transpose(0, 2, 1, 3).reshape(spec.height, spec.width, supersampling * supersampling)
    counts = np.zeros((spec.height, spec.width, layer_count + 1), dtype=np.int16)
    for layer in scene.layers:
        counts[..., layer.layer_index] = np.sum(top_reshaped == layer.layer_index, axis=2)
    counts[..., layer_count] = np.sum(top_reshaped == 255, axis=2)
    mode = np.argmax(counts, axis=2)
    layer_map = np.where(mode < layer_count, mode, 255).astype(np.uint8)

    stats = {
        "sample_count": int(height2 * width2),
        "layer_samples": layer_samples,
        "transmission_energy_ppm": transmission_energy,
        "reflection_energy_ppm": reflection_energy,
        "projection_energy_ppm": projection_energy,
        "screen_projection_energy_ppm": int(screen_projection_energy),
    }
    return BlockResult(
        spec=spec,
        rgba=rgba,
        layer_map=layer_map,
        stats=stats,
    )


def all_prepared_layers(scene: Scene) -> list[list[PreparedTriangle]]:
    return [_prepare_layer_triangles(scene, layer) for layer in scene.layers]

