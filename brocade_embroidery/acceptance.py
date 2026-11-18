"""Acceptance checks and human-readable verification report."""

from __future__ import annotations

import json
import os

import numpy as np

from .render import build_shadow_fields, build_tables, render_frame
from .scene_gen import generate_scene
from .shadow import penumbra_width


def isolated_pixel_count(rgba: np.ndarray) -> int:
    """Count isolated bright/dark pixels versus their four neighbours."""
    luminance = (0.2126 * rgba[:, :, 0] + 0.7152 * rgba[:, :, 1]
                 + 0.0722 * rgba[:, :, 2])
    center = luminance[1:-1, 1:-1]
    neighbour_mean = (
        luminance[:-2, 1:-1] + luminance[2:, 1:-1]
        + luminance[1:-1, :-2] + luminance[1:-1, 2:]
    ) / 4.0
    jump = np.abs(center - neighbour_mean)
    return int(np.count_nonzero(jump > 0.55))


def highlight_continuity(rgba: np.ndarray) -> dict:
    colour = rgba[:, :, :3]
    horizontal = np.abs(np.diff(colour, axis=1))
    vertical = np.abs(np.diff(colour, axis=0))
    return {
        "max_horizontal_channel_jump": round(float(horizontal.max()), 6),
        "max_vertical_channel_jump": round(float(vertical.max()), 6),
        "mean_gradient": round(float(np.mean(
            np.gradient(colour, axis=(0, 1)))), 6),
    }


def penumbra_trend_samples() -> list[dict]:
    samples = []
    for light_distance in (4.0, 8.0, 12.0):
        for receiver_distance in (1.0, 2.0, 4.0):
            samples.append({
                "light_to_kesi": light_distance,
                "kesi_to_receiver": receiver_distance,
                "sample_density": 16,
                "penumbra_width": round(penumbra_width(
                    light_distance, receiver_distance, 16), 6),
            })
    return samples


def landing_summary(scene: dict) -> dict:
    tables = build_tables(scene)
    _, landing = build_shadow_fields(scene, tables)
    per_light = {}
    for point in landing:
        bucket = per_light.setdefault(point.light_id, {
            "total": 0, "inside": 0, "outside": 0,
            "example_coordinates": [],
            "inside_penumbra_min": None,
            "inside_penumbra_max": None})
        bucket["total"] += 1
        if point.inside_receiver:
            bucket["inside"] += 1
            width = point.penumbra_width
            bucket["inside_penumbra_min"] = (
                width if bucket["inside_penumbra_min"] is None
                else min(bucket["inside_penumbra_min"], width))
            bucket["inside_penumbra_max"] = (
                width if bucket["inside_penumbra_max"] is None
                else max(bucket["inside_penumbra_max"], width))
        else:
            bucket["outside"] += 1
        if len(bucket["example_coordinates"]) < 8:
            bucket["example_coordinates"].append(point.as_dict())
    return per_light


def shadow_gray_distribution_check(scene: dict) -> dict:
    """Rebuild the same shadow fields twice and compare them byte-for-byte."""
    tables_a = build_tables(scene)
    tables_b = build_tables(scene)
    fields_a, _ = build_shadow_fields(scene, tables_a)
    fields_b, _ = build_shadow_fields(scene, tables_b)
    comparisons = []
    identical = True
    for key in sorted(fields_a, key=lambda item: str(item)):
        bytes_a = fields_a[key].transmission.astype("<f4").tobytes()
        bytes_b = fields_b[key].transmission.astype("<f4").tobytes()
        same = bytes_a == bytes_b
        identical = identical and same
        comparisons.append({
            "receiver": key[0],
            "light_id": key[1],
            "byte_identical": bool(same),
            "gray_mean": round(float(fields_a[key].transmission.mean()), 6),
            "gray_std": round(float(fields_a[key].transmission.std()), 6),
        })
    return {"byte_identical": bool(identical), "fields": comparisons}


def run_acceptance(output_dir: str, *, small: bool = True) -> dict:
    os.makedirs(output_dir, exist_ok=True)
    scene = generate_scene(small=small)
    scene_path = os.path.join(output_dir, "scene.acceptance.json")
    from .scene_gen import scene_canonical_json
    with open(scene_path, "wb") as handle:
        handle.write(scene_canonical_json(scene))
        handle.write(b"\n")

    reference_dir = os.path.join(output_dir, "reference")
    interrupted_dir = os.path.join(output_dir, "interrupted")
    reference = render_frame(scene, reference_dir, 0, 0.0,
                             allow_resume=False)
    # Simulate a hard stop after a subset of tiles, then a process restart.
    layout_count = len(reference.report["blocks"])
    partial_tiles = max(1, layout_count // 2)
    render_frame(scene, interrupted_dir, 0, 0.0,
                 allow_resume=False, max_tiles=partial_tiles)
    resumed = render_frame(scene, interrupted_dir, 0, 0.0,
                           allow_resume=True)
    rgba_identical = bool(np.array_equal(reference.rgba, resumed.rgba))
    owner_identical = bool(np.array_equal(
        reference.ownership, resumed.ownership))

    reference_png = os.path.join(
        reference_dir, "frame_00", "render.rgba.png")
    resumed_png = os.path.join(
        interrupted_dir, "frame_00", "render.rgba.png")
    with open(reference_png, "rb") as handle:
        png_a = handle.read()
    with open(resumed_png, "rb") as handle:
        png_b = handle.read()

    report = {
        "scene": scene_path,
        "tile_count": layout_count,
        "hard_stop_after_tiles": partial_tiles,
        "highlight_continuity": highlight_continuity(reference.rgba),
        "isolated_pixel_count": isolated_pixel_count(reference.rgba),
        "penumbra_trend": penumbra_trend_samples(),
        "landing_summary": landing_summary(scene),
        "shadow_distribution": shadow_gray_distribution_check(scene),
        "resume_verification": {
            "rgba_array_identical": rgba_identical,
            "ownership_array_identical": owner_identical,
            "png_bytes_identical": png_a == png_b,
        },
        "layer_contribution": reference.report["layer_contribution"],
        "block_statistics": [
            block["stats"] for block in reference.report["blocks"]
        ],
    }
    report_path = os.path.join(output_dir, "acceptance_report.json")
    with open(report_path, "w", encoding="utf-8") as handle:
        json.dump(report, handle, sort_keys=True, indent=2,
                  ensure_ascii=False)
    return report
