from __future__ import annotations

import json
import tempfile
from pathlib import Path
from typing import Any

from .canonical import DeterministicRNG, canonical_json_bytes, qfloat, sha256_json, stable_int
from .params import RenderParams
from .pipeline import RenderPipeline
from .scene import parse_scene
from .storage import Store

CASE_SPECS = (
    {
        "name": "sparse",
        "columns": 2,
        "rows": 2,
        "outline": (3.2, 9.0, 1.4, 7.5),
        "shadow": (1.8, 11.0, 1.0, 36.0),
        "smear": (10.0, 14.0, 18.0, 3.0),
        "grid_size": 180.0,
    },
    {
        "name": "balanced",
        "columns": 3,
        "rows": 2,
        "outline": (2.8, 7.2, 1.1, 6.5),
        "shadow": (1.55, 8.2, 0.8, 42.0),
        "smear": (8.5, 11.5, 15.0, 2.6),
        "grid_size": 150.0,
    },
    {
        "name": "dense",
        "columns": 3,
        "rows": 3,
        "outline": (2.2, 5.4, 0.85, 4.8),
        "shadow": (1.25, 5.6, 0.65, 51.0),
        "smear": (7.2, 9.0, 12.5, 2.1),
        "grid_size": 128.0,
    },
)

PALETTE = (
    "#d9a06f",
    "#7ca6b8",
    "#b8c17a",
    "#c78b8b",
    "#8e9bc9",
    "#d7be72",
    "#82b59a",
    "#b08bbf",
    "#c58d63",
)


def _zone_path(
    zone_id: str,
    x0: float,
    y0: float,
    width: float,
    height: float,
    rng: DeterministicRNG,
) -> dict[str, Any]:
    jitter_x = rng.uniform(-0.025, 0.025) * width
    jitter_y = rng.uniform(-0.025, 0.025) * height
    start = (x0, y0 + height * 0.48)
    top_curve_control = (
        x0 + width * 0.28 + jitter_x,
        y0 + height * 0.02 + jitter_y,
    )
    top_curve_end = (x0 + width * 0.58, y0 + height * 0.07)
    right_curve_control = (
        x0 + width * 0.985,
        y0 + height * (0.52 + rng.uniform(-0.04, 0.04)),
    )
    right_curve_end = (x0 + width * 0.84, y0 + height * 0.88)
    bottom_left = (x0 + width * 0.22, y0 + height * 0.93)
    left_top = (x0, y0 + height * 0.16)
    segments = [
        {"type": "line", "to": [left_top[0], left_top[1]]},
        {
            "type": "quadratic",
            "control": [top_curve_control[0], top_curve_control[1]],
            "to": [top_curve_end[0], top_curve_end[1]],
        },
        {"type": "line", "to": [x0 + width, y0 + height * 0.24]},
        {
            "type": "quadratic",
            "control": [right_curve_control[0], right_curve_control[1]],
            "to": [right_curve_end[0], right_curve_end[1]],
        },
        {"type": "line", "to": [bottom_left[0], bottom_left[1]]},
        {"type": "line", "to": [start[0], start[1]]},
    ]
    return {
        "id": f"path_{zone_id}",
        "closed": True,
        "start": [qfloat(start[0]), qfloat(start[1])],
        "segments": segments,
    }


def _make_scene(spec: dict[str, Any], seed: int) -> dict[str, Any]:
    width = 960.0
    height = 640.0
    margin = 42.0
    gap = 24.0
    columns = int(spec["columns"])
    rows = int(spec["rows"])
    cell_width = (width - margin * 2 - gap * (columns - 1)) / columns
    cell_height = (height - margin * 2 - gap * (rows - 1)) / rows
    zone_ids: list[str] = []
    paths: list[dict[str, Any]] = []
    zones: list[dict[str, Any]] = []
    index = 0
    for row in range(rows):
        for column in range(columns):
            zone_id = f"zone_{index:02d}"
            zone_ids.append(zone_id)
            rng = DeterministicRNG(stable_int(seed, spec["name"], "shape", index))
            x0 = margin + column * (cell_width + gap)
            y0 = margin + row * (cell_height + gap)
            inset_x = (cell_width - cell_width * 0.86) * 0.5
            inset_y = (cell_height - cell_height * 0.84) * 0.5
            paths.append(
                _zone_path(
                    zone_id,
                    x0 + inset_x,
                    y0 + inset_y,
                    cell_width * 0.86,
                    cell_height * 0.84,
                    rng,
                )
            )
            zones.append(
                {
                    "id": zone_id,
                    "label": f"Region {index:02d}",
                    "fill": PALETTE[(index + seed) % len(PALETTE)],
                    "path": f"path_{zone_id}",
                }
            )
            index += 1
    rotation = stable_int(seed, spec["name"], "draw-order") % len(zone_ids)
    draw_order = zone_ids[rotation:] + zone_ids[:rotation]
    return {
        "schema": "npr-scene-v1",
        "canvas": {"width": width, "height": height},
        "draw_order": draw_order,
        "paths": paths,
        "zones": zones,
    }


def _make_params(spec: dict[str, Any], seed: int) -> dict[str, Any]:
    outline = spec["outline"]
    shadow = spec["shadow"]
    smear = spec["smear"]
    return {
        "schema": "npr-params-v1",
        "grid_size": spec["grid_size"],
        "outline": {
            "width": outline[0],
            "spacing": outline[1],
            "jitter": outline[2],
            "length": outline[3],
            "color": "#24282e",
            "alpha": 1.0,
            "seed": stable_int(seed, spec["name"], "outline") % (1 << 31),
        },
        "shadow": {
            "width": shadow[0],
            "spacing": shadow[1],
            "jitter": shadow[2],
            "angle_deg": shadow[3],
            "color": "#414750",
            "alpha": 0.76,
            "reject_margin": 0.75,
            "seed": stable_int(seed, spec["name"], "shadow") % (1 << 31),
        },
        "smear": {
            "width": smear[0],
            "spacing": smear[1],
            "length": smear[2],
            "jitter": smear[3],
            "angle_deg": -14.0,
            "opacity": 0.4,
            "min_opacity": 0.09,
            "density_radius": 18.0,
            "density_weight": 0.7,
            "seed": stable_int(seed, spec["name"], "smear") % (1 << 31),
        },
    }


def _baseline(scene_raw: dict[str, Any], params_raw: dict[str, Any]) -> dict[str, Any]:
    scene = parse_scene(scene_raw)
    params = RenderParams.from_dict(params_raw)
    with tempfile.TemporaryDirectory(prefix="npr-baseline-") as temporary:
        store = Store(Path(temporary))
        result = RenderPipeline(store).render(scene, params)
        layer_hashes = {}
        for layer in (0, 1, 2):
            hashes = sorted(
                str(record["hash"])
                for record in result.manifest["batches"]
                if int(record["layer"]) == layer
            )
            layer_hashes[str(layer)] = sha256_json(hashes)
        return {
            "version": result.version,
            "version_hash": result.version_hash,
            "manifest_hash": result.manifest["manifest_hash"],
            "scene_id": scene.scene_id,
            "params_hash": params.params_hash,
            "batch_count": len(result.manifest["batches"]),
            "layer_hashes": layer_hashes,
        }


def generate_samples(
    root: str | Path,
    *,
    seed: int = 20261122,
    render_baselines: bool = True,
) -> dict[str, Any]:
    output = Path(root)
    output.mkdir(parents=True, exist_ok=True)
    manifest: dict[str, Any] = {
        "schema": "npr-sample-manifest-v1",
        "seed": seed,
        "cases": [],
    }
    for spec in CASE_SPECS:
        case_dir = output / spec["name"]
        case_dir.mkdir(parents=True, exist_ok=True)
        scene_raw = _make_scene(spec, seed)
        params_raw = _make_params(spec, seed)
        scene = parse_scene(scene_raw)
        params = RenderParams.from_dict(params_raw)
        scene_path = case_dir / "scene.json"
        params_path = case_dir / "params.json"
        scene_path.write_bytes(canonical_json_bytes(scene_raw) + b"\n")
        params_path.write_bytes(canonical_json_bytes(params_raw) + b"\n")
        baseline = (
            _baseline(scene_raw, params_raw)
            if render_baselines
            else {
                "version": None,
                "version_hash": None,
                "manifest_hash": None,
                "scene_id": scene.scene_id,
                "params_hash": params.params_hash,
                "batch_count": 0,
                "layer_hashes": {},
            }
        )
        baseline_path = case_dir / "baseline.json"
        baseline_path.write_bytes(canonical_json_bytes(baseline) + b"\n")
        manifest["cases"].append(
            {
                "name": spec["name"],
                "zone_count": spec["columns"] * spec["rows"],
                "grid_size": spec["grid_size"],
                "scene": f"{spec['name']}/scene.json",
                "params": f"{spec['name']}/params.json",
                "baseline": f"{spec['name']}/baseline.json",
                "scene_id": scene.scene_id,
                "params_hash": params.params_hash,
                "version_hash": baseline["version_hash"],
            }
        )
    (output / "manifest.json").write_bytes(canonical_json_bytes(manifest) + b"\n")
    return manifest


def load_sample(path: str | Path) -> tuple[dict[str, Any], dict[str, Any]]:
    scene_raw = json.loads((Path(path) / "scene.json").read_text(encoding="utf-8"))
    params_raw = json.loads((Path(path) / "params.json").read_text(encoding="utf-8"))
    return scene_raw, params_raw


__all__ = ["CASE_SPECS", "generate_samples", "load_sample"]
