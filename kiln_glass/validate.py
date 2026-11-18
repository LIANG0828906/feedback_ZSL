"""Scene validation with numbered, business-coded error reports.

Validation runs before any rendering.  All violations are collected and
reported together; :func:`validate_scene` raises :class:`SceneValidationError`
carrying every issue, and the CLI exits non-zero without rendering.
"""

from __future__ import annotations

from dataclasses import dataclass
import numpy as np


class SceneValidationError(ValueError):
    def __init__(self, issues: list["ValidationIssue"]):
        self.issues = issues
        super().__init__(f"{len(issues)} scene validation issue(s)")


@dataclass(frozen=True)
class ValidationIssue:
    code: str
    object_kind: str
    object_id: str
    message: str

    def as_dict(self) -> dict:
        return {
            "code": self.code,
            "object_kind": self.object_kind,
            "object_id": self.object_id,
            "message": self.message,
        }


# Business error codes (stable contract).
ERR_INDEX_OUT_OF_RANGE = "E1001_MESH_INDEX_OUT_OF_RANGE"
ERR_DEGENERATE_TRIANGLE = "E1002_DEGENERATE_TRIANGLE"
ERR_THICKNESS_OUT_OF_RANGE = "E1003_THICKNESS_OUT_OF_RANGE"
ERR_TRANSMITTANCE_OUT_OF_RANGE = "E1004_TRANSMITTANCE_OUT_OF_RANGE"
ERR_BRICK_ID_DUPLICATE = "E1005_BRICK_ID_DUPLICATE_OR_MISSING"
ERR_LAYER_ID_INVALID = "E1006_LAYER_ID_DUPLICATE_OR_MISSING"
ERR_GROUT_NEGATIVE = "E1007_GROUT_WIDTH_NEGATIVE"
ERR_VIEW_NOT_STRICT = "E1008_VIEW_SEQUENCE_NOT_STRICTLY_INCREASING"
ERR_LIGHT_INSIDE_BRICK = "E1009_LIGHT_INSIDE_BRICK"
ERR_INVALID_PARAMETER = "E1010_INVALID_SCENE_PARAMETER"


def _require(issues: list[ValidationIssue], condition: bool, code: str,
             kind: str, obj_id: str, message: str) -> None:
    if not condition:
        issues.append(ValidationIssue(code, kind, obj_id, message))


def _point_in_box(point: np.ndarray, origin: np.ndarray, size: np.ndarray,
                  eps: float = 1e-9) -> bool:
    return bool(
        np.all(point >= origin - eps) and np.all(point <= origin + size + eps)
    )


def validate_scene(description: dict) -> list[ValidationIssue]:
    """Return all validation issues found in the JSON scene description."""
    issues: list[ValidationIssue] = []

    top_keys = {"scene_seed", "wall", "lights", "receiver_plane",
                "camera", "view_angles", "render_config"}
    _require(issues, top_keys.issubset(description.keys()),
             ERR_INVALID_PARAMETER, "scene", "-",
             f"missing top-level keys: {sorted(top_keys - set(description))}")
    if issues:
        return issues

    wall = description["wall"]
    bricks_raw = wall.get("bricks", [])
    layers_raw = wall.get("layers", [])

    # ---- brick id / layer bookkeeping -------------------------------------
    brick_ids = [int(b["brick_id"]) for b in bricks_raw]
    duplicate_ids = sorted({bid for bid in brick_ids if brick_ids.count(bid) > 1})
    for bid in duplicate_ids:
        _require(issues, False, ERR_BRICK_ID_DUPLICATE, "brick", str(bid),
                 "brick_id is not unique")
    layer_indices = sorted(int(layer["index"]) for layer in layers_raw)
    expected_layers = list(range(len(layers_raw)))
    _require(
        issues,
        layer_indices == expected_layers and len(set(layer_indices)) == len(layer_indices),
        ERR_LAYER_ID_INVALID, "layer", str(layer_indices),
        "layer indices must be unique consecutive integers starting at 0",
    )

    declared_layers = set(layer_indices)
    for brick in bricks_raw:
        bid = str(brick["brick_id"])
        _require(issues, int(brick["layer"]) in declared_layers,
                 ERR_LAYER_ID_INVALID, "brick", bid,
                 f"brick references unknown layer {brick['layer']}")
    for layer in layers_raw:
        lid = str(layer["index"])
        _require(issues, float(layer["grout_width"]) >= 0.0,
                 ERR_GROUT_NEGATIVE, "layer", lid,
                 f"grout_width={layer['grout_width']} must be >= 0")
        listed = [int(x) for x in layer["brick_ids"]]
        _require(issues, sorted(listed) == sorted(set(listed)),
                 ERR_BRICK_ID_DUPLICATE, "layer", lid,
                 "layer brick list contains duplicates")
        missing = [x for x in listed if x not in brick_ids]
        _require(issues, not missing, ERR_BRICK_ID_DUPLICATE, "layer", lid,
                 f"layer references missing brick ids {missing}")
    listed_all = [int(x) for layer in layers_raw for x in layer["brick_ids"]]
    _require(issues, sorted(listed_all) == sorted(brick_ids),
             ERR_BRICK_ID_DUPLICATE, "wall", "-",
             "layer brick listings must cover every brick exactly once")

    # ---- per-brick geometry / material checks ------------------------------
    for brick in bricks_raw:
        bid = str(brick["brick_id"])
        vertices = np.asarray(brick["vertices"], dtype=np.float64)
        triangles = np.asarray(brick["triangles"], dtype=np.int64)
        vertex_count = vertices.shape[0]
        _require(issues, vertices.ndim == 2 and vertices.shape[1] == 3,
                 ERR_INVALID_PARAMETER, "brick", bid,
                 "vertices must have shape (V, 3)")
        _require(issues, triangles.ndim == 2 and triangles.shape[1] == 3,
                 ERR_INVALID_PARAMETER, "brick", bid,
                 "triangles must have shape (T, 3)")
        out_of_range = triangles[(triangles < 0) | (triangles >= vertex_count)]
        _require(issues, out_of_range.size == 0,
                 ERR_INDEX_OUT_OF_RANGE, "brick", bid,
                 f"{out_of_range.size} triangle indices outside [0, {vertex_count - 1}]")
        if out_of_range.size == 0 and triangles.size:
            p0 = vertices[triangles[:, 0]]
            p1 = vertices[triangles[:, 1]]
            p2 = vertices[triangles[:, 2]]
            cross = np.cross(p1 - p0, p2 - p0)
            area = 0.5 * np.linalg.norm(cross, axis=1)
            degenerate = int(np.count_nonzero(area <= 1e-12))
            _require(issues, degenerate == 0,
                     ERR_DEGENERATE_TRIANGLE, "brick", bid,
                     f"{degenerate} degenerate triangle(s) with zero area")

        thickness = float(brick["thickness"])
        _require(issues, 0.0 < thickness <= 1.0,
                 ERR_THICKNESS_OUT_OF_RANGE, "brick", bid,
                 f"thickness={thickness} outside (0, 1.0]")
        transmittance = float(brick["transmittance"])
        _require(issues, 0.0 < transmittance <= 1.0,
                 ERR_TRANSMITTANCE_OUT_OF_RANGE, "brick", bid,
                 f"transmittance={transmittance} outside (0, 1.0]")
        _require(issues, int(brick["fringe_seed"]) >= 0,
                 ERR_INVALID_PARAMETER, "brick", bid,
                 "fringe_seed must be a non-negative integer")
        size = np.asarray(brick["size"], dtype=np.float64)
        _require(issues, np.all(size > 0.0),
                 ERR_INVALID_PARAMETER, "brick", bid,
                 "brick size components must be positive")

    # ---- lights -------------------------------------------------------------
    light_ids = []
    for light in description["lights"]:
        lid = str(light["light_id"])
        light_ids.append(int(light["light_id"]))
        _require(issues, float(light["intensity"]) > 0.0,
                 ERR_INVALID_PARAMETER, "light", lid,
                 f"intensity={light['intensity']} must be positive")
        point = np.asarray(light["position"], dtype=np.float64)
        for brick in bricks_raw:
            origin = np.asarray(brick["origin"], dtype=np.float64)
            size = np.asarray(brick["size"], dtype=np.float64)
            if _point_in_box(point, origin, size):
                _require(issues, False, ERR_LIGHT_INSIDE_BRICK, "light", lid,
                         f"light lies inside brick {brick['brick_id']}")
    _require(issues, len(set(light_ids)) == len(light_ids) and len(light_ids) >= 1,
             ERR_INVALID_PARAMETER, "lights", "-",
             "light ids must be unique and at least one light required")

    # ---- receiver / camera / view sequence ---------------------------------
    receiver = description["receiver_plane"]
    _require(
        issues,
        receiver["x_min"] < receiver["x_max"]
        and receiver["y_min"] < receiver["y_max"],
        ERR_INVALID_PARAMETER, "receiver_plane", "-",
        "receiver plane bounds must be ordered",
    )
    camera = description["camera"]
    position = np.asarray(camera["position"], dtype=np.float64)
    target = np.asarray(camera["target"], dtype=np.float64)
    up = np.asarray(camera["up"], dtype=np.float64)
    _require(issues, np.linalg.norm(target - position) > 1e-9,
             ERR_INVALID_PARAMETER, "camera", "-",
             "camera target must differ from position")
    _require(issues, np.linalg.norm(up) > 1e-9,
             ERR_INVALID_PARAMETER, "camera", "-", "up vector must be non-zero")
    for key in ("focal_length", "sensor_width", "sensor_height"):
        _require(issues, float(camera[key]) > 0.0,
                 ERR_INVALID_PARAMETER, "camera", "-", f"{key} must be positive")

    angles = [float(v) for v in description["view_angles"]]
    _require(issues, len(angles) >= 1,
             ERR_VIEW_NOT_STRICT, "view_angles", "-",
             "at least one view angle required")
    non_increasing = [
        (i, angles[i]) for i in range(1, len(angles)) if angles[i] <= angles[i - 1]
    ]
    _require(issues, not non_increasing,
             ERR_VIEW_NOT_STRICT, "view_angles", "-",
             f"non-increasing entries at {non_increasing}")

    cfg = description["render_config"]
    for key in ("image_width", "image_height", "tile_width", "tile_height",
                "sample_density", "bubble_count"):
        _require(issues, int(cfg[key]) > 0,
                 ERR_INVALID_PARAMETER, "render_config", key,
                         f"{key} must be a positive integer")
    _require(
        issues,
        cfg["tile_width"] <= cfg["image_width"]
        and cfg["tile_height"] <= cfg["image_height"],
        ERR_INVALID_PARAMETER, "render_config", "-",
        "tile size must not exceed image size",
    )
    rgb = cfg.get("background_rgb")
    _require(
        issues,
        rgb is not None and len(rgb) == 3 and all(0.0 <= float(c) <= 1.0 for c in rgb),
        ERR_INVALID_PARAMETER, "render_config", "background_rgb",
        "background_rgb must be three values in [0, 1]",
    )
    return issues


def assert_valid(description: dict) -> None:
    issues = validate_scene(description)
    if issues:
        raise SceneValidationError(issues)
