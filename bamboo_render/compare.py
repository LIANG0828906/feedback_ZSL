from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .io_utils import sha256_bytes, write_json

CHANNEL_NAMES = {
    "rgba": ("R", "G", "B", "A"),
    "ownership": ("owner", "active_mask", "front_layer", "A"),
    "layer_premultiplied": ("layer", "R", "G", "B", "A"),
}


def compare_outputs(
    reference_dir: str | Path,
    candidate_dir: str | Path,
    report_path: str | Path,
    *,
    max_differences: int = 128,
) -> tuple[bool, dict[str, Any]]:
    reference_dir = Path(reference_dir)
    candidate_dir = Path(candidate_dir)
    differences: list[dict[str, Any]] = []
    checks: list[dict[str, Any]] = []
    chunk_size = _read_chunk_size(reference_dir) or _read_chunk_size(candidate_dir) or (64, 48)

    for name in ("rgba", "ownership", "layer_premultiplied"):
        left = _load_array(reference_dir / f"{name}.npy")
        right = _load_array(candidate_dir / f"{name}.npy")
        if left is None or right is None:
            differences.append(
                {
                    "kind": "missing_or_invalid_array",
                    "array": name,
                    "left_present": left is not None,
                    "right_present": right is not None,
                }
            )
            continue
        check = {
            "array": name,
            "shape": list(left.shape),
            "sha256_equal": sha256_bytes(np.ascontiguousarray(left).tobytes())
            == sha256_bytes(np.ascontiguousarray(right).tobytes()),
        }
        checks.append(check)
        if left.shape != right.shape:
            differences.append(
                {
                    "kind": "shape_mismatch",
                    "array": name,
                    "left_shape": list(left.shape),
                    "right_shape": list(right.shape),
                }
            )
            continue
        mismatch = np.any(left != right, axis=tuple(range(2, left.ndim)))
        coordinates = np.argwhere(mismatch)
        height = left.shape[-3]
        width = left.shape[-2]
        for coordinate in coordinates:
            if len(differences) >= max_differences:
                break
            y = int(coordinate[-2])
            x = int(coordinate[-1])
            if left.ndim == 4:
                layer_index = int(coordinate[0])
                left_pixel = left[layer_index, y, x]
                right_pixel = right[layer_index, y, x]
            else:
                layer_index = None
                left_pixel = left[y, x]
                right_pixel = right[y, x]
            channel_indices = np.nonzero(left_pixel != right_pixel)[0]
            channel_names = [CHANNEL_NAMES[name][int(index)] for index in channel_indices]
            difference = {
                "kind": "pixel_mismatch",
                "array": name,
                "chunk_id": (y // chunk_size[1]) * ((width + chunk_size[0] - 1) // chunk_size[0])
                + (x // chunk_size[0]),
                "x": x,
                "y": y,
                "channels": channel_names,
                "left_values": [int(value) for value in left_pixel[channel_indices]],
                "right_values": [int(value) for value in right_pixel[channel_indices]],
            }
            if layer_index is not None:
                difference["layer_slot"] = layer_index
            differences.append(difference)

    points_left = _load_bytes(reference_dir / "projection_points.json")
    points_right = _load_bytes(candidate_dir / "projection_points.json")
    points_equal = points_left is not None and points_left == points_right
    checks.append({"file": "projection_points.json", "sha256_equal": points_equal})
    if not points_equal:
        differences.append(
            {
                "kind": "projection_points_mismatch",
                "left_sha256": sha256_bytes(points_left) if points_left is not None else None,
                "right_sha256": sha256_bytes(points_right) if points_right is not None else None,
            }
        )

    png_left = _load_bytes(reference_dir / "rgba.png")
    png_right = _load_bytes(candidate_dir / "rgba.png")
    checks.append(
        {
            "file": "rgba.png",
            "sha256_equal": png_left is not None and png_left == png_right,
        }
    )
    if png_left != png_right:
        differences.append(
            {
                "kind": "png_bytes_mismatch",
                "left_sha256": sha256_bytes(png_left) if png_left is not None else None,
                "right_sha256": sha256_bytes(png_right) if png_right is not None else None,
            }
        )

    pixel_difference_count = sum(1 for item in differences if item.get("kind") == "pixel_mismatch")
    report = {
        "schema_version": 1,
        "ok": len(differences) == 0,
        "reference": "reference",
        "candidate": "candidate",
        "checks": checks,
        "difference_count": len(differences),
        "pixel_difference_count": pixel_difference_count,
        "truncated": pixel_difference_count >= max_differences,
        "differences": differences,
    }
    write_json(Path(report_path), report)
    return bool(report["ok"]), report


def _load_array(path: Path) -> np.ndarray | None:
    try:
        return np.load(path, allow_pickle=False)
    except (OSError, ValueError):
        return None


def _load_bytes(path: Path) -> bytes | None:
    try:
        return path.read_bytes()
    except OSError:
        return None


def _read_chunk_size(directory: Path) -> tuple[int, int] | None:
    try:
        report = json.loads((directory / "run_report.json").read_text(encoding="utf-8"))
        values = report["image"]["chunk_size"]
        if len(values) != 2:
            return None
        return int(values[0]), int(values[1])
    except (OSError, ValueError, KeyError, TypeError):
        return None
