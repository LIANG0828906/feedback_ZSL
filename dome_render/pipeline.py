"""End-to-end render pipeline, resume verification, and reports."""

from __future__ import annotations

import hashlib
import json
import os
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import RenderFailure
from .png import write_rgba16, write_rgba8
from .projection import build_projection_report, penumbra_trend_report, shadow_summary
from .renderer import BlockResult, BlockStats, PreparedRenderer, render_block
from .scene import Scene
from .snapshot import (
    build_snapshot,
    block_color_summary,
    load_snapshot,
    write_snapshot_atomic,
)


@dataclass
class InterruptFlag:
    requested: bool = False


def _write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False) + "\n"
    path.write_text(text, encoding="utf-8")


def _raw_digest(array: np.ndarray, dtype: str) -> str:
    return hashlib.sha256(np.asarray(array).astype(dtype, copy=False).tobytes()).hexdigest()


def _compare_block(
    stored_rgba: np.ndarray,
    stored_layer: np.ndarray,
    fresh: BlockResult,
    block_id: int,
    limit: int = 128,
) -> dict[str, Any] | None:
    differences: list[dict[str, Any]] = []
    total = 0
    channels = (
        ("rgba16.r", stored_rgba[..., 0], fresh.rgba16[..., 0]),
        ("rgba16.g", stored_rgba[..., 1], fresh.rgba16[..., 1]),
        ("rgba16.b", stored_rgba[..., 2], fresh.rgba16[..., 2]),
        ("rgba16.a", stored_rgba[..., 3], fresh.rgba16[..., 3]),
        ("layer_map.r", stored_layer[..., 0], fresh.layer_map[..., 0]),
        ("layer_map.g", stored_layer[..., 1], fresh.layer_map[..., 1]),
        ("layer_map.b", stored_layer[..., 2], fresh.layer_map[..., 2]),
        ("layer_map.a", stored_layer[..., 3], fresh.layer_map[..., 3]),
    )
    for channel_name, stored, expected in channels:
        mismatch = stored != expected
        count = int(np.count_nonzero(mismatch))
        if count == 0:
            continue
        total += count
        if len(differences) >= limit:
            continue
        rows, cols = np.nonzero(mismatch)
        for row, col in zip(rows, cols):
            if len(differences) >= limit:
                break
            differences.append(
                {
                    "block_id": block_id,
                    "pixel": [int(fresh.x + col), int(fresh.y + row)],
                    "channel": channel_name,
                    "stored": int(stored[row, col]),
                    "expected": int(expected[row, col]),
                }
            )
    if total == 0:
        return None
    return {
        "block_id": block_id,
        "difference_count": total,
        "reported_count": len(differences),
        "differences": differences,
    }


def _clear_completed_artifacts(out_dir: Path) -> None:
    for name in ("rgba.png", "layer_map.png", "projections.json", "report.json", "differences.json", "recovery.json"):
        path = out_dir / name
        if path.exists():
            path.unlink()


def _recovery_report(
    status: str,
    scene: Scene,
    completed_ids: list[int],
    recovery_count: int,
    snapshot_used: bool,
    snapshot_discarded: bool,
    discard_reason: str | None,
    resumed_ids: list[int],
    fresh_ids: list[int],
) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "status": status,
        "interrupted": status == "interrupted",
        "scene_digest": scene.digest,
        "recovery_count": recovery_count,
        "snapshot_used": snapshot_used,
        "snapshot_discarded": snapshot_discarded,
        "snapshot_discard_reason": discard_reason,
        "completed_block_count": len(completed_ids),
        "total_block_count": scene.renderer.block_count,
        "completed_block_ids": completed_ids,
        "resumed_block_ids": resumed_ids,
        "freshly_rendered_block_ids": fresh_ids,
    }


def _assemble_image(scene: Scene, blocks: dict[int, tuple[np.ndarray, np.ndarray]]) -> tuple[np.ndarray, np.ndarray]:
    rgba = np.zeros((scene.renderer.height, scene.renderer.width, 4), dtype=np.uint16)
    layer = np.zeros((scene.renderer.height, scene.renderer.width, 4), dtype=np.uint8)
    for block_id in sorted(blocks):
        x, y, width, height = scene.renderer.block_rect(block_id)
        block_rgba, block_layer = blocks[block_id]
        rgba[y : y + height, x : x + width] = block_rgba
        layer[y : y + height, x : x + width] = block_layer
    return rgba, layer


def _stable_stats(
    scene: Scene,
    renderer: PreparedRenderer,
    stats: BlockStats,
    blocks: dict[int, tuple[np.ndarray, np.ndarray]],
    rgba: np.ndarray,
    layer_map: np.ndarray,
    projection_report: dict[str, Any],
    recovery_count: int,
    snapshot_used: bool,
    snapshot_discarded: bool,
    resumed_ids: list[int],
    fresh_ids: list[int],
) -> dict[str, Any]:
    layers_out: list[dict[str, Any]] = []
    for layer in sorted(scene.layers, key=lambda item: item.index):
        board_samples = stats.board_samples.get(layer.index, 0)
        transmission_sum = stats.transmission_sum.get(layer.index, 0.0)
        reflection_sum = stats.reflection_sum.get(layer.index, 0.0)
        denominator = transmission_sum + reflection_sum
        star_entries = []
        for board in sorted(layer.boards, key=lambda item: item.board_id):
            field = renderer.stars[(layer.index, board.board_id)]
            star_entries.append(
                {
                    "board_id": board.board_id,
                    "seed": board.texture_seed,
                    "resolution": [board.resolution[0], board.resolution[1]],
                    "star_count": field.count,
                    "texture_digest": field.texture_digest,
                }
            )
        layers_out.append(
            {
                "index": layer.index,
                "visible_fragments": int(stats.visible_fragments.get(layer.index, 0)),
                "board_fragments": board_samples,
                "rib_fragments": int(stats.rib_samples.get(layer.index, 0)),
                "average_transmission": round(transmission_sum / board_samples, 9) if board_samples else 0.0,
                "transmission_contribution": round(transmission_sum, 9),
                "reflection_contribution": round(reflection_sum, 9),
                "transmission_ratio": round(transmission_sum / denominator, 9) if denominator > 1.0e-15 else 0.0,
                "reflection_ratio": round(reflection_sum / denominator, 9) if denominator > 1.0e-15 else 0.0,
                "stars": star_entries,
            }
        )
    block_entries = []
    for block_id in sorted(blocks):
        x, y, width, height = scene.renderer.block_rect(block_id)
        block_rgba, block_layer = blocks[block_id]
        block_entries.append(
            {
                "id": block_id,
                "rect": [x, y, width, height],
                "rgba_sha256": _raw_digest(block_rgba, "<u2"),
                "layer_sha256": _raw_digest(block_layer, "u1"),
                "color_summary": block_color_summary(block_rgba, block_layer),
            }
        )
    projection_sample_count = sum(int(light["entry_count"]) for light in projection_report["lights"])
    projection_accepted_count = sum(int(light["accepted_count"]) for light in projection_report["lights"])
    projection_canonical = json.dumps(projection_report, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return {
        "schema_version": 1,
        "status": "complete",
        "scene": scene.summary,
        "render": {
            "width": scene.renderer.width,
            "height": scene.renderer.height,
            "block_size": [scene.renderer.block_size[0], scene.renderer.block_size[1]],
            "supersample": scene.renderer.supersample,
            "shadow_samples": scene.renderer.shadow_samples,
            "projection_density": scene.renderer.projection_density,
            "exposure": scene.renderer.exposure,
            "premultiplied_alpha": True,
            "color_encoding": "sRGB-16",
            "tone_mapping": "1-exp(-linear*exposure)",
        },
        "layers": layers_out,
        "wall": {
            "visible_fragments": int(stats.wall_samples),
            "resolution": [scene.wall.resolution[0], scene.wall.resolution[1]],
        },
        "projection": {
            "sample_count": projection_sample_count,
            "accepted_count": projection_accepted_count,
            "outside_count": projection_sample_count - projection_accepted_count,
            "report_digest": hashlib.sha256(projection_canonical).hexdigest(),
            "shadow_maps": shadow_summary(renderer.shadows),
            "penumbra_trend": penumbra_trend_report(scene),
        },
        "blocks": {
            "completed_count": len(blocks),
            "total_count": scene.renderer.block_count,
            "completed_ids": sorted(blocks),
            "entries": block_entries,
        },
        "recovery": {
            "recovery_count": recovery_count,
            "snapshot_used": snapshot_used,
            "snapshot_discarded": snapshot_discarded,
            "resumed_block_ids": resumed_ids,
            "freshly_rendered_block_ids": fresh_ids,
        },
        "image_digest": _raw_digest(rgba, "<u2"),
        "layer_map_digest": _raw_digest(layer_map, "u1"),
    }


def run(
    scene: Scene,
    out_dir: Path,
    *,
    fresh: bool,
    interrupt: InterruptFlag,
    progress: bool = True,
) -> int:
    out_dir.mkdir(parents=True, exist_ok=True)
    _clear_completed_artifacts(out_dir)
    snapshot_path = out_dir / "snapshot.json"
    renderer = PreparedRenderer.build(scene)
    if fresh and snapshot_path.exists():
        snapshot_path.unlink()
    load_result = load_snapshot(snapshot_path, scene) if not fresh else None
    if load_result is not None and load_result.snapshot is not None and len(load_result.snapshot.blocks) == scene.renderer.block_count:
        # A complete snapshot is an output cache, not a resumable partial run.
        # Re-rendering from zero keeps repeated-run statistics stable.
        if snapshot_path.exists():
            snapshot_path.unlink()
        load_result = None
    snapshot_used = load_result is not None and load_result.snapshot is not None
    snapshot_discarded = load_result is not None and load_result.discarded
    discard_reason = load_result.reason if snapshot_discarded else None
    if snapshot_discarded and snapshot_path.exists():
        snapshot_path.unlink()

    loaded_blocks: dict[int, tuple[np.ndarray, np.ndarray]] = {}
    recovery_count = 0
    if snapshot_used and load_result is not None and load_result.snapshot is not None:
        loaded_blocks = dict(load_result.snapshot.blocks)
        recovery_count = load_result.snapshot.recovery_count + 1

    total_stats = BlockStats()
    blocks: dict[int, tuple[np.ndarray, np.ndarray]] = dict(loaded_blocks)
    completed_ids: list[int] = []
    resumed_ids: list[int] = []
    fresh_ids: list[int] = []
    interrupted = False
    delay_ms = int(os.environ.get("DOME_TEST_BLOCK_DELAY_MS", "0") or "0")

    if interrupt.requested:
        report = _recovery_report(
            "interrupted",
            scene,
            sorted(loaded_blocks),
            recovery_count,
            snapshot_used,
            snapshot_discarded,
            discard_reason,
            resumed_ids,
            fresh_ids,
        )
        _write_json(out_dir / "recovery.json", report)
        return 0

    for block_id in range(scene.renderer.block_count):
        if block_id in loaded_blocks:
            fresh_block = render_block(renderer, block_id)
            stored_rgba, stored_layer = loaded_blocks[block_id]
            difference = _compare_block(stored_rgba, stored_layer, fresh_block, block_id)
            if difference is not None:
                report = {
                    "ok": False,
                    "error": {
                        "code": "E_RESUME_MISMATCH",
                        "message": "Resumed block differs from deterministic recomputation.",
                        "block_id": block_id,
                        "difference_count": difference["difference_count"],
                        "reported_count": difference["reported_count"],
                        "differences": difference["differences"],
                    },
                }
                _write_json(out_dir / "differences.json", report)
                raise RenderFailure(
                    "E_RESUME_MISMATCH",
                    "Resumed block differs from deterministic recomputation; see differences.json.",
                    {
                        "block_id": block_id,
                        "difference_count": difference["difference_count"],
                        "differences_file": "differences.json",
                    },
                )
            total_stats.merge(fresh_block.stats)
            resumed_ids.append(block_id)
            if progress:
                print(f"[resume verify] block {block_id + 1}/{scene.renderer.block_count}", file=sys.stderr, flush=True)
        else:
            result = render_block(renderer, block_id)
            blocks[block_id] = (result.rgba16, result.layer_map)
            total_stats.merge(result.stats)
            fresh_ids.append(block_id)
            if progress:
                print(f"[render] block {block_id + 1}/{scene.renderer.block_count}", file=sys.stderr, flush=True)
        completed_ids = sorted(blocks)
        snapshot = build_snapshot(scene, blocks, recovery_count)
        write_snapshot_atomic(snapshot_path, snapshot)
        if delay_ms > 0:
            time.sleep(delay_ms / 1000.0)
        if interrupt.requested:
            interrupted = True
            break

    if interrupted:
        report = _recovery_report(
            "interrupted",
            scene,
            completed_ids,
            recovery_count,
            snapshot_used,
            snapshot_discarded,
            discard_reason,
            resumed_ids,
            fresh_ids,
        )
        _write_json(out_dir / "recovery.json", report)
        return 0

    if len(blocks) != scene.renderer.block_count:
        raise RenderFailure(
            "E_BLOCK_INCOMPLETE",
            "Render ended with incomplete blocks.",
            {"completed": len(blocks), "total": scene.renderer.block_count},
        )

    rgba, layer_map = _assemble_image(scene, blocks)
    write_rgba16(out_dir / "rgba.png", rgba)
    write_rgba8(out_dir / "layer_map.png", layer_map)
    projection_report = build_projection_report(scene, renderer.shadows)
    _write_json(out_dir / "projections.json", projection_report)
    stats_report = _stable_stats(
        scene,
        renderer,
        total_stats,
        blocks,
        rgba,
        layer_map,
        projection_report,
        recovery_count,
        snapshot_used,
        snapshot_discarded,
        resumed_ids,
        fresh_ids,
    )
    _write_json(out_dir / "report.json", stats_report)
    recovery = _recovery_report(
        "complete",
        scene,
        completed_ids,
        recovery_count,
        snapshot_used,
        snapshot_discarded,
        discard_reason,
        resumed_ids,
        fresh_ids,
    )
    _write_json(out_dir / "recovery.json", recovery)
    return 0
