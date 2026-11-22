"""Deterministic local sample scenes and analytic truth generation."""

from __future__ import annotations

import hashlib
import json
import math
import random
from pathlib import Path
from typing import Any

import numpy as np

from .engine import NprEngine
from .model import Camera, RenderParams, Scene, Snapshot
from .scene import save_scene, validate_scene_data

GENERATOR_VERSION = "npr-samples-v1"


def _camera_poses(seed: int, base_position: tuple[float, float, float]) -> tuple[Camera, ...]:
    target = np.asarray([0.0, 0.12, 0.0], dtype=np.float64)
    distance = math.sqrt(base_position[0] ** 2 + base_position[2] ** 2)
    base_angle = math.atan2(base_position[2], base_position[0])
    poses: list[Camera] = []
    offsets = (-0.38, -0.19, 0.0, 0.20, 0.40)
    for index, offset in enumerate(offsets):
        angle = base_angle + offset
        height = base_position[1] + math.sin(offset * 2.0 + index * 0.17) * 0.22
        position = (
            float(math.cos(angle) * distance),
            float(height),
            float(math.sin(angle) * distance),
        )
        target_shift = (
            math.sin(seed * 0.01 + index) * 0.06,
            0.12 + math.cos(seed * 0.02 + index) * 0.05,
            0.0,
        )
        poses.append(
            Camera(
                id=f"pose_{index}",
                position=position,
                target=tuple(float(v) for v in (target + np.asarray(target_shift))),
                up=(0.0, 1.0, 0.0),
                fov_y=43.0,
                near=0.05,
                far=100.0,
            )
        )
    return tuple(poses)


def make_sample_scene(name: str, seed: int) -> Scene:
    rng = random.Random(seed)

    def jitter(color: list[float], amount: float = 0.035) -> list[float]:
        return [float(np.clip(channel + rng.uniform(-amount, amount), 0.0, 1.0)) for channel in color]

    if name == "orb_and_crate":
        objects = [
            {
                "kind": "plane",
                "name": "floor",
                "point": [0.0, -0.82, 0.0],
                "normal": [0.0, 1.0, 0.0],
                "color": jitter([0.49, 0.47, 0.41]),
                "roughness": 0.82,
                "specular": 0.08,
            },
            {
                "kind": "sphere",
                "name": "coral_orb",
                "center": [-0.72, 0.10, 0.02],
                "radius": 0.92,
                "color": jitter([0.91, 0.31, 0.20]),
                "roughness": 0.38,
                "specular": 0.42,
            },
            {
                "kind": "box",
                "name": "teal_crate",
                "center": [0.82, 0.02, 0.25],
                "half_extents": [0.58, 0.70, 0.56],
                "color": jitter([0.10, 0.53, 0.55]),
                "roughness": 0.58,
                "specular": 0.22,
            },
            {
                "kind": "sphere",
                "name": "gold_moon",
                "center": [1.02, 1.02, -0.86],
                "radius": 0.30,
                "color": jitter([0.93, 0.68, 0.24]),
                "roughness": 0.28,
                "specular": 0.58,
            },
        ]
        base_position = (3.85, 2.15, 4.30)
        background = {"top": [0.08, 0.14, 0.24], "bottom": [0.32, 0.40, 0.46]}
        lights = [
            {"direction": [-0.42, 0.82, 0.39], "color": [1.0, 0.94, 0.82], "intensity": 1.05}
        ]
    elif name == "twin_signal":
        objects = [
            {
                "kind": "plane",
                "name": "floor",
                "point": [0.0, -0.92, 0.0],
                "normal": [0.0, 1.0, 0.0],
                "color": jitter([0.40, 0.45, 0.47]),
                "roughness": 0.74,
                "specular": 0.10,
            },
            {
                "kind": "sphere",
                "name": "violet_orb",
                "center": [-0.74, 0.12, 0.12],
                "radius": 0.80,
                "color": jitter([0.42, 0.29, 0.72]),
                "roughness": 0.40,
                "specular": 0.37,
            },
            {
                "kind": "sphere",
                "name": "ember_orb",
                "center": [0.66, -0.18, -0.20],
                "radius": 0.54,
                "color": jitter([0.92, 0.43, 0.18]),
                "roughness": 0.32,
                "specular": 0.52,
            },
            {
                "kind": "box",
                "name": "signal_block",
                "center": [0.12, -0.34, 0.90],
                "half_extents": [0.86, 0.36, 0.42],
                "color": jitter([0.14, 0.42, 0.62]),
                "roughness": 0.68,
                "specular": 0.18,
            },
        ]
        base_position = (4.15, 1.72, 3.90)
        background = {"top": [0.09, 0.10, 0.18], "bottom": [0.31, 0.36, 0.42]}
        lights = [
            {"direction": [-0.58, 0.68, 0.44], "color": [0.92, 0.90, 1.0], "intensity": 0.90},
            {"direction": [0.42, 0.52, -0.54], "color": [1.0, 0.64, 0.36], "intensity": 0.36},
        ]
    elif name == "signal_garden":
        objects = [
            {
                "kind": "plane",
                "name": "floor",
                "point": [0.0, -0.88, 0.0],
                "normal": [0.0, 1.0, 0.0],
                "color": jitter([0.44, 0.47, 0.38]),
                "roughness": 0.80,
                "specular": 0.07,
            },
            {
                "kind": "box",
                "name": "jade_pillar",
                "center": [-0.82, -0.02, 0.0],
                "half_extents": [0.50, 0.84, 0.50],
                "color": jitter([0.12, 0.48, 0.34]),
                "roughness": 0.57,
                "specular": 0.24,
            },
            {
                "kind": "sphere",
                "name": "cream_orb",
                "center": [0.52, 0.22, 0.10],
                "radius": 0.86,
                "color": jitter([0.90, 0.79, 0.58]),
                "roughness": 0.36,
                "specular": 0.40,
            },
            {
                "kind": "sphere",
                "name": "signal_red",
                "center": [1.13, 0.94, -0.34],
                "radius": 0.34,
                "color": jitter([0.87, 0.20, 0.16]),
                "roughness": 0.25,
                "specular": 0.62,
            },
        ]
        base_position = (3.70, 2.00, 4.55)
        background = {"top": [0.07, 0.15, 0.17], "bottom": [0.35, 0.42, 0.38]}
        lights = [
            {"direction": [-0.30, 0.86, 0.42], "color": [1.0, 0.92, 0.74], "intensity": 1.02}
        ]
    else:
        raise ValueError(f"unknown sample scene {name!r}")

    raw = {
        "schema_version": 1,
        "name": name,
        "seed": seed,
        "objects": objects,
        "camera_poses": [camera.to_dict() for camera in _camera_poses(seed, base_position)],
        "lights": lights,
        "background": background,
        "metadata": {
            "generator": GENERATOR_VERSION,
            "description": "Deterministic local NPR sample with sphere, plane and box geometry.",
        },
    }
    return validate_scene_data(raw, path=f"<generated:{name}>")


def truth_params() -> RenderParams:
    return RenderParams(
        levels=5,
        density=0.34,
        length=12.0,
        preview_width=120,
        preview_height=90,
        exact_width=360,
        exact_height=270,
        tile_width=40,
        tile_height=40,
        max_workers=4,
        thread_pixel_budget=32768,
        stroke_cache_budget=512,
        guide_width=96,
        guide_height=72,
        stroke_reference_width=320,
    )


def _truth_for_frame(
    scene: Scene,
    camera: Camera,
    frame: Any,
    *,
    sample_count: int = 22,
) -> dict[str, Any]:
    rng = random.Random(int(scene.seed) * 1009 + int(camera.id.rsplit("_", 1)[-1]))
    outline_coords = np.argwhere(frame.outline)
    coverage_coords = np.argwhere(frame.coverage)
    all_hits = np.argwhere(frame.levels >= 0)
    selected: list[tuple[int, int]] = []
    if outline_coords.size:
        picks = rng.sample(range(int(outline_coords.shape[0])), min(7, int(outline_coords.shape[0])))
        selected.extend((int(outline_coords[index][1]), int(outline_coords[index][0])) for index in picks)
    if coverage_coords.size:
        picks = rng.sample(
            range(int(coverage_coords.shape[0])), min(5, int(coverage_coords.shape[0]))
        )
        selected.extend(
            (int(coverage_coords[index][1]), int(coverage_coords[index][0])) for index in picks
        )
    picks = rng.sample(range(int(all_hits.shape[0])), min(12, int(all_hits.shape[0])))
    selected.extend((int(all_hits[index][1]), int(all_hits[index][0])) for index in picks)
    deduped: list[tuple[int, int]] = []
    seen: set[tuple[int, int]] = set()
    for x, y in selected:
        if (x, y) not in seen:
            deduped.append((x, y))
            seen.add((x, y))
        if len(deduped) >= sample_count:
            break
    samples = []
    alpha = frame.brush_rgba[..., 3].astype(np.float64) / 255.0
    for x, y in deduped:
        x0, x1 = max(0, x - 3), min(frame.width, x + 4)
        y0, y1 = max(0, y - 3), min(frame.height, y + 4)
        samples.append(
            {
                "x": x,
                "y": y,
                "expected_level": int(frame.levels[y, x]),
                "expected_outline": bool(frame.outline[y, x]),
                "brush_coverage_7x7": float(np.mean(alpha[y0:y1, x0:x1])),
            }
        )
    histogram = np.bincount(
        frame.levels.ravel(), minlength=frame.snapshot.params.levels
    ).astype(np.float64) / float(frame.levels.size)
    return {
        "camera_id": camera.id,
        "camera": camera.to_dict(),
        "width": frame.width,
        "height": frame.height,
        "levels": frame.snapshot.params.levels,
        "density": frame.snapshot.params.density,
        "length": frame.snapshot.params.length,
        "level_histogram": [float(value) for value in histogram],
        "outline_fraction": float(np.mean(frame.outline)),
        "stroke_alpha_density": float(np.mean(alpha)),
        "style_sha256": hashlib.sha256(frame.styled_rgb.tobytes()).hexdigest(),
        "samples": samples,
    }


def generate_truth(
    scene: Scene,
    *,
    persist_dir: str | Path = "persist/truth-build",
) -> dict[str, Any]:
    params = truth_params()
    engine = NprEngine(
        scene,
        params,
        scene.camera_poses[0],
        persist_dir=persist_dir,
        diagnostics_dir="diagnostics/truth-build",
        session_id=f"truth-{scene.content_hash()[:12]}",
    )
    poses = []
    for camera in scene.camera_poses:
        frame = engine.render_one_shot(
            snapshot=Snapshot(
                version=1,
                content_hash=scene.content_hash(),
                camera=camera,
                params=params,
            )
        )
        poses.append(_truth_for_frame(scene, camera, frame))
    return {
        "schema": 1,
        "generator": GENERATOR_VERSION,
        "scene": scene.name,
        "scene_hash": scene.content_hash(),
        "params": params.to_dict(),
        "camera_poses": poses,
    }


def generate_all(out_dir: str | Path) -> dict[str, Any]:
    root = Path(out_dir)
    scenes_dir = root / "scenes"
    truth_dir = root / "truth"
    cases_dir = root / "cases"
    for directory in (scenes_dir, truth_dir, cases_dir):
        directory.mkdir(parents=True, exist_ok=True)
    specs = [
        ("orb_and_crate", 1041),
        ("twin_signal", 2077),
        ("signal_garden", 3119),
    ]
    summary: list[dict[str, Any]] = []
    for name, seed in specs:
        scene = make_sample_scene(name, seed)
        scene_path = scenes_dir / f"{name}.json"
        save_scene(scene, scene_path)
        truth = generate_truth(scene, persist_dir=root.parent / "persist" / "truth-build")
        truth_path = truth_dir / f"{name}.truth.json"
        truth_path.write_text(
            json.dumps(truth, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
            encoding="utf-8",
        )
        summary.append(
            {
                "scene": name,
                "seed": seed,
                "scene_file": str(scene_path),
                "truth_file": str(truth_path),
                "content_hash": scene.content_hash(),
                "camera_poses": len(scene.camera_poses),
            }
        )
    (cases_dir / "corrupt_scene.json").write_text(
        '{"schema_version": 1, "name": "broken", "objects": [',
        encoding="utf-8",
    )
    (cases_dir / "invalid_scene_schema.json").write_text(
        json.dumps(
            {
                "schema_version": 1,
                "name": "invalid-kind",
                "objects": [{"kind": "torus", "color": [1, 1, 1]}],
                "camera_poses": [],
            },
            indent=2,
        )
        + "\n",
        encoding="utf-8",
    )
    invalid_params = truth_params().to_dict()
    invalid_params.update({"levels": 1, "density": 1.4, "length": 0.0})
    (cases_dir / "invalid_params.json").write_text(
        json.dumps(invalid_params, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    (cases_dir / "fault_tiles.json").write_text(
        json.dumps(
            {
                "tile_ids": [3, 7],
                "reason": "local deterministic fault injection",
            },
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    manifest = {
        "schema": 1,
        "generator": GENERATOR_VERSION,
        "scenes": summary,
        "cases": {
            "corrupt_scene": str(cases_dir / "corrupt_scene.json"),
            "invalid_scene_schema": str(cases_dir / "invalid_scene_schema.json"),
            "invalid_params": str(cases_dir / "invalid_params.json"),
            "fault_tiles": str(cases_dir / "fault_tiles.json"),
        },
    }
    (root / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
        encoding="utf-8",
    )
    return manifest


def verify_truth(scene: Scene, truth: dict[str, Any]) -> dict[str, Any]:
    if truth.get("scene_hash") != scene.content_hash():
        return {
            "passed": False,
            "reason": "scene hash mismatch",
            "expected": truth.get("scene_hash"),
            "actual": scene.content_hash(),
        }
    params = RenderParams.from_dict(truth["params"])
    engine = NprEngine(
        scene,
        params,
        scene.camera_poses[0],
        persist_dir="persist/truth-verify",
        diagnostics_dir="diagnostics/truth-verify",
    )
    mismatches: list[dict[str, Any]] = []
    checked = 0
    for pose_truth in truth["camera_poses"]:
        camera = Camera.from_dict(pose_truth["camera"])
        snapshot = Snapshot(
            version=1,
            content_hash=scene.content_hash(),
            camera=camera,
            params=params,
        )
        frame = engine.render_one_shot(snapshot=snapshot)
        style_sha = hashlib.sha256(frame.styled_rgb.tobytes()).hexdigest()
        if style_sha != pose_truth.get("style_sha256"):
            mismatches.append(
                {
                    "camera": camera.id,
                    "field": "style_sha256",
                    "expected": pose_truth.get("style_sha256"),
                    "actual": style_sha,
                }
            )
        alpha = frame.brush_rgba[..., 3].astype(np.float64) / 255.0
        for sample in pose_truth["samples"]:
            checked += 1
            x, y = int(sample["x"]), int(sample["y"])
            if int(frame.levels[y, x]) != int(sample["expected_level"]):
                mismatches.append(
                    {
                        "camera": camera.id,
                        "pixel": [x, y],
                        "field": "expected_level",
                        "expected": sample["expected_level"],
                        "actual": int(frame.levels[y, x]),
                    }
                )
            if bool(frame.outline[y, x]) != bool(sample["expected_outline"]):
                mismatches.append(
                    {
                        "camera": camera.id,
                        "pixel": [x, y],
                        "field": "expected_outline",
                        "expected": sample["expected_outline"],
                        "actual": bool(frame.outline[y, x]),
                    }
                )
            x0, x1 = max(0, x - 3), min(frame.width, x + 4)
            y0, y1 = max(0, y - 3), min(frame.height, y + 4)
            coverage = float(np.mean(alpha[y0:y1, x0:x1]))
            if abs(coverage - float(sample["brush_coverage_7x7"])) > 1e-6:
                mismatches.append(
                    {
                        "camera": camera.id,
                        "pixel": [x, y],
                        "field": "brush_coverage_7x7",
                        "expected": sample["brush_coverage_7x7"],
                        "actual": coverage,
                    }
                )
    return {"passed": not mismatches, "checked_samples": checked, "mismatches": mismatches[:20]}

