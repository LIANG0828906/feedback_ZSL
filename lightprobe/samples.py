from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .bake_types import BakeConfig
from .directions import make_directions
from .grid import build_grid
from .sampling import sample_radiance_batch
from .scene import load_scene_dict
from .sh import direct_irradiance
from .util import atomic_write_json, sha256_json

GENERATION_SEED = 19093
NORMALS = np.asarray(
    [
        [0.0, 1.0, 0.0],
        [0.0, -1.0, 0.0],
        [1.0, 0.0, 0.0],
        [-1.0, 0.0, 0.0],
        [0.0, 0.0, 1.0],
        [0.0, 0.0, -1.0],
    ],
    dtype=np.float64,
)


def _material(albedo: list[float], bounce: float) -> dict[str, Any]:
    return {"albedo": albedo, "bounce_gain": bounce}


def room_scene() -> dict[str, Any]:
    return {
        "schema_version": 1,
        "scene_id": "street_corner_room",
        "generation_seed": GENERATION_SEED,
        "bounds": {"min": [0.0, 0.0, 0.0], "max": [8.0, 4.0, 8.0]},
        "settings": {
            "probe_spacing": 2.0,
            "sh_order": 3,
            "direction_count": 64,
            "direction_seed": 7,
            "compensation_strength": 0.35,
            "strategy": "auto",
        },
        "sky": {
            "zenith_radiance": [0.86, 0.93, 1.10],
            "horizon_radiance": [0.63, 0.70, 0.82],
            "ground_radiance": [0.18, 0.15, 0.11],
            "intensity": 1.15,
        },
        "materials": {
            "plaster": _material([0.78, 0.76, 0.70], 0.20),
            "oak": _material([0.44, 0.27, 0.13], 0.12),
            "fabric": _material([0.18, 0.24, 0.34], 0.08),
            "metal": _material([0.55, 0.58, 0.62], 0.22),
        },
        "occluders": [
            {
                "id": "back_wall",
                "type": "aabb",
                "min": [0.0, 0.0, 7.55],
                "max": [8.0, 4.0, 8.0],
                "material": "plaster",
            },
            {
                "id": "table",
                "type": "aabb",
                "min": [2.2, 0.0, 2.4],
                "max": [5.3, 1.15, 4.3],
                "material": "oak",
            },
            {
                "id": "cabinet",
                "type": "aabb",
                "min": [0.0, 0.0, 0.0],
                "max": [1.5, 2.5, 2.2],
                "material": "oak",
            },
            {
                "id": "street_lamp",
                "type": "sphere",
                "center": [6.35, 2.75, 6.2],
                "radius": 0.55,
                "material": "metal",
            },
            {
                "id": "reading_chair",
                "type": "aabb",
                "min": [5.8, 0.0, 1.0],
                "max": [7.25, 1.25, 2.6],
                "material": "fabric",
            },
        ],
    }


def city_scene() -> dict[str, Any]:
    return {
        "schema_version": 1,
        "scene_id": "city_district",
        "generation_seed": GENERATION_SEED + 1,
        "bounds": {"min": [0.0, 0.0, 0.0], "max": [32.0, 10.0, 32.0]},
        "settings": {
            "probe_spacing": 2.0,
            "sh_order": 3,
            "direction_count": 64,
            "direction_seed": 11,
            "compensation_strength": 0.42,
            "strategy": "auto",
        },
        "sky": {
            "zenith_radiance": [0.64, 0.78, 1.15],
            "horizon_radiance": [0.92, 0.83, 0.66],
            "ground_radiance": [0.12, 0.11, 0.10],
            "intensity": 1.25,
        },
        "materials": {
            "concrete": _material([0.56, 0.55, 0.51], 0.16),
            "brick": _material([0.52, 0.27, 0.19], 0.13),
            "glass": _material([0.28, 0.38, 0.46], 0.28),
            "foliage": _material([0.12, 0.31, 0.13], 0.09),
        },
        "occluders": [
            {
                "id": "block_a",
                "type": "aabb",
                "min": [2.0, 0.0, 2.0],
                "max": [10.0, 8.0, 8.0],
                "material": "concrete",
            },
            {
                "id": "block_b",
                "type": "aabb",
                "min": [14.0, 0.0, 3.0],
                "max": [20.0, 12.0, 10.0],
                "material": "brick",
            },
            {
                "id": "block_c",
                "type": "aabb",
                "min": [22.0, 0.0, 1.0],
                "max": [30.0, 7.0, 7.0],
                "material": "glass",
            },
            {
                "id": "block_d",
                "type": "aabb",
                "min": [4.0, 0.0, 16.0],
                "max": [12.0, 9.0, 24.0],
                "material": "brick",
            },
            {
                "id": "block_e",
                "type": "aabb",
                "min": [16.0, 0.0, 18.0],
                "max": [26.0, 14.0, 27.0],
                "material": "concrete",
            },
            {
                "id": "block_f",
                "type": "aabb",
                "min": [28.0, 0.0, 14.0],
                "max": [31.0, 6.0, 22.0],
                "material": "glass",
            },
            {
                "id": "tree_a",
                "type": "sphere",
                "center": [12.0, 4.8, 12.5],
                "radius": 2.0,
                "material": "foliage",
            },
            {
                "id": "tree_b",
                "type": "sphere",
                "center": [21.5, 4.2, 20.0],
                "radius": 1.8,
                "material": "foliage",
            },
            {
                "id": "tree_c",
                "type": "sphere",
                "center": [7.0, 3.6, 13.0],
                "radius": 1.5,
                "material": "foliage",
            },
            {
                "id": "kiosk",
                "type": "aabb",
                "min": [13.0, 0.0, 11.0],
                "max": [16.0, 3.0, 14.0],
                "material": "concrete",
            },
        ],
    }


def generate_samples(output_dir: str | Path) -> dict[str, Any]:
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    scenes = {
        "street_corner_room": room_scene(),
        "city_district": city_scene(),
    }
    paths: dict[str, str] = {}
    for scene_id, payload in scenes.items():
        path = output / f"{scene_id}.json"
        atomic_write_json(path, payload)
        paths[scene_id] = str(path)
    baseline = build_baseline(scenes, truth_directions=256)
    baseline_path = output / "baseline_irradiance.json"
    atomic_write_json(baseline_path, baseline)
    suite = {
        "schema_version": 1,
        "generation_seed": GENERATION_SEED,
        "levels": ["street_corner_room", "city_district"],
        "scenes": paths,
        "baseline": str(baseline_path),
        "baseline_content_hash": baseline["content_hash"],
    }
    atomic_write_json(output / "suite.json", suite)
    return suite


def build_baseline(
    scenes: dict[str, dict[str, Any]], *, truth_directions: int
) -> dict[str, Any]:
    truth_set = make_directions(truth_directions, GENERATION_SEED + 7001)
    result: dict[str, Any] = {
        "schema_version": 1,
        "generation_seed": GENERATION_SEED,
        "truth": {
            "method": "direct_mc_diffuse_irradiance",
            "direction_count": truth_directions,
            "direction_seed": truth_set.seed,
            "direction_kind": truth_set.kind,
            "normals": NORMALS.tolist(),
            "quadrature": "4*pi/N * sum(radiance * max(n.dot(d),0))",
        },
        "levels": {},
    }
    for scene_id, payload in scenes.items():
        scene = load_scene_dict(payload, source_path=scene_id)
        config = BakeConfig.from_scene(scene).validated()
        grid = build_grid(scene, config.spacing)
        samples, _, _ = sample_radiance_batch(
            scene, grid.coordinates, truth_set.vectors
        )
        cosine = np.maximum(NORMALS @ truth_set.vectors.T, 0.0)
        irradiance = (4.0 * np.pi / truth_directions) * np.einsum(
            "nd,pdc->pnc", cosine, samples
        )
        probes = []
        for index, probe_id in enumerate(grid.probe_ids):
            probes.append(
                {
                    "probe_id": probe_id,
                    "coordinate_hash": grid.coordinate_hashes[index],
                    "position": [float(item) for item in grid.coordinates[index]],
                    "irradiance": [
                        [float(item) for item in vector] for vector in irradiance[index]
                    ],
                }
            )
        level = {
            "scene_id": scene_id,
            "grid_hash": grid.content_hash,
            "probe_count": grid.probe_count,
            "spacing": config.spacing,
            "probes": probes,
        }
        level["content_hash"] = sha256_json(
            {"scene_id": scene_id, "probes": probes, "truth": result["truth"]}
        )
        result["levels"][scene_id] = level
    result["content_hash"] = sha256_json(
        {"levels": result["levels"], "truth": result["truth"]}
    )
    return result


if __name__ == "__main__":  # pragma: no cover
    print(json.dumps(generate_samples("samples"), indent=2))

