"""Structured byte-level comparison of two render output trees."""

from __future__ import annotations

import os

from .scene import load_scene_dict, parse_scene
from .io_utils import read_ownership, read_pam, read_json


def compare_runs(reference_dir: str, candidate_dir: str,
                 scene_path: str | None = None) -> list[dict]:
    """Return structured per-pixel/per-channel differences (empty == equal)."""
    differences: list[dict] = []
    tile_w, tile_h = 32, 24
    if scene_path is not None:
        scene = parse_scene(load_scene_dict(scene_path))
        tile_w = scene.config.tile_width
        tile_h = scene.config.tile_height
    view_names = sorted(
        name for name in os.listdir(reference_dir)
        if name.startswith("view_") and os.path.isdir(os.path.join(reference_dir, name))
    )
    for view_name in view_names:
        view_index = int(view_name.split("_")[1])
        ref_image = read_pam(os.path.join(reference_dir, view_name, "image.pam"))
        cand_path = os.path.join(candidate_dir, view_name, "image.pam")
        if not os.path.exists(cand_path):
            differences.append(
                {"view": view_index, "tile": "-", "pixel": "-",
                 "channel": "file", "detail": f"missing {cand_path}"}
            )
            continue
        cand_image = read_pam(cand_path)
        if ref_image.shape != cand_image.shape:
            differences.append(
                {"view": view_index, "tile": "-", "pixel": "-",
                 "channel": "shape",
                 "detail": f"{ref_image.shape} != {cand_image.shape}"}
            )
            continue
        mismatch = np_argwhere(ref_image != cand_image)
        for row, col, channel in mismatch[:100]:
            differences.append(
                {
                    "view": view_index,
                    "tile": _tile_of(col, row, tile_w, tile_h),
                    "pixel": [int(col), int(row)],
                    "channel": int(channel),
                    "reference": int(ref_image[row, col, channel]),
                    "candidate": int(cand_image[row, col, channel]),
                }
            )
        ref_own = read_ownership(
            os.path.join(reference_dir, view_name, "ownership.kgown")
        )
        cand_own = read_ownership(
            os.path.join(candidate_dir, view_name, "ownership.kgown")
        )
        own_mismatch = np_argwhere(ref_own != cand_own)
        for row, col in own_mismatch[:50]:
            differences.append(
                {
                    "view": view_index,
                    "tile": _tile_of(col, row, tile_w, tile_h),
                    "pixel": [int(col), int(row)],
                    "channel": "ownership",
                    "reference": int(ref_own[row, col]),
                    "candidate": int(cand_own[row, col]),
                }
            )

    ref_landing = os.path.join(reference_dir, "grout_landings.json")
    cand_landing = os.path.join(candidate_dir, "grout_landings.json")
    if read_json(ref_landing) != read_json(cand_landing):
        differences.append(
            {"view": "-", "tile": "-", "pixel": "-",
             "channel": "grout_landings",
             "detail": "landing coordinate lists differ"}
        )
    return differences


def np_argwhere(condition):
    import numpy as np

    return np.argwhere(condition)


def _tile_of(col: int, row: int, tile_w: int, tile_h: int) -> str:
    return f"{int(row) // tile_h},{int(col) // tile_w}"
