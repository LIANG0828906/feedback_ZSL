"""Scene schema, deterministic sample construction and business validation."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .canonical import canonical_bytes, sha256_bytes, sha256_json
from .errors import (
    ERR_CAMERA,
    ERR_DEGENERATE,
    ERR_DUPLICATE_ID,
    ERR_FISH,
    ERR_LIGHT_INSIDE,
    ERR_LIGHT_SET,
    ERR_LAYER_SET,
    ERR_MESH_INDEX,
    ERR_MESH_INVALID,
    ERR_NEGATIVE_ABSORPTION,
    ERR_RECEIVER,
    ERR_SCHEMA,
    ERR_THICKNESS,
    ERR_TRANSMISSION,
    BusinessError,
    SceneValidationError,
)
from .geometry import (
    annulus_mesh,
    arc_shell_mesh,
    face_normals,
    mesh_arrays,
    point_in_closed_mesh,
    quad_mesh,
    triangle_area2,
)

RENDERER_REVISION = "1.0.0-r4"


def _err(code: str, component_id: str, message: str, path: str, **details: Any) -> BusinessError:
    return BusinessError(code, component_id, message, path, details or None)


def _finite_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(float(value))


def _vec3(value: Any) -> np.ndarray | None:
    if not isinstance(value, list) or len(value) != 3 or not all(_finite_number(v) for v in value):
        return None
    return np.asarray(value, dtype=np.float64)


@dataclass
class Component:
    component_id: str
    kind: str
    layer_index: int
    mesh: dict[str, Any]
    vertices: np.ndarray
    triangles: np.ndarray
    normals: np.ndarray
    ownership_id: int
    thickness: float | None = None
    transmission: float | None = None
    tint: np.ndarray = field(default_factory=lambda: np.ones(3, dtype=np.float64))
    ior: float = 1.5
    absorption: np.ndarray = field(default_factory=lambda: np.zeros(3, dtype=np.float64))
    scattering_color: np.ndarray = field(default_factory=lambda: np.ones(3, dtype=np.float64))
    particle_seed: int = 0
    particle_resolution: tuple[int, int] = (32, 12)
    particle_count: int = 32
    particle_strength: float = 0.25
    projection_radius: float | None = None


@dataclass
class Fish:
    fish_id: str
    mesh: dict[str, Any]
    vertices: np.ndarray
    triangles: np.ndarray
    normals: np.ndarray
    water_component_id: str
    color: np.ndarray
    opacity: float
    ownership_id: int


@dataclass
class Light:
    light_id: str
    position: np.ndarray
    color: np.ndarray
    intensity: float
    radius: float


@dataclass
class Receiver:
    component_id: str
    mesh: dict[str, Any]
    vertices: np.ndarray
    triangles: np.ndarray
    normals: np.ndarray
    plane_z: float
    inner_radius: float
    outer_radius: float
    angle_start_deg: float
    angle_end_deg: float
    base_color: np.ndarray
    pebble_palette: list[np.ndarray]
    ownership_id: int


@dataclass
class Camera:
    orbit_center: np.ndarray
    radius: float
    eye_height: float
    target: np.ndarray
    start_deg: float
    end_deg: float
    sample_count: int
    fov_deg: float

    def angles(self) -> np.ndarray:
        if self.sample_count == 1:
            return np.asarray([(self.start_deg + self.end_deg) * 0.5], dtype=np.float64)
        return np.linspace(self.start_deg, self.end_deg, self.sample_count, dtype=np.float64)

    def position(self, angle_deg: float) -> np.ndarray:
        angle = math.radians(angle_deg)
        return np.asarray(
            [
                self.orbit_center[0] + self.radius * math.cos(angle),
                self.orbit_center[1] + self.radius * math.sin(angle),
                self.orbit_center[2] + self.eye_height,
            ],
            dtype=np.float64,
        )


@dataclass
class RenderConfig:
    width: int
    height: int
    block_width: int
    block_height: int
    exposure: float
    shadow_samples: int
    shadow_map_resolution: int
    shadow_strength: float
    shadow_softness_base: float
    shadow_softness_scale: float
    background: np.ndarray
    ambient: float
    particle_modulation_scale: float
    reflection_strength: float

    @property
    def blocks_x(self) -> int:
        return (self.width + self.block_width - 1) // self.block_width

    @property
    def blocks_y(self) -> int:
        return (self.height + self.block_height - 1) // self.block_height

    @property
    def blocks_per_angle(self) -> int:
        return self.blocks_x * self.blocks_y


@dataclass
class Scene:
    source: dict[str, Any]
    scene_id: str
    glass: list[Component]
    water: list[Component]
    components: list[Component]
    fish: list[Fish]
    lights: list[Light]
    receiver: Receiver
    camera: Camera
    render: RenderConfig
    ownership_legend: dict[int, dict[str, Any]]
    summary: str

    @property
    def summary_record(self) -> dict[str, Any]:
        return {
            "renderer_revision": RENDERER_REVISION,
            "schema_version": self.source.get("schema_version"),
            "scene_id": self.scene_id,
            "scene_sha256": self.summary,
            "particle_seeds": {
                component.component_id: component.particle_seed for component in self.water
            },
            "angles_deg": self.camera.angles().round(10).tolist(),
            "render": {
                "width": self.render.width,
                "height": self.render.height,
                "block_width": self.render.block_width,
                "block_height": self.render.block_height,
                "shadow_samples": self.render.shadow_samples,
            },
        }


def _validate_mesh(
    mesh_value: Any,
    component_id: str,
    path: str,
    errors: list[BusinessError],
) -> tuple[np.ndarray, np.ndarray, np.ndarray] | None:
    if not isinstance(mesh_value, dict):
        errors.append(_err(ERR_SCHEMA, component_id, "mesh must be an object", path))
        return None
    vertices_value = mesh_value.get("vertices")
    triangles_value = mesh_value.get("triangles")
    if not isinstance(vertices_value, list) or not vertices_value:
        errors.append(_err(ERR_MESH_INVALID, component_id, "vertices must be a non-empty list", f"{path}.vertices"))
        return None
    if not isinstance(triangles_value, list) or not triangles_value:
        errors.append(_err(ERR_MESH_INVALID, component_id, "triangles must be a non-empty list", f"{path}.triangles"))
        return None

    vertices_list: list[list[float]] = []
    for index, vertex in enumerate(vertices_value):
        if not isinstance(vertex, list) or len(vertex) != 3 or not all(_finite_number(v) for v in vertex):
            errors.append(
                _err(
                    ERR_MESH_INVALID,
                    component_id,
                    "vertex must contain three finite numbers",
                    f"{path}.vertices[{index}]",
                )
            )
            return None
        vertices_list.append([float(v) for v in vertex])

    triangles_list: list[list[int]] = []
    for index, triangle in enumerate(triangles_value):
        if (
            not isinstance(triangle, list)
            or len(triangle) != 3
            or not all(isinstance(v, int) and not isinstance(v, bool) for v in triangle)
        ):
            errors.append(
                _err(
                    ERR_MESH_INVALID,
                    component_id,
                    "triangle must contain three integer indices",
                    f"{path}.triangles[{index}]",
                )
            )
            continue
        if any(v < 0 or v >= len(vertices_list) for v in triangle):
            errors.append(
                _err(
                    ERR_MESH_INDEX,
                    component_id,
                    "triangle index is outside the vertex array",
                    f"{path}.triangles[{index}]",
                    triangle=triangle,
                    vertex_count=len(vertices_list),
                )
            )
            continue
        triangles_list.append([int(v) for v in triangle])

    if len(triangles_list) != len(triangles_value):
        return None
    vertices = np.asarray(vertices_list, dtype=np.float64)
    triangles = np.asarray(triangles_list, dtype=np.int64)
    areas = triangle_area2(
        vertices[triangles[:, 0]],
        vertices[triangles[:, 1]],
        vertices[triangles[:, 2]],
    )
    for index in np.flatnonzero(areas <= 1e-12):
        errors.append(
            _err(
                ERR_DEGENERATE,
                component_id,
                "triangle is degenerate",
                f"{path}.triangles[{int(index)}]",
                area2=float(areas[index]),
            )
        )
    return vertices, triangles, face_normals(vertices, triangles)


def _ownership_palette(count: int) -> list[tuple[int, int, int, int]]:
    palette = [(0, 0, 0, 255)]
    for index in range(1, count + 1):
        hue = (index * 0.6180339887498949) % 1.0
        rgb = _hsv_to_rgb(hue, 0.68, 0.95)
        palette.append((int(round(rgb[0] * 255)), int(round(rgb[1] * 255)), int(round(rgb[2] * 255)), 255))
    return palette


def _hsv_to_rgb(h: float, s: float, v: float) -> tuple[float, float, float]:
    i = int(h * 6.0)
    f = h * 6.0 - i
    p = v * (1.0 - s)
    q = v * (1.0 - f * s)
    t = v * (1.0 - (1.0 - f) * s)
    i %= 6
    return [(v, t, p), (q, v, p), (p, v, t), (p, q, v), (t, p, v), (v, p, q)][i]


def validate_scene(raw: Any) -> Scene:
    errors: list[BusinessError] = []
    if not isinstance(raw, dict):
        raise SceneValidationError(
            [_err(ERR_SCHEMA, "scene", "scene root must be a JSON object", "$")]
        )

    if raw.get("schema_version") != "1.0":
        errors.append(_err(ERR_SCHEMA, "scene", "schema_version must be '1.0'", "$.schema_version"))
    scene_id = raw.get("scene_id")
    if not isinstance(scene_id, str) or not scene_id:
        errors.append(_err(ERR_SCHEMA, "scene", "scene_id must be a non-empty string", "$.scene_id"))
        scene_id = "invalid-scene"

    layers_value = raw.get("layers")
    if not isinstance(layers_value, list):
        errors.append(_err(ERR_SCHEMA, "scene", "layers must be a list", "$.layers"))
        layers_value = []

    seen_ids: dict[str, str] = {}
    raw_components: list[dict[str, Any]] = []
    glass_count = 0
    water_count = 0
    for index, layer_value in enumerate(layers_value):
        path = f"$.layers[{index}]"
        if not isinstance(layer_value, dict):
            errors.append(_err(ERR_SCHEMA, f"layer-{index}", "layer must be an object", path))
            continue
        component_id = layer_value.get("component_id")
        kind = layer_value.get("kind")
        layer_index = layer_value.get("layer_index")
        if not isinstance(component_id, str) or not component_id:
            errors.append(_err(ERR_SCHEMA, f"layer-{index}", "component_id is required", f"{path}.component_id"))
            component_id = f"layer-{index}"
        if component_id in seen_ids:
            errors.append(
                _err(
                    ERR_DUPLICATE_ID,
                    component_id,
                    "component id is duplicated",
                    f"{path}.component_id",
                    first_path=seen_ids[component_id],
                )
            )
        else:
            seen_ids[component_id] = f"{path}.component_id"
        if kind not in {"glass", "water"}:
            errors.append(_err(ERR_SCHEMA, component_id, "kind must be 'glass' or 'water'", f"{path}.kind"))
            continue
        if not isinstance(layer_index, int) or isinstance(layer_index, bool):
            errors.append(
                _err(ERR_SCHEMA, component_id, "layer_index must be an integer", f"{path}.layer_index")
            )
            layer_index = -1
        if kind == "glass":
            glass_count += 1
        else:
            water_count += 1
        raw_components.append(layer_value)

    if glass_count != 3:
        errors.append(_err(ERR_LAYER_SET, "scene", "scene must contain exactly three glass layers", "$.layers"))
    if water_count != 2:
        errors.append(_err(ERR_LAYER_SET, "scene", "scene must contain exactly two water layers", "$.layers"))

    for expected in range(3):
        if not any(
            c.get("kind") == "glass" and c.get("layer_index") == expected for c in raw_components
        ):
            errors.append(
                _err(
                    ERR_LAYER_SET,
                    f"glass-{expected:02d}",
                    f"missing or duplicate glass layer_index {expected}",
                    "$.layers",
                )
            )
    for expected in range(2):
        if not any(
            c.get("kind") == "water" and c.get("layer_index") == expected for c in raw_components
        ):
            errors.append(
                _err(
                    ERR_LAYER_SET,
                    f"water-{expected:02d}",
                    f"missing or duplicate water layer_index {expected}",
                    "$.layers",
                )
            )

    components: list[Component] = []
    component_lookup: dict[str, Component] = {}
    ownership_id = 1
    for index, layer_value in enumerate(raw_components):
        path = f"$.layers[{index}]"
        component_id = str(layer_value.get("component_id", f"layer-{index}"))
        kind = layer_value.get("kind")
        raw_layer_index = layer_value.get("layer_index")
        safe_layer_index = (
            int(raw_layer_index)
            if isinstance(raw_layer_index, int) and not isinstance(raw_layer_index, bool)
            else -1
        )
        mesh_value = layer_value.get("mesh")
        mesh_result = _validate_mesh(mesh_value, component_id, f"{path}.mesh", errors)
        if mesh_result is None:
            continue
        vertices, triangles, normals = mesh_result
        tint = _vec3(layer_value.get("tint", [1.0, 1.0, 1.0]))
        if tint is None or np.any(tint < 0.0) or np.any(tint > 1.0):
            errors.append(_err(ERR_SCHEMA, component_id, "tint must be three numbers in [0,1]", f"{path}.tint"))
            tint = np.ones(3)

        if kind == "glass":
            thickness = layer_value.get("thickness")
            transmission = layer_value.get("transmission")
            ior = layer_value.get("ior", 1.5)
            if not _finite_number(thickness) or not (0.0 < float(thickness) <= 0.5):
                errors.append(
                    _err(
                        ERR_THICKNESS,
                        component_id,
                        "glass thickness must be finite and in (0, 0.5]",
                        f"{path}.thickness",
                        value=thickness,
                    )
                )
                thickness = 0.01
            if not _finite_number(transmission) or not (0.0 <= float(transmission) <= 1.0):
                errors.append(
                    _err(
                        ERR_TRANSMISSION,
                        component_id,
                        "glass transmission must be finite and in [0, 1]",
                        f"{path}.transmission",
                        value=transmission,
                    )
                )
                transmission = 1.0
            if not _finite_number(ior) or not (1.0 < float(ior) <= 3.0):
                errors.append(
                    _err(ERR_SCHEMA, component_id, "glass ior must be in (1, 3]", f"{path}.ior")
                )
                ior = 1.5
            component = Component(
                component_id=component_id,
                kind="glass",
                layer_index=safe_layer_index,
                mesh=mesh_value,
                vertices=vertices,
                triangles=triangles,
                normals=normals,
                ownership_id=ownership_id,
                thickness=float(thickness),
                transmission=float(transmission),
                tint=tint,
                ior=float(ior),
            )
        else:
            absorption_value = _vec3(layer_value.get("absorption_coefficient"))
            scattering = _vec3(layer_value.get("scattering_color", [0.2, 0.6, 0.7]))
            seed = layer_value.get("particle_seed")
            resolution = layer_value.get("particle_resolution", [32, 12])
            particle_count = layer_value.get("particle_count", 32)
            projection_radius = layer_value.get("projection_radius")
            if absorption_value is None or np.any(absorption_value < 0.0):
                errors.append(
                    _err(
                        ERR_NEGATIVE_ABSORPTION,
                        component_id,
                        "water absorption coefficients must be three non-negative numbers",
                        f"{path}.absorption_coefficient",
                        value=layer_value.get("absorption_coefficient"),
                    )
                )
                absorption_value = np.zeros(3)
            if scattering is None or np.any(scattering < 0.0):
                errors.append(
                    _err(
                        ERR_SCHEMA,
                        component_id,
                        "scattering_color must contain three non-negative numbers",
                        f"{path}.scattering_color",
                    )
                )
                scattering = np.asarray([0.2, 0.5, 0.6])
            if not isinstance(seed, int) or isinstance(seed, bool):
                errors.append(
                    _err(ERR_SCHEMA, component_id, "particle_seed must be an integer", f"{path}.particle_seed")
                )
                seed = 0
            if (
                not isinstance(resolution, list)
                or len(resolution) != 2
                or not all(isinstance(v, int) and not isinstance(v, bool) and 2 <= v <= 512 for v in resolution)
            ):
                errors.append(
                    _err(
                        ERR_SCHEMA,
                        component_id,
                        "particle_resolution must be two integers in [2, 512]",
                        f"{path}.particle_resolution",
                    )
                )
                resolution = [32, 12]
            if not isinstance(particle_count, int) or isinstance(particle_count, bool) or not (0 <= particle_count <= 10000):
                errors.append(
                    _err(ERR_SCHEMA, component_id, "particle_count must be in [0, 10000]", f"{path}.particle_count")
                )
                particle_count = 0
            if projection_radius is not None and (not _finite_number(projection_radius) or projection_radius <= 0):
                errors.append(
                    _err(ERR_SCHEMA, component_id, "projection_radius must be positive", f"{path}.projection_radius")
                )
                projection_radius = None
            particle_strength = layer_value.get("particle_strength", 0.25)
            if not _finite_number(particle_strength) or float(particle_strength) < 0.0:
                errors.append(
                    _err(
                        ERR_SCHEMA,
                        component_id,
                        "particle_strength must be non-negative",
                        f"{path}.particle_strength",
                    )
                )
                particle_strength = 0.0
            component = Component(
                component_id=component_id,
                kind="water",
                layer_index=safe_layer_index,
                mesh=mesh_value,
                vertices=vertices,
                triangles=triangles,
                normals=normals,
                ownership_id=ownership_id,
                tint=tint,
                absorption=absorption_value,
                scattering_color=scattering,
                particle_seed=int(seed),
                particle_resolution=(int(resolution[0]), int(resolution[1])),
                particle_count=int(particle_count),
                particle_strength=float(particle_strength),
                projection_radius=float(projection_radius) if projection_radius is not None else None,
            )
        components.append(component)
        component_lookup[component_id] = component
        ownership_id += 1

    glass = sorted((c for c in components if c.kind == "glass"), key=lambda c: c.layer_index)
    water = sorted((c for c in components if c.kind == "water"), key=lambda c: c.layer_index)

    fish_values = raw.get("fish")
    if not isinstance(fish_values, list) or not fish_values:
        errors.append(_err(ERR_FISH, "fish", "fish must be a non-empty list", "$.fish"))
        fish_values = []
    fish: list[Fish] = []
    for index, fish_value in enumerate(fish_values):
        path = f"$.fish[{index}]"
        if not isinstance(fish_value, dict):
            errors.append(_err(ERR_SCHEMA, f"fish-{index}", "fish must be an object", path))
            continue
        fish_id = fish_value.get("fish_id")
        if not isinstance(fish_id, str) or not fish_id:
            errors.append(_err(ERR_SCHEMA, f"fish-{index}", "fish_id is required", f"{path}.fish_id"))
            fish_id = f"fish-{index}"
        if fish_id in seen_ids:
            errors.append(
                _err(ERR_DUPLICATE_ID, fish_id, "component/fish id is duplicated", f"{path}.fish_id")
            )
        else:
            seen_ids[fish_id] = f"{path}.fish_id"
        water_id = fish_value.get("water_component_id")
        if not isinstance(water_id, str) or water_id not in {c.component_id for c in water}:
            errors.append(
                _err(
                    ERR_FISH,
                    fish_id,
                    "water_component_id must reference a declared water layer",
                    f"{path}.water_component_id",
                )
            )
            water_id = water[0].component_id if water else "missing-water"
        mesh_result = _validate_mesh(fish_value.get("mesh"), fish_id, f"{path}.mesh", errors)
        color = _vec3(fish_value.get("color", [0.9, 0.35, 0.1]))
        opacity = fish_value.get("opacity", 0.95)
        if color is None or np.any(color < 0.0):
            errors.append(_err(ERR_FISH, fish_id, "fish color must be non-negative", f"{path}.color"))
            color = np.asarray([0.9, 0.35, 0.1])
        if not _finite_number(opacity) or not (0.0 <= float(opacity) <= 1.0):
            errors.append(_err(ERR_FISH, fish_id, "fish opacity must be in [0,1]", f"{path}.opacity"))
            opacity = 1.0
        if mesh_result is None:
            continue
        vertices, triangles, normals = mesh_result
        fish.append(
            Fish(
                fish_id=fish_id,
                mesh=fish_value["mesh"],
                vertices=vertices,
                triangles=triangles,
                normals=normals,
                water_component_id=str(water_id),
                color=color,
                opacity=float(opacity),
                ownership_id=ownership_id,
            )
        )
        ownership_id += 1

    lights_value = raw.get("lights")
    lights: list[Light] = []
    if not isinstance(lights_value, list) or len(lights_value) != 2:
        errors.append(_err(ERR_LIGHT_SET, "lights", "exactly two point lights are required", "$.lights"))
        lights_value = []
    for index, light_value in enumerate(lights_value):
        path = f"$.lights[{index}]"
        if not isinstance(light_value, dict):
            errors.append(_err(ERR_SCHEMA, f"light-{index}", "light must be an object", path))
            continue
        light_id = light_value.get("light_id")
        if not isinstance(light_id, str) or not light_id:
            errors.append(_err(ERR_SCHEMA, f"light-{index}", "light_id is required", f"{path}.light_id"))
            light_id = f"light-{index}"
        if light_id in seen_ids:
            errors.append(
                _err(ERR_DUPLICATE_ID, light_id, "component/light id is duplicated", f"{path}.light_id")
            )
        else:
            seen_ids[light_id] = f"{path}.light_id"
        position = _vec3(light_value.get("position"))
        color = _vec3(light_value.get("color", [1.0, 1.0, 1.0]))
        intensity = light_value.get("intensity", 1.0)
        radius = light_value.get("radius", 0.06)
        if position is None:
            errors.append(_err(ERR_LIGHT_SET, light_id, "light position must be three finite numbers", f"{path}.position"))
            position = np.zeros(3)
        if color is None or np.any(color < 0.0):
            errors.append(_err(ERR_LIGHT_SET, light_id, "light color must be non-negative", f"{path}.color"))
            color = np.ones(3)
        if not _finite_number(intensity) or float(intensity) < 0.0:
            errors.append(_err(ERR_LIGHT_SET, light_id, "light intensity must be non-negative", f"{path}.intensity"))
            intensity = 0.0
        if not _finite_number(radius) or float(radius) <= 0.0:
            errors.append(_err(ERR_LIGHT_SET, light_id, "light radius must be positive", f"{path}.radius"))
            radius = 0.06
        lights.append(
            Light(
                light_id=light_id,
                position=position,
                color=color,
                intensity=float(intensity),
                radius=float(radius),
            )
        )

    for light in lights:
        for component in components:
            if point_in_closed_mesh(light.position, component.vertices, component.triangles):
                errors.append(
                    _err(
                        ERR_LIGHT_INSIDE,
                        component.component_id,
                        "point light lies inside layer geometry",
                        "$.lights",
                        light_id=light.light_id,
                        light_position=light.position.tolist(),
                    )
                )

    receiver_value = raw.get("receiver")
    receiver: Receiver | None = None
    if not isinstance(receiver_value, dict):
        errors.append(_err(ERR_RECEIVER, "receiver", "receiver must be an object", "$.receiver"))
    else:
        receiver_id = receiver_value.get("component_id")
        if not isinstance(receiver_id, str) or not receiver_id:
            errors.append(
                _err(ERR_RECEIVER, "receiver", "component_id is required", "$.receiver.component_id")
            )
            receiver_id = "receiver"
        if receiver_id in seen_ids:
            errors.append(
                _err(ERR_DUPLICATE_ID, receiver_id, "component/receiver id is duplicated", "$.receiver.component_id")
            )
        mesh_result = _validate_mesh(receiver_value.get("mesh"), receiver_id, "$.receiver.mesh", errors)
        plane_z = receiver_value.get("plane_z")
        inner_radius = receiver_value.get("inner_radius")
        outer_radius = receiver_value.get("outer_radius")
        angle_start = receiver_value.get("angle_start_deg")
        angle_end = receiver_value.get("angle_end_deg")
        base_color = _vec3(receiver_value.get("base_color", [0.35, 0.32, 0.28]))
        palette_value = receiver_value.get("pebble_palette", [[0.46, 0.4, 0.33], [0.3, 0.33, 0.32]])
        if not all(_finite_number(v) for v in (plane_z, inner_radius, outer_radius, angle_start, angle_end)):
            errors.append(
                _err(ERR_RECEIVER, receiver_id, "receiver numeric bounds are invalid", "$.receiver")
            )
            plane_z, inner_radius, outer_radius, angle_start, angle_end = 0.0, 0.0, 1.0, 0.0, 360.0
        if float(inner_radius) < 0.0 or float(outer_radius) <= float(inner_radius):
            errors.append(
                _err(ERR_RECEIVER, receiver_id, "receiver radii must satisfy 0 <= inner < outer", "$.receiver")
            )
        if float(angle_end) <= float(angle_start):
            errors.append(
                _err(ERR_RECEIVER, receiver_id, "receiver angle_end_deg must exceed angle_start_deg", "$.receiver")
            )
        if base_color is None or np.any(base_color < 0.0):
            errors.append(_err(ERR_RECEIVER, receiver_id, "receiver base_color is invalid", "$.receiver.base_color"))
            base_color = np.asarray([0.35, 0.32, 0.28])
        pebble_palette: list[np.ndarray] = []
        if not isinstance(palette_value, list) or not palette_value:
            errors.append(
                _err(ERR_RECEIVER, receiver_id, "pebble_palette must be a non-empty list", "$.receiver.pebble_palette")
            )
            palette_value = [[0.4, 0.4, 0.4]]
        for item in palette_value:
            color = _vec3(item)
            if color is None or np.any(color < 0.0):
                errors.append(
                    _err(ERR_RECEIVER, receiver_id, "pebble palette entry is invalid", "$.receiver.pebble_palette")
                )
                color = np.asarray([0.4, 0.4, 0.4])
            pebble_palette.append(color)
        if mesh_result is not None:
            vertices, triangles, normals = mesh_result
            receiver = Receiver(
                component_id=receiver_id,
                mesh=receiver_value["mesh"],
                vertices=vertices,
                triangles=triangles,
                normals=normals,
                plane_z=float(plane_z),
                inner_radius=float(inner_radius),
                outer_radius=float(outer_radius),
                angle_start_deg=float(angle_start),
                angle_end_deg=float(angle_end),
                base_color=base_color,
                pebble_palette=pebble_palette,
                ownership_id=ownership_id,
            )
            ownership_id += 1

    camera_value = raw.get("camera")
    camera = None
    if not isinstance(camera_value, dict):
        errors.append(_err(ERR_CAMERA, "camera", "camera must be an object", "$.camera"))
    else:
        orbit_center = _vec3(camera_value.get("orbit_center", [0.0, 0.0, 0.0]))
        target = _vec3(camera_value.get("target"))
        radius = camera_value.get("radius")
        eye_height = camera_value.get("eye_height", 1.0)
        start_deg = camera_value.get("start_deg")
        end_deg = camera_value.get("end_deg")
        sample_count = camera_value.get("sample_count")
        fov_deg = camera_value.get("fov_deg", 45.0)
        if orbit_center is None:
            errors.append(_err(ERR_CAMERA, "camera", "orbit_center is invalid", "$.camera.orbit_center"))
            orbit_center = np.zeros(3)
        if target is None:
            errors.append(_err(ERR_CAMERA, "camera", "target is invalid", "$.camera.target"))
            target = np.asarray([0.0, 0.0, 0.8])
        if not _finite_number(radius) or float(radius) <= 0.0:
            errors.append(_err(ERR_CAMERA, "camera", "radius must be positive", "$.camera.radius"))
            radius = 4.0
        if not _finite_number(eye_height):
            errors.append(_err(ERR_CAMERA, "camera", "eye_height must be finite", "$.camera.eye_height"))
            eye_height = 1.0
        if not _finite_number(start_deg) or not _finite_number(end_deg):
            errors.append(_err(ERR_CAMERA, "camera", "arc angles must be finite", "$.camera"))
            start_deg, end_deg = -30.0, 30.0
        if not isinstance(sample_count, int) or isinstance(sample_count, bool) or not (1 <= sample_count <= 10000):
            errors.append(
                _err(ERR_CAMERA, "camera", "sample_count must be an integer in [1,10000]", "$.camera.sample_count")
            )
            sample_count = 1
        if not _finite_number(fov_deg) or not (1.0 <= float(fov_deg) <= 120.0):
            errors.append(_err(ERR_CAMERA, "camera", "fov_deg must be in [1,120]", "$.camera.fov_deg"))
            fov_deg = 45.0
        camera = Camera(
            orbit_center=orbit_center,
            radius=float(radius),
            eye_height=float(eye_height),
            target=target,
            start_deg=float(start_deg),
            end_deg=float(end_deg),
            sample_count=int(sample_count),
            fov_deg=float(fov_deg),
        )

    render_value = raw.get("render")
    if not isinstance(render_value, dict):
        errors.append(_err(ERR_SCHEMA, "render", "render must be an object", "$.render"))
        render_value = {}
    width = render_value.get("width", 96)
    height = render_value.get("height", 72)
    block_width = render_value.get("block_width", 24)
    block_height = render_value.get("block_height", 24)
    for name, value in (
        ("width", width),
        ("height", height),
        ("block_width", block_width),
        ("block_height", block_height),
    ):
        if not isinstance(value, int) or isinstance(value, bool) or not (1 <= value <= 4096):
            errors.append(_err(ERR_SCHEMA, "render", f"{name} must be in [1,4096]", f"$.render.{name}"))
    if isinstance(width, int) and isinstance(block_width, int) and block_width > width:
        errors.append(_err(ERR_SCHEMA, "render", "block_width must not exceed width", "$.render.block_width"))
    if isinstance(height, int) and isinstance(block_height, int) and block_height > height:
        errors.append(_err(ERR_SCHEMA, "render", "block_height must not exceed height", "$.render.block_height"))
    exposure = render_value.get("exposure", 1.0)
    shadow_samples = render_value.get("shadow_samples", 8)
    shadow_map_resolution = render_value.get("shadow_map_resolution", 128)
    shadow_strength = render_value.get("shadow_strength", 0.62)
    softness_base = render_value.get("shadow_softness_base", 0.018)
    softness_scale = render_value.get("shadow_softness_scale", 0.55)
    background = _vec3(render_value.get("background", [0.015, 0.03, 0.045]))
    ambient = render_value.get("ambient", 0.08)
    particle_scale = render_value.get("particle_modulation_scale", 0.45)
    reflection = render_value.get("reflection_strength", 0.08)
    for name, value in (
        ("exposure", exposure),
        ("shadow_strength", shadow_strength),
        ("shadow_softness_base", softness_base),
        ("shadow_softness_scale", softness_scale),
        ("ambient", ambient),
        ("particle_modulation_scale", particle_scale),
        ("reflection_strength", reflection),
    ):
        if not _finite_number(value) or float(value) < 0.0:
            errors.append(_err(ERR_SCHEMA, "render", f"{name} must be non-negative", f"$.render.{name}"))
    if not isinstance(shadow_samples, int) or isinstance(shadow_samples, bool) or not (1 <= shadow_samples <= 256):
        errors.append(
            _err(ERR_SCHEMA, "render", "shadow_samples must be in [1,256]", "$.render.shadow_samples")
        )
        shadow_samples = 8
    if (
        not isinstance(shadow_map_resolution, int)
        or isinstance(shadow_map_resolution, bool)
        or not (16 <= shadow_map_resolution <= 2048)
    ):
        errors.append(
            _err(
                ERR_SCHEMA,
                "render",
                "shadow_map_resolution must be in [16,2048]",
                "$.render.shadow_map_resolution",
            )
        )
        shadow_map_resolution = 128
    if background is None:
        errors.append(_err(ERR_SCHEMA, "render", "background is invalid", "$.render.background"))
        background = np.asarray([0.015, 0.03, 0.045])

    if errors:
        raise SceneValidationError(errors)

    assert camera is not None
    assert receiver is not None
    render = RenderConfig(
        width=int(width),
        height=int(height),
        block_width=int(block_width),
        block_height=int(block_height),
        exposure=float(exposure),
        shadow_samples=int(shadow_samples),
        shadow_map_resolution=int(shadow_map_resolution),
        shadow_strength=float(shadow_strength),
        shadow_softness_base=float(softness_base),
        shadow_softness_scale=float(softness_scale),
        background=background,
        ambient=float(ambient),
        particle_modulation_scale=float(particle_scale),
        reflection_strength=float(reflection),
    )

    palette = _ownership_palette(ownership_id - 1)
    legend: dict[int, dict[str, Any]] = {
        0: {"kind": "background", "component_id": "background", "color": list(palette[0])}
    }
    for component in components:
        legend[component.ownership_id] = {
            "kind": component.kind,
            "component_id": component.component_id,
            "layer_index": component.layer_index,
            "color": list(palette[component.ownership_id]),
        }
    for fish_item in fish:
        legend[fish_item.ownership_id] = {
            "kind": "fish",
            "component_id": fish_item.fish_id,
            "water_component_id": fish_item.water_component_id,
            "color": list(palette[fish_item.ownership_id]),
        }
    legend[receiver.ownership_id] = {
        "kind": "receiver",
        "component_id": receiver.component_id,
        "color": list(palette[receiver.ownership_id]),
    }

    scene = Scene(
        source=raw,
        scene_id=scene_id,
        glass=glass,
        water=water,
        components=components,
        fish=fish,
        lights=lights,
        receiver=receiver,
        camera=camera,
        render=render,
        ownership_legend=legend,
        summary=sha256_json(raw),
    )
    return scene


def load_scene(path: str | Path) -> Scene:
    text = Path(path).read_text(encoding="utf-8")

    def reject_constant(value: str) -> None:
        raise ValueError(f"invalid JSON constant: {value}")

    try:
        raw = json.loads(text, parse_constant=reject_constant)
    except (json.JSONDecodeError, ValueError) as exc:
        raise SceneValidationError(
            [_err(ERR_SCHEMA, "scene", f"invalid JSON: {exc}", "$")]
        ) from exc
    return validate_scene(raw)


def _fish_quad(radius: float, angle_deg: float, z: float, half_angle: float, half_height: float) -> list[list[float]]:
    angle = math.radians(angle_deg)
    points = []
    for da, dz in (
        (-half_angle, -half_height),
        (half_angle, -half_height),
        (half_angle, half_height),
        (-half_angle, half_height),
    ):
        a = angle + da
        points.append([radius * math.cos(a), radius * math.sin(a), z + dz])
    return points


def build_sample_scene() -> dict[str, Any]:
    """Construct the complete deterministic validation scene.

    No external geometry, textures or image data are used.
    """

    tank_angle0 = -150.0
    tank_angle1 = 150.0
    z0, z1 = 0.18, 1.70
    glass_specs = [
        ("glass-00", 0, 0.575, 0.040, 0.91, [0.62, 0.90, 1.0]),
        ("glass-01", 1, 0.785, 0.045, 0.88, [1.0, 0.82, 0.60]),
        ("glass-02", 2, 1.005, 0.050, 0.86, [0.55, 0.88, 1.0]),
    ]
    water_specs = [
        ("water-00", 0, 0.615, 0.170, [0.90, 0.22, 0.08], [0.06, 0.55, 0.72], 17041, 0.735),
        ("water-01", 1, 0.830, 0.175, [0.08, 0.22, 0.95], [0.75, 0.42, 0.12], 90210, 0.955),
    ]
    layers: list[dict[str, Any]] = []
    for component_id, layer_index, inner_radius, thickness, transmission, tint in glass_specs:
        layers.append(
            {
                "component_id": component_id,
                "kind": "glass",
                "layer_index": layer_index,
                "thickness": thickness,
                "transmission": transmission,
                "ior": 1.48 + 0.03 * layer_index,
                "tint": tint,
                "mesh": arc_shell_mesh(
                    inner_radius,
                    inner_radius + thickness,
                    z0,
                    z1,
                    tank_angle0,
                    tank_angle1,
                    28,
                    2,
                ),
            }
        )
    for component_id, layer_index, inner_radius, width, absorption, scattering, seed, projection_radius in water_specs:
        layers.append(
            {
                "component_id": component_id,
                "kind": "water",
                "layer_index": layer_index,
                "tint": [1.0, 1.0, 1.0],
                "absorption_coefficient": absorption,
                "scattering_color": scattering,
                "particle_seed": seed,
                "particle_resolution": [36, 14],
                "particle_strength": 0.34,
                "projection_radius": projection_radius,
                "mesh": arc_shell_mesh(
                    inner_radius,
                    inner_radius + width,
                    z0,
                    z1,
                    tank_angle0,
                    tank_angle1,
                    28,
                    2,
                ),
            }
        )

    fish_specs = [
        ("fish-01", 0.150, -17.0, 0.56, 0.560, 0.070, [0.94, 0.35, 0.08], "water-00"),
        ("fish-02", 0.235, 7.0, 0.71, 0.430, 0.062, [0.15, 0.55, 0.92], "water-01"),
        ("fish-03", 0.105, 23.0, 0.89, 0.620, 0.072, [0.94, 0.72, 0.16], "water-01"),
        ("fish-04", 0.205, -3.0, 1.02, 0.470, 0.058, [0.72, 0.22, 0.78], "water-00"),
    ]
    fish = [
        {
            "fish_id": fish_id,
            "water_component_id": water_id,
            "color": color,
            "opacity": 0.94,
            "mesh": quad_mesh(_fish_quad(radius, angle, z, half_angle, half_height)),
        }
        for fish_id, radius, angle, z, half_angle, half_height, color, water_id in fish_specs
    ]
    lights = [
        {
            "light_id": "key-warm",
            "position": [1.45, -1.20, 2.45],
            "color": [1.0, 0.83, 0.62],
            "intensity": 2.1,
            "radius": 0.075,
        },
        {
            "light_id": "fill-cool",
            "position": [-1.38, 1.15, 2.30],
            "color": [0.58, 0.77, 1.0],
            "intensity": 1.7,
            "radius": 0.065,
        },
    ]
    receiver_mesh = annulus_mesh(0.12, 1.075, 0.185, 0.0, 360.0, 5, 72)
    receiver = {
        "component_id": "pebbles",
        "mesh": receiver_mesh,
        "plane_z": 0.185,
        "inner_radius": 0.12,
        "outer_radius": 1.075,
        "angle_start_deg": 0.0,
        "angle_end_deg": 360.0,
        "base_color": [0.32, 0.30, 0.27],
        "pebble_palette": [
            [0.54, 0.47, 0.37],
            [0.36, 0.39, 0.38],
            [0.48, 0.42, 0.34],
            [0.29, 0.34, 0.36],
            [0.60, 0.52, 0.40],
        ],
    }
    return {
        "schema_version": "1.0",
        "scene_id": "glass-aquarium-reference-v1",
        "units": "m",
        "layers": layers,
        "fish": fish,
        "lights": lights,
        "receiver": receiver,
        "camera": {
            "orbit_center": [0.0, 0.0, 0.0],
            "radius": 4.05,
            "eye_height": 1.18,
            "target": [0.0, 0.0, 0.82],
            "start_deg": -32.0,
            "end_deg": 32.0,
            "sample_count": 9,
            "fov_deg": 44.0,
        },
        "render": {
            "width": 128,
            "height": 96,
            "block_width": 32,
            "block_height": 24,
            "exposure": 1.35,
            "shadow_samples": 10,
            "shadow_map_resolution": 160,
            "shadow_strength": 0.68,
            "shadow_softness_base": 0.016,
            "shadow_softness_scale": 0.60,
            "background": [0.012, 0.026, 0.042],
            "ambient": 0.14,
            "particle_modulation_scale": 0.50,
            "reflection_strength": 0.065,
        },
    }


def write_scene(scene: dict[str, Any], path: str | Path) -> str:
    data = canonical_bytes(scene)
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(data)
    return sha256_bytes(data)

