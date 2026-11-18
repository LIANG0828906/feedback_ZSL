"""Structured validation of the scene description.

Any failure produces a structured warning carrying a business error code and
the offending component identifier.  Callers stop before rendering when the
returned list is non-empty.
"""

from __future__ import annotations

import math
from typing import Any

import numpy as np

from .geometry import (
    EPS,
    normalize,
    point_plane_distance,
    triangle_area,
)


class SceneError(ValueError):
    def __init__(self, warnings: list[dict[str, Any]]):
        super().__init__(f"scene validation failed with {len(warnings)} issue(s)")
        self.warnings = warnings


def _warning(code: str, component: str, message: str,
             detail: dict[str, Any] | None = None) -> dict[str, Any]:
    record = {
        "error_code": code,
        "component_id": component,
        "message": message,
    }
    if detail:
        record["detail"] = detail
    return record


def _valid_number(value: Any) -> bool:
    return isinstance(value, (int, float, np.integer, np.floating)) and math.isfinite(float(value))


def _validate_vector(warnings: list[dict[str, Any]], component: str,
                     value: Any, length: int, code: str) -> bool:
    if (not isinstance(value, list) or len(value) != length
            or not all(_valid_number(item) for item in value)):
        warnings.append(_warning(code, component,
                                 f"expected {length} finite numeric components"))
        return False
    return True


def _validate_lights(scene: dict[str, Any],
                     warnings: list[dict[str, Any]]) -> None:
    lights = scene.get("lights", [])
    if len(lights) != 2:
        warnings.append(_warning("E-LIGHT-COUNT", "SCENE",
                                 "exactly two point lights are required",
                                 {"found": len(lights)}))
        return
    for light in lights:
        light_id = str(light.get("id", "UNKNOWN"))
        if not _validate_vector(warnings, light_id, light.get("position"), 3,
                                "E-LIGHT-POSITION"):
            continue
        intensity = light.get("intensity")
        if not _valid_number(intensity) or float(intensity) <= 0.0:
            warnings.append(_warning("E-LIGHT-INTENSITY", light_id,
                                     "light intensity must be positive"))
        if not _validate_vector(warnings, light_id, light.get("color"), 3,
                                "E-LIGHT-COLOR"):
            continue
        if any(float(channel) < 0.0 for channel in light["color"]):
            warnings.append(_warning("E-LIGHT-COLOR", light_id,
                                     "light color channels must be non-negative"))

        position = np.asarray(light["position"], dtype=np.float64)
        for layer in scene.get("layers", []):
            component = f"LIGHT:{light_id}/LAYER:{layer.get('layer_index')}"
            normal = normalize(np.asarray(layer["plane"]["normal"],
                                          dtype=np.float64))
            point = np.asarray(layer["plane"]["point"], dtype=np.float64)
            distance = point_plane_distance(position, normal, point)
            if distance < 1e-5:
                warnings.append(_warning(
                    "E-LIGHT-IN-LAYER-PLANE", component,
                    "point light lies in an embroidery layer plane",
                    {"distance": float(distance)},
                ))


def _validate_layer(layer: dict[str, Any], seen_indices: set[int],
                    seen_components: set[str],
                    warnings: list[dict[str, Any]]) -> None:
    layer_id = f"LAYER:{layer.get('layer_index', 'UNKNOWN')}"
    index = layer.get("layer_index")
    if not isinstance(index, int) or index < 0:
        warnings.append(_warning("E-LAYER-INDEX", layer_id,
                                 "layer index must be a non-negative integer"))
    elif index in seen_indices:
        warnings.append(_warning("E-LAYER-DUPLICATE", layer_id,
                                 "duplicate layer index"))
    else:
        seen_indices.add(index)

    component_ids = layer.get("component_ids", [])
    if not component_ids or len(set(component_ids)) != len(component_ids):
        warnings.append(_warning("E-COMPONENT-ID", layer_id,
                                 "component ids must be present and unique"))
    for component_id in component_ids:
        if component_id in seen_components:
            warnings.append(_warning("E-COMPONENT-DUPLICATE", str(component_id),
                                     "duplicate component id across scene"))
        else:
            seen_components.add(component_id)

    vertices = layer.get("vertices", [])
    triangles = layer.get("triangles", [])
    triangle_components = layer.get("triangle_components", [])
    vertex_count = len(vertices)
    if vertex_count < 3:
        warnings.append(_warning("E-MESH-EMPTY", layer_id,
                                 "mesh has fewer than three vertices"))
        return
    if len(triangle_components) != len(triangles):
        warnings.append(_warning("E-COMPONENT-MISSING", layer_id,
                                 "each triangle needs one component id",
                                 {"triangles": len(triangles),
                                  "components": len(triangle_components)}))
    valid_components = set(component_ids)
    vertex_array = np.asarray(vertices, dtype=np.float64)
    if vertex_array.shape != (vertex_count, 3) or not np.isfinite(vertex_array).all():
        warnings.append(_warning("E-MESH-VERTEX", layer_id,
                                 "vertices must be finite xyz triples"))

    for triangle_id, triangle in enumerate(triangles):
        component = (triangle_components[triangle_id]
                     if triangle_id < len(triangle_components) else layer_id)
        if len(triangle) != 3:
            warnings.append(_warning("E-MESH-TRIANGLE", str(component),
                                     "triangle must contain three indices",
                                     {"triangle": triangle_id}))
            continue
        if any((not isinstance(idx, int)) or idx < 0 or idx >= vertex_count
               for idx in triangle):
            warnings.append(_warning("E-MESH-INDEX-OUT-OF-BOUNDS", str(component),
                                     "triangle index out of bounds",
                                     {"triangle": triangle_id, "indices": triangle,
                                      "vertex_count": vertex_count}))
            continue
        if component not in valid_components:
            warnings.append(_warning("E-COMPONENT-MISSING", str(component),
                                     "triangle references undeclared component",
                                     {"triangle": triangle_id}))
        area = triangle_area(vertex_array[triangle[0]],
                             vertex_array[triangle[1]],
                             vertex_array[triangle[2]])
        if area < EPS:
            warnings.append(_warning("E-MESH-DEGENERATE", str(component),
                                     "degenerate triangle",
                                     {"triangle": triangle_id,
                                      "area": float(area)}))

    material = layer.get("material", {})
    kind = layer.get("kind")
    if kind == "satin":
        reflectivity = material.get("anisotropic_reflectivity")
        roughness = material.get("roughness")
        for code, name, value in (
                ("E-MATERIAL-REFLECTIVITY",
                 "anisotropic_reflectivity", reflectivity),
                ("E-MATERIAL-ROUGHNESS", "roughness", roughness)):
            if not _valid_number(value) or not 0.0 < float(value) < 1.0:
                warnings.append(_warning(code, layer_id,
                                         f"{name} must lie in open range (0, 1)",
                                         {"value": value}))
        if not isinstance(material.get("embroidery_seed"), int):
            warnings.append(_warning("E-SEED-TYPE", layer_id,
                                     "embroidery seed must be an integer"))
    elif kind == "kesi":
        transmittance = material.get("transmittance")
        if not _valid_number(transmittance) or not 0.0 < float(transmittance) < 1.0:
            warnings.append(_warning("E-MATERIAL-TRANSMITTANCE", layer_id,
                                     "transmittance must lie in open range (0, 1)",
                                     {"value": transmittance}))
        if not isinstance(material.get("hole_seed"), int):
            warnings.append(_warning("E-SEED-TYPE", layer_id,
                                     "hole seed must be an integer"))
        hole_rate = material.get("hole_rate")
        if not _valid_number(hole_rate) or not 0.0 < float(hole_rate) < 1.0:
            warnings.append(_warning("E-MATERIAL-HOLE-RATE", layer_id,
                                     "hole rate must lie in open range (0, 1)"))
    else:
        warnings.append(_warning("E-LAYER-KIND", layer_id,
                                 "layer kind must be satin or kesi"))


def _validate_frame_and_floor(scene: dict[str, Any],
                              warnings: list[dict[str, Any]]) -> None:
    frame = scene.get("frame", {})
    for key in ("width", "height", "tile_width", "tile_height",
                "penumbra_samples"):
        value = frame.get(key)
        if not isinstance(value, int) or value <= 0:
            warnings.append(_warning("E-FRAME-PARAM", "FRAME",
                                     f"{key} must be a positive integer",
                                     {"value": value}))
    if (isinstance(frame.get("width"), int)
            and isinstance(frame.get("tile_width"), int)
            and frame["tile_width"] > frame["width"]):
        warnings.append(_warning("E-FRAME-TILE", "FRAME",
                                 "tile width exceeds frame width"))
    if (isinstance(frame.get("height"), int)
            and isinstance(frame.get("tile_height"), int)
            and frame["tile_height"] > frame["height"]):
        warnings.append(_warning("E-FRAME-TILE", "FRAME",
                                 "tile height exceeds frame height"))
    fov = frame.get("fov_y_deg")
    if not _valid_number(fov) or not 0.0 < float(fov) < 180.0:
        warnings.append(_warning("E-FRAME-FOV", "FRAME",
                                 "fov_y_deg must be between 0 and 180"))

    floor = scene.get("floor")
    if not isinstance(floor, dict):
        warnings.append(_warning("E-FLOOR-MISSING", "FLOOR",
                                 "stage floor declaration is required"))
    else:
        if not _validate_vector(warnings, "FLOOR", floor.get("normal"), 3,
                                "E-FLOOR-PLANE"):
            return
        if np.linalg.norm(np.asarray(floor["normal"])) < EPS:
            warnings.append(_warning("E-FLOOR-PLANE", "FLOOR",
                                     "floor normal cannot be zero length"))
        if not _validate_vector(warnings, "FLOOR", floor.get("point"), 3,
                                "E-FLOOR-PLANE"):
            return
        for key in ("extent_x", "extent_z"):
            if not _valid_number(floor.get(key)) or float(floor[key]) <= 0:
                warnings.append(_warning("E-FLOOR-EXTENT", "FLOOR",
                                         f"{key} must be positive"))


def validate_scene(scene: dict[str, Any]) -> list[dict[str, Any]]:
    """Validate all business constraints; return a list of structured issues."""
    warnings: list[dict[str, Any]] = []
    if not isinstance(scene, dict):
        return [_warning("E-SCENE-TYPE", "SCENE", "scene must be an object")]
    _validate_frame_and_floor(scene, warnings)
    layers = scene.get("layers", [])
    if not layers:
        warnings.append(_warning("E-LAYER-MISSING", "SCENE",
                                 "at least one layer is required"))
    seen_indices: set[int] = set()
    seen_components: set[str] = set()
    expected_indices = set(range(len(layers)))
    for layer in layers:
        _validate_layer(layer, seen_indices, seen_components, warnings)
    actual_indices = {
        layer.get("layer_index") for layer in layers
        if isinstance(layer.get("layer_index"), int)
    }
    missing = sorted(expected_indices - actual_indices)
    if missing:
        warnings.append(_warning("E-LAYER-INDEX-MISSING", "SCENE",
                                 "non-contiguous or missing layer indices",
                                 {"missing": missing}))
    _validate_lights(scene, warnings)

    camera = scene.get("camera", {})
    if not _validate_vector(warnings, "CAMERA", camera.get("target"), 3,
                            "E-CAMERA-PARAM"):
        return warnings
    if not _valid_number(camera.get("radius")) or float(camera["radius"]) <= 0:
        warnings.append(_warning("E-CAMERA-PARAM", "CAMERA",
                                 "orbit radius must be positive"))
    orbit = camera.get("orbit_azimuth_deg", [])
    if not orbit or not all(_valid_number(angle) for angle in orbit):
        warnings.append(_warning("E-CAMERA-ORBIT", "CAMERA",
                                 "orbit path needs at least one finite angle"))
    return warnings


def ensure_valid_scene(scene: dict[str, Any]) -> None:
    warnings = validate_scene(scene)
    if warnings:
        raise SceneError(warnings)
