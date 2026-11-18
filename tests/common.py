from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import numpy as np

from fogvol.sample_scene import scene_spec


ROOT = Path(__file__).resolve().parents[1]
LUMINANCE = np.asarray([0.2126, 0.7152, 0.0722], dtype=np.float64)


def compact_scene() -> dict[str, Any]:
    raw = copy.deepcopy(scene_spec())
    raw["camera"]["width"] = 80
    raw["camera"]["height"] = 60
    raw["camera"]["aspect"] = 80.0 / 60.0
    raw["render"]["substeps"] = 2
    raw["render"]["max_samples"] = 512
    raw["render"]["chunk_size"] = [20, 15]
    return raw


def write_json(path: Path, document: dict[str, Any]) -> None:
    path.write_text(
        json.dumps(
            document,
            ensure_ascii=False,
            allow_nan=True,
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )

