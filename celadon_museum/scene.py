"""Scene loading, digesting, and deterministic business-rule validation."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .errors import ValidationError
from .math3d import as_vec3, normalize, quaternion_matrix, sha256_bytes


def canonical_json(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def scene_digest(document: dict[str, Any]) -> str:
    return sha256_bytes(canonical_json(document))


def _reject_json_constant(value: str) -> None:
    raise ValueError(f"invalid JSON constant {value}")


def _is_int(value: object) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _require(mapping: dict[str, Any], key: str, label: str, plate_id=None, layer_id=None):
    if key not in mapping:
        raise ValidationError(
            "E_MISSING_FIELD",
            f"{label} is missing required field {key!r}",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    return mapping[key]


def _finite_float(value: object, label: str, plate_id=None, layer_id=None) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValidationError(
            "E_INVALID_NUMBER",
            f"{label} must be a finite number",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    result = float(value)
    if not np.isfinite(result):
        raise ValidationError(
            "E_INVALID_NUMBER",
            f"{label} must be a finite number",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    return result


def _color3(value: object, label: str, plate_id=None, layer_id=None) -> np.ndarray:
    try:
        color = np.asarray(value, dtype=np.float64)
    except (TypeError, ValueError) as exc:
        raise ValidationError(
            "E_INVALID_COLOR",
            f"{label} must contain three finite numbers in [0, 1]",
            plate_id=plate_id,
            layer_id=layer_id,
        ) from exc
    if color.shape != (3,) or not np.all(np.isfinite(color)):
        raise ValidationError(
            "E_INVALID_COLOR",
            f"{label} must contain three finite numbers in [0, 1]",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    if np.any(color < 0.0) or np.any(color > 1.0):
        raise ValidationError(
            "E_INVALID_COLOR",
            f"{label} channels must be in [0, 1]",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    return color


@dataclass
class Layer:
    plate_id: int
    layer_id: int
    kind: str
    seed: int
    transmittance: float
    scatter_color: np.ndarray
    vertices: np.ndarray
    triangles: np.ndarray
    texture_resolution: tuple[int, int]
    carve_depth: float = 1.0


@dataclass
class Plate:
    plate_id: int
    center: np.ndarray
    orientation: np.ndarray
    rotation: np.ndarray
    radius: float = 0.0
    layers: list[Layer] = field(default_factory=list)
    projection_plane_z: float = 0.0


@dataclass
class Light:
    light_id: str
    position: np.ndarray
    intensity: float
    color: np.ndarray
    radius: float


@dataclass
class Camera:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    forward: np.ndarray
    right: np.ndarray
    true_up: np.ndarray
    vfov_deg: float
    near: float
    far: float


@dataclass
class Scene:
    document: dict[str, Any]
    digest: str
    width: int
    height: int
    tile_size: int
    camera: Camera
    lights: list[Light]
    plates: dict[int, Plate]
    layers: dict[tuple[int, int], Layer]
    render: dict[str, Any]
    background: np.ndarray
    source_name: str = "scene.json"

    @property
    def ordered_plates(self) -> list[Plate]:
        return [self.plates[key] for key in sorted(self.plates)]

    @property
    def ordered_layers(self) -> list[Layer]:
        return [self.layers[key] for key in sorted(self.layers)]

    @property
    def ordered_lights(self) -> list[Light]:
        return sorted(self.lights, key=lambda light: light.light_id)

    def tile_grid(self) -> tuple[int, int]:
        return (
            (self.width + self.tile_size - 1) // self.tile_size,
            (self.height + self.tile_size - 1) // self.tile_size,
        )


def load_scene_document(path: str | Path) -> dict[str, Any]:
    source = Path(path)
    try:
        raw = source.read_bytes()
    except OSError as exc:
        raise ValidationError("E_SCENE_READ", "scene file cannot be read") from exc
    try:
        document = json.loads(
            raw.decode("utf-8"),
            parse_constant=_reject_json_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        raise ValidationError("E_SCENE_JSON", "scene file is not valid UTF-8 JSON") from exc
    if not isinstance(document, dict):
        raise ValidationError("E_SCENE_SHAPE", "scene root must be a JSON object")
    return document


def _parse_plate(entry: dict[str, Any]) -> Plate:
    if not isinstance(entry, dict):
        raise ValidationError("E_PLATE_SHAPE", "each plate must be a JSON object")
    if "plate_id" not in entry:
        raise ValidationError("E_MISSING_PLATE_ID", "plate is missing plate_id")
    plate_id = entry["plate_id"]
    if not _is_int(plate_id) or plate_id < 0:
        raise ValidationError(
            "E_INVALID_PLATE_ID",
            "plate_id must be a non-negative integer",
            plate_id=plate_id if _is_int(plate_id) else None,
        )
    try:
        center = as_vec3(_require(entry, "center", "plate", plate_id=plate_id), "plate.center")
    except ValueError as exc:
        raise ValidationError(
            "E_INVALID_PLATE_GEOMETRY",
            str(exc),
            plate_id=plate_id,
        ) from exc
    try:
        quat = np.asarray(
            _require(entry, "orientation", "plate", plate_id=plate_id),
            dtype=np.float64,
        )
    except (TypeError, ValueError) as exc:
        raise ValidationError(
            "E_INVALID_ORIENTATION",
            "orientation must be a finite quaternion [w, x, y, z]",
            plate_id=plate_id,
        ) from exc
    if quat.shape != (4,) or not np.all(np.isfinite(quat)):
        raise ValidationError(
            "E_INVALID_ORIENTATION",
            "orientation must be a finite quaternion [w, x, y, z]",
            plate_id=plate_id,
        )
    norm = float(np.linalg.norm(quat))
    if norm <= 1.0e-10:
        raise ValidationError(
            "E_INVALID_ORIENTATION",
            "orientation quaternion must have non-zero length",
            plate_id=plate_id,
        )
    quat = quat / norm
    radius = 0.0
    if "radius" in entry:
        radius = _finite_float(entry["radius"], "plate.radius", plate_id=plate_id)
        if radius <= 0.0:
            raise ValidationError(
                "E_INVALID_PLATE_GEOMETRY",
                "plate.radius must be positive",
                plate_id=plate_id,
            )
    return Plate(
        plate_id=plate_id,
        center=center,
        orientation=quat,
        rotation=quaternion_matrix(quat),
        radius=radius,
    )


def _parse_mesh(
    entry: dict[str, Any], plate_id: int, layer_id: int
) -> tuple[np.ndarray, np.ndarray]:
    if "vertices" not in entry or "triangles" not in entry:
        raise ValidationError(
            "E_MESH_MISSING",
            "layer must contain vertices and triangles",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    try:
        vertices = np.asarray(entry["vertices"], dtype=np.float64)
    except (TypeError, ValueError) as exc:
        raise ValidationError(
            "E_MESH_VERTICES",
            "vertices must be a finite N x 3 array",
            plate_id=plate_id,
            layer_id=layer_id,
        ) from exc
    if vertices.ndim != 2 or vertices.shape[1] != 3 or vertices.shape[0] < 3:
        raise ValidationError(
            "E_MESH_VERTICES",
            "vertices must be a finite N x 3 array with at least three vertices",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    if not np.all(np.isfinite(vertices)):
        raise ValidationError(
            "E_MESH_VERTICES",
            "vertices contain a non-finite coordinate",
            plate_id=plate_id,
            layer_id=layer_id,
        )

    raw_triangles = entry["triangles"]
    if not isinstance(raw_triangles, list) or len(raw_triangles) == 0:
        raise ValidationError(
            "E_MESH_TRIANGLES",
            "triangles must be a non-empty list",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    triangles = np.empty((len(raw_triangles), 3), dtype=np.int64)
    for triangle_index, triangle in enumerate(raw_triangles):
        if not isinstance(triangle, list) or len(triangle) != 3:
            raise ValidationError(
                "E_MESH_TRIANGLE_SHAPE",
                "triangle must contain exactly three vertex indices",
                plate_id=plate_id,
                layer_id=layer_id,
                triangle_index=triangle_index,
            )
        for corner, index in enumerate(triangle):
            if not _is_int(index):
                raise ValidationError(
                    "E_MESH_INDEX_INVALID",
                    "triangle vertex index must be an integer",
                    plate_id=plate_id,
                    layer_id=layer_id,
                    triangle_index=triangle_index,
                    details={"corner": corner},
                )
            if index < 0 or index >= vertices.shape[0]:
                raise ValidationError(
                    "E_MESH_INDEX_OUT_OF_RANGE",
                    f"vertex index {index} is outside [0, {vertices.shape[0] - 1}]",
                    plate_id=plate_id,
                    layer_id=layer_id,
                    triangle_index=triangle_index,
                    details={"corner": corner, "vertex_index": index},
                )
            triangles[triangle_index, corner] = index
        a, b, c = triangles[triangle_index]
        cross = np.cross(vertices[b] - vertices[a], vertices[c] - vertices[a])
        if float(np.linalg.norm(cross)) <= 1.0e-12:
            raise ValidationError(
                "E_DEGENERATE_TRIANGLE",
                "triangle has zero area or repeated vertices",
                plate_id=plate_id,
                layer_id=layer_id,
                triangle_index=triangle_index,
            )
    return vertices, triangles


def _parse_layer(entry: dict[str, Any]) -> Layer:
    if not isinstance(entry, dict):
        raise ValidationError("E_LAYER_SHAPE", "each layer must be a JSON object")
    if "plate_id" not in entry:
        raise ValidationError("E_MISSING_PLATE_ID", "layer is missing plate_id")
    if "layer_id" not in entry:
        raise ValidationError(
            "E_MISSING_LAYER_ID",
            "layer is missing layer_id",
            plate_id=entry.get("plate_id") if _is_int(entry.get("plate_id")) else None,
        )
    plate_id = entry["plate_id"]
    layer_id = entry["layer_id"]
    if not _is_int(plate_id) or plate_id < 0:
        raise ValidationError(
            "E_INVALID_PLATE_ID",
            "plate_id must be a non-negative integer",
        )
    if not _is_int(layer_id) or layer_id < 0:
        raise ValidationError(
            "E_INVALID_LAYER_ID",
            "layer_id must be a non-negative integer",
            plate_id=plate_id,
        )
    kind = entry.get("kind")
    if kind not in ("body", "glaze"):
        raise ValidationError(
            "E_INVALID_LAYER_KIND",
            "layer.kind must be 'body' or 'glaze'",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    vertices, triangles = _parse_mesh(entry, plate_id, layer_id)
    if "transmittance" not in entry:
        raise ValidationError(
            "E_MISSING_TRANSMITTANCE",
            "layer is missing transmittance",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    transmittance = _finite_float(
        entry["transmittance"], "layer.transmittance", plate_id=plate_id, layer_id=layer_id
    )
    if transmittance < 0.0 or transmittance > 1.0:
        raise ValidationError(
            "E_TRANSMITTANCE_OUT_OF_RANGE",
            f"transmittance {transmittance:g} is outside [0, 1]",
            plate_id=plate_id,
            layer_id=layer_id,
            details={"transmittance": transmittance},
        )
    scatter = _color3(
        _require(entry, "scatter_color", "layer", plate_id=plate_id, layer_id=layer_id),
        "layer.scatter_color",
        plate_id=plate_id,
        layer_id=layer_id,
    )
    seed = _require(entry, "seed", "layer", plate_id=plate_id, layer_id=layer_id)
    if not _is_int(seed) or seed < 0:
        raise ValidationError(
            "E_INVALID_SEED",
            "layer.seed must be a non-negative integer",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    resolution = _require(
        entry,
        "texture_resolution",
        "layer",
        plate_id=plate_id,
        layer_id=layer_id,
    )
    if (
        not isinstance(resolution, list)
        or len(resolution) != 2
        or not all(_is_int(v) and 4 <= v <= 2048 for v in resolution)
    ):
        raise ValidationError(
            "E_INVALID_TEXTURE_RESOLUTION",
            "texture_resolution must contain two integers in [4, 2048]",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    carve_depth = _finite_float(
        entry.get("carve_depth", 1.0),
        "layer.carve_depth",
        plate_id=plate_id,
        layer_id=layer_id,
    )
    if carve_depth < 0.0:
        raise ValidationError(
            "E_INVALID_CARVE_DEPTH",
            "layer.carve_depth must be non-negative",
            plate_id=plate_id,
            layer_id=layer_id,
        )
    return Layer(
        plate_id=plate_id,
        layer_id=layer_id,
        kind=kind,
        seed=seed,
        transmittance=transmittance,
        scatter_color=scatter,
        vertices=vertices,
        triangles=triangles,
        texture_resolution=(int(resolution[0]), int(resolution[1])),
        carve_depth=carve_depth,
    )


def _parse_light(entry: dict[str, Any], seen: set[str]) -> Light:
    if not isinstance(entry, dict):
        raise ValidationError("E_LIGHT_SHAPE", "each light must be a JSON object")
    light_id = entry.get("id")
    if not (_is_int(light_id) or isinstance(light_id, str)) or str(light_id) == "":
        raise ValidationError("E_INVALID_LIGHT_ID", "light id must be a non-empty string or integer")
    light_key = str(light_id)
    if light_key in seen:
        raise ValidationError(
            "E_DUPLICATE_LIGHT_ID",
            f"light id {light_key!r} is duplicated",
            light_id=light_id,
        )
    seen.add(light_key)
    try:
        position = as_vec3(_require(entry, "position", "light"), "light.position")
    except ValueError as exc:
        raise ValidationError(
            "E_INVALID_LIGHT_GEOMETRY",
            str(exc),
            light_id=light_id,
        ) from exc
    intensity = _finite_float(_require(entry, "intensity", "light"), "light.intensity")
    if intensity <= 0.0:
        raise ValidationError(
            "E_INVALID_LIGHT_INTENSITY",
            "light.intensity must be positive",
            light_id=light_id,
        )
    color = _color3(
        _require(entry, "color", "light"),
        "light.color",
    )
    radius = _finite_float(entry.get("radius", 0.08), "light.radius")
    if radius <= 0.0:
        raise ValidationError(
            "E_INVALID_LIGHT_RADIUS",
            "light.radius must be positive",
            light_id=light_id,
        )
    return Light(
        light_id=light_key,
        position=position,
        intensity=intensity,
        color=color,
        radius=radius,
    )


def _parse_camera(entry: object) -> Camera:
    if not isinstance(entry, dict):
        raise ValidationError("E_CAMERA_SHAPE", "camera must be a JSON object")
    try:
        position = as_vec3(_require(entry, "position", "camera"), "camera.position")
        target = as_vec3(_require(entry, "target", "camera"), "camera.target")
        up = normalize(as_vec3(entry.get("up", [0.0, 0.0, 1.0]), "camera.up"), "camera.up")
    except ValueError as exc:
        raise ValidationError("E_INVALID_CAMERA", str(exc)) from exc
    forward = target - position
    if float(np.linalg.norm(forward)) <= 1.0e-10:
        raise ValidationError(
            "E_INVALID_CAMERA",
            "camera.target must differ from camera.position",
        )
    forward = normalize(forward, "camera.forward")
    if abs(float(np.dot(forward, up))) > 0.999:
        raise ValidationError(
            "E_INVALID_CAMERA",
            "camera.up must not be parallel to the view direction",
        )
    right = normalize(np.cross(forward, up), "camera.right")
    true_up = normalize(np.cross(right, forward), "camera.true_up")
    vfov = _finite_float(entry.get("vfov_deg", 42.0), "camera.vfov_deg")
    if vfov <= 1.0 or vfov >= 179.0:
        raise ValidationError("E_INVALID_CAMERA", "camera.vfov_deg must be in (1, 179)")
    near = _finite_float(entry.get("near", 0.01), "camera.near")
    far = _finite_float(entry.get("far", 100.0), "camera.far")
    if near <= 0.0 or far <= near:
        raise ValidationError(
            "E_INVALID_CAMERA",
            "camera near/far must satisfy 0 < near < far",
        )
    return Camera(
        position=position,
        target=target,
        up=up,
        forward=forward,
        right=right,
        true_up=true_up,
        vfov_deg=vfov,
        near=near,
        far=far,
    )


def _point_inside_mesh(point_local: np.ndarray, vertices: np.ndarray, triangles: np.ndarray) -> bool:
    """Ray parity test with merged edge hits for watertight meshes."""

    direction = np.array([0.731, 0.412, 0.543], dtype=np.float64)
    direction /= np.linalg.norm(direction)
    v0 = vertices[triangles[:, 0]]
    edge1 = vertices[triangles[:, 1]] - v0
    edge2 = vertices[triangles[:, 2]] - v0
    pvec = np.cross(direction, edge2)
    det = np.einsum("ij,ij->i", edge1, pvec)
    valid = np.abs(det) > 1.0e-12
    inv_det = np.zeros_like(det)
    inv_det[valid] = 1.0 / det[valid]
    tvec = point_local - v0
    u = np.einsum("ij,ij->i", tvec, pvec) * inv_det
    qvec = np.cross(tvec, edge1)
    v = np.einsum("j,ij->i", direction, qvec) * inv_det
    t = np.einsum("ij,ij->i", edge2, qvec) * inv_det
    hit = valid & (u >= -1.0e-9) & (u <= 1.0 + 1.0e-9) & (v >= -1.0e-9)
    hit &= (u + v <= 1.0 + 1.0e-9) & (t > 1.0e-9)
    hits = np.sort(t[hit])
    if hits.size == 0:
        return False
    clusters = 1
    for previous, current in zip(hits[:-1], hits[1:]):
        if abs(float(current - previous)) > 1.0e-7:
            clusters += 1
    return clusters % 2 == 1


def validate_scene(document: dict[str, Any], source_name: str = "scene.json") -> Scene:
    if document.get("schema") not in (None, "celadon-scene-v1"):
        raise ValidationError(
            "E_UNSUPPORTED_SCHEMA",
            "scene schema must be 'celadon-scene-v1'",
        )

    image = document.get("image")
    if not isinstance(image, dict):
        raise ValidationError("E_IMAGE_CONFIG", "image must be a JSON object")
    width = image.get("width")
    height = image.get("height")
    if not _is_int(width) or width <= 0 or not _is_int(height) or height <= 0:
        raise ValidationError(
            "E_IMAGE_CONFIG",
            "image.width and image.height must be positive integers",
        )
    if width * height > 16_000_000:
        raise ValidationError("E_IMAGE_CONFIG", "image dimensions are unreasonably large")

    render_entry = document.get("render", {})
    if render_entry is None:
        render_entry = {}
    if not isinstance(render_entry, dict):
        raise ValidationError("E_RENDER_CONFIG", "render must be a JSON object")
    tile_size = render_entry.get("tile_size", image.get("tile_size", 64))
    if not _is_int(tile_size) or tile_size <= 0:
        raise ValidationError("E_TILE_CONFIG", "tile_size must be a positive integer")
    soft_samples = render_entry.get(
        "soft_shadow_samples",
        render_entry.get("projection_sample_density", 12),
    )
    if not _is_int(soft_samples) or not 1 <= soft_samples <= 512:
        raise ValidationError(
            "E_PROJECTION_CONFIG",
            "soft_shadow_samples must be an integer in [1, 512]",
        )
    background = _color3(image.get("background", [0.012, 0.018, 0.024]), "image.background")
    ambient = _finite_float(render_entry.get("ambient", 0.30), "render.ambient")
    exposure = _finite_float(render_entry.get("exposure", 1.0), "render.exposure")
    max_display = _finite_float(render_entry.get("max_display", 0.94), "render.max_display")
    projection_strength = _finite_float(
        render_entry.get("projection_strength", 0.55), "render.projection_strength"
    )
    if ambient < 0.0 or exposure <= 0.0 or not 0.5 <= max_display <= 1.0:
        raise ValidationError(
            "E_RENDER_CONFIG",
            "ambient/exposure/max_display are outside supported ranges",
        )
    if projection_strength < 0.0:
        raise ValidationError("E_PROJECTION_CONFIG", "projection_strength must be non-negative")
    render = {
        "ambient": ambient,
        "exposure": exposure,
        "max_display": max_display,
        "projection_strength": projection_strength,
        "soft_shadow_samples": int(soft_samples),
        "projection_threshold": _finite_float(
            render_entry.get("projection_threshold", 0.57), "render.projection_threshold"
        ),
        "depth_scale": _finite_float(render_entry.get("depth_scale", 8.0), "render.depth_scale"),
        "tile_size": int(tile_size),
    }

    camera = _parse_camera(document.get("camera"))

    raw_plates = document.get("plates")
    if not isinstance(raw_plates, list) or len(raw_plates) == 0:
        raise ValidationError("E_PLATES", "plates must be a non-empty list")
    plates: dict[int, Plate] = {}
    for entry in raw_plates:
        plate = _parse_plate(entry)
        if plate.plate_id in plates:
            raise ValidationError(
                "E_DUPLICATE_PLATE_ID",
                f"plate_id {plate.plate_id} is duplicated",
                plate_id=plate.plate_id,
            )
        plates[plate.plate_id] = plate
    plate_ids = sorted(plates)
    expected_plate_ids = list(range(len(plate_ids)))
    if plate_ids != expected_plate_ids:
        missing_plate_id = next(
            index for index, value in enumerate(plate_ids) if value != index
        )
        raise ValidationError(
            "E_MISSING_PLATE_ID",
            f"plate_id sequence is missing {missing_plate_id}",
            plate_id=missing_plate_id,
        )

    raw_layers = document.get("layers")
    if not isinstance(raw_layers, list) or len(raw_layers) == 0:
        raise ValidationError("E_LAYERS", "layers must be a non-empty list")
    layers: dict[tuple[int, int], Layer] = {}
    for entry in raw_layers:
        layer = _parse_layer(entry)
        if layer.plate_id not in plates:
            raise ValidationError(
                "E_MISSING_PLATE",
                f"layer references missing plate_id {layer.plate_id}",
                plate_id=layer.plate_id,
                layer_id=layer.layer_id,
            )
        key = (layer.plate_id, layer.layer_id)
        if key in layers:
            raise ValidationError(
                "E_DUPLICATE_LAYER",
                f"layer ({layer.plate_id}, {layer.layer_id}) is duplicated",
                plate_id=layer.plate_id,
                layer_id=layer.layer_id,
            )
        layers[key] = layer

    for plate_id in sorted(plates):
        plate = plates[plate_id]
        plate_layers = [layers[key] for key in sorted(layers) if key[0] == plate_id]
        plate.layers = plate_layers
        layer_ids = {layer.layer_id for layer in plate_layers}
        missing = sorted({0, 1, 2} - layer_ids)
        if missing:
            raise ValidationError(
                "E_MISSING_LAYER",
                f"plate {plate_id} is missing layer_id {missing[0]}",
                plate_id=plate_id,
                layer_id=missing[0],
                details={"missing_layer_ids": missing},
            )
        extra = sorted(layer_ids - {0, 1, 2})
        if extra:
            raise ValidationError(
                "E_INVALID_LAYER_SET",
                "each plate must have exactly layer_id 0, 1, and 2",
                plate_id=plate_id,
                layer_id=extra[0],
            )
        body_count = sum(layer.kind == "body" for layer in plate_layers)
        glaze_count = sum(layer.kind == "glaze" for layer in plate_layers)
        if body_count != 1 or glaze_count != 2:
            raise ValidationError(
                "E_INVALID_LAYER_KIND_SET",
                "each plate requires exactly one body and two glaze layers",
                plate_id=plate_id,
                layer_id=0,
            )
        if plate.radius <= 0.0:
            radial = np.sqrt(
                np.sum(
                    np.stack([layer.vertices[:, :2] for layer in plate_layers], axis=0) ** 2,
                    axis=2,
                )
            )
            plate.radius = float(np.max(radial))
        if plate.radius <= 0.0:
            raise ValidationError(
                "E_INVALID_PLATE_GEOMETRY",
                "plate radius cannot be derived from its meshes",
                plate_id=plate_id,
            )
        max_z = max(
            float(np.max(layer.vertices[:, 2]))
            for layer in plate_layers
        )
        plate.projection_plane_z = max_z

    raw_lights = document.get("lights")
    if not isinstance(raw_lights, list) or len(raw_lights) == 0:
        raise ValidationError("E_LIGHTS", "lights must be a non-empty list")
    lights: list[Light] = []
    seen_lights: set[str] = set()
    for entry in raw_lights:
        lights.append(_parse_light(entry, seen_lights))

    for light in lights:
        for plate in plates.values():
            local = plate.rotation.T @ (light.position - plate.center)
            for layer in plate.layers:
                if _point_inside_mesh(local, layer.vertices, layer.triangles):
                    raise ValidationError(
                        "E_LIGHT_INSIDE_LAYER",
                        "light position lies inside layer geometry",
                        plate_id=plate.plate_id,
                        layer_id=layer.layer_id,
                        light_id=light.light_id,
                    )

    return Scene(
        document=document,
        digest=scene_digest(document),
        width=int(width),
        height=int(height),
        tile_size=int(tile_size),
        camera=camera,
        lights=lights,
        plates=plates,
        layers=layers,
        render=render,
        background=background,
        source_name=Path(source_name).name,
    )


def load_scene(path: str | Path) -> Scene:
    return validate_scene(load_scene_document(path), source_name=str(path))
