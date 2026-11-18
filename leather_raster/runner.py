from __future__ import annotations

import os
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .output import assemble_blocks, compare_outputs, write_render_outputs
from .projection import build_projection_data
from .render import all_prepared_layers, block_specs, render_block
from .scene import Scene
from .snapshot import LoadResult, SnapshotWriter, load_snapshot, snapshot_file
from .util import atomic_write_json


@dataclass(frozen=True)
class RenderOutcome:
    status: str
    exit_code: int
    completed_blocks: int
    total_blocks: int
    rendered_this_run: int
    loaded_blocks: int
    snapshot_status: str
    snapshot_reason: str | None
    report_path: str


def _write_recovery_report(out_dir: str | Path, report: dict[str, Any]) -> str:
    path = Path(out_dir) / "recovery_report.json"
    atomic_write_json(path, report)
    return str(path)


def run_render(
    scene: Scene,
    out_dir: str | Path,
    *,
    resume: bool = True,
    fresh: bool = False,
    interrupt_after_blocks: int | None = None,
    stop_event: Any | None = None,
    verify_baseline: str | Path | None = None,
    max_differences: int = 512,
) -> RenderOutcome:
    destination = Path(out_dir)
    destination.mkdir(parents=True, exist_ok=True)
    checkpoint = snapshot_file(destination)
    if fresh and checkpoint.exists():
        checkpoint.unlink()

    load_result = load_snapshot(scene, destination) if resume and not fresh else LoadResult(status="empty")
    if load_result.status == "rejected":
        if checkpoint.exists():
            checkpoint.unlink()
        load_result = LoadResult(
            status="empty",
            reason=load_result.reason,
            discarded_blocks=load_result.discarded_blocks,
        )
    elif not resume and not fresh and checkpoint.exists():
        checkpoint.unlink()
        load_result = LoadResult(status="empty")

    completed: dict[str, Any] = dict(load_result.blocks)
    writer = SnapshotWriter(scene, destination)
    if completed:
        writer.seed(completed)

    projection = build_projection_data(scene)
    prepared = all_prepared_layers(scene)
    specs = block_specs(scene)
    rendered_this_run = 0
    interrupted = False

    for spec in specs:
        if spec.block_id in completed:
            continue
        if stop_event is not None and stop_event.is_set():
            interrupted = True
            break
        result = render_block(scene, spec, prepared, projection)
        completed[spec.block_id] = result
        writer.commit(result)
        rendered_this_run += 1
        delay_ms = int(os.environ.get("LEATHER_RASTER_BLOCK_DELAY_MS", "0"))
        if delay_ms > 0:
            time.sleep(delay_ms / 1000.0)
        if interrupt_after_blocks is not None and rendered_this_run >= interrupt_after_blocks:
            interrupted = True
            break
        if stop_event is not None and stop_event.is_set():
            interrupted = True
            break

    total_blocks = len(specs)
    completed_count = sum(1 for spec in specs if spec.block_id in completed)
    next_block = next(
        (spec.block_id for spec in specs if spec.block_id not in completed),
        None,
    )
    snapshot_origin = load_result.status if load_result.status != "empty" else "fresh"
    if interrupted:
        report = {
            "schema_version": 1,
            "status": "interrupted",
            "exit_code": 0,
            "completed_blocks": completed_count,
            "total_blocks": total_blocks,
            "loaded_blocks_at_start": len(load_result.blocks),
            "rendered_blocks_this_run": rendered_this_run,
            "next_block_id": next_block,
            "snapshot_origin": snapshot_origin,
            "snapshot_rejected_on_load": load_result.reason is not None,
            "snapshot_rejection_reason": load_result.reason,
            "snapshot_discarded_blocks": load_result.discarded_blocks,
        }
        report_path = _write_recovery_report(destination, report)
        return RenderOutcome(
            status="interrupted",
            exit_code=0,
            completed_blocks=completed_count,
            total_blocks=total_blocks,
            rendered_this_run=rendered_this_run,
            loaded_blocks=len(load_result.blocks),
            snapshot_status=snapshot_origin,
            snapshot_reason=load_result.reason,
            report_path=report_path,
        )

    rgba, layer_map, ordered = assemble_blocks(scene, completed)
    write_render_outputs(scene, destination, rgba, layer_map, ordered, projection)
    if verify_baseline is not None:
        compare_outputs(
            scene,
            destination,
            verify_baseline,
            max_differences=max_differences,
        )

    report = {
        "schema_version": 1,
        "status": "completed",
        "exit_code": 0,
        "completed_blocks": completed_count,
        "total_blocks": total_blocks,
        "loaded_blocks_at_start": len(load_result.blocks),
        "rendered_blocks_this_run": rendered_this_run,
        "snapshot_origin": snapshot_origin,
        "snapshot_rejected_on_load": load_result.reason is not None,
        "snapshot_rejection_reason": load_result.reason,
        "snapshot_discarded_blocks": load_result.discarded_blocks,
        "verified_against_baseline": verify_baseline is not None,
    }
    report_path = _write_recovery_report(destination, report)
    return RenderOutcome(
        status="completed",
        exit_code=0,
        completed_blocks=completed_count,
        total_blocks=total_blocks,
        rendered_this_run=rendered_this_run,
        loaded_blocks=len(load_result.blocks),
        snapshot_status=snapshot_origin,
        snapshot_reason=load_result.reason,
        report_path=report_path,
    )

