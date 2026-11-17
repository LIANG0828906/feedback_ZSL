"""Scene JSON loading and validation.

Every validation failure raises :class:`ValidationError` carrying a
structured business code, the layer id (when applicable) and the JSON
field path, and maps to a non-zero process exit code.
"""

import json
import math
import os

import numpy as np

from .errors import SceneIOError, SceneParseError, ValidationError

_EPS = 1e-12


def load_scene(path):
    if not os.path.isfile(path):
        raise SceneIOError(f"scene file not found: {path}", field=path)
    try:
        with open(path, "r", encoding="utf-8") as fh:
            text = fh.read()
    except OSError as exc:
        raise SceneIOError(f"cannot read scene file: {exc}", field=path)
    try:
        scene = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SceneParseError(f"invalid JSON: {exc}", field=path)
    validate_scene(scene)
    return scene


def _fail(code, message, *, layer_id=None, field=None):
    raise ValidationError(message, code=code, layer_id=layer_id, field=field)


def _require(cond, code, message, *, layer_id=None, field=None):
    if not cond:
        _fail(code, message, layer_id=layer_id, field=field)


def _is_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _check_vec(value, field, length=3, lo=None, hi=None, layer_id=None):
    _require(isinstance(value, (list, tuple)) and len(value) == length,
             "FIELD_TYPE", f"expected vector of {length} numbers", layer_id=layer_id, field=field)
    for idx, item in enumerate(value):
        _require(_is_number(item), "FIELD_TYPE", "expected finite number",
                 layer_id=layer_id, field=f"{field}[{idx}]")
        if lo is not None and item < lo:
            _fail("FIELD_RANGE", f"value {item} < {lo}", layer_id=layer_id, field=f"{field}[{idx}]")
        if hi is not None and item > hi:
            _fail("FIELD_RANGE", f"value {item} > {hi}", layer_id=layer_id, field=f"{field}[{idx}]")


def validate_scene(scene):
    _require(isinstance(scene, dict), "FIELD_TYPE", "scene root must be an object", field="$")

    image = scene.get("image")
    _require(isinstance(image, dict), "FIELD_MISSING", "missing image block", field="image")
    for key in ("width", "height"):
        value = image.get(key)
        _require(isinstance(value, int) and value >= 8, "FIELD_RANGE",
                 f"image.{key} must be an integer >= 8", field=f"image.{key}")

    tile = scene.get("tile")
    _require(isinstance(tile, dict), "FIELD_MISSING", "missing tile block", field="tile")
    tile_size = tile.get("size")
    _require(isinstance(tile_size, int) and 8 <= tile_size <= 4096, "FIELD_RANGE",
             "tile.size must be an integer in [8, 4096]", field="tile.size")

    _validate_camera(scene.get("camera"))
    _validate_wall(scene.get("wall"))
    _validate_projection(scene.get("projection"))

    shading = scene.get("shading", {})
    _require(isinstance(shading, dict), "FIELD_TYPE", "shading must be an object", field="shading")

    layers = scene.get("layers")
    _require(isinstance(layers, list) and len(layers) >= 1, "FIELD_MISSING",
             "scene must declare at least one willow layer", field="layers")

    seen_layer_ids = set()
    for li, layer in enumerate(layers):
        _validate_layer(li, layer)
        lid = layer["layer_id"]
        _require(lid not in seen_layer_ids, "DUPLICATE_LAYER_ID",
                 f"duplicate layer_id {lid}", layer_id=lid, field=f"layers[{li}].layer_id")
        seen_layer_ids.add(lid)

    air_gaps = scene.get("air_gaps")
    _require(isinstance(air_gaps, list) and len(air_gaps) == len(layers) - 1,
             "AIR_GAP_INCONSISTENT",
             "air_gaps must contain exactly one positive gap between each pair of layers",
             field="air_gaps")
    ordered = sorted(layers, key=lambda item: item["plane_z"])
    for gi, gap in enumerate(air_gaps):
        _require(_is_number(gap) and gap > 0.0, "AIR_GAP_INCONSISTENT",
                 "air gap must be a positive number", field=f"air_gaps[{gi}]")
        actual = ordered[gi + 1]["plane_z"] - ordered[gi]["plane_z"]
        _require(abs(actual - gap) <= 1e-6, "AIR_GAP_INCONSISTENT",
                 f"declared air gap {gap} does not match plane_z spacing {actual}",
                 layer_id=ordered[gi + 1]["layer_id"], field=f"air_gaps[{gi}]")

    lamps = scene.get("lamps")
    _require(isinstance(lamps, list) and len(lamps) >= 1, "FIELD_MISSING",
             "scene must declare at least one oil lamp", field="lamps")
    seen_lamp_ids = set()
    for li, lamp in enumerate(lamps):
        _validate_lamp(li, lamp, seen_lamp_ids)
        seen_lamp_ids.add(lamp["lamp_id"])
        _check_lamp_outside_layers(lamp, layers)


def _validate_camera(camera):
    _require(isinstance(camera, dict), "FIELD_MISSING", "missing camera block", field="camera")
    for key in ("position", "target", "up"):
        _check_vec(camera.get(key), f"camera.{key}")
    fov = camera.get("fov_deg")
    _require(_is_number(fov) and 1.0 < fov < 179.0, "CAMERA_INVALID",
             "camera.fov_deg must be in (1, 179)", field="camera.fov_deg")
    pos = np.asarray(camera["position"], dtype=np.float64)
    target = np.asarray(camera["target"], dtype=np.float64)
    up = np.asarray(camera["up"], dtype=np.float64)
    fwd = target - pos
    _require(np.linalg.norm(fwd) > _EPS, "CAMERA_INVALID",
             "camera position and target coincide", field="camera.position")
    _require(np.linalg.norm(up) > _EPS, "CAMERA_INVALID",
             "camera up vector is zero", field="camera.up")
    _require(np.linalg.norm(np.cross(fwd, up)) > 1e-9, "CAMERA_INVALID",
             "camera up vector parallel to view direction", field="camera.up")


def _validate_wall(wall):
    _require(isinstance(wall, dict), "FIELD_MISSING", "missing wall block", field="wall")
    _require(_is_number(wall.get("z")), "WALL_INVALID", "wall.z must be a number", field="wall.z")
    _require(_is_number(wall.get("extent")) and wall["extent"] > 0.0, "WALL_INVALID",
             "wall.extent must be positive", field="wall.extent")
    _check_vec(wall.get("albedo"), "wall.albedo", lo=0.0, hi=1.0)


def _validate_projection(proj):
    _require(isinstance(proj, dict), "FIELD_MISSING", "missing projection block", field="projection")
    density = proj.get("edge_sample_density")
    _require(isinstance(density, int) and 1 <= density <= 64, "FIELD_RANGE",
             "projection.edge_sample_density must be an integer in [1, 64]",
             field="projection.edge_sample_density")
    softness = proj.get("softness")
    _require(_is_number(softness) and softness > 0.0, "FIELD_RANGE",
             "projection.softness must be positive", field="projection.softness")
    res = proj.get("field_resolution")
    _require(isinstance(res, int) and 16 <= res <= 1024, "FIELD_RANGE",
             "projection.field_resolution must be an integer in [16, 1024]",
             field="projection.field_resolution")


def _validate_layer(index, layer):
    field = f"layers[{index}]"
    _require(isinstance(layer, dict), "FIELD_TYPE", "layer must be an object", field=field)
    lid = layer.get("layer_id")
    _require(isinstance(lid, int) and not isinstance(lid, bool) and 0 <= lid < 254,
             "FIELD_TYPE", "layer_id must be an integer in [0, 253]",
             field=f"{field}.layer_id")
    _require(isinstance(layer.get("seed"), int), "FIELD_TYPE",
             "layer seed must be an integer", layer_id=lid, field=f"{field}.seed")
    _require(_is_number(layer.get("plane_z")), "FIELD_TYPE",
             "plane_z must be a number", layer_id=lid, field=f"{field}.plane_z")
    _require(_is_number(layer.get("thickness")) and layer["thickness"] > 0.0,
             "FIELD_RANGE", "thickness must be positive", layer_id=lid,
             field=f"{field}.thickness")

    trans = layer.get("transmittance")
    _require(_is_number(trans) and 0.0 <= trans <= 1.0, "TRANSMITTANCE_OUT_OF_RANGE",
             f"transmittance {trans!r} outside [0, 1]", layer_id=lid,
             field=f"{field}.transmittance")
    _check_vec(layer.get("dye_color"), f"{field}.dye_color", lo=0.0, hi=1.0, layer_id=lid)

    panel = layer.get("panel")
    _require(isinstance(panel, dict), "FIELD_MISSING", "missing panel block",
             layer_id=lid, field=f"{field}.panel")
    for key in ("width", "height"):
        _require(_is_number(panel.get(key)) and panel[key] > 0.0, "FIELD_RANGE",
                 f"panel.{key} must be positive", layer_id=lid, field=f"{field}.panel.{key}")
    for key in ("cells_u", "cells_v"):
        _require(isinstance(panel.get(key), int) and panel[key] >= 2, "FIELD_RANGE",
                 f"panel.{key} must be an integer >= 2", layer_id=lid,
                 field=f"{field}.panel.{key}")

    holes = layer.get("holes", [])
    _require(isinstance(holes, list), "FIELD_TYPE", "holes must be a list",
             layer_id=lid, field=f"{field}.holes")
    seen_holes = set()
    seen_cells = set()
    for hi, hole in enumerate(holes):
        hfield = f"{field}.holes[{hi}]"
        hid = hole.get("hole_id")
        _require(isinstance(hid, int) and hid >= 0, "FIELD_TYPE",
                 "hole_id must be a non-negative integer", layer_id=lid, field=f"{hfield}.hole_id")
        _require(hid not in seen_holes, "DUPLICATE_HOLE_ID",
                 f"duplicate hole_id {hid}", layer_id=lid, field=f"{hfield}.hole_id")
        seen_holes.add(hid)
        cell = hole.get("cell")
        _require(isinstance(cell, list) and len(cell) == 2
                 and all(isinstance(c, int) for c in cell), "FIELD_TYPE",
                 "hole cell must be [cu, cv] integers", layer_id=lid, field=f"{hfield}.cell")
        cu, cv = cell
        _require(0 <= cu < panel["cells_u"] and 0 <= cv < panel["cells_v"],
                 "HOLE_CELL_OUT_OF_RANGE", f"hole cell {cell} outside weave grid",
                 layer_id=lid, field=f"{hfield}.cell")
        _require((cu, cv) not in seen_cells, "DUPLICATE_HOLE_ID",
                 f"two holes share cell {cell}", layer_id=lid, field=f"{hfield}.cell")
        seen_cells.add((cu, cv))

    mesh = layer.get("mesh")
    _require(isinstance(mesh, dict), "FIELD_MISSING", "missing mesh block",
             layer_id=lid, field=f"{field}.mesh")
    vertices = mesh.get("vertices")
    _require(isinstance(vertices, list) and len(vertices) >= 3, "FIELD_TYPE",
             "mesh.vertices must list at least 3 vertices", layer_id=lid,
             field=f"{field}.mesh.vertices")
    for vi, vertex in enumerate(vertices):
        _check_vec(vertex, f"{field}.mesh.vertices[{vi}]", layer_id=lid)
    triangles = mesh.get("triangles")
    _require(isinstance(triangles, list) and len(triangles) >= 1, "FIELD_TYPE",
             "mesh.triangles must list at least 1 triangle", layer_id=lid,
             field=f"{field}.mesh.triangles")
    tri_hole_ids = mesh.get("tri_hole_ids")
    _require(isinstance(tri_hole_ids, list) and len(tri_hole_ids) == len(triangles),
             "FIELD_TYPE", "tri_hole_ids must align with triangles", layer_id=lid,
             field=f"{field}.mesh.tri_hole_ids")

    n_vertices = len(vertices)
    verts = np.asarray(vertices, dtype=np.float64)
    for ti, tri in enumerate(triangles):
        tfield = f"{field}.mesh.triangles[{ti}]"
        _require(isinstance(tri, list) and len(tri) == 3
                 and all(isinstance(i, int) and not isinstance(i, bool) for i in tri),
                 "FIELD_TYPE", "triangle must be 3 integer indices", layer_id=lid, field=tfield)
        for idx in tri:
            _require(0 <= idx < n_vertices, "MESH_INDEX_OUT_OF_RANGE",
                     f"vertex index {idx} out of range [0, {n_vertices})",
                     layer_id=lid, field=tfield)
        p0, p1, p2 = verts[tri[0]], verts[tri[1]], verts[tri[2]]
        area2 = np.linalg.norm(np.cross(p1 - p0, p2 - p0))
        _require(area2 > 1e-10, "TRIANGLE_DEGENERATE",
                 f"degenerate triangle with doubled area {area2:.3e}",
                 layer_id=lid, field=tfield)
        hid = tri_hole_ids[ti]
        _require(isinstance(hid, int) and (hid == -1 or hid in seen_holes),
                 "HOLE_ID_UNKNOWN", f"tri_hole_ids[{ti}]={hid} not declared in holes",
                 layer_id=lid, field=f"{field}.mesh.tri_hole_ids[{ti}]")


def _validate_lamp(index, lamp, seen_lamp_ids):
    field = f"lamps[{index}]"
    _require(isinstance(lamp, dict), "FIELD_TYPE", "lamp must be an object", field=field)
    lamp_id = lamp.get("lamp_id")
    _require(isinstance(lamp_id, int) and lamp_id >= 0, "FIELD_TYPE",
             "lamp_id must be a non-negative integer", field=f"{field}.lamp_id")
    _require(lamp_id not in seen_lamp_ids, "DUPLICATE_LAMP_ID",
             f"duplicate lamp_id {lamp_id}", field=f"{field}.lamp_id")
    _check_vec(lamp.get("position"), f"{field}.position")
    _check_vec(lamp.get("intensity"), f"{field}.intensity", lo=0.0)


def _check_lamp_outside_layers(lamp, layers):
    pos = lamp["position"]
    for layer in layers:
        half_t = layer["thickness"] * 0.5
        panel = layer["panel"]
        inside = (abs(pos[2] - layer["plane_z"]) <= half_t
                  and abs(pos[0]) <= panel["width"] * 0.5
                  and abs(pos[1]) <= panel["height"] * 0.5)
        if inside:
            _fail("LAMP_INSIDE_GEOMETRY",
                  f"lamp {lamp['lamp_id']} at {pos} lies inside layer slab",
                  layer_id=layer["layer_id"], field=f"lamps[{lamp['lamp_id']}].position")


def scene_digest(scene):
    """Stable content digest of the scene, used by snapshot validation."""
    import hashlib

    canonical = json.dumps(scene, sort_keys=True, separators=(",", ":"), ensure_ascii=True)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()
