from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]


def run_cli(*args: str, env: dict[str, str] | None = None, timeout: float = 60.0) -> subprocess.CompletedProcess[str]:
    command = [sys.executable, "-m", "leather_raster", *args]
    return subprocess.run(
        command,
        cwd=ROOT,
        text=True,
        capture_output=True,
        timeout=timeout,
        env=env,
    )


def write_scene(directory: Path, **overrides: Any) -> tuple[Path, dict[str, Any]]:
    from leather_raster.sample import write_sample_scene

    path = directory / "scene.json"
    scene = write_sample_scene(path, **overrides)
    return path, scene


def load_json(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def first_solid_triangle(scene: dict[str, Any], layer_position: int = 0) -> tuple[int, int, int]:
    layer = scene["layers"][layer_position]
    cutout_indices = {
        triangle_index
        for cutout in layer["cutouts"]
        for triangle_index in cutout["triangle_indices"]
    }
    for index, triangle in enumerate(layer["mesh"]["triangles"]):
        if index not in cutout_indices:
            return int(triangle[0]), int(triangle[1]), int(triangle[2])
    raise AssertionError("sample scene has no solid triangle")

