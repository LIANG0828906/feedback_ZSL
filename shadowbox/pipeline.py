"""Chunked rendering orchestration, atomic recovery, and stable reports."""

from __future__ import annotations

import json
import os
import signal
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np
from PIL import Image

from .checkpoint import (
    CheckpointInvalid,
    StoredBlock,
    atomic_write_bytes,
    atomic_write_json,
    block_summary,
    load_checkpoint,
    save_checkpoint,
    sha256_bytes,
    stored_block_from_render,
)
from .diff import (
    compare_arrays,
    load_raw_output,
    raw_depth_bytes,
    raw_rgba_bytes,
    write_diff_report,
)
from .procedural import generate_textures
from .render import (
    PreparedScene,
    block_bounds,
    block_grid,
    build_penumbra_probes,
    prepare_scene,
    render_block,
)
from .scene import ENGINE_VERSION, Scene

REPORT_FORMAT = "shadowbox-report-v1"
RESUME_FORMAT = "shadowbox-resume-v1"


@dataclass(frozen=True)
class RunResult:
    exit_code: int
    interrupted: bool
    report_path: Path | None
    resume_report_path: Path | None
    diff_report_path: Path | None
    recovery_count: int
    discarded_checkpoints: int


def _remove_if_exists(path: Path) -> None:
    try:
        path.unlink()
    except FileNotFoundError:
        pass


def _remove_final_outputs(directory: Path) -> None:
    for name in (
        "rgba.raw",
        "rgba.png",
        "rgba_straight.png",
        "optical_depth.f32",
        "report.json",
        "diff.json",
        "resume_report.json",
    ):
        _remove_if_exists(directory / name)


def _write_png(path: Path, array: np.ndarray) -> None:
    image = Image.fromarray(array, mode="RGBA")
    temporary = path.with_name(f".{path.name}.tmp.{os.getpid()}")
    image.save(temporary, format="PNG", optimize=False, compress_level=9)
    os.replace(temporary, path)


def _straight_rgba(premultiplied: np.ndarray) -> np.ndarray:
    rgba = np.zeros_like(premultiplied, dtype=np.uint8)
    alpha = premultiplied[:, :, 3].astype(np.float64) / 255.0
    valid = alpha > 0.0
    rgb = premultiplied[:, :, :3].astype(np.float64)
    straight = np.zeros_like(rgb)
    straight[valid] = rgb[valid] / alpha[valid, None]
    rgba[:, :, :3] = np.clip(np.rint(straight), 0, 255).astype(np.uint8)
    rgba[:, :, 3] = premultiplied[:, :, 3]
    return rgba


def _assemble_arrays(
    scene: Scene, blocks: dict[int, StoredBlock]
) -> tuple[np.ndarray, np.ndarray]:
    columns, rows = block_grid(scene.camera)
    rgba = np.zeros((scene.camera.height, scene.camera.width, 4), dtype=np.uint8)
    depth = np.zeros((scene.camera.height, scene.camera.width), dtype="<f4")
    for index in range(columns * rows):
        if index not in blocks:
            raise RuntimeError(f"cannot assemble output: block {index} is missing")
        block = blocks[index].block
        rgba[block.y : block.y + block.height, block.x : block.x + block.width] = (
            block.rgba
        )
        depth[
            block.y : block.y + block.height, block.x : block.x + block.width
        ] = block.optical_depth.astype("<f4")
    return rgba, depth


def _sum_metrics(
    scene: Scene, blocks: dict[int, StoredBlock]
) -> list[dict[str, int | float]]:
    ordered = scene.layers_inner_to_outer
    totals = [
        {
            "fragment_samples": 0,
            "paper_fragment_samples": 0,
            "hole_fragment_samples": 0,
            "transmittance_sum": 0.0,
        }
        for _ in ordered
    ]
    for index in sorted(blocks):
        for layer_position, metric in enumerate(blocks[index].block.layer_metrics):
            totals[layer_position]["fragment_samples"] += metric.fragment_samples
            totals[layer_position]["paper_fragment_samples"] += (
                metric.paper_fragment_samples
            )
            totals[layer_position]["hole_fragment_samples"] += (
                metric.hole_fragment_samples
            )
            totals[layer_position]["transmittance_sum"] += metric.transmittance_sum
    return totals


def _build_report(
    prepared: PreparedScene,
    blocks: dict[int, StoredBlock],
    rgba: np.ndarray,
    depth: np.ndarray,
    *,
    recovery_count: int,
    discarded_count: int,
) -> dict[str, Any]:
    scene = prepared.scene
    columns, rows = block_grid(scene.camera)
    totals = _sum_metrics(scene, blocks)
    layer_reports: list[dict[str, Any]] = []
    for layer, metrics, texture in zip(
        scene.layers_inner_to_outer,
        totals,
        prepared.textures.layers,
    ):
        paper_count = int(metrics["paper_fragment_samples"])
        average_transmittance = (
            float(metrics["transmittance_sum"]) / paper_count
            if paper_count
            else 0.0
        )
        projection = prepared.projections.get(layer.layer_index)
        layer_reports.append(
            {
                "layer_index": layer.layer_index,
                "name": layer.name,
                "world_z_mm": round(layer.world_z_mm, 6),
                "air_gap_to_prev_mm": round(layer.air_gap_to_prev_mm, 6),
                "scene_transmittance": round(layer.transmittance, 9),
                "visible_fragment_samples": int(metrics["fragment_samples"]),
                "paper_fragment_samples": paper_count,
                "hole_fragment_samples": int(metrics["hole_fragment_samples"]),
                "average_transmittance": round(average_transmittance, 6),
                "hole_count": texture.hole_count,
                "hole_fraction": round(texture.hole_fraction, 6),
                "fiber_table_sha256": texture.fiber_sha256,
                "hole_mask_sha256": texture.hole_sha256,
                "soft_shadow_samples": projection.sample_count if projection else 0,
                "projection_air_gap_mm": (
                    round(projection.air_gap_mm, 6) if projection else 0.0
                ),
                "penumbra_target_mm": (
                    round(projection.penumbra_target_mm, 6) if projection else 0.0
                ),
            }
        )

    straight = _straight_rgba(rgba)
    block_reports = [
        dict(blocks[index].summary)
        for index in range(columns * rows)
    ]
    probes = build_penumbra_probes(
        scene,
        prepared.textures,
        point_count=241,
        span_mm=60.0,
    )
    return {
        "format": REPORT_FORMAT,
        "engine_version": ENGINE_VERSION,
        "scene_sha256": scene.digest,
        "summary": {
            "resolution": [scene.camera.width, scene.camera.height],
            "block_grid": [columns, rows],
            "block_size": [
                scene.camera.block_width,
                scene.camera.block_height,
            ],
            "samples_per_pixel": scene.camera.samples_per_pixel,
            "block_count": columns * rows,
            "completed_blocks": len(blocks),
            "image_encoding": "premultiplied_srgb8",
            "image_rgba_sha256": sha256_bytes(raw_rgba_bytes(rgba)),
            "straight_rgba_sha256": sha256_bytes(raw_rgba_bytes(straight)),
            "optical_depth_sha256": sha256_bytes(raw_depth_bytes(depth)),
            "recovery_count": recovery_count,
            "discarded_checkpoints": discarded_count,
            "deterministic": True,
        },
        "layers": layer_reports,
        "penumbra_probes": probes,
        "blocks": block_reports,
    }


def _write_outputs(
    scene: Scene,
    rgba: np.ndarray,
    depth: np.ndarray,
    report: dict[str, Any],
    directory: Path,
) -> None:
    rgba_payload = raw_rgba_bytes(rgba)
    depth_payload = raw_depth_bytes(depth)
    atomic_write_bytes(directory / "rgba.raw", rgba_payload)
    atomic_write_bytes(directory / "optical_depth.f32", depth_payload)
    _write_png(directory / "rgba.png", rgba)
    _write_png(directory / "rgba_straight.png", _straight_rgba(rgba))
    atomic_write_json(directory / "report.json", report)


def _resume_report(
    scene: Scene,
    blocks: dict[int, StoredBlock],
    reason: str,
    recovery_count: int,
) -> dict[str, Any]:
    columns, rows = block_grid(scene.camera)
    total = columns * rows
    completed = sorted(blocks)
    next_block = 0
    for index in range(total):
        if index not in blocks:
            next_block = index
            break
    return {
        "format": RESUME_FORMAT,
        "scene_sha256": scene.digest,
        "reason": reason,
        "completed_blocks": completed,
        "completed_count": len(completed),
        "next_block": next_block,
        "total_blocks": total,
        "checkpoint_state": "partial",
        "recovery_count": recovery_count,
    }


def _install_stop_handlers() -> tuple[dict[int, Any], Callable[[], str | None]]:
    previous: dict[int, Any] = {}
    state: dict[str, str | None] = {"reason": None}

    def handler(signum, _frame):
        state["reason"] = "signal"

    for signal_name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, signal_name):
            signum = getattr(signal, signal_name)
            try:
                previous[signum] = signal.getsignal(signum)
                signal.signal(signum, handler)
            except (OSError, ValueError):
                pass

    def reason() -> str | None:
        return state["reason"]

    return previous, reason


def _restore_stop_handlers(previous: dict[int, Any]) -> None:
    for signum, handler in previous.items():
        try:
            signal.signal(signum, handler)
        except (OSError, ValueError):
            pass


def render_to_directory(
    scene: Scene,
    directory: str | Path,
    *,
    fresh: bool = False,
    interrupt_after_blocks: int | None = None,
    reference_directory: str | Path | None = None,
) -> RunResult:
    if interrupt_after_blocks is not None and interrupt_after_blocks < 1:
        raise ValueError("interrupt_after_blocks must be at least 1")
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    checkpoint_path = directory / "checkpoint.json"
    _remove_final_outputs(directory)
    if fresh:
        _remove_if_exists(checkpoint_path)

    prepared = prepare_scene(scene, generate_textures(scene))
    recovery_count = 0
    discarded_count = 0
    blocks: dict[int, StoredBlock] = {}
    checkpoint_state = "partial"
    if checkpoint_path.exists() and not fresh:
        try:
            loaded = load_checkpoint(checkpoint_path, scene)
        except CheckpointInvalid:
            _remove_if_exists(checkpoint_path)
            discarded_count = 1
        else:
            blocks = dict(loaded.blocks)
            discarded_count = loaded.discarded_count
            recovery_count = loaded.recovery_count
            if loaded.state == "partial":
                recovery_count += 1
            checkpoint_state = loaded.state

    columns, rows = block_grid(scene.camera)
    total_blocks = columns * rows
    previous_handlers, stop_reason = _install_stop_handlers()
    computed_this_run = 0
    interrupted_reason: str | None = None
    try:
        for block_index in range(total_blocks):
            if block_index in blocks:
                continue
            rendered = render_block(prepared, block_index)
            blocks[block_index] = stored_block_from_render(rendered)
            computed_this_run += 1
            complete = len(blocks) == total_blocks
            state = "complete" if complete else "partial"
            save_checkpoint(
                checkpoint_path,
                scene,
                blocks,
                state=state,
                recovery_count=recovery_count,
                discarded_count=discarded_count,
            )
            checkpoint_state = state
            if complete:
                break
            if interrupt_after_blocks is not None and (
                computed_this_run >= interrupt_after_blocks
            ):
                interrupted_reason = "requested-after-blocks"
                break
            if stop_reason() is not None:
                interrupted_reason = str(stop_reason())
                break
    finally:
        _restore_stop_handlers(previous_handlers)

    if len(blocks) < total_blocks:
        reason = interrupted_reason or "signal"
        resume = _resume_report(scene, blocks, reason, recovery_count)
        atomic_write_json(directory / "resume_report.json", resume)
        return RunResult(
            exit_code=0,
            interrupted=True,
            report_path=None,
            resume_report_path=directory / "resume_report.json",
            diff_report_path=None,
            recovery_count=recovery_count,
            discarded_checkpoints=discarded_count,
        )

    if checkpoint_state != "complete":
        save_checkpoint(
            checkpoint_path,
            scene,
            blocks,
            state="complete",
            recovery_count=recovery_count,
            discarded_count=discarded_count,
        )
    rgba, depth = _assemble_arrays(scene, blocks)
    report = _build_report(
        prepared,
        blocks,
        rgba,
        depth,
        recovery_count=recovery_count,
        discarded_count=discarded_count,
    )
    _write_outputs(scene, rgba, depth, report, directory)

    if reference_directory is not None:
        reference_rgba, reference_depth = load_raw_output(
            reference_directory,
            scene.camera.width,
            scene.camera.height,
        )
        comparison = compare_arrays(
            reference_rgba,
            rgba,
            reference_depth,
            depth,
            scene,
        )
        if not comparison["equal"]:
            diff_path = directory / "diff.json"
            write_diff_report(diff_path, comparison)
            return RunResult(
                exit_code=4,
                interrupted=False,
                report_path=directory / "report.json",
                resume_report_path=None,
                diff_report_path=diff_path,
                recovery_count=recovery_count,
                discarded_checkpoints=discarded_count,
            )

    return RunResult(
        exit_code=0,
        interrupted=False,
        report_path=directory / "report.json",
        resume_report_path=None,
        diff_report_path=None,
        recovery_count=recovery_count,
        discarded_checkpoints=discarded_count,
    )


def read_report(path: str | Path) -> dict[str, Any]:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)
