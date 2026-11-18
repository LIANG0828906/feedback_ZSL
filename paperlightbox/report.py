"""Stable, deterministic statistics and diff reports (no timestamps/paths)."""

from __future__ import annotations

import hashlib
import json

import numpy as np
from PIL import Image

from .pipeline import shadow_probes
from .scene import ordered_layers, scene_digest
from .texture import count_cutouts


def image_digest(png_path: str) -> str:
    with open(png_path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def recompute_tile_stats(rgba: np.ndarray, scene: dict) -> list:
    """Per-tile mean RGBA recomputed from the final in-memory image.

    The same means can be reproduced from the saved 8-bit PNG, which lets art
    reviewers reconcile the report with the deliverable image.
    """

    render = scene["render"]
    tw, th = int(render["tile_width"]), int(render["tile_height"])
    height, width = rgba.shape[:2]
    records = []
    tid = 0
    for y0 in range(0, height, th):
        for x0 in range(0, width, tw):
            block = rgba[y0:y0 + th, x0:x0 + tw]
            mean = block.reshape(-1, 4).mean(axis=0)
            records.append({
                "tile": tid,
                "mean_rgba": [round(float(v), 6) for v in mean],
            })
            tid += 1
    return records


def build_report(scene: dict, run_summary: dict) -> dict:
    layers = ordered_layers(scene)
    assembled = run_summary["assembled"]
    stats = assembled["stats"]

    layer_entries = []
    for layer in layers:
        index = int(layer["index"])
        mesh = layer["mesh"]
        nx, ny = int(mesh["grid"]["nx"]), int(mesh["grid"]["ny"])
        holes = np.asarray(mesh["holes_array"], dtype=bool)
        entry = stats.get(index, {})
        layer_entries.append({
            "layer": index,
            "visible_fragments": int(entry.get("visible_fragments", 0)),
            "mean_transmittance": round(
                float(entry.get("mean_transmittance", 0.0)), 6),
            "cutout_triangles": int(holes.sum()),
            "cutout_blobs": count_cutouts(holes, nx, ny),
            "air_gap": float(layer["air_gap"]),
            "soft_shadow_sample_points": int(
                scene["render"]["sample_density"]),
        })

    probes = shadow_probes(scene)
    rgba = assembled["rgba"]
    quantized = np.round(np.clip(rgba, 0.0, 1.0) * 255.0).astype(np.uint8)
    clipped_channels = int(np.sum(quantized[..., :3] >= 255))
    report = {
        "status": "ok",
        "scene_digest": scene_digest(scene),
        "image": {
            "width": int(scene["image"]["width"]),
            "height": int(scene["image"]["height"]),
            "png_sha256": image_digest(
                run_summary["output_dir"] + "/lightbox.png"),
            "pixel_sha256": hashlib.sha256(quantized.tobytes()).hexdigest(),
            "depth_sha256": hashlib.sha256(
                assembled["optical_depth"].astype(np.float32).tobytes()
            ).hexdigest(),
            "clipped_channels": clipped_channels,
        },
        "layers": layer_entries,
        "soft_shadow_probes": probes,
        "tiles": {
            "declared_size": [int(scene["render"]["tile_width"]),
                              int(scene["render"]["tile_height"])],
            "completed": run_summary["completed"],
            "total": run_summary["total_tiles"],
            "tile_color_summary": run_summary["tile_records"],
            "recomputed_tile_mean_rgba": recompute_tile_stats(rgba, scene),
        },
        "recovery_count": int(run_summary["recovery_count"]),
    }
    return report


def write_report(report: dict, path: str) -> None:
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(report, handle, sort_keys=True, indent=2)
        handle.write("\n")


def compare_outputs(reference_png: str, candidate_png: str,
                    reference_depth: str, candidate_depth: str,
                    tile_width: int, tile_height: int) -> dict:
    """Byte/pixel comparison producing a structured diff on mismatch."""

    ref = np.asarray(Image.open(reference_png).convert("RGBA"), dtype=np.uint8)
    cand = np.asarray(Image.open(candidate_png).convert("RGBA"),
                      dtype=np.uint8)
    diffs = []
    if ref.shape != cand.shape:
        diffs.append({"kind": "shape", "reference": list(ref.shape),
                      "candidate": list(cand.shape)})
    else:
        mismatch = np.argwhere(np.any(ref != cand, axis=2))
        for row in mismatch[:16]:
            y, x = int(row[0]), int(row[1])
            columns = (ref.shape[1] + tile_width - 1) // tile_width
            tile = (y // tile_height) * columns + (x // tile_width)
            diffs.append({
                "tile": int(tile),
                "pixel": [x, y],
                "channels": {
                    "reference_rgba": [int(v) for v in ref[y, x]],
                    "candidate_rgba": [int(v) for v in cand[y, x]],
                },
            })

    ref_depth = np.load(reference_depth)
    cand_depth = np.load(candidate_depth)
    depth_diffs = int(np.sum(ref_depth != cand_depth))
    return {
        "status": "identical" if not diffs and depth_diffs == 0 else "different",
        "pixel_mismatch_count": int(0) if ref.shape != cand.shape
        else int(np.sum(np.any(ref != cand, axis=2))),
        "depth_mismatch_count": depth_diffs,
        "diffs": diffs,
    }
