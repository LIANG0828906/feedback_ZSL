"""Structured byte-level comparison of completed render directories."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .util import canonical_json_bytes, object_digest


def _load_json(path: Path) -> Any:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def _block_for_pixel(
    x: int, y: int, tile_width: int, tile_height: int, blocks_x: int
) -> int:
    return (y // tile_height) * blocks_x + (x // tile_width)


def _compare_array(
    label: str,
    expected: np.ndarray,
    actual: np.ndarray,
    angle_index: int,
    tile_width: int,
    tile_height: int,
    blocks_x: int,
    channel_names: list[str],
    max_records: int,
) -> tuple[list[dict[str, Any]], int]:
    diffs: list[dict[str, Any]] = []
    total = 0
    if expected.shape != actual.shape:
        return (
            [
                {
                    "kind": label,
                    "angle": angle_index,
                    "block": None,
                    "pixel": None,
                    "channel": "shape",
                    "expected": list(expected.shape),
                    "actual": list(actual.shape),
                }
            ],
            1,
        )
    mismatch = expected != actual
    indices = np.argwhere(mismatch)
    total = int(indices.shape[0])
    for index in indices[:max_records]:
        record_index = tuple(int(value) for value in index)
        if len(record_index) >= 2:
            y, x = record_index[0], record_index[1]
            pixel = [x, y]
            block = _block_for_pixel(x, y, tile_width, tile_height, blocks_x)
        else:
            pixel = None
            block = None
        channel_index = record_index[-1] if expected.ndim == 3 else None
        channel = (
            channel_names[channel_index]
            if channel_index is not None and channel_index < len(channel_names)
            else channel_names[0]
        )
        diffs.append(
            {
                "kind": label,
                "angle": angle_index,
                "block": block,
                "pixel": pixel,
                "channel": channel,
                "expected": expected[record_index].item()
                if hasattr(expected[record_index], "item")
                else expected[record_index],
                "actual": actual[record_index].item()
                if hasattr(actual[record_index], "item")
                else actual[record_index],
            }
        )
    return diffs, total


def compare_runs(
    reference_dir: str | Path,
    resumed_dir: str | Path,
    max_records_per_file: int = 64,
) -> dict[str, Any]:
    reference = Path(reference_dir)
    resumed = Path(resumed_dir)
    diffs: list[dict[str, Any]] = []
    counts: dict[str, int] = {}
    reference_snapshot = _load_json(reference / "snapshot.json")
    expected = reference_snapshot["expected"]
    tile_width = expected["tile_width"]
    tile_height = expected["tile_height"]
    blocks_x = expected["blocks_x"]
    arrays = (
        (
            "rgba_premultiplied_linear",
            "_rgba_premultiplied_linear.npy",
            ["R", "G", "B", "A"],
        ),
        ("owner_id", "_owner_id.npy", ["owner_id"]),
        ("owner_layer", "_owner_layer.npy", ["owner_layer"]),
    )
    for angle_index in range(expected["total_angles"]):
        prefix = f"angle_{angle_index:03d}"
        for label, suffix, channels in arrays:
            reference_path = reference / "renders" / f"{prefix}{suffix}"
            resumed_path = resumed / "renders" / f"{prefix}{suffix}"
            if reference_path.read_bytes() == resumed_path.read_bytes():
                continue
            expected_array = np.load(reference_path)
            actual_array = np.load(resumed_path)
            file_diffs, total = _compare_array(
                label,
                expected_array,
                actual_array,
                angle_index,
                tile_width,
                tile_height,
                blocks_x,
                channels,
                max_records_per_file,
            )
            diffs.extend(file_diffs)
            counts[label] = counts.get(label, 0) + total
        reference_png = reference / "renders" / f"{prefix}_rgba.png"
        resumed_png = resumed / "renders" / f"{prefix}_rgba.png"
        if reference_png.read_bytes() != resumed_png.read_bytes():
            diffs.append(
                {
                    "kind": "rgba_png",
                    "angle": angle_index,
                    "block": None,
                    "pixel": None,
                    "channel": "file",
                    "expected": object_digest(reference_png.read_bytes()),
                    "actual": object_digest(resumed_png.read_bytes()),
                }
            )
            counts["rgba_png"] = counts.get("rgba_png", 0) + 1
    reference_landing = _load_json(reference / "landing_points.json")
    resumed_landing = _load_json(resumed / "landing_points.json")
    if canonical_json_bytes(reference_landing) != canonical_json_bytes(resumed_landing):
        diffs.append(
            {
                "kind": "landing_points",
                "angle": None,
                "block": None,
                "pixel": None,
                "channel": "coordinate_list",
                "expected": reference_landing.get("digest"),
                "actual": resumed_landing.get("digest"),
            }
        )
        counts["landing_points"] = 1
    reference_lightmaps = _load_json(reference / "diagnostics" / "lightmaps.json")
    resumed_lightmaps = _load_json(resumed / "diagnostics" / "lightmaps.json")
    if canonical_json_bytes(reference_lightmaps) != canonical_json_bytes(
        resumed_lightmaps
    ):
        diffs.append(
            {
                "kind": "receiver_lightmaps",
                "angle": None,
                "block": None,
                "pixel": None,
                "channel": "world_space_map",
                "expected": reference_lightmaps.get("digest"),
                "actual": resumed_lightmaps.get("digest"),
            }
        )
        counts["receiver_lightmaps"] = 1
    for receiver_id in ("wall", "floor"):
        for suffix in ("_linear.npy", ".png"):
            reference_path = (
                reference / "diagnostics" / f"{receiver_id}_lightmap{suffix}"
            )
            resumed_path = (
                resumed / "diagnostics" / f"{receiver_id}_lightmap{suffix}"
            )
            if reference_path.read_bytes() != resumed_path.read_bytes():
                diffs.append(
                    {
                        "kind": "receiver_lightmap_file",
                        "angle": None,
                        "block": None,
                        "pixel": None,
                        "channel": f"{receiver_id}{suffix}",
                        "expected": object_digest(reference_path.read_bytes()),
                        "actual": object_digest(resumed_path.read_bytes()),
                    }
                )
                counts["receiver_lightmap_file"] = (
                    counts.get("receiver_lightmap_file", 0) + 1
                )
    return {
        "format": "GLV-COMPARE-V1",
        "reference": str(reference),
        "resumed": str(resumed),
        "identical": len(diffs) == 0,
        "difference_counts": counts,
        "differences": diffs,
        "exit_code": 0 if not diffs else 4,
    }
