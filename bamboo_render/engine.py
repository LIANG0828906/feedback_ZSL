from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import SnapshotInvalid
from .io_utils import write_json
from .procedural import generate_textures
from .renderer import BlockRenderer, BlockResult, render_blocks
from .reports import save_outputs
from .scene import LoadedScene
from .snapshot import (
    commit_snapshot,
    discard_snapshot,
    load_snapshot,
    snapshot_path,
)


@dataclass(frozen=True)
class RenderOutcome:
    status: str
    output_dir: Path
    completed_blocks: int
    total_blocks: int
    reused_blocks: int
    recomputed_blocks: int
    recovery: dict[str, Any] | None


def run_render(
    loaded: LoadedScene,
    output_dir: str | Path,
    *,
    interrupt_after: int | None = None,
    restart: bool = False,
) -> RenderOutcome:
    if interrupt_after is not None and interrupt_after < 1:
        raise ValueError("interrupt_after must be at least 1")
    scene = loaded.spec
    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    textures = generate_textures(scene)
    renderer = BlockRenderer(scene, textures)
    expected_pairs = list(renderer.pair_ids)
    blocks: dict[int, BlockResult] = {}
    recovery: dict[str, Any] | None = None
    reused = 0
    discarded_reason: str | None = None
    snapshot_present = snapshot_path(output_dir).exists()

    if restart:
        discard_snapshot(output_dir)
        snapshot_present = False
        discarded_reason = "restart_requested"
    elif snapshot_present:
        try:
            state = load_snapshot(
                output_dir,
                scene,
                loaded.scene_digest,
                expected_pairs,
            )
        except SnapshotInvalid as exc:
            discard_snapshot(output_dir)
            discarded_reason = exc.code
            recovery = {
                "schema_version": 1,
                "status": "recomputed",
                "snapshot_discarded": True,
                "discard_reason": exc.code,
                "discard_detail": exc.reason,
                "discard_block_id": exc.block_id,
                "scene_digest": loaded.scene_digest,
                "blocks_total": scene.image.block_count,
                "blocks_reused": 0,
                "blocks_recomputed": 0,
            }
        else:
            blocks = dict(state.completed)
            reused = len(blocks)

    completed_before = reused
    recomputed = 0
    for block in scene.image.blocks():
        if block.block_id in blocks:
            continue
        result = renderer.render(block)
        blocks[block.block_id] = result
        commit_snapshot(output_dir, scene, loaded.scene_digest, blocks)
        recomputed += 1
        if interrupt_after is not None and recomputed >= interrupt_after:
            interruption = {
                "schema_version": 1,
                "status": "interrupted",
                "scene_digest": loaded.scene_digest,
                "blocks_total": scene.image.block_count,
                "blocks_completed": len(blocks),
                "blocks_committed_this_run": recomputed,
                "blocks_remaining": scene.image.block_count - len(blocks),
                "snapshot_file": "snapshot.json",
            }
            write_json(output_dir / "recovery_report.json", interruption)
            return RenderOutcome(
                status="interrupted",
                output_dir=output_dir,
                completed_blocks=len(blocks),
                total_blocks=scene.image.block_count,
                reused_blocks=reused,
                recomputed_blocks=recomputed,
                recovery=interruption,
            )

    if len(blocks) != scene.image.block_count:
        missing = sorted(set(range(scene.image.block_count)) - set(blocks))
        raise RuntimeError(f"render ended with missing blocks: {missing}")
    ordered_results = [blocks[index] for index in range(scene.image.block_count)]
    result = render_blocks(scene, textures, ordered_results)
    run_report, _ = save_outputs(
        scene,
        textures,
        result,
        output_dir,
        loaded.scene_digest,
        status="complete",
    )

    final_recovery: dict[str, Any] | None = recovery
    if discarded_reason is None and (snapshot_present and reused > 0):
        final_recovery = {
            "schema_version": 1,
            "status": "resumed",
            "snapshot_discarded": False,
            "scene_digest": loaded.scene_digest,
            "blocks_total": scene.image.block_count,
            "blocks_reused": reused,
            "blocks_recomputed": recomputed,
            "blocks_completed": len(blocks),
        }
    elif discarded_reason is not None:
        if final_recovery is None:
            final_recovery = {
                "schema_version": 1,
                "status": "recomputed",
                "snapshot_discarded": True,
                "discard_reason": discarded_reason,
                "scene_digest": loaded.scene_digest,
                "blocks_total": scene.image.block_count,
                "blocks_reused": 0,
                "blocks_recomputed": recomputed,
            }
        else:
            final_recovery["blocks_recomputed"] = recomputed
            final_recovery["blocks_completed"] = len(blocks)
    if final_recovery is not None:
        write_json(output_dir / "recovery_report.json", final_recovery)

    return RenderOutcome(
        status="complete",
        output_dir=output_dir,
        completed_blocks=len(blocks),
        total_blocks=scene.image.block_count,
        reused_blocks=reused,
        recomputed_blocks=recomputed,
        recovery=final_recovery,
    )
