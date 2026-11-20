from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def write_json(path: Path, value: Any) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    return path


def simple_config(
    root: Path,
    *,
    glyphs: list[str],
    tile_width: int = 6,
    tile_height: int = 6,
    pixel_size: float = 12,
    samples: int = 2,
    parallelism: int = 2,
    budget: dict[str, Any] | None = None,
    atlas_width: int = 128,
    atlas_height: int = 128,
) -> Path:
    document = {
        "format": "glyph-atlas-config/1",
        "font": "font.json",
        "glyphs": glyphs,
        "targets": [
            {
                "name": "t12",
                "pixel_size": pixel_size,
                "subpixel_x": 0.25,
                "subpixel_y": 0.125,
            }
        ],
        "tile": {"width": tile_width, "height": tile_height},
        "parallelism": parallelism,
        "samples": {"x": samples, "y": samples},
        "quantization_step": 1,
        "fill_rule": "nonzero",
        "budget": budget
        or {
            "max_task_sampling_points": 10_000_000,
            "max_task_memory_bytes": 1_000_000_000,
            "max_total_sampling_points": 100_000_000,
            "max_total_memory_bytes": 10_000_000_000,
        },
        "atlas": {"width": atlas_width, "height": atlas_height, "padding": 1},
        "coverage_tolerance": 0,
        "output_dir": "output",
        "snapshot_dir": "snapshots",
        "temp_dir": ".tmp",
    }
    return write_json(root / "config.json", document)


def square_hole_font(same_orientation: bool = True) -> dict[str, Any]:
    outer = {
        "points": [[100, 100], [900, 100], [900, 900], [100, 900]],
        "segments": [
            {"type": "line", "to": 1},
            {"type": "line", "to": 2},
            {"type": "line", "to": 3},
            {"type": "line", "to": 0},
        ],
    }
    inner_points = [[300, 300], [700, 300], [700, 700], [300, 700]]
    if not same_orientation:
        inner_points = [inner_points[0], inner_points[3], inner_points[2], inner_points[1]]
    inner = {
        "points": inner_points,
        "segments": [
            {"type": "line", "to": 1},
            {"type": "line", "to": 2},
            {"type": "line", "to": 3},
            {"type": "line", "to": 0},
        ],
    }
    return {
        "units_per_em": 1000,
        "default_fill_rule": "nonzero",
        "glyphs": [
            {
                "name": "hole",
                "unicode": 65,
                "advance_width": 1000,
                "contours": [outer, inner],
            }
        ],
    }

