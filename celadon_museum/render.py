"""Deterministic tile rasterization, celadon shading, and stack projections."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .math3d import smoothstep, tonemap_srgb
from .scene import Layer, Light, Plate, Scene
from .texture import LayerTextures, bilinear_sample, generate_layer_textures


@dataclass
class ProjectedTriangle:
    sx: np.ndarray
    sy: np.ndarray
    depth: np.ndarray
    local: np.ndarray
    world: np.ndarray
    normal: np.ndarray
    bbox: tuple[int, int, int, int]
    triangle_index: int


@dataclass
class LayerRenderData:
    layer: Layer
    plate: Plate
    textures: LayerTextures
    triangles: list[ProjectedTriangle]
    bboxes: np.ndarray
    local_z_min: float
    local_z_max: float


@dataclass
class FragmentSet:
    pixel: np.ndarray
    depth: np.ndarray
    local: np.ndarray
    world: np.ndarray
    normal: np.ndarray
    triangle_index: np.ndarray


@dataclass
class RenderContext:
    scene: Scene
    layers: list[LayerRenderData]
    layer_by_key: dict[tuple[int, int], LayerRenderData]
    plate_by_id: dict[int, Plate]
    plate_outer_texture: dict[int, LayerTextures]
    tan_half_fov: float
    aspect: float


def _clip_polygon_plane(
    polygon: list[tuple[np.ndarray, np.ndarray, np.ndarray]],
    threshold: float,
    keep_greater: bool,
) -> list[tuple[np.ndarray, np.ndarray, np.ndarray]]:
    if not polygon:
        return []
    output: list[tuple[np.ndarray, np.ndarray, np.ndarray]] = []
    previous = polygon[-1]
    previous_value = previous[0][2]
    previous_inside = previous_value >= threshold if keep_greater else previous_value <= threshold
    for current in polygon:
        current_value = current[0][2]
        current_inside = current_value >= threshold if keep_greater else current_value <= threshold
        if current_inside != previous_inside:
            denom = current_value - previous_value
            t = 0.0 if abs(denom) < 1.0e-15 else (threshold - previous_value) / denom
            cam = previous[0] + t * (current[0] - previous[0])
            world = previous[1] + t * (current[1] - previous[1])
            local = previous[2] + t * (current[2] - previous[2])
            output.append((cam, world, local))
        if current_inside:
            output.append(current)
        previous = current
        previous_value = current_value
        previous_inside = current_inside
    return output


def _project_triangles(scene: Scene, layer: Layer, plate: Plate) -> list[ProjectedTriangle]:
    camera = scene.camera
    local_vertices = layer.vertices
    world_vertices = local_vertices @ plate.rotation.T + plate.center
    relative = world_vertices - camera.position
    cam_vertices = np.stack(
        [
            relative @ camera.right,
            relative @ camera.true_up,
            relative @ camera.forward,
        ],
        axis=1,
    )
    tan_half = math.tan(math.radians(camera.vfov_deg) * 0.5)
    aspect = scene.width / scene.height
    triangles: list[ProjectedTriangle] = []
    for triangle_index, (ia, ib, ic) in enumerate(layer.triangles):
        indices = (int(ia), int(ib), int(ic))
        polygon = [
            (cam_vertices[index], world_vertices[index], local_vertices[index])
            for index in indices
        ]
        polygon = _clip_polygon_plane(polygon, camera.near, keep_greater=True)
        polygon = _clip_polygon_plane(polygon, camera.far, keep_greater=False)
        if len(polygon) < 3:
            continue
        cam_poly = np.asarray([vertex[0] for vertex in polygon], dtype=np.float64)
        world_poly = np.asarray([vertex[1] for vertex in polygon], dtype=np.float64)
        local_poly = np.asarray([vertex[2] for vertex in polygon], dtype=np.float64)
        depth = cam_poly[:, 2]
        if np.any(depth <= 1.0e-12):
            continue
        sx = (cam_poly[:, 0] / (depth * tan_half * aspect) * 0.5 + 0.5) * scene.width
        sy = (0.5 - cam_poly[:, 1] / (depth * tan_half) * 0.5) * scene.height
        world_normal = np.cross(
            world_vertices[ib] - world_vertices[ia],
            world_vertices[ic] - world_vertices[ia],
        )
        normal_norm = float(np.linalg.norm(world_normal))
        if normal_norm <= 1.0e-14:
            continue
        normal = world_normal / normal_norm
        x0 = max(int(math.floor(float(np.min(sx)))), 0)
        y0 = max(int(math.floor(float(np.min(sy)))), 0)
        x1 = min(int(math.ceil(float(np.max(sx)))), scene.width - 1)
        y1 = min(int(math.ceil(float(np.max(sy)))), scene.height - 1)
        if x1 < x0 or y1 < y0:
            continue
        for fan_index in range(1, len(polygon) - 1):
            selection = (0, fan_index, fan_index + 1)
            tri_sx = sx[list(selection)]
            tri_sy = sy[list(selection)]
            area = (tri_sx[1] - tri_sx[0]) * (tri_sy[2] - tri_sy[0]) - (
                tri_sy[1] - tri_sy[0]
            ) * (tri_sx[2] - tri_sx[0])
            if abs(float(area)) <= 1.0e-10:
                continue
            triangles.append(
                ProjectedTriangle(
                    sx=tri_sx,
                    sy=tri_sy,
                    depth=depth[list(selection)],
                    local=local_poly[list(selection)],
                    world=world_poly[list(selection)],
                    normal=normal,
                    bbox=(x0, y0, x1, y1),
                    triangle_index=triangle_index,
                )
            )
    return triangles


def build_render_context(scene: Scene) -> RenderContext:
    layers: list[LayerRenderData] = []
    layer_by_key: dict[tuple[int, int], LayerRenderData] = {}
    plate_outer_texture: dict[int, LayerTextures] = {}
    for layer in scene.ordered_layers:
        plate = scene.plates[layer.plate_id]
        textures = generate_layer_textures(layer)
        triangles = _project_triangles(scene, layer, plate)
        bboxes = np.asarray([triangle.bbox for triangle in triangles], dtype=np.int64)
        data = LayerRenderData(
            layer=layer,
            plate=plate,
            textures=textures,
            triangles=triangles,
            bboxes=bboxes,
            local_z_min=float(np.min(layer.vertices[:, 2])),
            local_z_max=float(np.max(layer.vertices[:, 2])),
        )
        layers.append(data)
        layer_by_key[(layer.plate_id, layer.layer_id)] = data
    for plate in scene.ordered_plates:
        outer_layer = max(plate.layers, key=lambda item: item.layer_id)
        plate_outer_texture[plate.plate_id] = layer_by_key[
            (plate.plate_id, outer_layer.layer_id)
        ].textures
    return RenderContext(
        scene=scene,
        layers=layers,
        layer_by_key=layer_by_key,
        plate_by_id=scene.plates,
        plate_outer_texture=plate_outer_texture,
        tan_half_fov=math.tan(math.radians(scene.camera.vfov_deg) * 0.5),
        aspect=scene.width / scene.height,
    )


def _rasterize_layer_tile(
    data: LayerRenderData,
    tile_id: int,
    row: int,
    col: int,
    tile_size: int,
    tile_width: int,
    tile_height: int,
) -> FragmentSet | None:
    tile_x0 = col * tile_size
    tile_y0 = row * tile_size
    tile_x1 = tile_x0 + tile_width - 1
    tile_y1 = tile_y0 + tile_height - 1
    if len(data.triangles) == 0:
        return None
    overlap = (
        (data.bboxes[:, 2] >= tile_x0)
        & (data.bboxes[:, 0] <= tile_x1)
        & (data.bboxes[:, 3] >= tile_y0)
        & (data.bboxes[:, 1] <= tile_y1)
    )
    candidates = np.nonzero(overlap)[0]
    pixel_parts: list[np.ndarray] = []
    depth_parts: list[np.ndarray] = []
    local_parts: list[np.ndarray] = []
    world_parts: list[np.ndarray] = []
    normal_parts: list[np.ndarray] = []
    triangle_parts: list[np.ndarray] = []
    for candidate_index in candidates:
        triangle = data.triangles[int(candidate_index)]
        x_lo = max(tile_x0, int(math.floor(float(np.min(triangle.sx)))))
        x_hi = min(tile_x1, int(math.ceil(float(np.max(triangle.sx)))))
        y_lo = max(tile_y0, int(math.floor(float(np.min(triangle.sy)))))
        y_hi = min(tile_y1, int(math.ceil(float(np.max(triangle.sy)))))
        if x_hi < x_lo or y_hi < y_lo:
            continue
        xs = np.arange(x_lo, x_hi + 1, dtype=np.float64) + 0.5
        ys = np.arange(y_lo, y_hi + 1, dtype=np.float64) + 0.5
        grid_x, grid_y = np.meshgrid(xs, ys)
        x0, x1, x2 = triangle.sx
        y0, y1, y2 = triangle.sy
        denom = (y1 - y2) * (x0 - x2) + (x2 - x1) * (y0 - y2)
        if abs(float(denom)) <= 1.0e-12:
            continue
        l0 = ((y1 - y2) * (grid_x - x2) + (x2 - x1) * (grid_y - y2)) / denom
        l1 = ((y2 - y0) * (grid_x - x2) + (x0 - x2) * (grid_y - y2)) / denom
        l2 = 1.0 - l0 - l1
        inside = (l0 >= -1.0e-7) & (l1 >= -1.0e-7) & (l2 >= -1.0e-7)
        count = int(np.count_nonzero(inside))
        if count == 0:
            continue
        l0 = l0[inside]
        l1 = l1[inside]
        l2 = l2[inside]
        pixel_x = grid_x[inside].astype(np.int64) - tile_x0
        pixel_y = grid_y[inside].astype(np.int64) - tile_y0
        pixel = pixel_y * tile_width + pixel_x
        z0, z1, z2 = triangle.depth
        inv_depth = l0 / z0 + l1 / z1 + l2 / z2
        valid = inv_depth > 1.0e-14
        if not np.all(valid):
            pixel = pixel[valid]
            l0, l1, l2, inv_depth = l0[valid], l1[valid], l2[valid], inv_depth[valid]
            if pixel.size == 0:
                continue
        depth = 1.0 / inv_depth
        local = (
            l0[:, None] * triangle.local[0][None, :] / z0
            + l1[:, None] * triangle.local[1][None, :] / z1
            + l2[:, None] * triangle.local[2][None, :] / z2
        ) * depth[:, None]
        world = (
            l0[:, None] * triangle.world[0][None, :] / z0
            + l1[:, None] * triangle.world[1][None, :] / z1
            + l2[:, None] * triangle.world[2][None, :] / z2
        ) * depth[:, None]
        pixel_parts.append(pixel)
        depth_parts.append(depth)
        local_parts.append(local)
        world_parts.append(world)
        normal_parts.append(np.broadcast_to(triangle.normal, (pixel.size, 3)).copy())
        triangle_parts.append(np.full(pixel.size, triangle.triangle_index, dtype=np.int64))
    if not pixel_parts:
        return None
    pixel = np.concatenate(pixel_parts)
    depth = np.concatenate(depth_parts)
    local = np.concatenate(local_parts, axis=0)
    world = np.concatenate(world_parts, axis=0)
    normal = np.concatenate(normal_parts, axis=0)
    triangle_index = np.concatenate(triangle_parts)
    order = np.lexsort((triangle_index, depth, pixel))
    sorted_pixel = pixel[order]
    first = np.ones(sorted_pixel.size, dtype=bool)
    first[1:] = sorted_pixel[1:] != sorted_pixel[:-1]
    selected = order[first]
    return FragmentSet(
        pixel=pixel[selected],
        depth=depth[selected],
        local=local[selected],
        world=world[selected],
        normal=normal[selected],
        triangle_index=triangle_index[selected],
    )


def _texture_coordinates(
    data: LayerRenderData, local: np.ndarray, normal: np.ndarray
) -> tuple[np.ndarray, np.ndarray, bool]:
    plate = data.plate
    on_top = np.abs(normal[:, 2]) > 0.58
    u = np.zeros(local.shape[0], dtype=np.float64)
    v = np.zeros(local.shape[0], dtype=np.float64)
    if np.any(on_top):
        u[on_top] = (local[on_top, 0] / plate.radius + 1.0) * 0.5
        v[on_top] = (local[on_top, 1] / plate.radius + 1.0) * 0.5
    if np.any(~on_top):
        side = ~on_top
        u[side] = np.mod(
            np.arctan2(local[side, 1], local[side, 0]) / (2.0 * np.pi) + 0.5,
            1.0,
        )
        z_span = max(data.local_z_max - data.local_z_min, 1.0e-9)
        v[side] = (local[side, 2] - data.local_z_min) / z_span
    return u, v, bool(np.any(~on_top))


def _layer_signature(seed: int) -> np.ndarray:
    value = (seed * 1103515245 + 12345) & 0xFFFFFFFF
    a = ((value >> 8) & 0xFFFF) / 65535.0 - 0.5
    b = ((value >> 16) & 0xFFFF) / 65535.0 - 0.5
    return np.array([1.0 + 0.060 * a, 1.0 + 0.040 * b, 1.0 - 0.035 * a], dtype=np.float64)


def _light_samples(light: Light, count: int) -> np.ndarray:
    if count <= 1:
        return light.position[None, :].copy()
    indices = np.arange(count, dtype=np.float64) + 0.5
    radial = np.sqrt(indices / count) * light.radius
    angle = indices * 2.399963229728653
    offsets = np.stack(
        [radial * np.cos(angle), radial * np.sin(angle), np.zeros_like(radial)],
        axis=1,
    )
    return light.position[None, :] + offsets


def _openness_for_local_queries(
    query_local: np.ndarray,
    light_local: np.ndarray,
    texture: LayerTextures,
    radius: float,
    plane_z: float,
) -> tuple[np.ndarray, np.ndarray]:
    query_z = query_local[:, 2]
    light_z = light_local[:, 2]
    denominator = query_z[:, None] - light_z[None, :]
    light_z_broadcast = np.broadcast_to(light_z[None, :], denominator.shape)
    safe = np.abs(denominator) > 1.0e-12
    t = np.zeros_like(denominator)
    t[safe] = (plane_z - light_z_broadcast[safe]) / denominator[safe]
    valid = safe & (t > 0.0) & (t < 1.0) & (light_z_broadcast > plane_z + 1.0e-10)
    upper = light_local[None, :, :] + t[:, :, None] * (
        query_local[:, None, :] - light_local[None, :, :]
    )
    radial = np.sqrt(upper[:, :, 0] ** 2 + upper[:, :, 1] ** 2)
    inside = 1.0 - smoothstep(radius * 0.982, radius * 1.018, radial)
    u = (upper[:, :, 0] / radius + 1.0) * 0.5
    v = (upper[:, :, 1] / radius + 1.0) * 0.5
    aperture = bilinear_sample(texture.aperture, u, v, wrap_u=False)
    open_value = 1.0 - inside * (1.0 - aperture)
    open_value = np.where(valid, open_value, 0.0)
    inside = np.where(valid, inside, 0.0)
    return open_value.mean(axis=1), inside.mean(axis=1)


def _projection_contribution(
    context: RenderContext,
    lower_plate: Plate,
    world: np.ndarray,
    normal: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, int]:
    energy_color = np.zeros((world.shape[0], 3), dtype=np.float64)
    metric = np.zeros(world.shape[0], dtype=np.float64)
    sample_count = 0
    scene = context.scene
    sample_n = int(scene.render["soft_shadow_samples"])
    upper_plates = [
        plate
        for plate in scene.ordered_plates
        if plate.center[2] > lower_plate.center[2] + 1.0e-6
    ]
    if not upper_plates:
        return energy_color, metric, 0
    facing = 0.18 + 0.82 * np.clip(normal[:, 2], 0.0, 1.0)
    for upper_plate in upper_plates:
        query_local = (world - upper_plate.center) @ upper_plate.rotation
        texture = context.plate_outer_texture[upper_plate.plate_id]
        for light in scene.ordered_lights:
            if light.position[2] <= upper_plate.center[2] + upper_plate.projection_plane_z:
                continue
            samples = _light_samples(light, sample_n)
            light_local = (samples - upper_plate.center) @ upper_plate.rotation
            open_fraction, inside_fraction = _openness_for_local_queries(
                query_local,
                light_local,
                texture,
                upper_plate.radius,
                upper_plate.projection_plane_z,
            )
            distance = np.linalg.norm(light.position[None, :] - world, axis=1)
            attenuation = 1.0 / (1.0 + 0.085 * distance * distance)
            lateral = (
                open_fraction
                * light.intensity
                * attenuation
                * facing
                * scene.render["projection_strength"]
                * 0.34
            )
            upper_color = context.layer_by_key[
                (upper_plate.plate_id, max(layer.layer_id for layer in upper_plate.layers))
            ].layer.scatter_color
            energy_color += (
                lateral[:, None]
                * light.color[None, :]
                * upper_color[None, :]
                * texture.aperture.mean()
                * 0.85
                + lateral[:, None]
                * light.color[None, :]
                * upper_color[None, :]
                * 0.15
            )
            metric += (
                open_fraction
                * inside_fraction
                * light.intensity
                * attenuation
                * facing
            )
            sample_count += int(world.shape[0] * sample_n)
    return energy_color, metric, sample_count


def _shade_layer_fragments(
    context: RenderContext,
    data: LayerRenderData,
    fragments: FragmentSet,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, int]:
    scene = context.scene
    camera_position = scene.camera.position
    normal = fragments.normal.copy()
    view_to_camera = camera_position[None, :] - fragments.world
    view_norm = np.linalg.norm(view_to_camera, axis=1)
    view_dir = view_to_camera / np.maximum(view_norm[:, None], 1.0e-12)
    flip = np.einsum("ij,ij->i", normal, view_dir) < 0.0
    normal[flip] *= -1.0
    u, v, has_side = _texture_coordinates(data, fragments.local, normal)
    crackle = bilinear_sample(data.textures.crackle, u, v, wrap_u=False)
    carve = bilinear_sample(data.textures.carve, u, v, wrap_u=False)
    if has_side:
        side = np.abs(normal[:, 2]) <= 0.58
        crackle[side] = bilinear_sample(
            data.textures.crackle, u[side], v[side], wrap_u=True
        )
        carve[side] = bilinear_sample(
            data.textures.carve, u[side], v[side], wrap_u=True
        )
    cos_incidence = np.clip(
        np.abs(np.einsum("ij,ij->i", normal, view_dir)),
        0.0,
        1.0,
    )
    transmittance = data.layer.transmittance * (0.88 + 0.12 * carve) * (1.0 - 0.22 * crackle)
    transmittance *= 0.70 + 0.30 * cos_incidence
    transmittance = np.clip(transmittance, 0.005, 0.995)
    tint = data.layer.scatter_color[None, :] * (0.90 + 0.10 * carve[:, None])
    crackle_tint = np.array([0.965, 1.0, 1.035], dtype=np.float64)
    tint *= 1.0 + 0.55 * crackle[:, None] * (crackle_tint[None, :] - 1.0)
    tint *= _layer_signature(data.layer.seed)[None, :]

    ambient_tint = np.array([0.82, 1.0, 0.96], dtype=np.float64)
    radiance = tint * (scene.render["ambient"] * ambient_tint[None, :])
    for light in scene.ordered_lights:
        light_delta = light.position[None, :] - fragments.world
        distance = np.linalg.norm(light_delta, axis=1)
        light_dir = light_delta / np.maximum(distance[:, None], 1.0e-12)
        ndotl = np.clip(np.einsum("ij,ij->i", normal, light_dir), 0.0, 1.0)
        wrap = 0.34 + 0.66 * ndotl
        attenuation = 1.0 / (1.0 + 0.14 * distance * distance)
        diffuse = light.intensity * attenuation * wrap
        radiance += (
            tint
            * light.color[None, :]
            * diffuse[:, None]
            * 0.255
        )
        transmitted = (
            light.intensity
            * attenuation
            * transmittance
            * (0.055 + 0.080 * ndotl)
        )
        radiance += tint * light.color[None, :] * transmitted[:, None]
        half_vector = light_dir + view_dir
        half_norm = np.linalg.norm(half_vector, axis=1)
        half_vector /= np.maximum(half_norm[:, None], 1.0e-12)
        specular = np.power(
            np.clip(np.einsum("ij,ij->i", normal, half_vector), 0.0, 1.0), 18.0
        )
        radiance += (
            light.color[None, :]
            * (0.018 * light.intensity * attenuation * specular)[:, None]
        )

    projection, projection_metric, projected_samples = _projection_contribution(
        context,
        data.plate,
        fragments.world,
        normal,
    )
    radiance += projection
    relief_modulation = np.clip(0.76 + 0.28 * carve - 0.14 * crackle, 0.52, 1.02)
    radiance *= relief_modulation[:, None]
    alpha = np.clip(1.0 - 0.72 * transmittance, 0.05, 0.975)
    return radiance, alpha, transmittance, projection_metric, projected_samples


def render_tile(
    context: RenderContext,
    tile_id: int,
    row: int,
    col: int,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, dict[str, Any]]:
    scene = context.scene
    tile_size = scene.tile_size
    tile_x0 = col * tile_size
    tile_y0 = row * tile_size
    tile_width = min(tile_size, scene.width - tile_x0)
    tile_height = min(tile_size, scene.height - tile_y0)
    pixel_count = tile_width * tile_height
    rgb = np.broadcast_to(scene.background[None, :], (pixel_count, 3)).copy()
    depth = np.zeros(pixel_count, dtype=np.float64)
    projection_image = np.zeros(pixel_count, dtype=np.float64)
    layer_keys = sorted(scene.layers)
    visible = {key: 0 for key in layer_keys}
    transmittance_sums = {key: 0.0 for key in layer_keys}
    shadow_samples = 0
    for key in layer_keys:
        data = context.layer_by_key[key]
        fragments = _rasterize_layer_tile(
            data,
            tile_id,
            row,
            col,
            tile_size,
            tile_width,
            tile_height,
        )
        if fragments is None:
            continue
        radiance, alpha, transmittance, projection_metric, projected_samples = (
            _shade_layer_fragments(context, data, fragments)
        )
        pixel = fragments.pixel
        premultiplied = radiance * alpha[:, None]
        rgb[pixel] = premultiplied + rgb[pixel] * (1.0 - alpha[:, None])
        depth[pixel] += -np.log(np.maximum(transmittance, 1.0e-4))
        projection_image[pixel] += projection_metric
        visible[key] = int(pixel.size)
        transmittance_sums[key] = float(np.sum(transmittance))
        shadow_samples += projected_samples
    tone = tonemap_srgb(
        rgb.reshape(tile_height, tile_width, 3),
        scene.render["exposure"],
        scene.render["max_display"],
    )
    rgba = np.empty((tile_height, tile_width, 4), dtype=np.uint8)
    rgba[:, :, :3] = np.rint(tone * 255.0).astype(np.uint8)
    rgba[:, :, 3] = 255
    depth_image = depth.reshape(tile_height, tile_width).astype(np.float32)
    projection_image = projection_image.reshape(tile_height, tile_width).astype(np.float32)
    stats = {
        "tile_id": tile_id,
        "row": row,
        "col": col,
        "width": tile_width,
        "height": tile_height,
        "layer_visible": {f"{key[0]}:{key[1]}": visible[key] for key in layer_keys},
        "layer_transmittance_sum": {
            f"{key[0]}:{key[1]}": transmittance_sums[key] for key in layer_keys
        },
        "soft_shadow_samples": int(shadow_samples),
        "projection_energy_sum": float(np.sum(projection_image, dtype=np.float64)),
    }
    return rgba, depth_image, projection_image, stats


def _largest_transition_width(
    offsets: np.ndarray, profile: np.ndarray
) -> tuple[float, list[dict[str, float]]]:
    low = float(np.min(profile))
    high = float(np.max(profile))
    if high - low < 0.2:
        return 0.0, []
    normalized = (profile - low) / (high - low)
    widths: list[float] = []
    for index in range(normalized.size - 1):
        a = float(normalized[index])
        b = float(normalized[index + 1])
        if (a - 0.1) * (b - 0.1) <= 0.0 and a != b:
            x10_a = offsets[index]
            x10_b = offsets[index + 1]
            x10 = x10_a + (0.1 - a) * (x10_b - x10_a) / (b - a)
            for later in range(index + 1, normalized.size - 1):
                c = float(normalized[later])
                d = float(normalized[later + 1])
                if (c - 0.9) * (d - 0.9) <= 0.0 and c != d:
                    x90_c = offsets[later]
                    x90_d = offsets[later + 1]
                    x90 = x90_c + (0.9 - c) * (x90_d - x90_c) / (d - c)
                    widths.append(abs(float(x90 - x10)))
                    break
    width = max(widths) if widths else 0.0
    comparison = []
    for offset in (-0.06, -0.03, 0.0, 0.03, 0.06):
        value = float(np.interp(offset, offsets, profile))
        comparison.append({"offset": offset, "brightness": value})
    return width, comparison


def compute_penumbra_trend(context: RenderContext) -> list[dict[str, Any]]:
    scene = context.scene
    plates = sorted(scene.ordered_plates, key=lambda plate: (plate.center[2], plate.plate_id))
    if len(plates) < 2:
        return []
    lower_plate = plates[0]
    upper_plate = plates[1]
    base_gap = float(upper_plate.center[2] - lower_plate.center[2])
    if base_gap <= 1.0e-6:
        return []
    light = max(scene.ordered_lights, key=lambda item: item.intensity)
    texture = context.plate_outer_texture[upper_plate.plate_id]
    resy, resx = texture.aperture.shape
    gradient = np.abs(np.diff(texture.aperture, axis=1, append=texture.aperture[:, -1:]))
    gradient += np.abs(np.diff(texture.aperture, axis=0, append=texture.aperture[-1:, :]))
    uu, vv = np.meshgrid(
        (np.arange(resx, dtype=np.float64) + 0.5) / resx,
        (np.arange(resy, dtype=np.float64) + 0.5) / resy,
    )
    radial = np.sqrt((uu - 0.5) ** 2 + (vv - 0.5) ** 2) * 2.0
    candidates = (
        (uu > 0.55)
        & (uu < 0.93)
        & (vv > 0.36)
        & (vv < 0.64)
        & (radial > 0.16)
        & (radial < 0.92)
        & (texture.aperture > 0.15)
        & (texture.aperture < 0.85)
    )
    if np.any(candidates):
        score = np.where(candidates, gradient, -1.0)
        v_index, u_index = np.unravel_index(int(np.argmax(score)), score.shape)
    else:
        u_index = int(round(0.75 * resx))
        v_index = int(round(0.50 * resy))
    edge_u = (u_index + 0.5) / resx
    edge_v = (v_index + 0.5) / resy
    edge_local = np.array(
        [
            (edge_u * 2.0 - 1.0) * upper_plate.radius,
            (edge_v * 2.0 - 1.0) * upper_plate.radius,
            upper_plate.projection_plane_z,
        ]
    )
    lower_top_z = max(float(np.max(layer.vertices[:, 2])) for layer in lower_plate.layers)
    lower_plane_local = np.array([0.0, 0.0, lower_top_z])
    line_offsets = np.linspace(-0.16, 0.16, 129)
    sample_n = int(scene.render["soft_shadow_samples"])
    results: list[dict[str, Any]] = []
    for multiplier in (1.0, 1.5, 2.0):
        gap = base_gap * multiplier
        upper_center = upper_plate.center.copy()
        upper_center[2] = lower_plate.center[2] + gap
        edge_world = upper_center + upper_plate.rotation @ edge_local
        lower_light_local = lower_plate.rotation.T @ (light.position - lower_plate.center)
        edge_lower_local = lower_plate.rotation.T @ (edge_world - lower_plate.center)
        denom = edge_lower_local[2] - lower_light_local[2]
        if abs(denom) < 1.0e-12:
            continue
        t = (lower_plane_local[2] - lower_light_local[2]) / denom
        center_local = lower_light_local + t * (edge_lower_local - lower_light_local)
        center_world = lower_plate.center + lower_plate.rotation @ center_local
        direction = lower_plate.rotation[:, 0]
        line_world = center_world[None, :] + line_offsets[:, None] * direction[None, :]
        query_local = (line_world - upper_center) @ upper_plate.rotation
        samples = _light_samples(light, sample_n)
        light_local = (samples - upper_center) @ upper_plate.rotation
        open_fraction, _ = _openness_for_local_queries(
            query_local,
            light_local,
            texture,
            upper_plate.radius,
            upper_plate.projection_plane_z,
        )
        width, comparison = _largest_transition_width(line_offsets, open_fraction)
        results.append(
            {
                "gap": round(gap, 9),
                "soft_shadow_samples": sample_n,
                "width_10_90": round(width, 9),
                "samples": comparison,
            }
        )
    return results


def connected_spot_components(
    projection_image: np.ndarray,
    minimum_relative_energy: float = 0.16,
) -> tuple[int, list[dict[str, Any]]]:
    maximum = float(np.max(projection_image)) if projection_image.size else 0.0
    if maximum <= 1.0e-9:
        return 0, []
    mask = projection_image >= maximum * minimum_relative_energy
    height, width = projection_image.shape
    visited = np.zeros_like(mask, dtype=bool)
    spots: list[dict[str, Any]] = []
    for y in range(height):
        for x in range(width):
            if not mask[y, x] or visited[y, x]:
                continue
            stack = [(y, x)]
            visited[y, x] = True
            count = 0
            sum_x = 0.0
            sum_y = 0.0
            peak = 0.0
            min_x = max_x = x
            min_y = max_y = y
            while stack:
                cy, cx = stack.pop()
                count += 1
                sum_x += cx
                sum_y += cy
                peak = max(peak, float(projection_image[cy, cx]))
                min_x = min(min_x, cx)
                max_x = max(max_x, cx)
                min_y = min(min_y, cy)
                max_y = max(max_y, cy)
                for ny, nx in ((cy - 1, cx), (cy + 1, cx), (cy, cx - 1), (cy, cx + 1)):
                    if (
                        0 <= ny < height
                        and 0 <= nx < width
                        and mask[ny, nx]
                        and not visited[ny, nx]
                    ):
                        visited[ny, nx] = True
                        stack.append((ny, nx))
            spots.append(
                {
                    "pixel_count": count,
                    "centroid": [round(sum_x / count, 3), round(sum_y / count, 3)],
                    "peak": round(peak, 9),
                    "bbox": [min_x, min_y, max_x, max_y],
                }
            )
    spots.sort(key=lambda item: (-item["peak"], item["centroid"][1], item["centroid"][0]))
    return len(spots), spots
