"""Deterministic scene construction and structural validation."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .hashing import hash_u64, signed_float, unit_float

SCHEMA = "elytron-iridescence-scene/v1"
DEFAULT_SEED = 254

BUSINESS_CODES = {
    "json": "E_JSON_PARSE",
    "schema": "E_SCHEMA",
    "mesh": "E_MESH_STRUCTURE",
    "mesh_index": "E_MESH_INDEX_OOB",
    "degenerate": "E_DEGENERATE_TRIANGLE",
    "face_missing": "E_FACE_ID_MISSING",
    "face_duplicate": "E_FACE_ID_DUPLICATE",
    "face_seed": "E_FACE_SEED_INVALID",
    "thickness": "E_FILM_THICKNESS_RANGE",
    "refractive": "E_FILM_REFRACTIVE_RANGE",
    "absorption": "E_FILM_ABSORPTION_RANGE",
    "light": "E_LIGHT_INVALID",
    "light_inside": "E_LIGHT_INSIDE_ELYTROM",
    "camera": "E_CAMERA_INVALID",
    "leaf": "E_LEAF_INVALID",
    "render": "E_RENDER_INVALID",
    "threshold": "E_HUE_THRESHOLD_INVALID",
}


class SceneValidationError(Exception):
    def __init__(self, warnings: list[dict[str, Any]]):
        super().__init__("scene validation failed")
        self.warnings = warnings


@dataclass(frozen=True)
class SceneBundle:
    scene: dict[str, Any]
    vertices: np.ndarray
    triangles: np.ndarray
    face_ids: np.ndarray
    face_seeds: np.ndarray
    scene_digest: str
    film_signature: str
    scene_summary: dict[str, Any]


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    )


def canonical_bytes(value: Any) -> bytes:
    return (canonical_json(value) + "\n").encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_value(value: Any) -> str:
    return sha256_bytes(canonical_bytes(value))


def _midpoint(a: int, b: int, vertices: list[np.ndarray], cache: dict[tuple[int, int], int]) -> int:
    key = (a, b) if a < b else (b, a)
    if key in cache:
        return cache[key]
    point = vertices[a] + vertices[b]
    point /= np.linalg.norm(point)
    vertices.append(point)
    index = len(vertices) - 1
    cache[key] = index
    return index


def _icosphere(level: int = 2) -> tuple[list[np.ndarray], list[tuple[int, int, int]]]:
    phi = (1.0 + math.sqrt(5.0)) / 2.0
    vertices = [
        np.array(v, dtype=np.float64)
        for v in [
            (-1, phi, 0),
            (1, phi, 0),
            (-1, -phi, 0),
            (1, -phi, 0),
            (0, -1, phi),
            (0, 1, phi),
            (0, -1, -phi),
            (0, 1, -phi),
            (phi, 0, -1),
            (phi, 0, 1),
            (-phi, 0, -1),
            (-phi, 0, 1),
        ]
    ]
    vertices = [v / np.linalg.norm(v) for v in vertices]
    faces = [
        (0, 11, 5),
        (0, 5, 1),
        (0, 1, 7),
        (0, 7, 10),
        (0, 10, 11),
        (1, 5, 9),
        (5, 11, 4),
        (11, 10, 2),
        (10, 7, 6),
        (7, 1, 8),
        (3, 9, 4),
        (3, 4, 2),
        (3, 2, 6),
        (3, 6, 8),
        (3, 8, 9),
        (4, 9, 5),
        (2, 4, 11),
        (6, 2, 10),
        (8, 6, 7),
        (9, 8, 1),
    ]
    for _ in range(level):
        cache: dict[tuple[int, int], int] = {}
        next_faces: list[tuple[int, int, int]] = []
        for a, b, c in faces:
            ab = _midpoint(a, b, vertices, cache)
            bc = _midpoint(b, c, vertices, cache)
            ca = _midpoint(c, a, vertices, cache)
            next_faces.extend(
                [(a, ab, ca), (b, bc, ab), (c, ca, bc), (ab, bc, ca)]
            )
        faces = next_faces
    return vertices, faces


def _generate_mesh(seed: int) -> dict[str, Any]:
    unit_vertices, unit_faces = _icosphere(2)
    phase_a = unit_float(seed, 101) * math.tau
    phase_b = unit_float(seed, 102) * math.tau
    phase_c = unit_float(seed, 103) * math.tau
    vertices: list[list[float]] = []
    for direction in unit_vertices:
        x0, y0, z0 = direction
        ripple = (
            1.0
            + 0.036 * math.sin(3.1 * x0 + phase_a) * math.cos(2.3 * z0 + phase_b)
            + 0.018 * math.cos(4.1 * y0 + phase_c)
        )
        x = 0.78 * x0 * ripple
        y = 0.13 + 0.36 * y0 * ripple
        z = 0.52 * z0 * ripple
        vertices.append([round(x, 10), round(y, 10), round(z, 10)])

    oriented: list[tuple[int, int, int]] = []
    for a, b, c in unit_faces:
        va = np.asarray(vertices[a], dtype=np.float64)
        vb = np.asarray(vertices[b], dtype=np.float64)
        vc = np.asarray(vertices[c], dtype=np.float64)
        centroid = (va + vb + vc) / 3.0
        centroid[1] -= 0.13
        normal = np.cross(vb - va, vc - va)
        if float(np.dot(normal, centroid)) < 0.0:
            b, c = c, b
        oriented.append((a, b, c))

    face_ids = list(range(1, len(oriented) + 1))
    seeds = [int(hash_u64(seed, face_id, 0xC0FFEE) & 0x7FFFFFFF) for face_id in face_ids]
    return {
        "vertices": vertices,
        "triangles": [list(face) for face in oriented],
        "face_ids": face_ids,
        "curvature_seeds": seeds,
        "degenerate_area_eps": 1e-12,
    }


def generate_scene(seed: int = DEFAULT_SEED) -> dict[str, Any]:
    """Create the complete scene from code; no asset files are consulted."""
    mesh = _generate_mesh(seed)
    cameras = []
    for index in range(7):
        azimuth = math.radians(-12.0 + 4.0 * index)
        elevation = math.radians(27.0)
        target = np.array([0.0, 0.06, 0.0])
        offset = np.array(
            [
                math.sin(azimuth) * math.cos(elevation),
                math.sin(elevation),
                math.cos(azimuth) * math.cos(elevation),
            ]
        ) * 2.35
        position = target + offset
        cameras.append(
            {
                "id": f"C{index:02d}",
                "position": [round(float(v), 10) for v in position],
                "target": [0.0, 0.06, 0.0],
                "up": [0.0, 1.0, 0.0],
                "fov_y_deg": 38.0,
                "width": 192,
                "height": 144,
            }
        )
    return {
        "schema": SCHEMA,
        "seed": int(seed),
        "mesh": mesh,
        "film": {
            "base_thickness_nm": 448.0,
            "thickness_perturbation_nm": 10.0,
            "thickness_gradient_nm": 65.0,
            "refractive_index": 1.38,
            "absorption_coefficient": 0.0009,
            "thickness_bounds_nm": [350.0, 540.0],
            "refractive_index_bounds": [1.05, 2.20],
            "absorption_bounds": [0.0, 0.01],
            "thickness_seed": int(hash_u64(seed, 201) & 0x7FFFFFFF),
            "microstructure_seed": int(hash_u64(seed, 202) & 0x7FFFFFFF),
            "normal_table_size": 9,
            "normal_tilt": 0.11,
            "spectrum_nm": [380.0, 780.0, 4.0],
            "substrate_refractive_index": 1.50,
            "hue_jump_threshold_deg": 8.0,
        },
        "lights": [
            {
                "id": "L1",
                "position": [0.78, 1.54, -0.95],
                "intensity": 3.4,
                "color": [1.0, 0.94, 0.86],
            },
            {
                "id": "L2",
                "position": [-0.82, 1.83, -0.82],
                "intensity": 2.9,
                "color": [0.92, 0.96, 1.0],
            },
            {
                "id": "L3",
                "position": [0.05, 1.43, -0.82],
                "intensity": 2.6,
                "color": [1.0, 0.97, 0.91],
            },
        ],
        "cameras": cameras,
        "leaf": {
            "y": -0.42,
            "bounds": [-1.5, 1.5, -1.1, 1.1],
            "base_color": [0.055, 0.16, 0.055],
            "caustic_sampling_density": 1,
            "caustic_gain": 8.0,
            "penumbra": {
                "base_width": 0.011,
                "distance_gain": 0.075,
                "min_pixel_width": 1.25,
            },
            "report_grid": {
                "x": [-0.75, -0.375, 0.0, 0.375, 0.75],
                "z": [-0.45, -0.225, 0.0, 0.225, 0.45],
            },
        },
        "render": {
            "block_size": [48, 36],
            "exposure": 1.7,
        },
    }


def save_scene(scene: dict[str, Any], path: str) -> None:
    with open(path, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(json.dumps(scene, indent=2, sort_keys=True, allow_nan=False))
        handle.write("\n")


def load_scene(path: str) -> dict[str, Any]:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise SceneValidationError(
            [
                {
                    "code": BUSINESS_CODES["json"],
                    "face_id": 0,
                    "message": f"cannot read scene JSON: {exc}",
                    "details": {},
                }
            ]
        ) from exc
    if not isinstance(value, dict):
        raise SceneValidationError(
            [
                {
                    "code": BUSINESS_CODES["schema"],
                    "face_id": 0,
                    "message": "scene root must be a JSON object",
                    "details": {},
                }
            ]
        )
    return value


def _warning(
    code: str,
    face_id: int,
    message: str,
    **details: Any,
) -> dict[str, Any]:
    return {
        "code": code,
        "face_id": int(face_id) if face_id is not None else 0,
        "message": message,
        "details": details,
    }


def _finite_vector(value: Any, length: int) -> bool:
    if not isinstance(value, list) or len(value) != length:
        return False
    try:
        return all(math.isfinite(float(item)) for item in value)
    except (TypeError, ValueError):
        return False


def _triangle_areas(vertices: np.ndarray, triangles: np.ndarray) -> np.ndarray:
    a = vertices[triangles[:, 0]]
    b = vertices[triangles[:, 1]]
    c = vertices[triangles[:, 2]]
    return 0.5 * np.linalg.norm(np.cross(b - a, c - a), axis=1)


def _solid_angle_sum(point: np.ndarray, vertices: np.ndarray, triangles: np.ndarray) -> float:
    a = vertices[triangles[:, 0]] - point
    b = vertices[triangles[:, 1]] - point
    c = vertices[triangles[:, 2]] - point
    la = np.linalg.norm(a, axis=1)
    lb = np.linalg.norm(b, axis=1)
    lc = np.linalg.norm(c, axis=1)
    numerator = np.einsum("ij,ij->i", a, np.cross(b, c))
    denominator = (
        la * lb * lc
        + np.einsum("ij,ij->i", a, b) * lc
        + np.einsum("ij,ij->i", b, c) * la
        + np.einsum("ij,ij->i", c, a) * lb
    )
    angles = 2.0 * np.arctan2(numerator, denominator)
    return float(np.sum(angles))


def _nearest_face(point: np.ndarray, vertices: np.ndarray, triangles: np.ndarray) -> int:
    centers = vertices[triangles].mean(axis=1)
    return int(np.argmin(np.linalg.norm(centers - point, axis=1))) + 1


def _validate_mesh(mesh: Any, warnings: list[dict[str, Any]]) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray] | None:
    if not isinstance(mesh, dict):
        warnings.append(_warning(BUSINESS_CODES["mesh"], 0, "mesh must be an object"))
        return None
    vertices_raw = mesh.get("vertices")
    triangles_raw = mesh.get("triangles")
    face_ids_raw = mesh.get("face_ids")
    seeds_raw = mesh.get("curvature_seeds")
    if not isinstance(vertices_raw, list) or len(vertices_raw) == 0:
        warnings.append(_warning(BUSINESS_CODES["mesh"], 0, "mesh.vertices must be a non-empty array"))
        return None
    if not isinstance(triangles_raw, list) or len(triangles_raw) == 0:
        warnings.append(_warning(BUSINESS_CODES["mesh"], 0, "mesh.triangles must be a non-empty array"))
        return None
    try:
        vertices = np.asarray(vertices_raw, dtype=np.float64)
    except (TypeError, ValueError):
        warnings.append(_warning(BUSINESS_CODES["mesh"], 0, "mesh.vertices must be numeric"))
        return None
    if vertices.ndim != 2 or vertices.shape[1] != 3 or not np.all(np.isfinite(vertices)):
        warnings.append(_warning(BUSINESS_CODES["mesh"], 0, "mesh.vertices must be finite Nx3 values"))
        return None

    triangle_list: list[list[int]] = []
    for ordinal, triangle in enumerate(triangles_raw):
        if (
            not isinstance(triangle, list)
            or len(triangle) != 3
            or any(isinstance(index, bool) or not isinstance(index, int) for index in triangle)
        ):
            warnings.append(
                _warning(
                    BUSINESS_CODES["mesh"],
                    ordinal + 1,
                    "triangle indices must be three integers",
                    triangle_ordinal=ordinal,
                )
            )
            triangle_list.append([0, 0, 0])
            continue
        triangle_list.append(triangle)
    if warnings:
        return None
    triangles = np.asarray(triangle_list, dtype=np.int64)
    vertex_count = int(vertices.shape[0])

    face_count = len(triangle_list)
    for ordinal, triangle in enumerate(triangle_list):
        for local_index, vertex_index in enumerate(triangle):
            if vertex_index < 0 or vertex_index >= vertex_count:
                fid = ordinal + 1
                if isinstance(face_ids_raw, list) and ordinal < len(face_ids_raw):
                    try:
                        fid = int(face_ids_raw[ordinal])
                    except (TypeError, ValueError):
                        pass
                warnings.append(
                    _warning(
                        BUSINESS_CODES["mesh_index"],
                        fid,
                        "triangle vertex index is outside the vertex array",
                        vertex_index=vertex_index,
                        vertex_count=vertex_count,
                        triangle_ordinal=ordinal,
                        local_index=local_index,
                    )
                )
                break
    if warnings:
        return None

    if not isinstance(face_ids_raw, list) or len(face_ids_raw) != face_count:
        present = set(face_ids_raw) if isinstance(face_ids_raw, list) else set()
        missing = next((fid for fid in range(1, face_count + 1) if fid not in present), face_count + 1)
        warnings.append(
            _warning(
                BUSINESS_CODES["face_missing"],
                missing,
                "face_ids must contain one integer for every triangle",
                triangle_count=face_count,
                face_id_count=len(face_ids_raw) if isinstance(face_ids_raw, list) else 0,
            )
        )
        return None
    if any(isinstance(fid, bool) or not isinstance(fid, int) for fid in face_ids_raw):
        warnings.append(_warning(BUSINESS_CODES["mesh"], 1, "face_ids must be integers"))
        return None
    face_ids = np.asarray(face_ids_raw, dtype=np.int64)
    seen: set[int] = set()
    for fid in face_ids_raw:
        if fid in seen:
            warnings.append(
                _warning(
                    BUSINESS_CODES["face_duplicate"],
                    fid,
                    "face_id occurs more than once",
                )
            )
        seen.add(fid)
    expected = set(range(1, face_count + 1))
    missing_ids = sorted(expected - seen)
    for fid in missing_ids:
        warnings.append(
            _warning(
                BUSINESS_CODES["face_missing"],
                fid,
                "face_id is missing from the contiguous 1..triangle_count range",
            )
        )

    if not isinstance(seeds_raw, list) or len(seeds_raw) != face_count:
        warnings.append(
            _warning(
                BUSINESS_CODES["face_seed"],
                1,
                "curvature_seeds must match the triangle count",
                triangle_count=face_count,
            )
        )
        return None
    if any(isinstance(item, bool) or not isinstance(item, int) or item < 0 for item in seeds_raw):
        warnings.append(_warning(BUSINESS_CODES["face_seed"], 1, "curvature_seeds must be non-negative integers"))
        return None
    face_seeds = np.asarray(seeds_raw, dtype=np.int64)
    if warnings:
        return None

    epsilon = float(mesh.get("degenerate_area_eps", 1e-12))
    if not math.isfinite(epsilon) or epsilon < 0.0:
        warnings.append(_warning(BUSINESS_CODES["mesh"], 1, "degenerate_area_eps must be finite and non-negative"))
        return None
    areas = _triangle_areas(vertices, triangles)
    for ordinal, area in enumerate(areas):
        if not math.isfinite(float(area)) or float(area) <= epsilon:
            warnings.append(
                _warning(
                    BUSINESS_CODES["degenerate"],
                    int(face_ids[ordinal]),
                    "triangle area is at or below the degenerate threshold",
                    area=float(area),
                    epsilon=epsilon,
                    triangle_ordinal=ordinal,
                )
            )
    if warnings:
        return None
    return vertices, triangles, face_ids, face_seeds


def _validate_film(
    film: Any,
    vertices: np.ndarray,
    triangles: np.ndarray,
    face_ids: np.ndarray,
    face_seeds: np.ndarray,
    warnings: list[dict[str, Any]],
) -> None:
    if not isinstance(film, dict):
        warnings.append(_warning(BUSINESS_CODES["schema"], 1, "film must be an object"))
        return
    required_numbers = [
        "base_thickness_nm",
        "thickness_perturbation_nm",
        "thickness_gradient_nm",
        "refractive_index",
        "absorption_coefficient",
        "normal_tilt",
    ]
    for key in required_numbers:
        value = film.get(key)
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(float(value)):
            warnings.append(_warning(BUSINESS_CODES["schema"], 1, f"film.{key} must be a finite number"))
    for key in ["thickness_seed", "microstructure_seed", "normal_table_size"]:
        value = film.get(key)
        if isinstance(value, bool) or not isinstance(value, int):
            warnings.append(_warning(BUSINESS_CODES["schema"], 1, f"film.{key} must be an integer"))
    for key in ["thickness_bounds_nm", "refractive_index_bounds", "absorption_bounds"]:
        value = film.get(key)
        if not _finite_vector(value, 2):
            warnings.append(_warning(BUSINESS_CODES["schema"], 1, f"film.{key} must be two finite numbers"))
    if warnings:
        return

    tb = [float(x) for x in film["thickness_bounds_nm"]]
    nb = [float(x) for x in film["refractive_index_bounds"]]
    ab = [float(x) for x in film["absorption_bounds"]]
    if tb[0] >= tb[1] or tb[0] <= 0:
        warnings.append(_warning(BUSINESS_CODES["schema"], 1, "thickness bounds must be positive and increasing"))
    if nb[0] >= nb[1] or nb[0] <= 1.0:
        warnings.append(_warning(BUSINESS_CODES["schema"], 1, "refractive-index bounds must be above 1 and increasing"))
    if ab[0] >= ab[1] or ab[0] < 0:
        warnings.append(_warning(BUSINESS_CODES["schema"], 1, "absorption bounds must be non-negative and increasing"))
    if int(film["normal_table_size"]) < 2:
        warnings.append(_warning(BUSINESS_CODES["schema"], 1, "normal_table_size must be at least 2"))
    if float(film["normal_tilt"]) < 0:
        warnings.append(_warning(BUSINESS_CODES["schema"], 1, "normal_tilt must be non-negative"))
    threshold = film.get("hue_jump_threshold_deg")
    if (
        isinstance(threshold, bool)
        or not isinstance(threshold, (int, float))
        or not math.isfinite(float(threshold))
        or not 0.0 < float(threshold) <= 180.0
    ):
        warnings.append(_warning(BUSINESS_CODES["threshold"], 1, "hue_jump_threshold_deg must be in (0, 180]"))
    spectrum = film.get("spectrum_nm")
    if (
        not _finite_vector(spectrum, 3)
        or float(spectrum[0]) < 380.0
        or float(spectrum[1]) > 780.0
        or float(spectrum[0]) >= float(spectrum[1])
        or float(spectrum[2]) <= 0.0
    ):
        warnings.append(_warning(BUSINESS_CODES["schema"], 1, "spectrum_nm must be [start, end, step] within 380..780"))
    if warnings:
        return

    thickness = float(film["base_thickness_nm"])
    perturb = float(film["thickness_perturbation_nm"])
    gradient = float(film["thickness_gradient_nm"])
    n_value = float(film["refractive_index"])
    alpha = float(film["absorption_coefficient"])
    if perturb < 0.0 or gradient < 0.0:
        warnings.append(_warning(BUSINESS_CODES["schema"], 1, "film thickness perturbations must be non-negative"))
    if not (tb[0] <= thickness - perturb - gradient and thickness + perturb + gradient <= tb[1]):
        first_face = int(face_ids[0])
        warnings.append(
            _warning(
                BUSINESS_CODES["thickness"],
                first_face,
                "base thickness plus the full perturbation envelope is outside film thickness bounds",
                base_thickness_nm=thickness,
                minimum_possible_nm=thickness - perturb - gradient,
                maximum_possible_nm=thickness + perturb + gradient,
                bounds_nm=tb,
            )
        )
    if not (nb[0] <= n_value <= nb[1]):
        warnings.append(
            _warning(
                BUSINESS_CODES["refractive"],
                int(face_ids[0]),
                "refractive index is outside the declared bounds",
                refractive_index=n_value,
                bounds=nb,
            )
        )
    if not (ab[0] <= alpha <= ab[1]):
        warnings.append(
            _warning(
                BUSINESS_CODES["absorption"],
                int(face_ids[0]),
                "absorption coefficient is outside the declared bounds",
                absorption_coefficient=alpha,
                bounds=ab,
            )
        )
    if warnings:
        return

    # Sample the actual per-face thickness field so a scene with a valid
    # conservative envelope but bad table entries still reports a face ID.
    seed = int(film["thickness_seed"])
    centers = vertices[triangles].mean(axis=1)
    for index, face_id in enumerate(face_ids):
        delta = perturb * signed_float(seed, int(face_id), int(face_seeds[index]), 11)
        point = centers[index]
        spatial = gradient * (
            0.55 * math.sin(2.3 * float(point[0]) + 0.7)
            + 0.45 * math.cos(2.0 * float(point[2]) - 0.4)
        )
        thickness_here = thickness + delta + spatial
        if not tb[0] <= thickness_here <= tb[1]:
            warnings.append(
                _warning(
                    BUSINESS_CODES["thickness"],
                    int(face_id),
                    "sampled face thickness is outside the declared bounds",
                    thickness_nm=thickness_here,
                    bounds_nm=tb,
                )
            )
            break


def _validate_lights(
    lights: Any,
    vertices: np.ndarray,
    triangles: np.ndarray,
    warnings: list[dict[str, Any]],
) -> None:
    if not isinstance(lights, list) or len(lights) == 0:
        warnings.append(_warning(BUSINESS_CODES["light"], 0, "lights must be a non-empty array"))
        return
    ids: set[str] = set()
    for index, light in enumerate(lights):
        if not isinstance(light, dict):
            warnings.append(_warning(BUSINESS_CODES["light"], index + 1, "light must be an object"))
            continue
        light_id = light.get("id")
        if not isinstance(light_id, str) or not light_id:
            warnings.append(_warning(BUSINESS_CODES["light"], index + 1, "light.id must be a non-empty string"))
            continue
        if light_id in ids:
            warnings.append(
                _warning(
                    BUSINESS_CODES["light"],
                    index + 1,
                    "light.id must be unique",
                    source_id=light_id,
                )
            )
        ids.add(light_id)
        if not _finite_vector(light.get("position"), 3):
            warnings.append(_warning(BUSINESS_CODES["light"], index + 1, "light.position must be finite xyz", source_id=light_id))
            continue
        intensity = light.get("intensity")
        if (
            isinstance(intensity, bool)
            or not isinstance(intensity, (int, float))
            or not math.isfinite(float(intensity))
            or float(intensity) < 0.0
        ):
            warnings.append(_warning(BUSINESS_CODES["light"], index + 1, "light.intensity must be finite and non-negative"))
        color = light.get("color")
        if not _finite_vector(color, 3) or any(float(channel) < 0.0 for channel in color):
            warnings.append(_warning(BUSINESS_CODES["light"], index + 1, "light.color must be three non-negative numbers"))
        position = np.asarray(light["position"], dtype=np.float64)
        solid_angle = _solid_angle_sum(position, vertices, triangles)
        if abs(solid_angle) > 2.0 * math.pi:
            nearest = _nearest_face(position, vertices, triangles)
            warnings.append(
                _warning(
                    BUSINESS_CODES["light_inside"],
                    nearest,
                    "point light is inside the closed elytron mesh",
                    source_id=light_id,
                    position=[float(v) for v in position],
                    solid_angle=solid_angle,
                )
            )


def _validate_cameras(cameras: Any, warnings: list[dict[str, Any]]) -> None:
    if not isinstance(cameras, list) or len(cameras) == 0:
        warnings.append(_warning(BUSINESS_CODES["camera"], 0, "cameras must be a non-empty array"))
        return
    ids: set[str] = set()
    for index, camera in enumerate(cameras):
        fid = index + 1
        if not isinstance(camera, dict):
            warnings.append(_warning(BUSINESS_CODES["camera"], fid, "camera must be an object"))
            continue
        camera_id = camera.get("id")
        if not isinstance(camera_id, str) or not camera_id:
            warnings.append(_warning(BUSINESS_CODES["camera"], fid, "camera.id must be a non-empty string"))
        elif camera_id in ids:
            warnings.append(_warning(BUSINESS_CODES["camera"], fid, "camera.id must be unique"))
        else:
            ids.add(camera_id)
        if not _finite_vector(camera.get("position"), 3) or not _finite_vector(camera.get("target"), 3):
            warnings.append(_warning(BUSINESS_CODES["camera"], fid, "camera position and target must be finite xyz"))
            continue
        if not _finite_vector(camera.get("up"), 3):
            warnings.append(_warning(BUSINESS_CODES["camera"], fid, "camera.up must be finite xyz"))
            continue
        position = np.asarray(camera["position"], dtype=np.float64)
        target = np.asarray(camera["target"], dtype=np.float64)
        up = np.asarray(camera["up"], dtype=np.float64)
        forward = target - position
        length = float(np.linalg.norm(forward))
        if length <= 1e-9:
            warnings.append(_warning(BUSINESS_CODES["camera"], fid, "camera position and target must differ"))
            continue
        up_norm = float(np.linalg.norm(up))
        if up_norm <= 1e-9 or float(np.linalg.norm(np.cross(forward / length, up / up_norm))) <= 1e-9:
            warnings.append(_warning(BUSINESS_CODES["camera"], fid, "camera.up must be non-zero and not parallel to the view"))
        fov = camera.get("fov_y_deg")
        if (
            isinstance(fov, bool)
            or not isinstance(fov, (int, float))
            or not math.isfinite(float(fov))
            or not 0.0 < float(fov) < 180.0
        ):
            warnings.append(_warning(BUSINESS_CODES["camera"], fid, "camera.fov_y_deg must be in (0, 180)"))
        for key in ["width", "height"]:
            value = camera.get(key)
            if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
                warnings.append(_warning(BUSINESS_CODES["camera"], fid, f"camera.{key} must be a positive integer"))


def _validate_leaf_and_render(scene: dict[str, Any], warnings: list[dict[str, Any]]) -> None:
    leaf = scene.get("leaf")
    if not isinstance(leaf, dict):
        warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "leaf must be an object"))
    else:
        y = leaf.get("y")
        if isinstance(y, bool) or not isinstance(y, (int, float)) or not math.isfinite(float(y)):
            warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "leaf.y must be finite"))
        if not _finite_vector(leaf.get("bounds"), 4):
            warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "leaf.bounds must be four finite numbers"))
        else:
            bounds = [float(v) for v in leaf["bounds"]]
            if not (bounds[0] < bounds[1] and bounds[2] < bounds[3]):
                warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "leaf bounds must be increasing"))
        if not _finite_vector(leaf.get("base_color"), 3):
            warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "leaf.base_color must be three finite numbers"))
        density = leaf.get("caustic_sampling_density")
        if isinstance(density, bool) or not isinstance(density, int) or density < 1:
            warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "caustic_sampling_density must be a positive integer"))
        gain = leaf.get("caustic_gain")
        if isinstance(gain, bool) or not isinstance(gain, (int, float)) or not math.isfinite(float(gain)) or float(gain) < 0:
            warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "caustic_gain must be finite and non-negative"))
        penumbra = leaf.get("penumbra")
        if not isinstance(penumbra, dict):
            warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "leaf.penumbra must be an object"))
        else:
            for key in ["base_width", "distance_gain", "min_pixel_width"]:
                value = penumbra.get(key)
                if (
                    isinstance(value, bool)
                    or not isinstance(value, (int, float))
                    or not math.isfinite(float(value))
                    or float(value) < 0.0
                ):
                    warnings.append(_warning(BUSINESS_CODES["leaf"], 0, f"leaf.penumbra.{key} must be finite and non-negative"))
        grid = leaf.get("report_grid")
        if not isinstance(grid, dict) or not isinstance(grid.get("x"), list) or not isinstance(grid.get("z"), list):
            warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "leaf.report_grid requires x and z arrays"))
        elif not grid["x"] or not grid["z"]:
            warnings.append(_warning(BUSINESS_CODES["leaf"], 0, "leaf.report_grid axes must be non-empty"))

    render = scene.get("render")
    if not isinstance(render, dict):
        warnings.append(_warning(BUSINESS_CODES["render"], 0, "render must be an object"))
    else:
        block = render.get("block_size")
        if (
            not isinstance(block, list)
            or len(block) != 2
            or any(isinstance(v, bool) or not isinstance(v, int) or v <= 0 for v in block)
        ):
            warnings.append(_warning(BUSINESS_CODES["render"], 0, "render.block_size must be two positive integers"))
        exposure = render.get("exposure")
        if (
            isinstance(exposure, bool)
            or not isinstance(exposure, (int, float))
            or not math.isfinite(float(exposure))
            or float(exposure) <= 0
        ):
            warnings.append(_warning(BUSINESS_CODES["render"], 0, "render.exposure must be finite and positive"))


def validate_scene(scene: dict[str, Any]) -> SceneBundle:
    warnings: list[dict[str, Any]] = []
    if scene.get("schema") != SCHEMA:
        warnings.append(
            _warning(
                BUSINESS_CODES["schema"],
                0,
                f"unsupported scene schema; expected {SCHEMA}",
                actual=scene.get("schema"),
            )
        )
    seed = scene.get("seed")
    if isinstance(seed, bool) or not isinstance(seed, int):
        warnings.append(_warning(BUSINESS_CODES["schema"], 0, "seed must be an integer"))
    mesh_result = _validate_mesh(scene.get("mesh"), warnings)
    if mesh_result is None:
        raise SceneValidationError(warnings)
    vertices, triangles, face_ids, face_seeds = mesh_result
    _validate_film(scene.get("film"), vertices, triangles, face_ids, face_seeds, warnings)
    _validate_lights(scene.get("lights"), vertices, triangles, warnings)
    _validate_cameras(scene.get("cameras"), warnings)
    _validate_leaf_and_render(scene, warnings)
    if warnings:
        raise SceneValidationError(warnings)

    digest = sha256_value(scene)
    film_signature = sha256_value(scene["film"])
    summary = {
        "seed": int(scene["seed"]),
        "vertex_count": int(vertices.shape[0]),
        "triangle_count": int(triangles.shape[0]),
        "face_id_min": int(face_ids.min()),
        "face_id_max": int(face_ids.max()),
        "light_ids": sorted(light["id"] for light in scene["lights"]),
        "camera_ids": [camera["id"] for camera in scene["cameras"]],
        "image_sizes": [
            [camera["id"], int(camera["width"]), int(camera["height"])]
            for camera in scene["cameras"]
        ],
        "block_size": [int(v) for v in scene["render"]["block_size"]],
        "film_signature": film_signature,
    }
    return SceneBundle(
        scene=scene,
        vertices=vertices,
        triangles=triangles,
        face_ids=face_ids,
        face_seeds=face_seeds,
        scene_digest=digest,
        film_signature=film_signature,
        scene_summary=summary,
    )
