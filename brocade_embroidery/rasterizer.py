"""Tile-based deterministic software rasterizer."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .geometry import (
    Camera,
    barycentric,
    interpolate,
    normalize,
    ray_plane_intersection,
)
from .sampling import EmbroideryTable, HoleTable, bilinear_sample
from .shading import inter_layer_attenuation, shade_kesi, shade_satin
from .shadow import ReceiverField, layer_uv_axes


@dataclass
class LayerTables:
    embroidery: dict[int, EmbroideryTable]
    holes: dict[int, HoleTable]


@dataclass
class TileResult:
    tile_id: int
    rgba: np.ndarray
    ownership: np.ndarray
    stats: dict


def component_id_map(scene: dict) -> dict[str, int]:
    mapping = {"BACKGROUND": 0}
    next_id = 1
    for layer in scene["layers"]:
        for component in layer["component_ids"]:
            mapping[component] = next_id
            next_id += 1
    mapping["FLOOR"] = next_id
    return mapping


def tile_layout(scene: dict) -> list[tuple[int, int, int, int, int]]:
    frame = scene["frame"]
    columns = int(np.ceil(frame["width"] / frame["tile_width"]))
    rows = int(np.ceil(frame["height"] / frame["tile_height"]))
    layout = []
    for row in range(rows):
        for col in range(columns):
            tile_x = col * frame["tile_width"]
            tile_y = row * frame["tile_height"]
            width = min(frame["tile_width"], frame["width"] - tile_x)
            height = min(frame["tile_height"], frame["height"] - tile_y)
            layout.append((row * columns + col, tile_x, tile_y, width, height))
    return layout


def _floor_rgba(scene: dict, camera: Camera, tile_x: int, tile_y: int,
                width: int, height: int,
                shadow_fields: dict[tuple, ReceiverField]) -> np.ndarray:
    origins, directions = camera.rays()
    directions = directions[tile_y:tile_y + height, tile_x:tile_x + width]
    origins = origins[tile_y:tile_y + height, tile_x:tile_x + width]
    floor = scene["floor"]
    normal = normalize(np.asarray(floor["normal"], dtype=np.float64))
    point = np.asarray(floor["point"], dtype=np.float64)
    flat_directions = directions.reshape(-1, 3)
    flat_origins = origins.reshape(-1, 3)
    hits, finite = ray_plane_intersection(flat_origins, flat_directions,
                                          normal, point)
    base = np.asarray(floor["color"], dtype=np.float32)
    color = np.zeros((hits.shape[0], 4), dtype=np.float32)
    color[:, :3] = base[None, :] * 0.35
    color[:, 3] = 1.0
    for light in scene["lights"]:
        light_position = np.asarray(light["position"], dtype=np.float64)
        light_color = np.asarray(light["color"], dtype=np.float32)
        delta = light_position[None, :] - hits
        distance = np.linalg.norm(delta, axis=-1)
        light_vector = delta / np.maximum(distance[:, None], 1e-9)
        attenuation = np.float32(light["intensity"]) / (
            np.float32(1.0) + np.float32(0.04)
            * (distance * distance).astype(np.float32))
        diffuse = np.clip(np.dot(light_vector, normal), 0.0, 1.0).astype(np.float32)
        field = shadow_fields.get(("floor", light["id"]))
        if field is not None:
            rel = hits - field.origin
            u = (np.dot(rel, field.u_axis) + 0.5 * field.extent_u) / field.extent_u
            v = (np.dot(rel, field.v_axis) + 0.5 * field.extent_v) / field.extent_v
            transmission = bilinear_sample(field.transmission, u, v)
            inside = (u >= 0) & (u <= 1) & (v >= 0) & (v <= 1)
            transmission = np.where(inside, transmission, 1.0)
        else:
            transmission = np.ones_like(attenuation)
        color[:, :3] += (base[None, :] * attenuation[:, None]
                         * diffuse[:, None] * light_color[None, :]
                         * transmission[:, None])
    color[:, :3] *= color[:, 3:4]
    color[~finite] = 0.0
    return np.clip(color, 0.0, 1.0).reshape(height, width, 4)


def prepare_layer_geometry(layer: dict, camera: Camera) -> dict:
    vertices = np.asarray(layer["vertices"], dtype=np.float64)
    triangles = np.asarray(layer["triangles"], dtype=np.int64)
    pixel, depth = camera.project(vertices)
    face_vertices = vertices[triangles]
    face_pixels = pixel[triangles]
    face_depth = depth[triangles]

    v0 = vertices[triangles[:, 0]]
    v1 = vertices[triangles[:, 1]]
    v2 = vertices[triangles[:, 2]]
    face_normals = np.cross(v1 - v0, v2 - v0)
    face_normals = face_normals / np.maximum(
        np.linalg.norm(face_normals, axis=-1, keepdims=True), 1e-12)
    vertex_normals = np.zeros_like(vertices)
    for corner in range(3):
        np.add.at(vertex_normals, triangles[:, corner], face_normals)
    vertex_normals = vertex_normals / np.maximum(
        np.linalg.norm(vertex_normals, axis=-1, keepdims=True), 1e-12)
    invalid = ~np.isfinite(vertex_normals).all(axis=-1)
    vertex_normals[invalid] = normalize(np.asarray(layer["plane"]["normal"]))
    return {
        "triangles": triangles,
        "face_pixels": face_pixels,
        "face_depth": face_depth,
        "face_vertices": face_vertices,
        "face_normals": vertex_normals[triangles],
        "components": np.asarray(layer["triangle_components"]),
        "bounds_min": face_pixels.min(axis=1),
        "bounds_max": face_pixels.max(axis=1),
    }


def render_tile(scene: dict, camera: Camera, tile: tuple,
                tables: LayerTables,
                shadow_fields: dict[tuple, ReceiverField],
                component_lookup: dict[str, int],
                prepared_layers: list[dict] | None = None) -> TileResult:
    tile_id, tile_x, tile_y, tile_width, tile_height = tile
    rgba = _floor_rgba(scene, camera, tile_x, tile_y,
                       tile_width, tile_height, shadow_fields)
    ownership = np.zeros((tile_height, tile_width), dtype=np.uint16)
    closest_depth = np.full((tile_height, tile_width), np.inf, dtype=np.float64)
    stats = {
        "tile_id": tile_id,
        "visible_fragments": 0,
        "reflection_sum": 0.0,
        "transmission_sample_count": 0,
        "reflection_by_layer": {},
        "transmission_by_layer": {},
    }
    if prepared_layers is None:
        prepared_layers = [prepare_layer_geometry(layer, camera)
                           for layer in scene["layers"]]

    for layer, prepared in zip(scene["layers"], prepared_layers):
        u_axis, v_axis = layer_uv_axes(layer)
        plane_point = np.asarray(layer["plane"]["point"], dtype=np.float64)
        layer_reflection = 0.0
        layer_transmission = 0.0
        bounds_min = prepared["bounds_min"]
        bounds_max = prepared["bounds_max"]
        candidates = np.flatnonzero(
            (bounds_max[:, 0] >= tile_x)
            & (bounds_min[:, 0] <= tile_x + tile_width - 1)
            & (bounds_max[:, 1] >= tile_y)
            & (bounds_min[:, 1] <= tile_y + tile_height - 1)
        )
        for face_id in candidates:
            px_triangle = prepared["face_pixels"][face_id]
            min_x = max(tile_x, int(np.floor(px_triangle[:, 0].min())))
            max_x = min(tile_x + tile_width - 1,
                        int(np.ceil(px_triangle[:, 0].max())))
            min_y = max(tile_y, int(np.floor(px_triangle[:, 1].min())))
            max_y = min(tile_y + tile_height - 1,
                        int(np.ceil(px_triangle[:, 1].max())))
            if min_x > max_x or min_y > max_y:
                continue
            xs = np.arange(min_x, max_x + 1, dtype=np.float64)
            ys = np.arange(min_y, max_y + 1, dtype=np.float64)
            grid_x, grid_y = np.meshgrid(xs, ys)
            sample_points = np.stack([grid_x, grid_y], axis=-1)
            weights = barycentric(sample_points, px_triangle[0],
                                  px_triangle[1], px_triangle[2])
            inside = np.all(weights >= -1e-7, axis=-1)
            if not np.any(inside):
                continue
            local_x = (grid_x[inside] - tile_x).astype(np.int64)
            local_y = (grid_y[inside] - tile_y).astype(np.int64)
            fragment_weights = weights[inside]
            world = interpolate(fragment_weights,
                                prepared["face_vertices"][face_id])
            normal = interpolate(fragment_weights,
                                 prepared["face_normals"][face_id])
            depth = np.sum(fragment_weights
                           * prepared["face_depth"][face_id], axis=-1)
            delta = world - plane_point[None, :]
            u = np.dot(delta, u_axis) / layer["width"] + 0.5
            v = np.dot(delta, v_axis) / layer["height"] + 0.5
            uv = np.stack([u, v], axis=-1)

            if layer["kind"] == "satin":
                table = tables.embroidery[layer["layer_index"]]
                color, reflection = shade_satin(
                    layer, world, normal, uv, camera.eye,
                    scene["lights"], table, shadow_fields)
                alpha = np.ones(color.shape[0], dtype=np.float32)
                transmission = np.zeros(color.shape[0], dtype=np.float32)
            else:
                table = tables.holes[layer["layer_index"]]
                color, alpha, transmission = shade_kesi(
                    layer, world, normal, uv, camera.eye,
                    scene["lights"], table, shadow_fields)
                reflection = np.zeros(color.shape[0], dtype=np.float32)
                view_vector = normalize(camera.eye[None, :] - world)
                gap_factor = inter_layer_attenuation(
                    layer.get("air_gap_to_previous", 0.0),
                    view_vector, normalize(normal))
                color = color * gap_factor[:, None]
                transmission = transmission * gap_factor

            if color.shape[-1] == 3:
                color = np.concatenate(
                    [color, np.ones((color.shape[0], 1), dtype=np.float32)],
                    axis=-1)
            premultiplied = color * alpha[:, None]
            current = rgba[local_y, local_x]
            one_minus_alpha = (1.0 - alpha)[:, None]
            rgba[local_y, local_x] = np.clip(
                premultiplied + current * one_minus_alpha, 0.0, 1.0)

            update = depth < closest_depth[local_y, local_x]
            if np.any(update):
                updated_y = local_y[update]
                updated_x = local_x[update]
                closest_depth[updated_y, updated_x] = depth[update]
                component = prepared["components"][face_id]
                ownership[updated_y, updated_x] = component_lookup[component]

            fragment_count = int(world.shape[0])
            stats["visible_fragments"] += fragment_count
            stats["transmission_sample_count"] += fragment_count
            layer_reflection += float(np.sum(reflection))
            layer_transmission += float(np.sum(transmission))
        stats["reflection_by_layer"][str(layer["layer_index"])] = layer_reflection
        stats["transmission_by_layer"][str(layer["layer_index"])] = layer_transmission
        stats["reflection_sum"] += layer_reflection
    return TileResult(tile_id=tile_id,
                      rgba=rgba.astype(np.float32),
                      ownership=ownership,
                      stats=stats)
