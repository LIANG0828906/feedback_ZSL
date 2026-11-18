from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any

from glass_verify.sample import make_sample_scene

ROOT = Path(__file__).resolve().parents[1]


def run_cli(*args: str, timeout: float = 120.0) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-m", "glass_verify", *args],
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False) + "\n",
        encoding="utf-8",
    )


def compact_scene(seed: int = 424242) -> dict[str, Any]:
    raw = make_sample_scene(seed)
    raw["camera"]["width"] = 60
    raw["camera"]["height"] = 40
    raw["render"]["chunk_size"] = [20, 20]
    raw["render"]["supersample"] = 1
    raw["projection"]["samples_per_axis"] = 3
    raw["projection"]["profile_points"] = 9
    raw["projection"]["depth_levels"] = 3
    return raw
