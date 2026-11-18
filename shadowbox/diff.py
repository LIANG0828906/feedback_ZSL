"""Pixel-level comparison and structured difference reports."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np

from .checkpoint import atomic_write_json, sha256_bytes
from .render import block_grid
from .scene import Scene

CHANNEL_NAMES = ("R", "G", "B", "A")


def raw_rgba_bytes(array: np.ndarray) -> bytes:
    return np.asarray(array, dtype="u1").tobytes(order="C")


def raw_depth_bytes(array: np.ndarray) -> bytes:
    return np.asarray(array, dtype="<f4").tobytes(order="C")


def compare_arrays(
    reference_rgba: np.ndarray,
    candidate_rgba: np.ndarray,
    reference_depth: np.ndarray,
    candidate_depth: np.ndarray,
    scene: Scene,
    *,
    max_reported: int = 64,
) -> dict[str, Any]:
    if reference_rgba.shape != candidate_rgba.shape:
        raise ValueError("RGBA array shapes differ")
    if reference_depth.shape != candidate_depth.shape:
        raise ValueError("depth array shapes differ")
    rgba_mismatch = np.any(reference_rgba != candidate_rgba, axis=2)
    depth_equal = reference_depth.view(np.uint32) == candidate_depth.view(np.uint32)
    depth_mismatch = ~depth_equal
    mismatch = rgba_mismatch | depth_mismatch
    columns, _ = block_grid(scene.camera)
    total = int(np.count_nonzero(mismatch))
    if total == 0:
        return {
            "format": "shadowbox-diff-v1",
            "equal": True,
            "mismatch_pixels": 0,
            "differences": [],
            "reference_rgba_sha256": sha256_bytes(raw_rgba_bytes(reference_rgba)),
            "candidate_rgba_sha256": sha256_bytes(raw_rgba_bytes(candidate_rgba)),
            "reference_depth_sha256": sha256_bytes(raw_depth_bytes(reference_depth)),
            "candidate_depth_sha256": sha256_bytes(raw_depth_bytes(candidate_depth)),
        }
    ys, xs = np.nonzero(mismatch)
    differences: list[dict[str, Any]] = []
    for y, x in zip(ys[:max_reported], xs[:max_reported]):
        channels: list[str] = []
        reference_values: dict[str, Any] = {}
        candidate_values: dict[str, Any] = {}
        for channel_index, name in enumerate(CHANNEL_NAMES):
            reference_value = int(reference_rgba[y, x, channel_index])
            candidate_value = int(candidate_rgba[y, x, channel_index])
            if reference_value != candidate_value:
                channels.append(name)
                reference_values[name] = reference_value
                candidate_values[name] = candidate_value
        if depth_mismatch[y, x]:
            channels.append("DEPTH")
            reference_values["DEPTH"] = float(reference_depth[y, x])
            candidate_values["DEPTH"] = float(candidate_depth[y, x])
        block_index = int(y // scene.camera.block_height) * columns + int(
            x // scene.camera.block_width
        )
        differences.append(
            {
                "block_index": block_index,
                "pixel": [int(x), int(y)],
                "channels": channels,
                "reference": reference_values,
                "candidate": candidate_values,
            }
        )
    return {
        "format": "shadowbox-diff-v1",
        "equal": False,
        "mismatch_pixels": total,
        "reported_differences": len(differences),
        "truncated": total > len(differences),
        "differences": differences,
        "reference_rgba_sha256": sha256_bytes(raw_rgba_bytes(reference_rgba)),
        "candidate_rgba_sha256": sha256_bytes(raw_rgba_bytes(candidate_rgba)),
        "reference_depth_sha256": sha256_bytes(raw_depth_bytes(reference_depth)),
        "candidate_depth_sha256": sha256_bytes(raw_depth_bytes(candidate_depth)),
    }


def load_raw_output(
    directory: str | Path, width: int, height: int
) -> tuple[np.ndarray, np.ndarray]:
    directory = Path(directory)
    rgba_payload = (directory / "rgba.raw").read_bytes()
    depth_payload = (directory / "optical_depth.f32").read_bytes()
    if len(rgba_payload) != width * height * 4:
        raise ValueError("reference rgba.raw has an unexpected length")
    if len(depth_payload) != width * height * 4:
        raise ValueError("reference optical_depth.f32 has an unexpected length")
    rgba = np.frombuffer(rgba_payload, dtype=np.uint8).reshape(height, width, 4)
    depth = np.frombuffer(depth_payload, dtype="<f4").reshape(height, width)
    return rgba, depth


def write_diff_report(path: str | Path, report: dict[str, Any]) -> None:
    atomic_write_json(path, report)
