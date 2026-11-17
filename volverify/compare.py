from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from .io_utils import atomic_write_json
from .renderer import make_chunk_layout
from .scene import Scene


def _load_rgba(path: Path) -> np.ndarray:
    with Image.open(path) as image:
        if image.mode != "RGBA":
            image = image.convert("RGBA")
        return np.asarray(image, dtype=np.uint8).copy()


def compare_render_dirs(
    scene: Scene,
    left_dir: Path | str,
    right_dir: Path | str,
    max_samples_per_chunk: int = 1000,
) -> dict[str, Any]:
    left = Path(left_dir)
    right = Path(right_dir)
    left_image = _load_rgba(left / "frame.rgba.png")
    right_image = _load_rgba(right / "frame.rgba.png")
    if left_image.shape != right_image.shape:
        return {
            "schema_version": 1,
            "status": "different",
            "code": "DIFF_IMAGE_SIZE_MISMATCH",
            "left_shape": list(left_image.shape),
            "right_shape": list(right_image.shape),
            "chunks": [],
        }
    height, width = left_image.shape[:2]
    left_depth_bytes = (left / "frame.depth.f32").read_bytes()
    right_depth_bytes = (right / "frame.depth.f32").read_bytes()
    expected_depth_bytes = width * height * 4
    if (
        len(left_depth_bytes) != expected_depth_bytes
        or len(right_depth_bytes) != expected_depth_bytes
    ):
        return {
            "schema_version": 1,
            "status": "different",
            "code": "DIFF_DEPTH_LENGTH_MISMATCH",
            "expected_depth_bytes": expected_depth_bytes,
            "left_depth_bytes": len(left_depth_bytes),
            "right_depth_bytes": len(right_depth_bytes),
            "chunks": [],
        }
    left_depth = np.frombuffer(left_depth_bytes, dtype="<f4").reshape(
        height, width
    )
    right_depth = np.frombuffer(right_depth_bytes, dtype="<f4").reshape(
        height, width
    )

    rgba_delta = np.abs(
        left_image.astype(np.int16) - right_image.astype(np.int16)
    )
    depth_delta = np.abs(
        left_depth.astype(np.float64) - right_depth.astype(np.float64)
    )
    mismatch = np.any(rgba_delta > 0, axis=2) | (depth_delta > 0.0)
    chunks = make_chunk_layout(
        width, height, scene.render.chunk_width, scene.render.chunk_height
    )
    chunk_payloads: list[dict[str, Any]] = []
    total_mismatch = 0
    for chunk in chunks:
        x1 = chunk.x0 + chunk.width
        y1 = chunk.y0 + chunk.height
        local = mismatch[chunk.y0 : y1, chunk.x0 : x1]
        count = int(np.count_nonzero(local))
        if count == 0:
            continue
        total_mismatch += count
        ys, xs = np.nonzero(local)
        samples = []
        for local_y, local_x in zip(ys[:max_samples_per_chunk], xs[:max_samples_per_chunk]):
            y = int(chunk.y0 + local_y)
            x = int(chunk.x0 + local_x)
            samples.append(
                {
                    "pixel": [x, y],
                    "rgba_delta": [
                        int(value) for value in rgba_delta[y, x]
                    ],
                    "depth_delta": float(depth_delta[y, x]),
                }
            )
        local_rgba = rgba_delta[chunk.y0 : y1, chunk.x0 : x1]
        local_depth = depth_delta[chunk.y0 : y1, chunk.x0 : x1]
        chunk_payloads.append(
            {
                "chunk_id": (
                    (chunk.y0 // scene.render.chunk_height)
                    * ((width + scene.render.chunk_width - 1) // scene.render.chunk_width)
                    + (chunk.x0 // scene.render.chunk_width)
                ),
                "x0": chunk.x0,
                "y0": chunk.y0,
                "width": chunk.width,
                "height": chunk.height,
                "mismatch_pixels": count,
                "max_rgba_delta": int(local_rgba.max()),
                "mean_rgba_delta": float(local_rgba.mean()),
                "max_depth_delta": float(local_depth.max()),
                "mean_depth_delta": float(local_depth.mean()),
                "samples": samples,
                "samples_truncated": count > len(samples),
            }
        )

    image_file_identical = (
        (left / "frame.rgba.png").read_bytes()
        == (right / "frame.rgba.png").read_bytes()
    )
    depth_file_identical = left_depth_bytes == right_depth_bytes
    byte_identical = image_file_identical and depth_file_identical
    status = (
        "identical"
        if total_mismatch == 0 and byte_identical
        else "different"
    )
    return {
        "schema_version": 1,
        "status": status,
        "code": (
            "INCREMENTAL_RESULT_MATCH"
            if status == "identical"
            else "INCREMENTAL_RESULT_MISMATCH"
        ),
        "left": str(left),
        "right": str(right),
        "width": width,
        "height": height,
        "total_mismatch_pixels": total_mismatch,
        "image_file_identical": image_file_identical,
        "depth_file_identical": depth_file_identical,
        "byte_identical": byte_identical,
        "chunks": chunk_payloads,
    }


def write_comparison(report: dict[str, Any], path: Path) -> None:
    atomic_write_json(path, report)

