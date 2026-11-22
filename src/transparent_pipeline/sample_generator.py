"""Deterministic local sample generator and independent analytic truth."""

from __future__ import annotations

import math
from pathlib import Path
from typing import Any

import numpy as np

from .errors import geometry_error, parameter_error
from .scattering import analytic_segment, hg_phase, phase_cosine
from .scene import SCENE_SCHEMA_VERSION, TRUTH_SCHEMA_VERSION, Scene, validate_scene_dict, validate_truth_dict
from .util import sha256_json, write_json_atomic

DEFAULT_SEED = 20261123
TRUTH_TOLERANCE_GEOMETRY = 1.0e-8
TRUTH_TOLERANCE_TRANSMITTANCE = 1.0e-10
TRUTH_TOLERANCE_SCATTERING = 2.0e-3


def _normalize(value: np.ndarray) -> np.ndarray:
    return value / np.linalg.norm(value)


def _sphere_entry(
    origin: np.ndarray,
    direction: np.ndarray,
    center: np.ndarray,
    radius: float,
) -> tuple[float, np.ndarray, np.ndarray]:
    offset = origin - center
    b = 2.0 * float(np.dot(offset, direction))
    c = float(np.dot(offset, offset)) - radius * radius
    discriminant = b * b - 4.0 * c
    if discriminant < 0.0:
        raise geometry_error("truth.ray", "analytic truth ray misses sphere")
    root = math.sqrt(max(0.0, discriminant))
    candidates = [(-b - root) * 0.5, (-b + root) * 0.5]
    t = min(candidate for candidate in candidates if candidate > 1.0e-9)
    point = origin + t * direction
    normal = (point - center) / radius
    return t, point, normal


def _box_entry(
    origin: np.ndarray,
    direction: np.ndarray,
    lower: np.ndarray,
    upper: np.ndarray,
) -> tuple[float, np.ndarray, np.ndarray]:
    near = -math.inf
    far = math.inf
    near_axis = 0
    far_axis = 0
    for axis in range(3):
        if abs(direction[axis]) < 1.0e-15:
            if origin[axis] < lower[axis] or origin[axis] > upper[axis]:
                raise geometry_error("truth.ray", "analytic truth ray misses box")
            continue
        t1 = (lower[axis] - origin[axis]) / direction[axis]
        t2 = (upper[axis] - origin[axis]) / direction[axis]
        if t1 > t2:
            t1, t2 = t2, t1
        if t1 > near:
            near = t1
            near_axis = axis
        if t2 < far:
            far = t2
            far_axis = axis
    if near <= 1.0e-9 or far <= near:
        raise geometry_error("truth.ray", "analytic truth ray does not enter box")
    point = origin + near * direction
    normal = np.zeros(3, dtype=np.float64)
    if abs(point[near_axis] - lower[near_axis]) < abs(point[near_axis] - upper[near_axis]):
        normal[near_axis] = -1.0 if direction[near_axis] > 0.0 else 1.0
    else:
        normal[near_axis] = 1.0 if direction[near_axis] > 0.0 else -1.0
    return near, point, normal


def _refract(incident: np.ndarray, outward_normal: np.ndarray, n1: float, n2: float) -> np.ndarray:
    normal = -outward_normal if float(np.dot(incident, outward_normal)) > 0.0 else outward_normal
    cosine = max(0.0, min(1.0, -float(np.dot(incident, normal))))
    eta = n1 / n2
    k = 1.0 - eta * eta * (1.0 - cosine * cosine)
    if k < 0.0:
        return _normalize(incident - 2.0 * float(np.dot(incident, normal)) * normal)
    return _normalize(eta * incident + (eta * cosine - math.sqrt(k)) * normal)


def _box_exit_from_inside(
    point: np.ndarray,
    direction: np.ndarray,
    lower: np.ndarray,
    upper: np.ndarray,
) -> tuple[float, np.ndarray, np.ndarray]:
    distances: list[tuple[float, int, float]] = []
    for axis in range(3):
        if abs(direction[axis]) < 1.0e-15:
            continue
        bound = upper[axis] if direction[axis] > 0.0 else lower[axis]
        distance = (bound - point[axis]) / direction[axis]
        if distance > 1.0e-9:
            distances.append((distance, axis, bound))
    if not distances:
        raise geometry_error("truth.ray", "box internal ray has no exit")
    distance, axis, bound = min(distances)
    exit_point = point + distance * direction
    normal = np.zeros(3, dtype=np.float64)
    normal[axis] = 1.0 if bound == upper[axis] else -1.0
    return distance, exit_point, normal


def _analytic_transmission_path(
    medium: dict[str, Any],
    origin: np.ndarray,
    direction: np.ndarray,
    wavelength_index: int,
) -> dict[str, Any]:
    ior = float(medium["ior"][wavelength_index])
    boundary = medium["boundary"]
    if boundary["type"] == "sphere":
        center = np.asarray(boundary["center"], dtype=np.float64)
        radius = float(boundary["radius"])
        _, entry, normal = _sphere_entry(origin, direction, center, radius)
        inside = _refract(direction, normal, 1.0, ior)
        exit_distance = -2.0 * float(np.dot(entry - center, inside))
        if exit_distance <= 0.0:
            raise geometry_error("truth.ray", "sphere internal ray has no exit")
        exit_point = entry + exit_distance * inside
        exit_normal = _normalize(exit_point - center)
    elif boundary["type"] == "box":
        lower = np.asarray(boundary["min"], dtype=np.float64)
        upper = np.asarray(boundary["max"], dtype=np.float64)
        _, entry, normal = _box_entry(origin, direction, lower, upper)
        inside = _refract(direction, normal, 1.0, ior)
        exit_distance, exit_point, exit_normal = _box_exit_from_inside(entry, inside, lower, upper)
    else:
        raise geometry_error("truth.medium", "analytic truth supports sphere and box media")

    condition = ior * math.sin(math.acos(max(-1.0, min(1.0, abs(float(np.dot(inside, exit_normal)))))))
    if condition > 1.0 + 1.0e-12:
        raise geometry_error("truth.ray", "analytic truth ray reaches total internal reflection; choose another ray")
    return {
        "entry": entry,
        "exit": exit_point,
        "inside_direction": inside,
        "path_length": float(exit_distance),
        "exit_normal": exit_normal,
    }


def _truth_rays(scene: dict[str, Any]) -> list[dict[str, Any]]:
    sphere = next(medium for medium in scene["media"] if medium["boundary"]["type"] == "sphere")
    boxes = [medium for medium in scene["media"] if medium["boundary"]["type"] == "box"]
    fog = next(medium for medium in boxes if "fog" in medium["id"])
    water = next(medium for medium in boxes if "water" in medium["id"])
    sphere_center = np.asarray(sphere["boundary"]["center"], dtype=np.float64)
    fog_center = (
        np.asarray(fog["boundary"]["min"], dtype=np.float64)
        + np.asarray(fog["boundary"]["max"], dtype=np.float64)
    ) * 0.5
    water_center_xz = (
        np.asarray(water["boundary"]["min"], dtype=np.float64)
        + np.asarray(water["boundary"]["max"], dtype=np.float64)
    ) * 0.5
    trajectory = scene["camera"]["trajectory"]
    selected_indices = [0, len(trajectory) - 1] if len(trajectory) > 1 else [0]
    cases: list[dict[str, Any]] = []
    for sample_index in selected_indices:
        camera = trajectory[sample_index]
        eye = np.asarray(camera["eye"], dtype=np.float64)
        time = float(camera["time"])
        prefix = f"t{time:.2f}"
        water_origin = eye + np.array([0.0, 1.2, 0.0])
        cases.extend(
            [
                {
                    "id": f"{prefix}_glass_center",
                    "medium_id": sphere["id"],
                    "origin": eye,
                    "direction": _normalize(sphere_center + np.array([0.04, -0.03, 0.0]) - eye),
                    "camera_sample_index": sample_index,
                    "time": time,
                },
                {
                    "id": f"{prefix}_fog_oblique",
                    "medium_id": fog["id"],
                    "origin": eye,
                    "direction": _normalize(fog_center - eye),
                    "camera_sample_index": sample_index,
                    "time": time,
                },
                {
                    "id": f"{prefix}_water_layer",
                    "medium_id": water["id"],
                    "origin": water_origin,
                    "direction": _normalize(
                        np.array([water_center_xz[0], water_center_xz[1] - 0.2, water_center_xz[2]])
                        - water_origin
                    ),
                    "camera_sample_index": sample_index,
                    "time": time,
                },
            ]
        )
    return cases


def generate_truth(scene: dict[str, Any]) -> dict[str, Any]:
    validate_scene_dict(scene)
    scene_hash = sha256_json(scene)
    media = {medium["id"]: medium for medium in scene["media"]}
    lights = {light["id"]: light for light in scene["lights"]}
    wave_count = len(scene["wavelengths_nm"])
    boundary_exit: list[dict[str, Any]] = []
    transmittance: list[dict[str, Any]] = []
    scattering: list[dict[str, Any]] = []

    for ray_case in _truth_rays(scene):
        medium = media[ray_case["medium_id"]]
        origin = ray_case["origin"]
        direction = ray_case["direction"]
        for wavelength_index in range(wave_count):
            path = _analytic_transmission_path(medium, origin, direction, wavelength_index)
            sigma_a = float(medium["absorption"][wavelength_index])
            sigma_s = float(medium["scattering"][wavelength_index])
            sigma_t = sigma_a + sigma_s
            optical_depth = sigma_t * path["path_length"]
            transmission = math.exp(-optical_depth)
            case_id = f"{ray_case['id']}_w{wavelength_index}"
            boundary_exit.append(
                {
                    "id": f"{case_id}_boundary_exit",
                    "source_case": ray_case["id"],
                    "camera_sample_index": ray_case["camera_sample_index"],
                    "time": ray_case["time"],
                    "medium_id": medium["id"],
                    "wavelength_index": wavelength_index,
                    "origin": origin.tolist(),
                    "direction": direction.tolist(),
                    "expected_entry": path["entry"].tolist(),
                    "expected_exit": path["exit"].tolist(),
                    "expected_path_length": path["path_length"],
                    "tolerance": TRUTH_TOLERANCE_GEOMETRY,
                }
            )
            transmittance.append(
                {
                    "id": f"{case_id}_transmittance",
                    "source_case": ray_case["id"],
                    "camera_sample_index": ray_case["camera_sample_index"],
                    "time": ray_case["time"],
                    "medium_id": medium["id"],
                    "wavelength_index": wavelength_index,
                    "origin": origin.tolist(),
                    "direction": direction.tolist(),
                    "expected_path_length": path["path_length"],
                    "expected_optical_depth": optical_depth,
                    "expected_transmittance": transmission,
                    "tolerance": TRUTH_TOLERANCE_TRANSMITTANCE,
                }
            )
            for light in scene["lights"]:
                light_id = light["id"]
                cosine = phase_cosine(path["inside_direction"], np.asarray(light["direction"], dtype=np.float64))
                phase = hg_phase(cosine, float(light["anisotropy"][wavelength_index]))
                reference = analytic_segment(
                    length=path["path_length"],
                    sigma_s=sigma_s,
                    sigma_t=sigma_t,
                    radiance=float(light["radiance"][wavelength_index]),
                    phase=phase,
                )
                scattering.append(
                    {
                        "id": f"{case_id}_{light_id}_scattering",
                        "source_case": ray_case["id"],
                        "camera_sample_index": ray_case["camera_sample_index"],
                        "time": ray_case["time"],
                        "medium_id": medium["id"],
                        "light_id": light_id,
                        "wavelength_index": wavelength_index,
                        "origin": origin.tolist(),
                        "direction": direction.tolist(),
                        "inside_direction": path["inside_direction"].tolist(),
                        "expected_path_length": path["path_length"],
                        "expected_phase_cosine": cosine,
                        "expected_integral": reference,
                        "tolerance": TRUTH_TOLERANCE_SCATTERING,
                    }
                )

    truth = {
        "schema_version": TRUTH_SCHEMA_VERSION,
        "scene_hash": scene_hash,
        "wavelengths_nm": scene["wavelengths_nm"],
        "boundary_exit": boundary_exit,
        "ray_transmittance": transmittance,
        "ray_scattering": scattering,
        "notes": {
            "transmittance": "T = exp(-(sigma_a + sigma_s) * L) for one homogeneous internal segment",
            "scattering": "S = L_light * sigma_s * p_HG * (1-exp(-sigma_t*L))/sigma_t",
            "geometry": "Entry refraction uses Snell's law; exit is the internal boundary intersection before the exit refraction.",
        },
    }
    validate_truth_dict(truth, scene)
    return truth


def generate_scene(seed: int = DEFAULT_SEED) -> dict[str, Any]:
    if isinstance(seed, bool) or not isinstance(seed, int):
        raise parameter_error("seed", "seed must be an integer")
    rng = np.random.default_rng(seed)
    ior_offset = float(rng.uniform(-0.005, 0.005))
    fog_density = float(rng.uniform(0.045, 0.075))

    scene: dict[str, Any] = {
        "schema_version": SCENE_SCHEMA_VERSION,
        "seed": seed,
        "scene_id": f"media-lab-{seed}",
        "wavelengths_nm": [450.0, 550.0, 650.0],
        "media": [
            {
                "id": "glass_sphere",
                "kind": "glass",
                "boundary": {
                    "type": "sphere",
                    "center": [-1.08, 0.08, -0.15],
                    "radius": 0.78,
                },
                "ior": [1.49 + ior_offset, 1.51 + ior_offset, 1.53 + ior_offset],
                "absorption": [0.055, 0.045, 0.035],
                "scattering": [0.007, 0.006, 0.005],
                "color": [0.55, 0.78, 1.0],
            },
            {
                "id": "fog_chamber",
                "kind": "fog",
                "boundary": {
                    "type": "box",
                    "min": [0.28, -1.02, -0.88],
                    "max": [1.92, 1.04, 0.82],
                },
                "ior": [1.0012, 1.0013, 1.0014],
                "absorption": [0.018, 0.015, 0.012],
                "scattering": [fog_density, fog_density * 0.82, fog_density * 0.66],
                "color": [0.82, 0.88, 0.98],
            },
            {
                "id": "water_surface",
                "kind": "water",
                "boundary": {
                    "type": "box",
                    "min": [-2.75, -1.36, -1.55],
                    "max": [2.75, -1.04, 1.55],
                },
                "ior": [1.331, 1.334, 1.338],
                "absorption": [0.085, 0.060, 0.040],
                "scattering": [0.022, 0.018, 0.014],
                "color": [0.30, 0.62, 0.92],
            },
        ],
        "lights": [
            {
                "id": "key_volume",
                "type": "directional_volume",
                "direction": [0.32, -0.66, 0.68],
                "radiance": [1.35, 1.22, 1.08],
                "anisotropy": [0.28, 0.32, 0.36],
            },
            {
                "id": "fill_volume",
                "type": "directional_volume",
                "direction": [-0.58, -0.22, 0.79],
                "radiance": [0.34, 0.38, 0.43],
                "anisotropy": [-0.08, 0.0, 0.08],
            },
            {
                "id": "rim_volume",
                "type": "directional_volume",
                "direction": [0.16, 0.74, 0.66],
                "radiance": [0.22, 0.21, 0.19],
                "anisotropy": [0.58, 0.52, 0.46],
            },
        ],
        "camera": {
            "width": 48,
            "height": 36,
            "fov_y_deg": 52.0,
            "near_far": [0.1, 100.0],
            "trajectory": [
                {"time": 0.0, "eye": [0.0, 0.0, -5.2], "target": [0.0, 0.0, 0.0], "up": [0.0, 1.0, 0.0]},
                {"time": 0.25, "eye": [0.18, 0.08, -5.08], "target": [0.08, 0.0, 0.0], "up": [0.0, 1.0, 0.0]},
                {"time": 0.5, "eye": [0.36, 0.16, -4.96], "target": [0.16, 0.0, 0.0], "up": [0.0, 1.0, 0.0]},
                {"time": 0.75, "eye": [0.52, 0.22, -4.86], "target": [0.22, -0.02, 0.0], "up": [0.0, 1.0, 0.0]},
                {"time": 1.0, "eye": [0.68, 0.28, -4.76], "target": [0.30, -0.03, 0.0], "up": [0.0, 1.0, 0.0]},
            ],
            "default_sample_index": 0,
        },
    }
    validate_scene_dict(scene)
    return scene


def generate_sample(directory: str | Path, seed: int = DEFAULT_SEED) -> dict[str, Any]:
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    scene = generate_scene(seed)
    truth = generate_truth(scene)
    scene_path = directory / "scene.json"
    truth_path = directory / "truth.json"
    write_json_atomic(scene_path, scene)
    write_json_atomic(truth_path, truth)
    manifest = {
        "seed": seed,
        "scene_file": scene_path.name,
        "truth_file": truth_path.name,
        "scene_hash": sha256_json(scene),
        "truth_hash": sha256_json(truth),
        "wavelength_count": len(scene["wavelengths_nm"]),
        "medium_count": len(scene["media"]),
        "light_count": len(scene["lights"]),
        "camera_sample_count": len(scene["camera"]["trajectory"]),
        "boundary_exit_count": len(truth["boundary_exit"]),
        "transmittance_count": len(truth["ray_transmittance"]),
        "scattering_count": len(truth["ray_scattering"]),
    }
    write_json_atomic(directory / "manifest.json", manifest)
    Scene.from_dict(scene)
    return manifest


__all__ = [
    "DEFAULT_SEED",
    "generate_sample",
    "generate_scene",
    "generate_truth",
]
