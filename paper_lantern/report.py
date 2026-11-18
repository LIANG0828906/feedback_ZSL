from __future__ import annotations

import json
import math
from pathlib import Path

import numpy as np

from .scene import Scene
from .state import OutputArtifacts, RenderState
from .util import atomic_write_bytes, stable_float


def build_success_report(
    scene: Scene,
    state: RenderState,
    artifacts: OutputArtifacts,
    snapshot_status: str,
) -> dict:
    layer_reports: list[dict] = []
    for layer_position, layer in enumerate(scene.layers):
        paper_count = int(state.paper_fragments[layer_position])
        average_transmittance = (
            float(state.effective_transmission_sum[layer_position]) / paper_count
            if paper_count
            else 0.0
        )
        mean_premultiplied = np.mean(
            state.layer_rgba[layer_position, ..., :3], axis=(0, 1)
        )
        mean_alpha = float(np.mean(state.layer_rgba[layer_position, ..., 3]))
        layer_file = f"layers/layer_{layer.layer_index:02d}.rgba"
        layer_reports.append(
            {
                "layer_index": layer.layer_index,
                "visible_fragments": int(state.visible_fragments[layer_position]),
                "paper_fragments": paper_count,
                "cutout_fragments": int(state.cutout_fragments[layer_position]),
                "average_transmittance": stable_float(average_transmittance),
                "mean_premultiplied_rgb": [
                    stable_float(value) for value in mean_premultiplied
                ],
                "mean_alpha": stable_float(mean_alpha),
                "rgba_file": layer_file,
                "rgba_sha256": artifacts.digests[layer_file],
            }
        )
    probes: list[dict] = []
    previous_width = -math.inf
    monotonic = True
    for distance in scene.shadow.probe_light_distances:
        width = math.tan(scene.shadow.softness) * float(distance)
        if width < previous_width - 1e-15:
            monotonic = False
        previous_width = width
        probes.append(
            {
                "light_distance": stable_float(distance),
                "half_penumbra_width": stable_float(width),
            }
        )
    return {
        "status": "ok",
        "exit_code": 0,
        "scene_digest": scene.digest,
        "image": {
            "width": scene.image.width,
            "height": scene.image.height,
            "tile_width": scene.image.tile_width,
            "tile_height": scene.image.tile_height,
            "rgba_file": "image.rgba",
            "rgba_sha256": artifacts.digests["image.rgba"],
            "png_file": "image.png",
            "png_sha256": artifacts.digests["image.png"],
            "depth_file": "depth.f32",
            "depth_sha256": artifacts.digests["depth.f32"],
        },
        "layers": layer_reports,
        "soft_shadow": {
            "samples_per_fragment": scene.shadow.samples,
            "softness_radians": stable_float(scene.shadow.softness),
            "sample_rays": int(state.shadow_sample_rays),
            "occluder_tests": int(state.shadow_occluder_tests),
            "penumbra_model": "half_width=tan(softness)*light_distance",
            "penumbra_probe": probes,
            "penumbra_probe_monotonic": monotonic,
        },
        "tiles": {
            "completed": len(state.completed_blocks),
            "total": scene.tile_count,
            "columns": scene.tile_columns,
            "rows": scene.tile_rows,
            "blocks": list(state.block_summaries),
        },
        "textures": {
            "manifest": "textures/manifest.json",
        },
        "recovery_count": int(state.recovery_count),
        "snapshot_status": snapshot_status,
    }


def write_json_report(path: str | Path, report: dict) -> None:
    payload = json.dumps(
        report,
        indent=2,
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8")
    payload += b"\n"
    atomic_write_bytes(Path(path), payload)


def build_validation_report(issues: list[dict]) -> dict:
    return {
        "status": "error",
        "exit_code": 2,
        "code": "SCENE_VALIDATION_FAILED",
        "image_written": False,
        "issues": issues,
    }


def build_resume_report(scene: Scene, state: RenderState) -> dict:
    completed = list(state.completed_blocks)
    return {
        "status": "recoverable_interrupt",
        "exit_code": 0,
        "scene_digest": scene.digest,
        "snapshot": "snapshot.bin",
        "completed_blocks": completed,
        "completed_count": len(completed),
        "total_blocks": scene.tile_count,
        "next_block": len(completed) if len(completed) < scene.tile_count else None,
        "block_summaries": list(state.block_summaries),
        "recovery_count": int(state.recovery_count),
        "image_written": False,
    }


def compare_resume_outputs(
    scene: Scene,
    resumed: OutputArtifacts,
    reference: OutputArtifacts,
    limit: int = 64,
) -> tuple[bool, dict]:
    differences: list[dict] = []
    total = 0
    rgba_a = resumed.rgba_u8
    rgba_b = reference.rgba_u8
    channel_names = ("R", "G", "B", "A")
    mismatch = np.argwhere(rgba_a != rgba_b)
    for y, x, channel in mismatch:
        total += 1
        if len(differences) >= limit:
            continue
        block = int(y // scene.image.tile_height) * scene.tile_columns + int(
            x // scene.image.tile_width
        )
        differences.append(
            {
                "block": block,
                "x": int(x),
                "y": int(y),
                "channel": channel_names[int(channel)],
                "resumed_value": int(rgba_a[y, x, channel]),
                "reference_value": int(rgba_b[y, x, channel]),
            }
        )
    depth_a_u32 = np.ascontiguousarray(resumed.depth_f32).view(np.uint32)
    depth_b_u32 = np.ascontiguousarray(reference.depth_f32).view(np.uint32)
    depth_mismatch = np.argwhere(depth_a_u32 != depth_b_u32)
    for y, x in depth_mismatch:
        total += 1
        if len(differences) >= limit:
            continue
        block = int(y // scene.image.tile_height) * scene.tile_columns + int(
            x // scene.image.tile_width
        )
        differences.append(
            {
                "block": block,
                "x": int(x),
                "y": int(y),
                "channel": "depth",
                "resumed_value": float(resumed.depth_f32[y, x]),
                "reference_value": float(reference.depth_f32[y, x]),
            }
        )
    report = {
        "status": "error",
        "exit_code": 4,
        "code": "RESUME_MISMATCH",
        "scene_digest": scene.digest,
        "difference_count": int(total),
        "differences_truncated": total > limit,
        "differences": differences,
    }
    return total == 0, report

