"""Scene JSON loading, validation and canonical hashing."""

from __future__ import annotations

import hashlib
import json

import numpy as np

from .errors import SceneError
from .geometry import point_in_grid_paper, triangle_area
from .texture import cutout_triangle_mask

REQUIRED_LAYER_KEYS = ("index", "mesh", "transmittance", "scatter_color",
                       "air_gap", "seed")


def load_scene(path: str) -> dict:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            scene = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise SceneError(1, f"scene JSON unreadable or invalid: {exc}") from exc
    validate_scene(scene)
    return scene


def canonical_scene_bytes(scene: dict) -> bytes:
    def strip(value):
        if isinstance(value, dict):
            return {key: strip(item) for key, item in value.items()
                    if not key.endswith("_array")}
        if isinstance(value, list):
            return [strip(item) for item in value]
        if isinstance(value, np.ndarray):
            return value.tolist()
        return value

    normalized = strip(scene)
    if isinstance(normalized, dict) and isinstance(normalized.get("layers"),
                                                   list):
        normalized["layers"] = sorted(
            normalized["layers"],
            key=lambda item: item.get("index", 0)
            if isinstance(item, dict) else 0)
    return json.dumps(normalized, sort_keys=True,
                      separators=(",", ":")).encode("utf-8")


def scene_digest(scene: dict) -> str:
    return hashlib.sha256(canonical_scene_bytes(scene)).hexdigest()


def _require(condition: bool, code: int, reason: str,
             layer: int | None = None) -> None:
    if not condition:
        raise SceneError(code, reason, layer)


def _as_vec3(value, code: int, what: str, layer: int | None = None) -> np.ndarray:
    if not (isinstance(value, list) and len(value) == 3
            and all(isinstance(v, (int, float)) for v in value)):
        raise SceneError(code, f"{what} must be a 3-component number list", layer)
    arr = np.asarray(value, dtype=np.float64)
    if not np.all(np.isfinite(arr)):
        raise SceneError(code, f"{what} contains non-finite numbers", layer)
    return arr


def _validate_mesh(layer: dict, index: int) -> dict:
    mesh = layer["mesh"]
    _require(isinstance(mesh, dict), 2, "mesh must be an object", index)

    if "grid" in mesh:
        meta = mesh["grid"]
        for key in ("nx", "ny", "xmin", "xmax", "ymin", "ymax", "z"):
            _require(key in meta, 2, f"grid mesh missing '{key}'", index)
        nx, ny = int(meta["nx"]), int(meta["ny"])
        _require(nx > 0 and ny > 0, 3, "grid dimensions must be positive", index)
        _require(meta["xmax"] > meta["xmin"] and meta["ymax"] > meta["ymin"],
                 3, "grid rectangle must have positive area", index)
        holes = mesh.get("holes")
        _require(isinstance(holes, list) and len(holes) == 2 * nx * ny,
                 2, "holes array length must equal triangle count", index)
        _require(all(isinstance(h, int) and h in (0, 1) for h in holes),
                 10, "holes entries must be 0/1 integers", index)
        mesh["holes_array"] = np.asarray(holes, dtype=bool)

        expected = cutout_triangle_mask(nx, ny, int(layer["seed"]),
                                        int(mesh.get("blob_count", 3)),
                                        float(mesh.get("hole_fraction", 0.18)),
                                        list(mesh.get("tunnel_cells", [])))
        if not np.array_equal(mesh["holes_array"], expected):
            raise SceneError(
                10,
                "stored cutout mask does not match deterministic mask "
                f"for seed {layer['seed']}", index)
        return mesh

    verts_raw = mesh.get("vertices")
    tris_raw = mesh.get("triangles")
    _require(isinstance(verts_raw, list) and len(verts_raw) >= 3,
             2, "mesh vertices missing or too short", index)
    _require(isinstance(tris_raw, list) and tris_raw,
             2, "mesh triangles missing", index)
    verts = np.asarray(verts_raw, dtype=np.float64)
    _require(verts.ndim == 2 and verts.shape[1] == 3
             and np.all(np.isfinite(verts)),
             2, "vertices must be finite (N, 3) arrays", index)
    tris = np.asarray(tris_raw, dtype=np.int64)
    _require(tris.ndim == 2 and tris.shape[1] == 3,
             2, "triangles must be (M, 3) index arrays", index)
    if np.any(tris < 0) or np.any(tris >= len(verts)):
        bad = int(np.argmax((tris < 0) | (tris >= len(verts))))
        row, col = np.unravel_index(bad, tris.shape)
        raise SceneError(2,
                         f"triangle {int(row)} references vertex "
                         f"{int(tris[row, col])} but only {len(verts)} exist",
                         index)
    for tri_idx, tri in enumerate(tris):
        if len(set(int(v) for v in tri)) < 3 or triangle_area(
                verts[tri[0]], verts[tri[1]], verts[tri[2]]) <= 1e-12:
            raise SceneError(3,
                             f"triangle {tri_idx} is degenerate or duplicate-indexed",
                             index)
    mesh["vertices_array"] = verts
    mesh["triangles_array"] = tris
    return mesh


def validate_scene(scene: dict) -> None:
    _require(isinstance(scene, dict), 1, "scene root must be an object")
    for key in ("layers", "camera", "light", "image", "render"):
        _require(key in scene, 1, f"scene missing '{key}'")

    layers = scene["layers"]
    _require(isinstance(layers, list) and len(layers) >= 2,
             5, "at least two layers are required")
    indices = []
    for layer in layers:
        for key in REQUIRED_LAYER_KEYS:
            _require(key in layer, 1, f"layer missing '{key}'",
                     layer.get("index"))
        index = int(layer["index"])
        indices.append(index)
        _require(isinstance(layer["transmittance"], (int, float))
                 and 0.0 < float(layer["transmittance"]) <= 1.0,
                 4,
                 f"transmittance {layer['transmittance']} outside (0, 1]",
                 index)
        color = layer["scatter_color"]
        _require(isinstance(color, list) and len(color) == 3
                 and all(isinstance(c, (int, float)) and 0.0 <= c <= 1.0
                         for c in color),
                 4, "scatter_color must be three values in [0, 1]", index)
        _require(isinstance(layer["air_gap"], (int, float))
                 and float(layer["air_gap"]) > 0.0,
                 9, "air_gap must be a positive number", index)
        _require(isinstance(layer["seed"], int) and layer["seed"] >= 0,
                 1, "layer seed must be a non-negative integer", index)
        _validate_mesh(layer, index)

    expected_indices = list(range(len(layers)))
    if sorted(indices) != expected_indices or len(set(indices)) != len(indices):
        raise SceneError(5,
                         f"layer indices {sorted(indices)} must be unique and "
                         f"form 0..{len(layers) - 1}")

    cam = scene["camera"]
    eye = _as_vec3(cam.get("position"), 7, "camera.position")
    target = _as_vec3(cam.get("target"), 7, "camera.target")
    up = _as_vec3(cam.get("up", [0.0, 1.0, 0.0]), 7, "camera.up")
    _require(np.linalg.norm(eye - target) > 1e-9, 7,
             "camera position must differ from target")
    _require(np.linalg.norm(up) > 1e-9, 7, "camera up must be non-zero")
    fov = cam.get("fov_y_deg")
    _require(isinstance(fov, (int, float)) and 0.0 < fov < 180.0,
             7, "fov_y_deg must lie in (0, 180)")

    light = scene["light"]
    light_pos = _as_vec3(light.get("position"), 8, "light.position")
    _require(isinstance(light.get("intensity"), (int, float))
             and float(light["intensity"]) > 0.0,
             8, "light.intensity must be positive")
    _require(isinstance(light.get("radius"), (int, float))
             and float(light["radius"]) >= 0.0,
             8, "light.radius must be non-negative")

    image = scene["image"]
    for key in ("width", "height"):
        _require(isinstance(image.get(key), int) and image[key] > 0,
                 9, f"image.{key} must be a positive integer")
    render = scene["render"]
    for key in ("tile_width", "tile_height", "sample_density"):
        _require(isinstance(render.get(key), int) and render[key] > 0,
                 9, f"render.{key} must be a positive integer")
    _require(render["tile_width"] <= image["width"]
             and render["tile_height"] <= image["height"],
             9, "tile size must not exceed image size")

    sorted_layers = sorted(layers, key=lambda item: item["index"])
    for layer in sorted_layers:
        mesh = layer["mesh"]
        if "grid" not in mesh:
            continue
        plane_z = float(mesh["grid"]["z"])
        if abs(light_pos[2] - plane_z) < 1e-9:
            on_plane = light_pos.copy()
            if point_in_grid_paper(on_plane, mesh):
                raise SceneError(6,
                                 "light lies on solid paper of this layer",
                                 int(layer["index"]))


def ordered_layers(scene: dict) -> list[dict]:
    """Layers sorted inner->outer by declared index (order independent)."""

    return sorted(scene["layers"], key=lambda item: int(item["index"]))
