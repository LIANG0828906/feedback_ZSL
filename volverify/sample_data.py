from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import numpy as np

from .io_utils import atomic_write_bytes, atomic_write_json, sha256_bytes

VALID_PROFILE = "valid"
INVALID_PROFILES = (
    "invalid_truncated",
    "invalid_nan",
    "invalid_tf_overlap",
    "invalid_tf_uncovered",
    "invalid_light_inside",
    "invalid_camera",
    "invalid_range_violation",
    "invalid_tf_boundary",
)


def _density_grid(
    dims: tuple[int, int, int],
    bounds_min: np.ndarray,
    bounds_max: np.ndarray,
) -> np.ndarray:
    x = np.linspace(bounds_min[0], bounds_max[0], dims[0])
    y = np.linspace(bounds_min[1], bounds_max[1], dims[1])
    z = np.linspace(bounds_min[2], bounds_max[2], dims[2])
    xx, yy, zz = np.meshgrid(x, y, z, indexing="ij")
    radius = np.sqrt(
        (xx / 0.47) ** 2 + (yy / 0.33) ** 2 + (zz / 0.28) ** 2
    )
    body = 0.62 * np.exp(-2.2 * radius**2)
    shell = 0.36 * np.exp(-((radius - 0.72) / 0.055) ** 2)
    lobe = 0.18 * np.exp(
        -(
            (xx - 0.22) ** 2 / 0.07
            + (yy + 0.11) ** 2 / 0.09
            + (zz - 0.07) ** 2 / 0.055
        )
    )
    striation = (
        0.065
        * (0.5 + 0.5 * np.sin(15.0 * xx + 4.0 * zz))
        * np.exp(-radius**2 / 0.72)
    )
    return np.clip(body + shell + lobe + striation, 0.0, 1.0).astype(
        np.float32
    )


def base_scene_spec(
    frame_id: str,
    dims: tuple[int, int, int],
    volume_sha256: str,
    width: int = 64,
    height: int = 48,
) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "frame_id": frame_id,
        "volume": {
            "dims": list(dims),
            "bounds": {
                "min": [-0.6, -0.45, -0.4],
                "max": [0.6, 0.45, 0.4],
            },
            "density_range": [0.0, 1.0],
            "data_file": "volume.f32",
            "dtype": "float32",
            "order": "C",
            "sha256": volume_sha256,
        },
        "camera": {
            "position": [0.0, 0.0, 2.15],
            "look_at": [0.0, 0.0, 0.0],
            "up": [0.0, 1.0, 0.0],
            "fov_y_degrees": 48.0,
            "aspect_ratio": width / height,
            "width": width,
            "height": height,
        },
        "lights": [
            {
                "id": "key",
                "type": "point",
                "position": [-0.34, 0.42, 0.62],
                "color": [1.0, 0.72, 0.46],
                "intensity": 3.2,
                "phase_g": 0.16,
                "min_distance": 0.03,
            },
            {
                "id": "rim",
                "type": "point",
                "position": [0.46, -0.28, 0.48],
                "color": [0.34, 0.58, 1.0],
                "intensity": 1.7,
                "phase_g": -0.08,
                "min_distance": 0.03,
            },
            {
                "id": "sun",
                "type": "directional",
                "direction": [-0.22, -0.38, -0.9],
                "color": [0.62, 0.76, 1.0],
                "intensity": 2.2,
                "phase_g": 0.1,
            },
        ],
        "transfer": {
            "density_range": [0.0, 1.0],
            "transition_width": 0.04,
            "segments": [
                {
                    "range": [0.0, 0.25],
                    "absorption": [0.14, 0.42],
                    "emission": [
                        [0.0, 0.0, 0.0],
                        [0.025, 0.035, 0.055],
                    ],
                },
                {
                    "range": [0.25, 0.55],
                    "absorption": [0.42, 1.75],
                    "emission": [
                        [0.08, 0.12, 0.2],
                        [0.9, 0.35, 0.12],
                    ],
                },
                {
                    "range": [0.55, 0.8],
                    "absorption": [1.75, 3.1],
                    "emission": [
                        [0.9, 0.35, 0.12],
                        [0.2, 0.8, 1.0],
                    ],
                },
                {
                    "range": [0.8, 1.0],
                    "absorption": [3.1, 0.8],
                    "emission": [
                        [0.2, 0.8, 1.0],
                        [0.05, 0.1, 0.15],
                    ],
                },
            ],
        },
        "sampling": {
            "reference_step": 0.02,
            "refinement_factor": 2.0,
            "convergence_levels": 3,
            "convergence_tolerance": 0.02,
            "suspicious_opacity_jump_threshold": 0.02,
        },
        "render": {
            "chunk_size": [22, 17],
            "exposure": 3.0,
            "gamma": 2.2,
        },
    }


def _write_profile(
    root: Path,
    frame_id: str,
    scene: dict[str, Any],
    density: np.ndarray,
) -> Path:
    profile_dir = root / frame_id
    profile_dir.mkdir(parents=True, exist_ok=True)
    volume_path = profile_dir / "volume.f32"
    volume_bytes = np.asarray(density, dtype="<f4").tobytes(order="C")
    atomic_write_bytes(volume_path, volume_bytes)
    scene["frame_id"] = frame_id
    scene["volume"]["sha256"] = sha256_bytes(volume_bytes)
    scene_path = profile_dir / "scene.json"
    atomic_write_json(scene_path, scene)
    return scene_path


def generate_sample_suite(
    root: Path | str,
    dims: tuple[int, int, int] = (56, 44, 36),
    width: int = 64,
    height: int = 48,
) -> dict[str, Any]:
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)
    bounds_min = np.asarray([-0.6, -0.45, -0.4], dtype=np.float64)
    bounds_max = np.asarray([0.6, 0.45, 0.4], dtype=np.float64)
    density = _density_grid(dims, bounds_min, bounds_max)
    placeholder_hash = "0" * 64
    base = base_scene_spec(
        VALID_PROFILE, dims, placeholder_hash, width=width, height=height
    )

    scene_paths: dict[str, Path] = {}
    scene_paths[VALID_PROFILE] = _write_profile(
        root, VALID_PROFILE, copy.deepcopy(base), density
    )

    truncated = root / "invalid_truncated"
    truncated.mkdir(parents=True, exist_ok=True)
    full_bytes = np.asarray(density, dtype="<f4").tobytes(order="C")
    atomic_write_bytes(truncated / "volume.f32", full_bytes[: len(full_bytes) // 2])
    truncated_scene = copy.deepcopy(base)
    truncated_scene["frame_id"] = "invalid_truncated"
    truncated_scene["volume"]["sha256"] = sha256_bytes(full_bytes)
    atomic_write_json(truncated / "scene.json", truncated_scene)
    scene_paths["invalid_truncated"] = truncated / "scene.json"

    nan_density = density.copy()
    nan_density[9, 11, 13] = np.nan
    scene_paths["invalid_nan"] = _write_profile(
        root, "invalid_nan", copy.deepcopy(base), nan_density
    )

    overlap_scene = copy.deepcopy(base)
    overlap_scene["transfer"]["segments"][1]["range"] = [0.2, 0.55]
    scene_paths["invalid_tf_overlap"] = _write_profile(
        root, "invalid_tf_overlap", overlap_scene, density
    )

    uncovered_scene = copy.deepcopy(base)
    uncovered_scene["transfer"]["segments"][-1]["range"] = [0.8, 0.9]
    scene_paths["invalid_tf_uncovered"] = _write_profile(
        root, "invalid_tf_uncovered", uncovered_scene, density
    )

    light_inside_scene = copy.deepcopy(base)
    light_inside_scene["lights"][0]["position"] = [0.0, 0.0, 0.0]
    scene_paths["invalid_light_inside"] = _write_profile(
        root, "invalid_light_inside", light_inside_scene, density
    )

    camera_scene = copy.deepcopy(base)
    camera_scene["camera"]["fov_y_degrees"] = 190.0
    scene_paths["invalid_camera"] = _write_profile(
        root, "invalid_camera", camera_scene, density
    )

    range_scene = copy.deepcopy(base)
    range_density = density.copy()
    range_density[3, 4, 5] = 1.25
    scene_paths["invalid_range_violation"] = _write_profile(
        root, "invalid_range_violation", range_scene, range_density
    )

    boundary_scene = copy.deepcopy(base)
    boundary_scene["transfer"]["transition_width"] = 0.0
    boundary_scene["transfer"]["segments"][1]["emission"][0] = [
        0.09,
        0.12,
        0.2,
    ]
    scene_paths["invalid_tf_boundary"] = _write_profile(
        root, "invalid_tf_boundary", boundary_scene, density
    )

    manifest = {
        "schema_version": 1,
        "frames": [
            {
                "frame_id": frame_id,
                "scene": str(scene_paths[frame_id].relative_to(root)),
            }
            for frame_id in (VALID_PROFILE, *INVALID_PROFILES)
        ],
    }
    atomic_write_json(root / "batch_manifest.json", manifest)
    summary = {
        "root": str(root),
        "valid_scene": str(scene_paths[VALID_PROFILE]),
        "batch_manifest": str(root / "batch_manifest.json"),
        "profiles": {
            frame_id: str(path) for frame_id, path in scene_paths.items()
        },
    }
    atomic_write_json(root / "sample_manifest.json", summary)
    return summary


def clone_valid_scene(
    source_scene: Path | str, destination_dir: Path | str
) -> tuple[Path, dict[str, Any], np.ndarray]:
    source_scene = Path(source_scene)
    destination_dir = Path(destination_dir)
    destination_dir.mkdir(parents=True, exist_ok=True)
    scene = json.loads(source_scene.read_text(encoding="utf-8"))
    source_volume = source_scene.parent / scene["volume"]["data_file"]
    raw = source_volume.read_bytes()
    destination_volume = destination_dir / "volume.f32"
    atomic_write_bytes(destination_volume, raw)
    density = np.frombuffer(raw, dtype="<f4").copy()
    scene_path = destination_dir / "scene.json"
    atomic_write_json(scene_path, scene)
    return scene_path, scene, density

