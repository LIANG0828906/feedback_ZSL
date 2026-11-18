from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from .snapshot import TileLayout
from .util import write_json_atomic

MAX_DIFFERENCES = 2000


def _load_rgba(path: Path) -> np.ndarray:
    with Image.open(path) as image:
        return np.asarray(image.convert("RGBA"), dtype=np.uint8)


def _load_owner(path: Path) -> np.ndarray:
    with Image.open(path) as image:
        return np.asarray(image, dtype=np.uint16)


def _tile_index_for_pixel(
    layout: TileLayout,
    x: int,
    y: int,
) -> int:
    tile_x = x // layout.tile_width
    tile_y = y // layout.tile_height
    return tile_y * layout.tiles_x + tile_x


def compare_outputs(
    output_dir: str | Path,
    reference_dir: str | Path,
    layout: TileLayout,
) -> dict[str, Any]:
    output_dir = Path(output_dir)
    reference_dir = Path(reference_dir)
    differences: list[dict[str, Any]] = []
    total_differences = 0
    image_names = (
        ("render.png", ("R", "G", "B", "A"), _load_rgba),
        (
            "fabric_composite.png",
            ("FABRIC_R", "FABRIC_G", "FABRIC_B", "FABRIC_A"),
            _load_rgba,
        ),
        ("ownership.png", ("OWNER",), _load_owner),
    )
    for filename, channels, loader in image_names:
        actual_path = output_dir / filename
        expected_path = reference_dir / filename
        if not actual_path.exists() or not expected_path.exists():
            total_differences += 1
            if len(differences) < MAX_DIFFERENCES:
                differences.append(
                    {
                        "tile_index": 0,
                        "x": 0,
                        "y": 0,
                        "channel": "FILE",
                        "actual": actual_path.exists(),
                        "expected": expected_path.exists(),
                    }
                )
            continue
        actual = loader(actual_path)
        expected = loader(expected_path)
        if actual.shape != expected.shape:
            total_differences += 1
            if len(differences) < MAX_DIFFERENCES:
                differences.append(
                    {
                        "tile_index": 0,
                        "x": 0,
                        "y": 0,
                        "channel": "SHAPE",
                        "actual": list(actual.shape),
                        "expected": list(expected.shape),
                    }
                )
            continue
        unequal = actual != expected
        if np.any(unequal):
            coordinates = np.argwhere(unequal)
            total_differences += int(len(coordinates))
            for coordinate in coordinates[: MAX_DIFFERENCES - len(differences)]:
                y = int(coordinate[0])
                x = int(coordinate[1])
                channel_index = (
                    int(coordinate[2]) if len(coordinate) == 3 else 0
                )
                differences.append(
                    {
                        "tile_index": _tile_index_for_pixel(layout, x, y),
                        "x": x,
                        "y": y,
                        "channel": channels[channel_index],
                        "actual": int(actual[tuple(int(v) for v in coordinate)]),
                        "expected": int(
                            expected[tuple(int(v) for v in coordinate)]
                        ),
                    }
                )
            if len(differences) >= MAX_DIFFERENCES:
                break

    projection_mismatches: list[dict[str, Any]] = []
    actual_report_path = output_dir / "render_report.json"
    reference_report_path = reference_dir / "render_report.json"
    if actual_report_path.exists() and reference_report_path.exists():
        with open(actual_report_path, "r", encoding="utf-8") as handle:
            actual_report = json.load(handle)
        with open(reference_report_path, "r", encoding="utf-8") as handle:
            reference_report = json.load(handle)
        actual_points = actual_report.get("projection", {}).get("points", [])
        reference_points = reference_report.get("projection", {}).get("points", [])
        if actual_points != reference_points:
            limit = min(len(actual_points), len(reference_points))
            for index in range(limit):
                if actual_points[index] != reference_points[index]:
                    projection_mismatches.append(
                        {
                            "projection_index": index,
                            "tassel_id": reference_points[index].get("tassel_id"),
                            "light_id": reference_points[index].get("light_id"),
                            "actual": actual_points[index],
                            "expected": reference_points[index],
                        }
                    )
            if len(actual_points) != len(reference_points):
                projection_mismatches.append(
                    {
                        "projection_index": limit,
                        "actual_count": len(actual_points),
                        "expected_count": len(reference_points),
                    }
                )
    elif actual_report_path.exists() != reference_report_path.exists():
        projection_mismatches.append(
            {
                "projection_index": 0,
                "actual_report": actual_report_path.exists(),
                "expected_report": reference_report_path.exists(),
            }
        )

    result = {
        "schema_version": "1.0",
        "status": "match"
        if total_differences == 0 and not projection_mismatches
        else "mismatch",
        "business_code": "OK"
        if total_differences == 0 and not projection_mismatches
        else "RESUME_MISMATCH",
        "difference_count": total_differences,
        "projection_mismatch_count": len(projection_mismatches),
        "truncated": total_differences > len(differences),
        "differences": differences,
        "projection_mismatches": projection_mismatches,
    }
    if result["status"] == "mismatch":
        write_json_atomic(output_dir / "differences.json", result)
    else:
        difference_path = output_dir / "differences.json"
        if difference_path.exists():
            difference_path.unlink()
    return result

