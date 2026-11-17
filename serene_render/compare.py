from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import numpy as np

from .snapshot import atomic_write_json


CHANNELS = ("R", "G", "B", "A")


def _sha256_file(path: Path) -> str | None:
    if not path.exists():
        return None
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _load_json(path: Path) -> dict[str, Any] | None:
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        return None
    return value if isinstance(value, dict) else None


def _tile_info(output_dir: Path, width: int) -> tuple[int, int]:
    report = _load_json(output_dir / "report.json")
    tile_size = 64
    if report is not None:
        image = report.get("image")
        if isinstance(image, dict) and isinstance(image.get("tile_size"), int):
            tile_size = int(image["tile_size"])
    columns = (width + tile_size - 1) // tile_size
    return tile_size, columns


def _block_index(x: int, y: int, tile_size: int, columns: int) -> int:
    return (y // tile_size) * columns + (x // tile_size)


def compare_outputs(
    reference_dir: str | Path,
    candidate_dir: str | Path,
    max_differences: int = 200,
) -> list[dict[str, Any]]:
    reference = Path(reference_dir)
    candidate = Path(candidate_dir)
    differences: list[dict[str, Any]] = []

    ref_rgba_path = reference / "render_rgba.npy"
    candidate_rgba_path = candidate / "render_rgba.npy"
    ref_layer_path = reference / "layer_map.npy"
    candidate_layer_path = candidate / "layer_map.npy"

    ref_rgba: np.ndarray | None = None
    candidate_rgba: np.ndarray | None = None
    ref_layer: np.ndarray | None = None
    candidate_layer: np.ndarray | None = None
    try:
        ref_rgba = np.load(ref_rgba_path, allow_pickle=False)
        candidate_rgba = np.load(candidate_rgba_path, allow_pickle=False)
    except (OSError, ValueError):
        differences.append(
            {
                "block_index": None,
                "pixel": None,
                "channel": "rgba_array",
                "message": "one or both RGBA arrays are missing or unreadable",
            }
        )
    try:
        ref_layer = np.load(ref_layer_path, allow_pickle=False)
        candidate_layer = np.load(candidate_layer_path, allow_pickle=False)
    except (OSError, ValueError):
        differences.append(
            {
                "block_index": None,
                "pixel": None,
                "channel": "layer_array",
                "message": "one or both layer-map arrays are missing or unreadable",
            }
        )

    if ref_rgba is not None and candidate_rgba is not None:
        if ref_rgba.shape != candidate_rgba.shape:
            differences.append(
                {
                    "block_index": None,
                    "pixel": None,
                    "channel": "shape",
                    "reference": list(ref_rgba.shape),
                    "candidate": list(candidate_rgba.shape),
                }
            )
        else:
            tile_size, columns = _tile_info(reference, int(ref_rgba.shape[1]))
            changed = np.any(ref_rgba != candidate_rgba, axis=2)
            for y, x in np.argwhere(changed):
                pixel_differences: list[str] = []
                for channel, name in enumerate(CHANNELS):
                    if ref_rgba[y, x, channel] != candidate_rgba[y, x, channel]:
                        pixel_differences.append(name)
                differences.append(
                    {
                        "block_index": _block_index(
                            int(x), int(y), tile_size, columns
                        ),
                        "pixel": [int(x), int(y)],
                        "channel": pixel_differences,
                        "reference": [
                            int(value) for value in ref_rgba[y, x]
                        ],
                        "candidate": [
                            int(value) for value in candidate_rgba[y, x]
                        ],
                    }
                )
                if len(differences) >= max_differences:
                    break
            if len(differences) >= max_differences:
                differences.append(
                    {
                        "block_index": None,
                        "pixel": None,
                        "channel": "truncated",
                        "message": f"more than {max_differences} differences",
                    }
                )

    if ref_layer is not None and candidate_layer is not None:
        if ref_layer.shape != candidate_layer.shape:
            differences.append(
                {
                    "block_index": None,
                    "pixel": None,
                    "channel": "layer_shape",
                    "reference": list(ref_layer.shape),
                    "candidate": list(candidate_layer.shape),
                }
            )
        else:
            tile_size, columns = _tile_info(reference, int(ref_layer.shape[1]))
            changed = ref_layer != candidate_layer
            for y, x in np.argwhere(changed):
                differences.append(
                    {
                        "block_index": _block_index(
                            int(x), int(y), tile_size, columns
                        ),
                        "pixel": [int(x), int(y)],
                        "channel": "layer",
                        "reference": int(ref_layer[y, x]),
                        "candidate": int(candidate_layer[y, x]),
                    }
                )
                if len(differences) >= max_differences:
                    break

    for filename, channel in (
        ("projection_points.json", "projection_points"),
        ("report.json", "report"),
        ("render.png", "render_png"),
        ("layer_map.png", "layer_map_png"),
    ):
        ref_path = reference / filename
        candidate_path = candidate / filename
        ref_hash = _sha256_file(ref_path)
        candidate_hash = _sha256_file(candidate_path)
        if ref_hash != candidate_hash:
            differences.append(
                {
                    "block_index": None,
                    "pixel": None,
                    "channel": channel,
                    "reference_sha256": ref_hash,
                    "candidate_sha256": candidate_hash,
                }
            )
            if len(differences) >= max_differences:
                break

    if differences:
        atomic_write_json(
            candidate / "resume_diff.json",
            {
                "ok": False,
                "code": "RESUME_OUTPUT_MISMATCH",
                "difference_count": len(differences),
                "differences": differences,
            },
        )
    else:
        stale_diff = candidate / "resume_diff.json"
        if stale_diff.exists():
            stale_diff.unlink()
    return differences
