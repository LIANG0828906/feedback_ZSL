"""Chunked deterministic renderer with atomic snapshot recovery."""

from __future__ import annotations

import io
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .geometry import build_geometry, rasterize_scene
from .io_utils import atomic_write_bytes, atomic_write_json
from .pngio import encode_rgb_png, owner_visualization
from .report import build_projection_document, build_report
from .scene import Scene
from .shadow import build_shadow
from .shading import render_chunk
from .snapshot import (
    SnapshotLoad,
    SnapshotState,
    SnapshotStore,
    chunk_summary,
    summaries_for_completed,
)
from .texture import TextureBank


@dataclass
class RenderResult:
    status: str
    scene: Scene
    state: SnapshotState
    snapshot_load_status: str
    computed_chunks: list[int]
    resumed_chunks: list[int]
    snapshot_path: Path
    report: dict[str, Any] | None = None
    outputs: dict[str, Path] | None = None


def _npy_bytes(array: np.ndarray) -> bytes:
    buffer = io.BytesIO()
    np.save(buffer, array, allow_pickle=False)
    return buffer.getvalue()


def _write_completed_outputs(
    scene: Scene,
    state: SnapshotState,
    texture_bank: TextureBank,
    shadow,
    output_dir: Path,
) -> tuple[dict[str, Path], dict[str, Any]]:
    report = build_report(scene, texture_bank, shadow, state)
    projections = build_projection_document(scene, shadow)
    mapping = {
        "schema": "rosewindow-ownership-map-v1",
        "value_zero": "non-glass",
        "pieces": scene.piece_mapping(),
    }
    outputs = {
        "rgb_npy": output_dir / "rgb.npy",
        "rgb_png": output_dir / "rgb.png",
        "owner_npy": output_dir / "ownership.npy",
        "owner_png": output_dir / "ownership.png",
        "projection_json": output_dir / "shadow_projections.json",
        "ownership_map_json": output_dir / "ownership_map.json",
        "report_json": output_dir / "report.json",
    }
    atomic_write_bytes(outputs["rgb_npy"], _npy_bytes(state.rgb))
    atomic_write_bytes(outputs["rgb_png"], encode_rgb_png(state.rgb))
    atomic_write_bytes(outputs["owner_npy"], _npy_bytes(state.owner))
    atomic_write_bytes(
        outputs["owner_png"], encode_rgb_png(owner_visualization(state.owner))
    )
    atomic_write_json(outputs["projection_json"], projections)
    atomic_write_json(outputs["ownership_map_json"], mapping)
    atomic_write_json(outputs["report_json"], report)
    return outputs, report


def _write_run_info(
    output_dir: Path,
    *,
    status: str,
    scene: Scene,
    snapshot_load_status: str,
    snapshot_detail: str | None,
    completed_chunks: list[int],
    computed_chunks: list[int],
    snapshot_path: Path,
    elapsed_seconds: float,
) -> None:
    payload = {
        "schema": "rosewindow-run-v1",
        "status": status,
        "scene_digest": scene.digest,
        "snapshot_load_status": snapshot_load_status,
        "snapshot_detail": snapshot_detail,
        "snapshot_path": str(snapshot_path),
        "completed_chunk_count": len(completed_chunks),
        "total_chunk_count": scene.chunk_count,
        "completed_chunks": completed_chunks,
        "computed_chunks_this_run": computed_chunks,
        "resumed_chunk_count": len(completed_chunks) - len(computed_chunks),
        "elapsed_seconds": round(elapsed_seconds, 6),
    }
    atomic_write_json(output_dir / "run.json", payload)


def _write_recovery(
    output_dir: Path,
    scene: Scene,
    state: SnapshotState,
    snapshot: SnapshotStore,
) -> None:
    completed = sorted(state.completed_chunks)
    complete_set = set(completed)
    remaining = [
        chunk_id
        for chunk_id in range(scene.chunk_count)
        if chunk_id not in complete_set
    ]
    payload = {
        "schema": "rosewindow-recovery-v1",
        "status": "interrupted",
        "exit_code": 0,
        "scene_digest": scene.digest,
        "snapshot_path": str(snapshot.path),
        "snapshot_committed": True,
        "completed_chunk_count": len(completed),
        "total_chunk_count": scene.chunk_count,
        "completed_chunks": completed,
        "remaining_chunks": remaining,
        "next_action": "rerun the same command to resume from the committed snapshot",
    }
    atomic_write_json(output_dir / "recovery_report.json", payload)


def render_scene(
    scene: Scene,
    output_dir: str | Path,
    *,
    snapshot_dir: str | Path | None = None,
    fresh: bool = False,
    chunk_delay_ms: float = 0.0,
    interrupt_check: Callable[[], bool] | None = None,
) -> RenderResult:
    started = time.monotonic()
    destination = Path(output_dir)
    destination.mkdir(parents=True, exist_ok=True)
    state_dir = (
        Path(snapshot_dir)
        if snapshot_dir is not None
        else destination / ".rosewindow_snapshot"
    )
    snapshot = SnapshotStore(scene, state_dir)
    loaded: SnapshotLoad = snapshot.load(fresh=fresh)
    state = loaded.state
    resumed_chunks = sorted(state.completed_chunks)

    texture_bank = TextureBank(scene.seed, scene.textures, scene.pieces)
    geometry = build_geometry(scene)
    shadow = build_shadow(scene, geometry.edges)
    buffers = rasterize_scene(scene, geometry)

    computed_chunks: list[int] = []
    complete_set = set(state.completed_chunks)
    for chunk_id in range(scene.chunk_count):
        if chunk_id in complete_set:
            continue
        if interrupt_check is not None and interrupt_check():
            break
        x0, y0, x1, y1 = scene.chunk_bounds(chunk_id)
        chunk = render_chunk(
            scene,
            texture_bank,
            buffers,
            shadow,
            x0,
            y0,
            x1,
            y1,
        )
        state.rgb[y0:y1, x0:x1] = chunk.rgb
        state.owner[y0:y1, x0:x1] = chunk.owner
        state.transmission_sums += chunk.transmission_sums
        state.reflection_sums += chunk.reflection_sums
        state.pixel_counts += chunk.pixel_counts
        state.max_linear_value = max(
            state.max_linear_value, chunk.max_linear_value
        )
        state.completed_chunks.add(chunk_id)
        state.chunk_summaries[chunk_id] = chunk_summary(
            chunk_id, chunk.rgb, chunk.owner
        )
        snapshot.commit(state)
        computed_chunks.append(chunk_id)
        if chunk_delay_ms > 0.0:
            time.sleep(chunk_delay_ms / 1000.0)
        if interrupt_check is not None and interrupt_check():
            break

    if len(state.completed_chunks) < scene.chunk_count:
        if not snapshot.path.exists():
            snapshot.commit(state)
        _write_recovery(destination, scene, state, snapshot)
        _write_run_info(
            destination,
            status="interrupted",
            scene=scene,
            snapshot_load_status=loaded.status,
            snapshot_detail=loaded.detail,
            completed_chunks=sorted(state.completed_chunks),
            computed_chunks=computed_chunks,
            snapshot_path=snapshot.path,
            elapsed_seconds=time.monotonic() - started,
        )
        return RenderResult(
            status="interrupted",
            scene=scene,
            state=state,
            snapshot_load_status=loaded.status,
            computed_chunks=computed_chunks,
            resumed_chunks=resumed_chunks,
            snapshot_path=snapshot.path,
        )

    # A loaded completed snapshot may carry metadata in a different order.
    # Recompute summaries from the actual pixels before writing stable reports.
    state.chunk_summaries = summaries_for_completed(scene, state)
    outputs, report = _write_completed_outputs(
        scene, state, texture_bank, shadow, destination
    )
    _write_run_info(
        destination,
        status="complete",
        scene=scene,
        snapshot_load_status=loaded.status,
        snapshot_detail=loaded.detail,
        completed_chunks=sorted(state.completed_chunks),
        computed_chunks=computed_chunks,
        snapshot_path=snapshot.path,
        elapsed_seconds=time.monotonic() - started,
    )
    try:
        if (destination / "recovery_report.json").exists():
            (destination / "recovery_report.json").unlink()
    except OSError:
        pass
    return RenderResult(
        status="complete",
        scene=scene,
        state=state,
        snapshot_load_status=loaded.status,
        computed_chunks=computed_chunks,
        resumed_chunks=resumed_chunks,
        snapshot_path=snapshot.path,
        report=report,
        outputs=outputs,
    )
