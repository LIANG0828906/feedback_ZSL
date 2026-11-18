"""Whole-replacement progress snapshots and strict resume validation."""

from __future__ import annotations

import hashlib
import json
import shutil
from pathlib import Path
from typing import Any

from .errors import SnapshotInvalid
from .patterns import PatternSet
from .render import BlockResult, color_summary
from .scene import Scene
from .tiles import read_tile
from .util import (
    atomic_write_json,
    canonical_json_bytes,
    object_digest,
    sha256_bytes,
    sha256_file,
)

SNAPSHOT_FORMAT = "GLV-PROGRESS-V1"
PROGRESS_VERSION = 1
RENDERER_VERSION = "1.0.0"
PACKAGE_DIR = Path(__file__).resolve().parent

GENERATED_PATHS = (
    "snapshot.json",
    "blocks",
    "renders",
    "diagnostics",
    "report.json",
    "report.md",
    "recovery.json",
    "landing_points.json",
    "patterns.bin",
    "manifest.json",
    "resume_reset.json",
)


def expected_descriptor(scene: Scene) -> dict[str, Any]:
    blocks_x, blocks_y = scene.render.block_counts(
        scene.camera.width, scene.camera.height
    )
    return {
        "width": scene.camera.width,
        "height": scene.camera.height,
        "tile_width": scene.render.tile_width,
        "tile_height": scene.render.tile_height,
        "blocks_x": blocks_x,
        "blocks_y": blocks_y,
        "blocks_per_angle": blocks_x * blocks_y,
        "total_angles": len(scene.camera.angles_deg),
        "total_blocks": blocks_x * blocks_y * len(scene.camera.angles_deg),
        "angles_deg": [float(value) for value in scene.camera.angles_deg],
    }


def generation_digest(scene: Scene, patterns: PatternSet) -> str:
    return object_digest(
        {
            "renderer": "gallery_louvers",
            "renderer_version": RENDERER_VERSION,
            "renderer_code_digest": renderer_code_digest(),
            "scene_digest": scene.digest,
            "pattern_digest": patterns.digest,
            "expected": expected_descriptor(scene),
            "seed_manifest": scene.pattern_seed_manifest(),
        }
    )


def renderer_code_digest() -> str:
    digest = hashlib.sha256()
    for path in sorted(PACKAGE_DIR.glob("*.py"), key=lambda item: item.name):
        digest.update(path.name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def _snapshot_checksum(state: dict[str, Any]) -> str:
    payload = dict(state)
    payload.pop("snapshot_checksum", None)
    return sha256_bytes(canonical_json_bytes(payload))


def _new_state(
    scene: Scene, patterns: PatternSet, generation: str
) -> dict[str, Any]:
    state: dict[str, Any] = {
        "format": SNAPSHOT_FORMAT,
        "progress_version": PROGRESS_VERSION,
        "generation_digest": generation,
        "scene_digest": scene.digest,
        "pattern_digest": patterns.digest,
        "expected": expected_descriptor(scene),
        "completed_blocks": 0,
        "completed": [],
        "next_angle_index": 0,
        "next_block_index": 0,
        "status": "rendering",
    }
    state["snapshot_checksum"] = _snapshot_checksum(state)
    return state


def _write_state(workdir: Path, state: dict[str, Any]) -> None:
    state["snapshot_checksum"] = _snapshot_checksum(state)
    atomic_write_json(workdir / "snapshot.json", state)


def _read_state(path: Path) -> dict[str, Any]:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            state = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise SnapshotInvalid(f"cannot read snapshot: {exc}") from exc
    if not isinstance(state, dict):
        raise SnapshotInvalid("snapshot root must be an object")
    checksum = state.get("snapshot_checksum")
    if not isinstance(checksum, str) or checksum != _snapshot_checksum(state):
        raise SnapshotInvalid("snapshot checksum mismatch")
    return state


def reset_outputs(workdir: str | Path, reason: str) -> None:
    workdir = Path(workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    for relative in GENERATED_PATHS:
        path = workdir / relative
        if path.is_dir():
            shutil.rmtree(path)
        elif path.exists():
            path.unlink()
    for temporary in workdir.glob(".*.tmp-*"):
        if temporary.is_file():
            temporary.unlink()
    atomic_write_json(
        workdir / "resume_reset.json",
        {
            "format": "GLV-RESUME-RESET-V1",
            "action": "discarded_all_progress",
            "reason": reason,
            "restart_angle_index": 0,
            "restart_block_index": 0,
        },
    )


def _validate_entry(
    workdir: Path,
    entry: Any,
    ordinal: int,
    scene: Scene,
    patterns: PatternSet,
    generation: str,
    blocks_x: int,
    blocks_per_angle: int,
) -> None:
    if not isinstance(entry, dict):
        raise SnapshotInvalid(f"completed[{ordinal}] is not an object")
    angle_index = ordinal // blocks_per_angle
    block_index = ordinal % blocks_per_angle
    if entry.get("ordinal") != ordinal:
        raise SnapshotInvalid(f"completed[{ordinal}] has a stale ordinal")
    if entry.get("angle_index") != angle_index or entry.get("block_index") != block_index:
        raise SnapshotInvalid(f"completed[{ordinal}] is not in contiguous block order")
    expected_origin = [
        (block_index % blocks_x) * scene.render.tile_width,
        (block_index // blocks_x) * scene.render.tile_height,
    ]
    if entry.get("pixel_origin") != expected_origin:
        raise SnapshotInvalid(f"completed[{ordinal}] has a stale pixel origin")
    expected_relative = (
        f"blocks/angle_{angle_index:03d}/block_{block_index:03d}.gbt"
    )
    if entry.get("tile_file") != expected_relative:
        raise SnapshotInvalid(f"completed[{ordinal}] references an unexpected tile")
    tile_path = workdir / expected_relative
    if not tile_path.is_file():
        raise SnapshotInvalid(f"completed[{ordinal}] tile is missing")
    if sha256_file(tile_path) != entry.get("tile_sha256"):
        raise SnapshotInvalid(f"completed[{ordinal}] tile checksum mismatch")
    tile = read_tile(tile_path)
    metadata = tile.header.get("metadata", {})
    if metadata.get("generation_digest") != generation:
        raise SnapshotInvalid(f"completed[{ordinal}] generation digest mismatch")
    if metadata.get("scene_digest") != scene.digest:
        raise SnapshotInvalid(f"completed[{ordinal}] scene digest mismatch")
    if metadata.get("pattern_digest") != patterns.digest:
        raise SnapshotInvalid(f"completed[{ordinal}] pattern digest mismatch")
    if metadata.get("angle_index") != angle_index or metadata.get("block_index") != block_index:
        raise SnapshotInvalid(f"completed[{ordinal}] tile identity mismatch")
    expected_size = [
        min(scene.render.tile_width, scene.camera.width - expected_origin[0]),
        min(scene.render.tile_height, scene.camera.height - expected_origin[1]),
    ]
    if entry.get("pixel_size") != expected_size:
        raise SnapshotInvalid(f"completed[{ordinal}] pixel size mismatch")
    computed_summary = color_summary(tile.rgba, tile.owner_id, tile.owner_layer)
    if canonical_json_bytes(computed_summary) != canonical_json_bytes(
        entry.get("color_summary")
    ):
        raise SnapshotInvalid(f"completed[{ordinal}] color summary mismatch")
    if canonical_json_bytes(metadata.get("stats")) != canonical_json_bytes(
        entry.get("stats")
    ):
        raise SnapshotInvalid(f"completed[{ordinal}] stats mismatch")


def validate_state(
    workdir: Path, state: dict[str, Any], scene: Scene, patterns: PatternSet
) -> str:
    generation = generation_digest(scene, patterns)
    if state.get("format") != SNAPSHOT_FORMAT:
        raise SnapshotInvalid("unsupported snapshot format")
    if state.get("progress_version") != PROGRESS_VERSION:
        raise SnapshotInvalid("unsupported progress version")
    if state.get("scene_digest") != scene.digest:
        raise SnapshotInvalid("scene digest changed (possibly a leaf seed change)")
    if state.get("pattern_digest") != patterns.digest:
        raise SnapshotInvalid("aperture seed or pattern lookup changed")
    if state.get("generation_digest") != generation:
        raise SnapshotInvalid("generation digest changed")
    expected = expected_descriptor(scene)
    if canonical_json_bytes(state.get("expected")) != canonical_json_bytes(expected):
        raise SnapshotInvalid("render sample/tile configuration changed")
    completed = state.get("completed")
    if not isinstance(completed, list):
        raise SnapshotInvalid("completed must be an array")
    count = state.get("completed_blocks")
    if not isinstance(count, int) or count != len(completed):
        raise SnapshotInvalid("completed_blocks does not match completed array")
    if count < 0 or count > expected["total_blocks"]:
        raise SnapshotInvalid("completed_blocks is outside the sample range")
    blocks_per_angle = expected["blocks_per_angle"]
    blocks_x = expected["blocks_x"]
    for ordinal, entry in enumerate(completed):
        _validate_entry(
            workdir,
            entry,
            ordinal,
            scene,
            patterns,
            generation,
            blocks_x,
            blocks_per_angle,
        )
    status = state.get("status")
    if status not in ("rendering", "complete"):
        raise SnapshotInvalid("unknown snapshot status")
    if status == "complete" and count != expected["total_blocks"]:
        raise SnapshotInvalid("complete snapshot is missing blocks")
    next_angle, next_block = (
        divmod(count, blocks_per_angle)
        if count < expected["total_blocks"]
        else (expected["total_angles"], 0)
    )
    if state.get("next_angle_index") != next_angle:
        raise SnapshotInvalid("next_angle_index is inconsistent")
    if state.get("next_block_index") != next_block:
        raise SnapshotInvalid("next_block_index is inconsistent")
    return generation


def prepare_run(
    workdir: str | Path,
    scene: Scene,
    patterns: PatternSet,
    force_fresh: bool = False,
) -> tuple[dict[str, Any], str | None, bool]:
    workdir = Path(workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    generation = generation_digest(scene, patterns)
    snapshot_path = workdir / "snapshot.json"
    reset_reason: str | None = None
    resumed = False
    if force_fresh:
        reset_reason = "fresh run requested"
        reset_outputs(workdir, reset_reason)
    elif snapshot_path.exists():
        try:
            state = _read_state(snapshot_path)
            generation = validate_state(workdir, state, scene, patterns)
            resumed = bool(state.get("completed_blocks", 0) > 0)
            if not resumed:
                reset_reason = None
            return state, reset_reason, resumed
        except Exception as exc:
            reset_reason = f"{type(exc).__name__}: {exc}"
            reset_outputs(workdir, reset_reason)
    else:
        reset_reason = "no committed snapshot"
        reset_outputs(workdir, reset_reason)
    state = _new_state(scene, patterns, generation)
    _write_state(workdir, state)
    return state, reset_reason, False


def commit_block(
    workdir: str | Path,
    state: dict[str, Any],
    result: BlockResult,
    scene: Scene,
    patterns: PatternSet,
    tile_sha256: str,
) -> None:
    workdir = Path(workdir)
    descriptor = expected_descriptor(scene)
    ordinal = int(state["completed_blocks"])
    if result.angle_index != state["next_angle_index"]:
        raise SnapshotInvalid("renderer attempted a stale angle offset")
    if result.block_index != state["next_block_index"]:
        raise SnapshotInvalid("renderer attempted a stale block offset")
    relative = (
        f"blocks/angle_{result.angle_index:03d}/block_{result.block_index:03d}.gbt"
    )
    entry = {
        "ordinal": ordinal,
        "angle_index": result.angle_index,
        "block_index": result.block_index,
        "tile_file": relative,
        "tile_sha256": tile_sha256,
        "pixel_origin": [result.x0, result.y0],
        "pixel_size": [result.width, result.height],
        "color_summary": result.stats["color_summary"],
        "stats": {key: value for key, value in result.stats.items()},
    }
    state["completed"].append(entry)
    state["completed_blocks"] = len(state["completed"])
    blocks_per_angle = descriptor["blocks_per_angle"]
    next_angle, next_block = divmod(state["completed_blocks"], blocks_per_angle)
    if state["completed_blocks"] == descriptor["total_blocks"]:
        state["status"] = "complete"
        state["next_angle_index"] = descriptor["total_angles"]
        state["next_block_index"] = 0
    else:
        state["status"] = "rendering"
        state["next_angle_index"] = next_angle
        state["next_block_index"] = next_block
    _write_state(workdir, state)
