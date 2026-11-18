"""Render orchestration with graceful interruption and snapshot-only resume."""

from __future__ import annotations

import signal
import sys
from pathlib import Path
from typing import Any

from .patterns import build_patterns
from .projection import ProjectionSystem
from .lightmaps import build_receiver_lightmaps
from .render import (
    assemble_angle,
    landing_document,
    render_block,
)
from .report import build_report, write_report
from .scene import load_scene
from .snapshot import (
    commit_block,
    generation_digest,
    prepare_run,
)
from .tiles import write_tile
from .util import atomic_write_bytes, atomic_write_json


class GracefulStop:
    """Convert handleable console interrupts into an after-block stop request."""

    def __init__(self) -> None:
        self.requested = False
        self.signal_name: str | None = None
        self._previous: dict[int, Any] = {}

    def _handler(self, signum: int, _frame: Any) -> None:
        self.requested = True
        try:
            self.signal_name = signal.Signals(signum).name
        except ValueError:
            self.signal_name = f"signal-{signum}"

    def __enter__(self) -> "GracefulStop":
        for name in ("SIGINT", "SIGTERM", "SIGBREAK"):
            candidate = getattr(signal, name, None)
            if candidate is None:
                continue
            try:
                self._previous[int(candidate)] = signal.getsignal(candidate)
                signal.signal(candidate, self._handler)
            except (OSError, ValueError):
                continue
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        for signum, handler in self._previous.items():
            try:
                signal.signal(signum, handler)
            except (OSError, ValueError):
                pass


def _write_recovery(
    workdir: Path,
    scene_path: str,
    state: dict[str, Any],
    signal_name: str,
) -> None:
    payload = {
        "format": "GLV-RECOVERY-V1",
        "status": "interrupted_after_committed_block",
        "signal": signal_name,
        "completed_blocks": state["completed_blocks"],
        "total_blocks": state["expected"]["total_blocks"],
        "next_angle_index": state["next_angle_index"],
        "next_block_index": state["next_block_index"],
        "snapshot": "snapshot.json",
        "snapshot_checksum": state["snapshot_checksum"],
        "current_block_completed": True,
        "exit_code": 0,
        "resume_command": (
            f'{sys.executable} -m gallery_louvers render "{scene_path}" '
            f'--out "{workdir}"'
        ),
    }
    atomic_write_json(workdir / "recovery.json", payload)


def run_render(
    scene_path: str | Path,
    workdir: str | Path,
    force_fresh: bool = False,
    stop_after_blocks: int | None = None,
    quiet: bool = False,
) -> dict[str, Any]:
    with GracefulStop() as prepare_stop:
        return _run_render_impl(
            scene_path,
            workdir,
            prepare_stop,
            force_fresh=force_fresh,
            stop_after_blocks=stop_after_blocks,
            quiet=quiet,
        )


def _run_render_impl(
    scene_path: str | Path,
    workdir: str | Path,
    prepare_stop: GracefulStop,
    force_fresh: bool = False,
    stop_after_blocks: int | None = None,
    quiet: bool = False,
) -> dict[str, Any]:
    scene_path = str(Path(scene_path))
    workdir = Path(workdir)
    scene = load_scene(scene_path)
    patterns = build_patterns(scene)
    state, reset_reason, resumed = prepare_run(
        workdir, scene, patterns, force_fresh=force_fresh
    )
    reused_blocks = int(state["completed_blocks"])
    atomic_write_bytes(workdir / "patterns.bin", patterns.atlas_bytes)
    if not quiet:
        print(
            f"scene={scene.raw.get('scene_id')} scene_digest={scene.digest[:12]} "
            f"pattern_digest={patterns.digest[:12]}",
            file=sys.stderr,
        )
        if reset_reason:
            print(f"progress_reset={reset_reason}", file=sys.stderr)
        if resumed:
            print(f"resume_from_committed_blocks={reused_blocks}", file=sys.stderr)

    projection = ProjectionSystem(scene, patterns)
    landing_records = projection.landing_points()
    landing_doc = landing_document(scene, patterns, landing_records)
    atomic_write_json(workdir / "landing_points.json", landing_doc)

    blocks_per_angle = state["expected"]["blocks_per_angle"]
    total_blocks = state["expected"]["total_blocks"]
    new_blocks = 0
    stop_name = "STOP_AFTER_BLOCKS"
    generation = generation_digest(scene, patterns)
    with GracefulStop() as stop:
        if prepare_stop.requested:
            stop.requested = True
            stop.signal_name = prepare_stop.signal_name
        while int(state["completed_blocks"]) < total_blocks:
            if stop_after_blocks is not None and new_blocks >= stop_after_blocks:
                stop_name = "STOP_AFTER_BLOCKS"
                _write_recovery(workdir, scene_path, state, stop_name)
                if not quiet:
                    print(
                        f"stopped_after_block completed={state['completed_blocks']}",
                        file=sys.stderr,
                    )
                return {
                    "status": "interrupted",
                    "completed_blocks": state["completed_blocks"],
                    "reused_blocks": reused_blocks,
                    "recovery": str(workdir / "recovery.json"),
                }
            ordinal = int(state["completed_blocks"])
            angle_index, block_index = divmod(ordinal, blocks_per_angle)
            result = render_block(
                scene, patterns, projection, angle_index, block_index
            )
            tile_relative = (
                f"blocks/angle_{angle_index:03d}/block_{block_index:03d}.gbt"
            )
            tile_path = workdir / tile_relative
            tile_path.parent.mkdir(parents=True, exist_ok=True)
            metadata = {
                "generation_digest": generation,
                "scene_digest": scene.digest,
                "pattern_digest": patterns.digest,
                "angle_index": angle_index,
                "block_index": block_index,
                "pixel_origin": [result.x0, result.y0],
                "pixel_size": [result.width, result.height],
                "stats": result.stats,
            }
            tile_sha = write_tile(
                tile_path,
                result.rgba,
                result.owner_id,
                result.owner_layer,
                metadata,
            )
            commit_block(
                workdir, state, result, scene, patterns, tile_sha
            )
            new_blocks += 1
            if not quiet:
                print(
                    f"committed angle={angle_index} block={block_index} "
                    f"completed={state['completed_blocks']}/{total_blocks}",
                    file=sys.stderr,
                )
            if prepare_stop.requested:
                stop.requested = True
                stop.signal_name = prepare_stop.signal_name
            if stop.requested:
                stop_name = stop.signal_name or "SIGINT"
                _write_recovery(workdir, scene_path, state, stop_name)
                if not quiet:
                    print(
                        f"graceful_stop signal={stop_name} "
                        f"completed={state['completed_blocks']}",
                        file=sys.stderr,
                    )
                return {
                    "status": "interrupted",
                    "completed_blocks": state["completed_blocks"],
                    "reused_blocks": reused_blocks,
                    "signal": stop_name,
                    "recovery": str(workdir / "recovery.json"),
                }
        if stop.requested:
            stop_name = stop.signal_name or "SIGINT"

    angle_entries = [
        assemble_angle(scene, workdir, angle_index, state["completed"])
        for angle_index in range(len(scene.camera.angles_deg))
    ]
    lightmap_document = build_receiver_lightmaps(
        scene,
        projection,
        workdir,
        width=min(384, scene.camera.width),
        height=min(288, scene.camera.height),
    )
    atomic_write_json(workdir / "diagnostics" / "lightmaps.json", lightmap_document)
    run_meta = {
        "resumed": bool(resumed),
        "reused_blocks": reused_blocks,
        "new_blocks": new_blocks,
        "reset_reason": reset_reason,
        "final_status": "complete",
        "graceful_stop_signal_seen": stop.requested,
        "graceful_stop_signal": stop.signal_name,
    }
    report = build_report(
        scene,
        patterns,
        projection,
        workdir,
        state,
        angle_entries,
        landing_doc,
        lightmap_document,
        run_meta,
    )
    write_report(workdir, report, landing_doc)
    recovery_path = workdir / "recovery.json"
    if recovery_path.exists():
        recovery_path.unlink()
    if not quiet:
        print(
            f"complete angles={len(angle_entries)} blocks={state['completed_blocks']} "
            f"report={workdir / 'report.md'}",
            file=sys.stderr,
        )
    return {
        "status": "complete",
        "completed_blocks": state["completed_blocks"],
        "reused_blocks": reused_blocks,
        "new_blocks": new_blocks,
        "report": str(workdir / "report.json"),
        "manifest": str(workdir / "manifest.json"),
    }
