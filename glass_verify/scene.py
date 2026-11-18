from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import SceneIssue, SceneValidationError

SCHEMA_VERSION = 1
EPS = 1.0e-9


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(float(value))


def _vec3(value: Any) -> np.ndarray | None:
    if not isinstance(value, list) or len(value) != 3 or not all(_is_number(v) for v in value):
        return None
    return np.asarray(value, dtype=np.float64)


def _unit(value: np.ndarray) -> np.ndarray:
    length = float(np.linalg.norm(value))
    if length <= EPS:
        return np.zeros(3, dtype=np.float64)
    return value / length


def _canonical_json(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def scene_digest(raw: dict[str, Any]) -> str:
    return "sha256:" + hashlib.sha256(_canonical_json(raw)).hexdigest()


def _issue(
    issues: list[SceneIssue],
    code: str,
    reason: str,
    *,
    sector_id: int | None = None,
    pane_id: int | None = None,
    path: str = "$",
    value: Any = None,
) -> None:
    issues.append(
        SceneIssue(
            code=code,
            reason=reason,
            sector_id=sector_id,
            pane_id=pane_id,
            path=path,
            value=value,
        )
    )


@dataclass(frozen=True)
class Camera:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    fov_y_deg: float
    width: int
    height: int
    near: float
    far: float


@dataclass(frozen=True)
class Window:
    origin: np.ndarray
    normal: np.ndarray
    axis_u: np.ndarray
    axis_v: np.ndarray


@dataclass(frozen=True)
class Light:
    direction: np.ndarray
    irradiance: np.ndarray
    bands_nm: tuple[float, float, float]
    angular_radius_deg: float


@dataclass(frozen=True)
class TextureConfig:
    seed: int
    table_size: int
    transmission_strength: float
    reflection_strength: float
    octaves: int


@dataclass(frozen=True)
class Pane:
    sector_id: int
    pane_id: int
    vertices: np.ndarray
    triangles: np.ndarray
    transmission_color: np.ndarray
    transmittance: float
    glaze_reflectance: float
    lead_width: float
    lead_depth: float
    normal: np.ndarray
    uv_min: np.ndarray
    uv_max: np.ndarray
    boundary_edges: tuple[tuple[int, int], ...]


@dataclass(frozen=True)
class Wall:
    wall_id: str
    point: np.ndarray
    normal: np.ndarray
    axis_u: np.ndarray
    axis_v: np.ndarray
    extent_u: float
    extent_v: float
    albedo: np.ndarray
    receives_shadow: bool


@dataclass(frozen=True)
class RenderConfig:
    chunk_size: tuple[int, int]
    supersample: int
    exposure: float
    background: np.ndarray
    ambient_light: np.ndarray
    reflection_tint: np.ndarray
    composite_order: str
    grazing_reflectance_cap: float
    output_rounding: str


@dataclass(frozen=True)
class ProjectionConfig:
    wall_index: int
    samples_per_axis: int
    penumbra_per_depth: float
    profile_points: int
    depth_levels: int


@dataclass(frozen=True)
class Scene:
    raw: dict[str, Any]
    digest: str
    camera: Camera
    window: Window
    light: Light
    texture: TextureConfig
    panes: tuple[Pane, ...]
    walls: tuple[Wall, ...]
    render: RenderConfig
    projection: ProjectionConfig
    sector_ids: tuple[int, ...]

    @property
    def pane_by_key(self) -> dict[tuple[int, int], Pane]:
        return {(pane.sector_id, pane.pane_id): pane for pane in self.panes}


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    duplicates: list[str] = []
    for key, value in pairs:
        if key in result:
            duplicates.append(key)
        result[key] = value
    if duplicates:
        raise ValueError("duplicate JSON object keys: " + ", ".join(sorted(set(duplicates))))
    return result


def load_scene(path: str | Path) -> Scene:
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise SceneValidationError(
            [SceneIssue("scene_io", f"cannot read scene JSON: {exc.strerror or exc}", path="$")]
        ) from exc
    try:
        raw = json.loads(
            text,
            object_pairs_hook=_reject_duplicate_keys,
            parse_constant=lambda value: (_ for _ in ()).throw(ValueError(f"invalid JSON constant {value}")),
        )
    except (json.JSONDecodeError, ValueError) as exc:
        raise SceneValidationError(
            [SceneIssue("json_parse", f"invalid scene JSON: {exc}", path="$")]
        ) from exc
    return validate_scene(raw)


def validate_scene(raw: Any) -> Scene:
    issues: list[SceneIssue] = []
    if not isinstance(raw, dict):
        raise SceneValidationError(
            [SceneIssue("scene_type", "scene root must be a JSON object", path="$")]
        )

    if raw.get("schema_version") != SCHEMA_VERSION:
        _issue(
            issues,
            "schema_version",
            f"schema_version must be {SCHEMA_VERSION}",
            path="$.schema_version",
            value=raw.get("schema_version"),
        )

    window_data = raw.get("window")
    if not isinstance(window_data, dict):
        _issue(issues, "window_missing", "window object is required", path="$.window")
        window = None
    else:
        origin = _vec3(window_data.get("origin"))
        normal = _vec3(window_data.get("normal"))
        axis_u = _vec3(window_data.get("axis_u"))
        axis_v = _vec3(window_data.get("axis_v"))
        if origin is None:
            _issue(issues, "window_origin", "window.origin must be a finite 3-vector", path="$.window.origin")
        if normal is None or np.linalg.norm(normal) <= EPS:
            _issue(issues, "window_normal", "window.normal must be a nonzero finite 3-vector", path="$.window.normal")
            normal = None
        else:
            normal = _unit(normal)
        if axis_u is None or np.linalg.norm(axis_u) <= EPS:
            _issue(issues, "window_axis_u", "window.axis_u must be a nonzero finite 3-vector", path="$.window.axis_u")
            axis_u = None
        else:
            axis_u = _unit(axis_u)
        if axis_v is None or np.linalg.norm(axis_v) <= EPS:
            _issue(issues, "window_axis_v", "window.axis_v must be a nonzero finite 3-vector", path="$.window.axis_v")
            axis_v = None
        else:
            axis_v = _unit(axis_v)
        if normal is not None and axis_u is not None and abs(float(np.dot(normal, axis_u))) > 1.0e-6:
            _issue(issues, "window_axis_orthogonality", "window.axis_u must be perpendicular to window.normal", path="$.window.axis_u")
        if normal is not None and axis_v is not None and abs(float(np.dot(normal, axis_v))) > 1.0e-6:
            _issue(issues, "window_axis_orthogonality", "window.axis_v must be perpendicular to window.normal", path="$.window.axis_v")
        if axis_u is not None and axis_v is not None and abs(float(np.dot(axis_u, axis_v))) > 1.0e-6:
            _issue(issues, "window_axis_orthogonality", "window.axis_u and window.axis_v must be perpendicular", path="$.window.axis_v")
        window = Window(origin, normal, axis_u, axis_v) if all(
            value is not None for value in (origin, normal, axis_u, axis_v)
        ) else None

    pane_entries = raw.get("panes")
    pane_records: list[dict[str, Any]] = []
    if not isinstance(pane_entries, list) or not pane_entries:
        _issue(issues, "panes_missing", "panes must be a non-empty array", path="$.panes")
        pane_entries = []
    if len(pane_entries) > 65533:
        _issue(issues, "too_many_panes", "at most 65533 panes are supported by the uint16 attribution map", path="$.panes")

    sector_ids_seen: dict[int, int] = {}
    pane_ids_by_sector: dict[int, dict[int, int]] = {}
    for index, entry in enumerate(pane_entries):
        path = f"$.panes[{index}]"
        if not isinstance(entry, dict):
            _issue(issues, "pane_type", "pane must be an object", path=path)
            continue
        sector_id = entry.get("sector_id")
        pane_id = entry.get("pane_id")
        if not isinstance(sector_id, int) or isinstance(sector_id, bool) or sector_id < 0:
            _issue(issues, "sector_index", "sector_id must be a non-negative integer", path=path + ".sector_id", value=sector_id)
            sector_id = None
        if not isinstance(pane_id, int) or isinstance(pane_id, bool) or pane_id < 0:
            _issue(issues, "pane_index", "pane_id must be a non-negative integer", path=path + ".pane_id", value=pane_id)
            pane_id = None
        if sector_id is not None:
            sector_ids_seen[sector_id] = sector_ids_seen.get(sector_id, 0) + 1
            if pane_id is not None:
                pane_ids_by_sector.setdefault(sector_id, {})
                pane_ids_by_sector[sector_id][pane_id] = pane_ids_by_sector[sector_id].get(pane_id, 0) + 1
        pane_records.append(
            {
                "entry": entry,
                "path": path,
                "sector_id": sector_id,
                "pane_id": pane_id,
                "index": index,
            }
        )

    valid_sector_ids = sorted(sector_ids_seen)
    declared_sectors = raw.get("sectors")
    if declared_sectors is not None:
        if not isinstance(declared_sectors, list) or not declared_sectors:
            _issue(
                issues,
                "sector_declaration",
                "sectors must be a non-empty array when present",
                path="$.sectors",
            )
        else:
            declared_ids: list[int] = []
            for index, declaration in enumerate(declared_sectors):
                if isinstance(declaration, dict):
                    declared_id = declaration.get("sector_id")
                else:
                    declared_id = declaration
                if not isinstance(declared_id, int) or isinstance(declared_id, bool) or declared_id < 0:
                    _issue(
                        issues,
                        "sector_index",
                        "declared sector_id must be a non-negative integer",
                        sector_id=declared_id if isinstance(declared_id, int) else None,
                        path=f"$.sectors[{index}]",
                        value=declared_id,
                    )
                    continue
                declared_ids.append(declared_id)
            seen_declared: set[int] = set()
            for sector_id in declared_ids:
                if sector_id in seen_declared:
                    _issue(
                        issues,
                        "sector_duplicate",
                        f"sector_id {sector_id} is declared more than once",
                        sector_id=sector_id,
                        path="$.sectors",
                    )
                seen_declared.add(sector_id)
            for sector_id in sorted(seen_declared):
                if sector_id not in sector_ids_seen:
                    _issue(
                        issues,
                        "sector_missing",
                        f"declared sector_id {sector_id} has no panes",
                        sector_id=sector_id,
                        path="$.sectors",
                    )
            for sector_id in valid_sector_ids:
                if sector_id not in seen_declared:
                    _issue(
                        issues,
                        "sector_missing",
                        f"sector_id {sector_id} is not declared in sectors",
                        sector_id=sector_id,
                        path="$.sectors",
                    )
    if valid_sector_ids:
        expected = list(range(valid_sector_ids[-1] + 1))
        for sector_id in expected:
            count = sector_ids_seen.get(sector_id, 0)
            if count == 0:
                _issue(
                    issues,
                    "sector_missing",
                    f"sector_id {sector_id} is missing; sector ids must be contiguous from 0",
                    sector_id=sector_id,
                    path="$.panes",
                )
            elif count > 1:
                # A repeated sector id is valid when it contains distinct panes. This
                # branch is retained only for clarity in the sector index accounting.
                pass
        for sector_id, count in sector_ids_seen.items():
            pane_counts = pane_ids_by_sector.get(sector_id, {})
            if pane_counts:
                pane_max = max(pane_counts)
                for pane_id in range(pane_max + 1):
                    occurrences = pane_counts.get(pane_id, 0)
                    if occurrences == 0:
                        _issue(
                            issues,
                            "pane_missing",
                            f"pane_id {pane_id} is missing in sector {sector_id}",
                            sector_id=sector_id,
                            path="$.panes",
                        )
                    elif occurrences > 1:
                        _issue(
                            issues,
                            "pane_duplicate",
                            f"pane_id {pane_id} is duplicated in sector {sector_id}",
                            sector_id=sector_id,
                            pane_id=pane_id,
                            path="$.panes",
                        )

    parsed_panes: list[Pane] = []
    for record in pane_records:
        entry = record["entry"]
        path = record["path"]
        sector_id = record["sector_id"]
        pane_id = record["pane_id"]
        start_issue_count = len(issues)
        vertices: np.ndarray | None = None
        triangles: np.ndarray | None = None
        normal: np.ndarray | None = None
        uv_min = np.zeros(2, dtype=np.float64)
        uv_max = np.ones(2, dtype=np.float64)
        boundary: tuple[tuple[int, int], ...] = ()

        vertices_data = entry.get("vertices")
        if not isinstance(vertices_data, list) or len(vertices_data) < 3:
            _issue(issues, "vertices", "vertices must contain at least three 3-vectors", sector_id=sector_id, pane_id=pane_id, path=path + ".vertices")
        else:
            parsed_vertices = [_vec3(item) for item in vertices_data]
            if any(item is None for item in parsed_vertices):
                _issue(issues, "vertex_value", "every vertex must be a finite 3-vector", sector_id=sector_id, pane_id=pane_id, path=path + ".vertices")
            else:
                vertices = np.asarray(parsed_vertices, dtype=np.float64)
                if window is not None:
                    distances = np.abs((vertices - window.origin) @ window.normal)
                    if float(np.max(distances)) > 0.25:
                        _issue(
                            issues,
                            "pane_plane_offset",
                            "pane vertices are more than 0.25 world units from the declared window plane",
                            sector_id=sector_id,
                            pane_id=pane_id,
                            path=path + ".vertices",
                        )
                    uv = np.column_stack(
                        ((vertices - window.origin) @ window.axis_u, (vertices - window.origin) @ window.axis_v)
                    )
                    uv_min = np.min(uv, axis=0)
                    uv_max = np.max(uv, axis=0)
                    if np.any((uv_max - uv_min) <= EPS):
                        _issue(
                            issues,
                            "pane_bbox_degenerate",
                            "pane has a degenerate projection onto the window plane",
                            sector_id=sector_id,
                            pane_id=pane_id,
                            path=path + ".vertices",
                        )

        triangles_data = entry.get("triangles")
        if not isinstance(triangles_data, list) or not triangles_data:
            _issue(issues, "triangles", "triangles must be a non-empty array", sector_id=sector_id, pane_id=pane_id, path=path + ".triangles")
        else:
            parsed_triangles: list[list[int]] = []
            for tri_index, tri in enumerate(triangles_data):
                tri_path = f"{path}.triangles[{tri_index}]"
                if (
                    not isinstance(tri, list)
                    or len(tri) != 3
                    or not all(isinstance(v, int) and not isinstance(v, bool) and v >= 0 for v in tri)
                ):
                    _issue(issues, "triangle_indices", "triangle must contain three non-negative integer indices", sector_id=sector_id, pane_id=pane_id, path=tri_path)
                    continue
                if len(set(tri)) != 3:
                    _issue(issues, "triangle_indices", "triangle indices must be distinct", sector_id=sector_id, pane_id=pane_id, path=tri_path, value=tri)
                    continue
                if vertices is not None and any(v >= len(vertices) for v in tri):
                    _issue(issues, "index_out_of_range", "triangle index is outside the vertex array", sector_id=sector_id, pane_id=pane_id, path=tri_path, value=tri)
                    continue
                parsed_triangles.append(tri)
            if parsed_triangles:
                triangles = np.asarray(parsed_triangles, dtype=np.int64)
                if vertices is not None:
                    a = vertices[triangles[:, 0]]
                    b = vertices[triangles[:, 1]]
                    c = vertices[triangles[:, 2]]
                    areas = 0.5 * np.linalg.norm(np.cross(b - a, c - a), axis=1)
                    degenerate = np.flatnonzero(areas <= 1.0e-10)
                    if degenerate.size:
                        tri_index = int(degenerate[0])
                        _issue(
                            issues,
                            "degenerate_triangle",
                            f"triangle {tri_index} is degenerate",
                            sector_id=sector_id,
                            pane_id=pane_id,
                            path=f"{path}.triangles[{tri_index}]",
                            value=int(triangles[tri_index].tolist()[0]),
                        )
                    edge_counts: dict[tuple[int, int], int] = {}
                    for tri in triangles:
                        for i, j in ((0, 1), (1, 2), (2, 0)):
                            key = tuple(sorted((int(tri[i]), int(tri[j]))))
                            edge_counts[key] = edge_counts.get(key, 0) + 1
                    non_manifold = [key for key, count in edge_counts.items() if count > 2]
                    if non_manifold:
                        _issue(
                            issues,
                            "non_manifold_edge",
                            "a pane triangle edge is shared by more than two triangles",
                            sector_id=sector_id,
                            pane_id=pane_id,
                            path=path + ".triangles",
                        )
                    boundary = tuple(sorted(key for key, count in edge_counts.items() if count == 1))
                    if areas.size:
                        tri0 = triangles[0]
                        tri_normal = _unit(np.cross(vertices[tri0[1]] - vertices[tri0[0]], vertices[tri0[2]] - vertices[tri0[0]]))
                        if window is not None and abs(float(np.dot(tri_normal, window.normal))) > 1.0e-5:
                            normal = tri_normal
                        else:
                            normal = tri_normal if window is None else _unit(window.normal)

        transmission_color = _vec3(entry.get("transmission_color"))
        if transmission_color is None or np.any(transmission_color < 0.0) or np.any(transmission_color > 1.0):
            _issue(issues, "transmission_color", "transmission_color must be a finite RGB vector in [0,1]", sector_id=sector_id, pane_id=pane_id, path=path + ".transmission_color", value=entry.get("transmission_color"))
        transmittance = entry.get("transmittance")
        if not _is_number(transmittance) or not 0.0 <= float(transmittance) <= 1.0:
            _issue(issues, "transmittance_range", "transmittance must be in [0,1]", sector_id=sector_id, pane_id=pane_id, path=path + ".transmittance", value=transmittance)
        reflectance = entry.get("glaze_reflectance")
        if not _is_number(reflectance) or not 0.0 <= float(reflectance) <= 1.0:
            _issue(issues, "reflectance_range", "glaze_reflectance must be in [0,1]", sector_id=sector_id, pane_id=pane_id, path=path + ".glaze_reflectance", value=reflectance)
        lead_width = entry.get("lead_width")
        if not _is_number(lead_width) or float(lead_width) <= 0.0:
            _issue(issues, "lead_width_range", "lead_width must be greater than zero", sector_id=sector_id, pane_id=pane_id, path=path + ".lead_width", value=lead_width)
        lead_depth = entry.get("lead_depth")
        if not _is_number(lead_depth) or float(lead_depth) < 0.0:
            _issue(issues, "lead_depth_range", "lead_depth must be zero or greater", sector_id=sector_id, pane_id=pane_id, path=path + ".lead_depth", value=lead_depth)

        if (
            len(issues) == start_issue_count
            and vertices is not None
            and triangles is not None
            and transmission_color is not None
            and normal is not None
        ):
            parsed_panes.append(
                Pane(
                    sector_id=int(sector_id),
                    pane_id=int(pane_id),
                    vertices=vertices,
                    triangles=triangles,
                    transmission_color=transmission_color,
                    transmittance=float(transmittance),
                    glaze_reflectance=float(reflectance),
                    lead_width=float(lead_width),
                    lead_depth=float(lead_depth),
                    normal=normal,
                    uv_min=uv_min,
                    uv_max=uv_max,
                    boundary_edges=boundary,
                )
            )

    camera_data = raw.get("camera")
    camera: Camera | None = None
    if not isinstance(camera_data, dict):
        _issue(issues, "camera_missing", "camera object is required", path="$.camera")
    else:
        position = _vec3(camera_data.get("position"))
        target = _vec3(camera_data.get("target"))
        up = _vec3(camera_data.get("up"))
        if position is None:
            _issue(issues, "camera_position", "camera.position must be a finite 3-vector", path="$.camera.position")
        if target is None:
            _issue(issues, "camera_target", "camera.target must be a finite 3-vector", path="$.camera.target")
        if up is None or (up is not None and np.linalg.norm(up) <= EPS):
            _issue(issues, "camera_up", "camera.up must be a nonzero finite 3-vector", path="$.camera.up")
            up = None
        else:
            up = _unit(up)
        fov = camera_data.get("fov_y_deg")
        if not _is_number(fov) or not 0.0 < float(fov) < 179.0:
            _issue(issues, "camera_fov", "camera.fov_y_deg must be in (0,179)", path="$.camera.fov_y_deg", value=fov)
        width = camera_data.get("width")
        height = camera_data.get("height")
        if not isinstance(width, int) or isinstance(width, bool) or not 1 <= width <= 4096:
            _issue(issues, "camera_width", "camera.width must be an integer in [1,4096]", path="$.camera.width", value=width)
        if not isinstance(height, int) or isinstance(height, bool) or not 1 <= height <= 4096:
            _issue(issues, "camera_height", "camera.height must be an integer in [1,4096]", path="$.camera.height", value=height)
        near = camera_data.get("near")
        far = camera_data.get("far")
        if not _is_number(near) or float(near) <= 0.0:
            _issue(issues, "camera_near", "camera.near must be greater than zero", path="$.camera.near", value=near)
        if not _is_number(far) or not _is_number(near) or float(far) <= float(near):
            _issue(issues, "camera_far", "camera.far must be greater than camera.near", path="$.camera.far", value=far)
        if position is not None and target is not None and np.linalg.norm(target - position) <= EPS:
            _issue(issues, "camera_direction", "camera.position and camera.target must differ", path="$.camera.target")
        if position is not None and target is not None and up is not None:
            forward = _unit(target - position)
            if np.linalg.norm(np.cross(forward, up)) <= 1.0e-7:
                _issue(issues, "camera_up_parallel", "camera.up must not be parallel to the view direction", path="$.camera.up")
        if all(
            value is not None for value in (position, target, up, fov, width, height, near, far)
        ) and not any(issue.path.startswith("$.camera") for issue in issues):
            camera = Camera(position, target, up, float(fov), int(width), int(height), float(near), float(far))

    light_data = raw.get("directional_light")
    light: Light | None = None
    if not isinstance(light_data, dict):
        _issue(issues, "light_missing", "directional_light object is required", path="$.directional_light")
    else:
        direction = _vec3(light_data.get("direction"))
        if direction is None or np.linalg.norm(direction) <= EPS:
            _issue(issues, "light_direction", "directional_light.direction must be a nonzero finite 3-vector", path="$.directional_light.direction")
            direction = None
        else:
            direction = _unit(direction)
        irradiance_value = light_data.get("irradiance")
        if _is_number(irradiance_value) and float(irradiance_value) >= 0.0:
            irradiance = np.full(3, float(irradiance_value), dtype=np.float64)
        else:
            irradiance = _vec3(irradiance_value)
        if irradiance is None or np.any(irradiance < 0.0):
            _issue(issues, "light_irradiance", "directional_light.irradiance must be a finite non-negative scalar or RGB vector", path="$.directional_light.irradiance")
            irradiance = None
        bands = light_data.get("bands_nm")
        if (
            not isinstance(bands, list)
            or len(bands) != 3
            or not all(_is_number(v) and 380.0 <= float(v) <= 780.0 for v in bands)
        ):
            _issue(issues, "light_bands", "directional_light.bands_nm must contain three wavelengths in [380,780] nm", path="$.directional_light.bands_nm")
            bands = None
        angular = light_data.get("angular_radius_deg", 0.266)
        if not _is_number(angular) or not 0.0 <= float(angular) < 45.0:
            _issue(issues, "light_angular_radius", "directional_light.angular_radius_deg must be in [0,45)", path="$.directional_light.angular_radius_deg", value=angular)
        first_sector = min(sector_ids_seen) if sector_ids_seen else None
        if direction is not None and window is not None and abs(float(np.dot(direction, window.normal))) <= 1.0e-4:
            _issue(
                issues,
                "light_parallel_to_window_plane",
                "directional light is parallel to the window plane; lead projection is degenerate",
                sector_id=first_sector,
                path="$.directional_light.direction",
            )
        if all(value is not None for value in (direction, irradiance, bands)):
            light = Light(direction, irradiance, tuple(float(v) for v in bands), float(angular))

    texture_data = raw.get("texture")
    texture: TextureConfig | None = None
    if not isinstance(texture_data, dict):
        _issue(issues, "texture_missing", "texture object is required", path="$.texture")
    else:
        seed = texture_data.get("seed")
        table_size = texture_data.get("table_size")
        t_strength = texture_data.get("transmission_strength", 0.30)
        r_strength = texture_data.get("reflection_strength", 0.45)
        octaves = texture_data.get("octaves", 3)
        if not isinstance(seed, int) or isinstance(seed, bool):
            _issue(issues, "texture_seed", "texture.seed must be an integer", path="$.texture.seed", value=seed)
        if not isinstance(table_size, int) or isinstance(table_size, bool) or not 8 <= table_size <= 256:
            _issue(issues, "texture_table_size", "texture.table_size must be an integer in [8,256]", path="$.texture.table_size", value=table_size)
        if not _is_number(t_strength) or not 0.0 <= float(t_strength) <= 1.0:
            _issue(issues, "texture_transmission_strength", "texture.transmission_strength must be in [0,1]", path="$.texture.transmission_strength", value=t_strength)
        if not _is_number(r_strength) or not 0.0 <= float(r_strength) <= 1.0:
            _issue(issues, "texture_reflection_strength", "texture.reflection_strength must be in [0,1]", path="$.texture.reflection_strength", value=r_strength)
        if not isinstance(octaves, int) or isinstance(octaves, bool) or not 1 <= octaves <= 6:
            _issue(issues, "texture_octaves", "texture.octaves must be an integer in [1,6]", path="$.texture.octaves", value=octaves)
        if all(value is not None for value in (seed, table_size, t_strength, r_strength, octaves)):
            texture = TextureConfig(int(seed), int(table_size), float(t_strength), float(r_strength), int(octaves))

    walls_data = raw.get("walls")
    walls: list[Wall] = []
    if not isinstance(walls_data, list) or not walls_data:
        _issue(issues, "walls_missing", "walls must be a non-empty array", path="$.walls")
        walls_data = []
    for index, entry in enumerate(walls_data):
        path = f"$.walls[{index}]"
        if not isinstance(entry, dict):
            _issue(issues, "wall_type", "wall must be an object", path=path)
            continue
        wall_id = entry.get("id")
        point = _vec3(entry.get("point"))
        wall_normal = _vec3(entry.get("normal"))
        axis_u = _vec3(entry.get("axis_u"))
        axis_v = _vec3(entry.get("axis_v"))
        extent_u = entry.get("extent_u")
        extent_v = entry.get("extent_v")
        albedo = _vec3(entry.get("albedo", [0.35, 0.33, 0.30]))
        receives = entry.get("receives_shadow", False)
        start = len(issues)
        if not isinstance(wall_id, str) or not wall_id:
            _issue(issues, "wall_id", "wall.id must be a non-empty string", path=path + ".id")
        if point is None:
            _issue(issues, "wall_point", "wall.point must be a finite 3-vector", path=path + ".point")
        if wall_normal is None or np.linalg.norm(wall_normal) <= EPS:
            _issue(issues, "wall_normal", "wall.normal must be a nonzero finite 3-vector", path=path + ".normal")
            wall_normal = None
        else:
            wall_normal = _unit(wall_normal)
        if axis_u is None or np.linalg.norm(axis_u) <= EPS:
            _issue(issues, "wall_axis", "wall.axis_u must be a nonzero finite 3-vector", path=path + ".axis_u")
            axis_u = None
        else:
            axis_u = _unit(axis_u)
        if axis_v is None or np.linalg.norm(axis_v) <= EPS:
            _issue(issues, "wall_axis", "wall.axis_v must be a nonzero finite 3-vector", path=path + ".axis_v")
            axis_v = None
        else:
            axis_v = _unit(axis_v)
        if not _is_number(extent_u) or float(extent_u) <= 0.0:
            _issue(issues, "wall_extent", "wall.extent_u must be greater than zero", path=path + ".extent_u")
        if not _is_number(extent_v) or float(extent_v) <= 0.0:
            _issue(issues, "wall_extent", "wall.extent_v must be greater than zero", path=path + ".extent_v")
        if albedo is None or np.any(albedo < 0.0) or np.any(albedo > 1.0):
            _issue(issues, "wall_albedo", "wall.albedo must be a finite RGB vector in [0,1]", path=path + ".albedo")
        if not isinstance(receives, bool):
            _issue(issues, "wall_receives_shadow", "wall.receives_shadow must be boolean", path=path + ".receives_shadow")
        if wall_normal is not None and axis_u is not None and abs(float(np.dot(wall_normal, axis_u))) > 1.0e-6:
            _issue(issues, "wall_axis_orthogonality", "wall.axis_u must be perpendicular to wall.normal", path=path + ".axis_u")
        if wall_normal is not None and axis_v is not None and abs(float(np.dot(wall_normal, axis_v))) > 1.0e-6:
            _issue(issues, "wall_axis_orthogonality", "wall.axis_v must be perpendicular to wall.normal", path=path + ".axis_v")
        if len(issues) == start and all(
            value is not None for value in (point, wall_normal, axis_u, axis_v)
        ):
            walls.append(
                Wall(
                    str(wall_id),
                    point,
                    wall_normal,
                    axis_u,
                    axis_v,
                    float(extent_u),
                    float(extent_v),
                    albedo,
                    receives,
                )
            )
    wall_ids = [wall.wall_id for wall in walls]
    if len(set(wall_ids)) != len(wall_ids):
        _issue(issues, "wall_duplicate", "wall ids must be unique", path="$.walls")

    render_data = raw.get("render")
    render: RenderConfig | None = None
    if not isinstance(render_data, dict):
        _issue(issues, "render_missing", "render object is required", path="$.render")
    else:
        chunk_size = render_data.get("chunk_size")
        if (
            not isinstance(chunk_size, list)
            or len(chunk_size) != 2
            or not all(isinstance(v, int) and not isinstance(v, bool) and v > 0 for v in chunk_size)
        ):
            _issue(issues, "chunk_size", "render.chunk_size must contain two positive integers", path="$.render.chunk_size")
            chunk = None
        else:
            chunk = (int(chunk_size[0]), int(chunk_size[1]))
        supersample = render_data.get("supersample", 1)
        if not isinstance(supersample, int) or isinstance(supersample, bool) or not 1 <= supersample <= 4:
            _issue(issues, "supersample", "render.supersample must be an integer in [1,4]", path="$.render.supersample")
            supersample = None
        exposure = render_data.get("exposure", 1.0)
        if not _is_number(exposure) or float(exposure) <= 0.0:
            _issue(issues, "exposure", "render.exposure must be greater than zero", path="$.render.exposure")
            exposure = None
        background = _vec3(render_data.get("background", [0.01, 0.012, 0.016]))
        if background is None or np.any(background < 0.0):
            _issue(issues, "background", "render.background must be a finite non-negative RGB vector", path="$.render.background")
            background = None
        ambient = _vec3(render_data.get("ambient_light", [0.08, 0.09, 0.11]))
        if ambient is None or np.any(ambient < 0.0):
            _issue(issues, "ambient_light", "render.ambient_light must be a finite non-negative RGB vector", path="$.render.ambient_light")
            ambient = None
        reflection_tint = _vec3(render_data.get("reflection_tint", [0.76, 0.82, 0.92]))
        if reflection_tint is None or np.any(reflection_tint < 0.0) or np.any(reflection_tint > 1.0):
            _issue(issues, "reflection_tint", "render.reflection_tint must be a finite RGB vector in [0,1]", path="$.render.reflection_tint")
            reflection_tint = None
        order = render_data.get("composite_order", "transmission_then_reflection")
        if order not in ("transmission_then_reflection", "reflection_then_transmission"):
            _issue(issues, "composite_order", "render.composite_order must be transmission_then_reflection or reflection_then_transmission", path="$.render.composite_order")
            order = None
        grazing_cap = render_data.get("grazing_reflectance_cap", 0.35)
        if not _is_number(grazing_cap) or not 0.0 <= float(grazing_cap) <= 1.0:
            _issue(issues, "grazing_reflectance_cap", "render.grazing_reflectance_cap must be in [0,1]", path="$.render.grazing_reflectance_cap")
            grazing_cap = None
        rounding = render_data.get("output_rounding", "half_up")
        if rounding not in ("half_up", "truncate"):
            _issue(issues, "output_rounding", "render.output_rounding must be half_up or truncate", path="$.render.output_rounding")
            rounding = None
        if all(value is not None for value in (chunk, supersample, exposure, background, ambient, reflection_tint, order, grazing_cap, rounding)):
            render = RenderConfig(
                chunk,
                int(supersample),
                float(exposure),
                background,
                ambient,
                reflection_tint,
                str(order),
                float(grazing_cap),
                str(rounding),
            )

    projection_data = raw.get("projection")
    projection: ProjectionConfig | None = None
    if not isinstance(projection_data, dict):
        _issue(issues, "projection_missing", "projection object is required", path="$.projection")
    else:
        wall_index = projection_data.get("wall_index", 0)
        samples = projection_data.get("samples_per_axis", 5)
        penumbra = projection_data.get("penumbra_per_depth", 3.0)
        profile_points = projection_data.get("profile_points", 25)
        depth_levels = projection_data.get("depth_levels", 3)
        if not isinstance(wall_index, int) or isinstance(wall_index, bool) or not 0 <= wall_index < len(walls):
            _issue(issues, "projection_wall_index", "projection.wall_index must select an existing wall", path="$.projection.wall_index", value=wall_index)
        if not isinstance(samples, int) or isinstance(samples, bool) or not 1 <= samples <= 16:
            _issue(issues, "projection_samples", "projection.samples_per_axis must be an integer in [1,16]", path="$.projection.samples_per_axis")
        if not _is_number(penumbra) or float(penumbra) < 0.0:
            _issue(issues, "projection_penumbra", "projection.penumbra_per_depth must be zero or greater", path="$.projection.penumbra_per_depth")
        if not isinstance(profile_points, int) or isinstance(profile_points, bool) or not 7 <= profile_points <= 257:
            _issue(issues, "projection_profile_points", "projection.profile_points must be an integer in [7,257]", path="$.projection.profile_points")
        if not isinstance(depth_levels, int) or isinstance(depth_levels, bool) or not 1 <= depth_levels <= 9:
            _issue(issues, "projection_depth_levels", "projection.depth_levels must be an integer in [1,9]", path="$.projection.depth_levels")
        if all(value is not None for value in (wall_index, samples, penumbra, profile_points, depth_levels)):
            projection = ProjectionConfig(int(wall_index), int(samples), float(penumbra), int(profile_points), int(depth_levels))

    if issues:
        raise SceneValidationError(issues)
    assert window is not None and camera is not None and light is not None
    assert texture is not None and render is not None and projection is not None
    assert walls

    panes = sorted(parsed_panes, key=lambda pane: (pane.sector_id, pane.pane_id))
    sector_ids = tuple(sorted({pane.sector_id for pane in panes}))
    return Scene(
        raw=raw,
        digest=scene_digest(raw),
        camera=camera,
        window=window,
        light=light,
        texture=texture,
        panes=tuple(panes),
        walls=tuple(walls),
        render=render,
        projection=projection,
        sector_ids=sector_ids,
    )
