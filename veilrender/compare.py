"""Structured pixel-level comparison for deterministic output verification."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from .errors import RenderConsistencyError
from .scene import canonical_json

_CHANNELS = ("R", "G", "B", "A")


def _read_rgba(path: Path) -> np.ndarray:
    with Image.open(path) as image:
        return np.asarray(image.convert("RGBA"), dtype=np.uint8)


def _tile_metadata(directory: Path) -> dict[str, Any] | None:
    path = directory / "run_meta.json"
    if not path.is_file():
        return None
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    return data if isinstance(data, dict) else None


def _tile_id(x: int, y: int, metadata: dict[str, Any] | None, fallback: dict[str, Any]) -> int:
    image = (metadata or {}).get("image", fallback)
    tile_width = int(image["tile_width"])
    tile_height = int(image["tile_height"])
    width = int(image["width"])
    tiles_x = (width + tile_width - 1) // tile_width
    return (y // tile_height) * tiles_x + (x // tile_width)


def _json_leaf_differences(
    expected: Any,
    actual: Any,
    path: str = "$",
) -> list[dict[str, Any]]:
    if type(expected) is not type(actual):
        return [
            {
                "artifact": "projection_landings.json",
                "tile_id": None,
                "pixel": None,
                "channel": "JSON",
                "json_path": path,
                "expected": expected,
                "actual": actual,
            }
        ]
    if isinstance(expected, dict):
        differences: list[dict[str, Any]] = []
        keys = sorted(set(expected) | set(actual))
        for key in keys:
            if key not in expected or key not in actual:
                differences.append(
                    {
                        "artifact": "projection_landings.json",
                        "tile_id": None,
                        "pixel": None,
                        "channel": "JSON",
                        "json_path": f"{path}.{key}",
                        "expected": expected.get(key),
                        "actual": actual.get(key),
                    }
                )
            else:
                differences.extend(
                    _json_leaf_differences(expected[key], actual[key], f"{path}.{key}")
                )
        return differences
    if isinstance(expected, list):
        differences = []
        if len(expected) != len(actual):
            differences.append(
                {
                    "artifact": "projection_landings.json",
                    "tile_id": None,
                    "pixel": None,
                    "channel": "JSON",
                    "json_path": f"{path}.length",
                    "expected": len(expected),
                    "actual": len(actual),
                }
            )
        for index in range(min(len(expected), len(actual))):
            differences.extend(
                _json_leaf_differences(
                    expected[index],
                    actual[index],
                    f"{path}[{index}]",
                )
            )
        return differences
    if expected != actual:
        return [
            {
                "artifact": "projection_landings.json",
                "tile_id": None,
                "pixel": None,
                "channel": "JSON",
                "json_path": path,
                "expected": expected,
                "actual": actual,
            }
        ]
    return []


def compare_directories(
    expected_dir: str | Path,
    actual_dir: str | Path,
    *,
    output_path: str | Path | None = None,
) -> dict[str, Any]:
    expected = Path(expected_dir)
    actual = Path(actual_dir)
    expected_meta = _tile_metadata(expected)
    actual_meta = _tile_metadata(actual)
    fallback = {
        "width": 1,
        "height": 1,
        "tile_width": 1,
        "tile_height": 1,
    }
    differences: list[dict[str, Any]] = []
    for filename, artifact_name in (
        ("rgba.png", "rgba.png"),
        ("layer_ownership.png", "layer_ownership.png"),
    ):
        expected_image = _read_rgba(expected / filename)
        actual_image = _read_rgba(actual / filename)
        if expected_image.shape != actual_image.shape:
            differences.append(
                {
                    "artifact": artifact_name,
                    "tile_id": 0,
                    "pixel": {"x": 0, "y": 0},
                    "channel": "SHAPE",
                    "expected": list(expected_image.shape),
                    "actual": list(actual_image.shape),
                }
            )
            continue
        unequal = expected_image != actual_image
        for y, x, channel_index in np.argwhere(unequal):
            differences.append(
                {
                    "artifact": artifact_name,
                    "tile_id": _tile_id(
                        int(x),
                        int(y),
                        actual_meta or expected_meta,
                        fallback,
                    ),
                    "pixel": {"x": int(x), "y": int(y)},
                    "channel": _CHANNELS[int(channel_index)],
                    "expected": int(expected_image[y, x, channel_index]),
                    "actual": int(actual_image[y, x, channel_index]),
                }
            )

    expected_projection = json.loads(
        (expected / "projection_landings.json").read_text(encoding="utf-8")
    )
    actual_projection = json.loads(
        (actual / "projection_landings.json").read_text(encoding="utf-8")
    )
    differences.extend(
        _json_leaf_differences(expected_projection, actual_projection)
    )

    result = {
        "schema_version": "veil-differences/1",
        "expected_directory": str(expected),
        "actual_directory": str(actual),
        "difference_count": len(differences),
        "differences": differences,
    }
    destination = Path(output_path) if output_path is not None else actual / "differences.json"
    if differences:
        destination.write_text(canonical_json(result) + "\n", encoding="utf-8")
        raise RenderConsistencyError(result)
    if destination.exists():
        destination.unlink()
    return result

