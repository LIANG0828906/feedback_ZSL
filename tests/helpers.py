from __future__ import annotations

import copy
import json
import subprocess
import sys
from pathlib import Path
from typing import Any

from rosewindow.sample_scene import build_sample_scene
from rosewindow.scene import parse_scene


ROOT = Path(__file__).resolve().parents[1]


def small_scene_raw() -> dict[str, Any]:
    raw = copy.deepcopy(build_sample_scene())
    raw["image"]["width"] = 320
    raw["image"]["height"] = 200
    raw["image"]["chunk_size"] = 64
    return raw


def write_json(path: Path, value: Any) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def run_cli(*arguments: str, timeout: float = 60.0) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-m", "rosewindow", *arguments],
        cwd=ROOT,
        text=True,
        capture_output=True,
        timeout=timeout,
        check=False,
    )


def completed_scene_small():
    return parse_scene(small_scene_raw())
