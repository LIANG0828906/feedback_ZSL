from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from .raster import chunk_count
from .scene import Scene


def _load_rgb(path: Path) -> np.ndarray:
    with Image.open(path) as image:
        return np.asarray(image.convert("RGB"), dtype=np.uint8)


def _load_labels(path: Path) -> np.ndarray:
    with Image.open(path) as image:
        return np.asarray(image, dtype=np.uint16)


def compare_outputs(
    reference_dir: Path,
    candidate_dir: Path,
    scene: Scene,
    max_differences: int = 64,
) -> dict[str, Any]:
    differences: list[dict[str, Any]] = []
    mismatch_counts = {"rgb_pixels": 0, "label_pixels": 0, "channels": 0}
    reference_rgb_path = reference_dir / "color.png"
    candidate_rgb_path = candidate_dir / "color.png"
    reference_labels_path = reference_dir / "attribution.png"
    candidate_labels_path = candidate_dir / "attribution.png"
    reference_projection_path = reference_dir / "projection_hits.json"
    candidate_projection_path = candidate_dir / "projection_hits.json"
    errors: list[str] = []

    for path in (
        reference_rgb_path,
        candidate_rgb_path,
        reference_labels_path,
        candidate_labels_path,
        reference_projection_path,
        candidate_projection_path,
    ):
        if not path.exists():
            errors.append(f"missing output file {path.name}")
    if errors:
        return {
            "schema_version": 1,
            "ok": False,
            "errors": errors,
            "differences": [],
            "difference_count": 0,
        }

    reference_rgb = _load_rgb(reference_rgb_path)
    candidate_rgb = _load_rgb(candidate_rgb_path)
    reference_labels = _load_labels(reference_labels_path)
    candidate_labels = _load_labels(candidate_labels_path)
    if reference_rgb.shape != candidate_rgb.shape:
        errors.append(
            f"RGB shape mismatch: {reference_rgb.shape} != {candidate_rgb.shape}"
        )
    if reference_labels.shape != candidate_labels.shape:
        errors.append(
            f"label shape mismatch: {reference_labels.shape} != {candidate_labels.shape}"
        )

    columns, _, _ = chunk_count(scene)
    chunk_width, chunk_height = scene.render.chunk_size

    def chunk_id(x: int, y: int) -> int:
        return (y // chunk_height) * columns + (x // chunk_width)

    if not errors:
        rgb_mask = np.any(reference_rgb != candidate_rgb, axis=2)
        label_mask = reference_labels != candidate_labels
        mismatch_counts["rgb_pixels"] = int(np.count_nonzero(rgb_mask))
        mismatch_counts["label_pixels"] = int(np.count_nonzero(label_mask))
        mismatch_counts["channels"] = int(
            np.count_nonzero(reference_rgb != candidate_rgb)
            + np.count_nonzero(label_mask)
        )
        channel_names = ("R", "G", "B")
        for y, x in np.argwhere(rgb_mask)[:max_differences]:
            for channel in range(3):
                reference_value = int(reference_rgb[y, x, channel])
                candidate_value = int(candidate_rgb[y, x, channel])
                if reference_value != candidate_value:
                    differences.append(
                        {
                            "chunk_id": chunk_id(int(x), int(y)),
                            "x": int(x),
                            "y": int(y),
                            "channel": channel_names[channel],
                            "reference": reference_value,
                            "candidate": candidate_value,
                        }
                    )
                    if len(differences) >= max_differences:
                        break
            if len(differences) >= max_differences:
                break
        if len(differences) < max_differences:
            for y, x in np.argwhere(label_mask):
                differences.append(
                    {
                        "chunk_id": chunk_id(int(x), int(y)),
                        "x": int(x),
                        "y": int(y),
                        "channel": "label",
                        "reference": int(reference_labels[y, x]),
                        "candidate": int(candidate_labels[y, x]),
                    }
                )
                if len(differences) >= max_differences:
                    break

    projection_equal = (
        reference_projection_path.read_bytes() == candidate_projection_path.read_bytes()
    )
    if not projection_equal:
        errors.append("projection_hits.json differs")
    color_bytes_equal = (
        reference_rgb_path.read_bytes() == candidate_rgb_path.read_bytes()
    )
    attribution_bytes_equal = (
        reference_labels_path.read_bytes() == candidate_labels_path.read_bytes()
    )
    if not color_bytes_equal:
        errors.append("color.png bytes differ")
    if not attribution_bytes_equal:
        errors.append("attribution.png bytes differ")
    ok = (
        not errors
        and mismatch_counts["channels"] == 0
        and projection_equal
        and color_bytes_equal
        and attribution_bytes_equal
    )
    return {
        "schema_version": 1,
        "ok": bool(ok),
        "error_count": len(errors),
        "errors": errors,
        "mismatch_counts": mismatch_counts,
        "projection_hits_equal": bool(projection_equal),
        "color_png_bytes_equal": bool(color_bytes_equal),
        "attribution_png_bytes_equal": bool(attribution_bytes_equal),
        "difference_count": int(
            mismatch_counts["channels"]
            + (0 if projection_equal else 1)
            + (0 if color_bytes_equal else 1)
            + (0 if attribution_bytes_equal else 1)
        ),
        "differences": differences,
        "differences_truncated": bool(
            mismatch_counts["channels"] > len(differences)
        ),
    }
