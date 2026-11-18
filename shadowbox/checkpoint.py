"""Atomic block snapshots and strict, all-or-nothing checkpoint recovery."""

from __future__ import annotations

import base64
import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .render import BlockRender, LayerBlockMetrics, block_bounds, block_grid
from .scene import ENGINE_VERSION, Scene

CHECKPOINT_FORMAT = "shadowbox-checkpoint-v1"


class CheckpointInvalid(ValueError):
    def __init__(self, reason: str):
        self.reason = reason
        super().__init__(reason)


@dataclass(frozen=True)
class StoredBlock:
    block: BlockRender
    summary: dict[str, Any]


@dataclass(frozen=True)
class LoadedCheckpoint:
    blocks: dict[int, StoredBlock]
    recovery_count: int
    discarded_count: int
    state: str


def canonical_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def array_bytes(array: np.ndarray, dtype: str) -> bytes:
    return np.asarray(array, dtype=dtype).tobytes(order="C")


def block_summary(block: BlockRender, metrics: tuple[LayerBlockMetrics, ...]) -> dict:
    rgba_bytes = array_bytes(block.rgba, "u1")
    depth_bytes = array_bytes(block.optical_depth, "<f4")
    pixels = block.width * block.height
    mean = np.mean(block.rgba.astype(np.float64) / 255.0, axis=(0, 1))
    return {
        "block_index": block.block_index,
        "x": block.x,
        "y": block.y,
        "width": block.width,
        "height": block.height,
        "pixel_count": pixels,
        "rgba_sha256": sha256_bytes(rgba_bytes),
        "depth_sha256": sha256_bytes(depth_bytes),
        "mean_rgba": [round(float(channel), 6) for channel in mean],
        "alpha_coverage": round(
            float(np.count_nonzero(block.rgba[:, :, 3]) / max(pixels, 1)), 6
        ),
        "depth_mean": round(float(np.mean(block.optical_depth)), 6),
        "visible_fragment_samples": sum(item.fragment_samples for item in metrics),
        "paper_fragment_samples": sum(
            item.paper_fragment_samples for item in metrics
        ),
    }


def stored_block_from_render(block: BlockRender) -> StoredBlock:
    return StoredBlock(
        block=block,
        summary=block_summary(block, block.layer_metrics),
    )


def _checkpoint_payload(
    scene: Scene,
    blocks: dict[int, StoredBlock],
    state: str,
    recovery_count: int,
    discarded_count: int,
) -> dict[str, Any]:
    columns, rows = block_grid(scene.camera)
    entries: list[dict[str, Any]] = []
    for index in sorted(blocks):
        stored = blocks[index]
        block = stored.block
        entries.append(
            {
                "block_index": index,
                "rgba_b64": base64.b64encode(
                    array_bytes(block.rgba, "u1")
                ).decode("ascii"),
                "depth_b64": base64.b64encode(
                    array_bytes(block.optical_depth, "<f4")
                ).decode("ascii"),
                "summary": stored.summary,
                "metrics": [
                    {
                        "fragment_samples": item.fragment_samples,
                        "paper_fragment_samples": item.paper_fragment_samples,
                        "hole_fragment_samples": item.hole_fragment_samples,
                        "transmittance_sum": item.transmittance_sum,
                    }
                    for item in block.layer_metrics
                ],
            }
        )
    payload: dict[str, Any] = {
        "format": CHECKPOINT_FORMAT,
        "engine_version": ENGINE_VERSION,
        "scene_sha256": scene.digest,
        "state": state,
        "recovery_count": recovery_count,
        "discarded_checkpoints": discarded_count,
        "grid": {
            "columns": columns,
            "rows": rows,
            "width": scene.camera.width,
            "height": scene.camera.height,
            "block_width": scene.camera.block_width,
            "block_height": scene.camera.block_height,
        },
        "completed_blocks": sorted(blocks),
        "blocks": entries,
    }
    payload["checksum"] = sha256_bytes(canonical_bytes(payload))
    return payload


def atomic_write_json(path: str | Path, value: Any) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=False,
        indent=2,
        allow_nan=False,
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(path, payload)


def atomic_write_bytes(path: str | Path, payload: bytes) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp.{os.getpid()}")
    with open(temporary, "wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    try:
        directory_fd = os.open(str(path.parent), os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(directory_fd)
    except OSError:
        pass
    finally:
        os.close(directory_fd)


def save_checkpoint(
    path: str | Path,
    scene: Scene,
    blocks: dict[int, StoredBlock],
    *,
    state: str,
    recovery_count: int,
    discarded_count: int,
) -> None:
    if state not in ("partial", "complete"):
        raise ValueError(f"invalid checkpoint state {state!r}")
    payload = _checkpoint_payload(
        scene,
        blocks,
        state,
        recovery_count,
        discarded_count,
    )
    # Canonical serialization also makes the checksum and atomic replacement stable.
    atomic_write_bytes(
        path,
        json.dumps(
            payload,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        ).encode("utf-8")
        + b"\n",
    )


def _decode_block(
    scene: Scene,
    entry: Any,
    expected_index: int,
) -> StoredBlock:
    if not isinstance(entry, dict):
        raise CheckpointInvalid("block entry is not an object")
    if entry.get("block_index") != expected_index:
        raise CheckpointInvalid("block entry index does not match completed list")
    x, y, width, height = block_bounds(scene, expected_index)
    summary = entry.get("summary")
    if not isinstance(summary, dict):
        raise CheckpointInvalid("block summary is missing")
    expected_location = (x, y, width, height)
    actual_location = (
        summary.get("x"),
        summary.get("y"),
        summary.get("width"),
        summary.get("height"),
    )
    if actual_location != expected_location:
        raise CheckpointInvalid("block extent does not match current scene grid")
    try:
        rgba_bytes = base64.b64decode(entry["rgba_b64"], validate=True)
        depth_bytes = base64.b64decode(entry["depth_b64"], validate=True)
    except (KeyError, ValueError) as exc:
        raise CheckpointInvalid("block payload is not valid base64") from exc
    expected_rgba = width * height * 4
    expected_depth = width * height * 4
    if len(rgba_bytes) != expected_rgba:
        raise CheckpointInvalid("RGBA payload length is invalid")
    if len(depth_bytes) != expected_depth:
        raise CheckpointInvalid("depth payload length is invalid")
    if sha256_bytes(rgba_bytes) != summary.get("rgba_sha256"):
        raise CheckpointInvalid("RGBA payload digest mismatch")
    if sha256_bytes(depth_bytes) != summary.get("depth_sha256"):
        raise CheckpointInvalid("depth payload digest mismatch")
    rgba = np.frombuffer(rgba_bytes, dtype=np.uint8).reshape(height, width, 4).copy()
    depth = np.frombuffer(depth_bytes, dtype="<f4").reshape(height, width).copy()
    raw_metrics = entry.get("metrics")
    ordered_layers = scene.layers_inner_to_outer
    if not isinstance(raw_metrics, list) or len(raw_metrics) != len(ordered_layers):
        raise CheckpointInvalid("layer metrics count is invalid")
    metrics: list[LayerBlockMetrics] = []
    for raw in raw_metrics:
        if not isinstance(raw, dict):
            raise CheckpointInvalid("layer metrics entry is invalid")
        values = (
            raw.get("fragment_samples"),
            raw.get("paper_fragment_samples"),
            raw.get("hole_fragment_samples"),
        )
        if not all(isinstance(value, int) and value >= 0 for value in values):
            raise CheckpointInvalid("layer metric counters are invalid")
        transmittance_sum = raw.get("transmittance_sum")
        if not isinstance(transmittance_sum, (int, float)):
            raise CheckpointInvalid("transmittance sum is invalid")
        metrics.append(
            LayerBlockMetrics(
                fragment_samples=values[0],
                paper_fragment_samples=values[1],
                hole_fragment_samples=values[2],
                transmittance_sum=float(transmittance_sum),
            )
        )
    if sum(item.fragment_samples for item in metrics) != summary.get(
        "visible_fragment_samples"
    ):
        raise CheckpointInvalid("visible fragment summary mismatch")
    if sum(item.paper_fragment_samples for item in metrics) != summary.get(
        "paper_fragment_samples"
    ):
        raise CheckpointInvalid("paper fragment summary mismatch")
    block = BlockRender(
        block_index=expected_index,
        x=x,
        y=y,
        width=width,
        height=height,
        rgba=rgba,
        optical_depth=depth,
        layer_metrics=tuple(metrics),
    )
    recomputed = block_summary(block, tuple(metrics))
    if recomputed != summary:
        raise CheckpointInvalid("block color summary mismatch")
    return StoredBlock(block=block, summary=summary)


def load_checkpoint(path: str | Path, scene: Scene) -> LoadedCheckpoint:
    path = Path(path)
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise CheckpointInvalid("checkpoint is unreadable or malformed") from exc
    if not isinstance(payload, dict):
        raise CheckpointInvalid("checkpoint root is not an object")
    checksum = payload.pop("checksum", None)
    if not isinstance(checksum, str) or checksum != sha256_bytes(canonical_bytes(payload)):
        raise CheckpointInvalid("checkpoint checksum mismatch")
    if payload.get("format") != CHECKPOINT_FORMAT:
        raise CheckpointInvalid("checkpoint format mismatch")
    if payload.get("engine_version") != ENGINE_VERSION:
        raise CheckpointInvalid("engine version mismatch")
    if payload.get("scene_sha256") != scene.digest:
        raise CheckpointInvalid("scene digest mismatch")
    columns, rows = block_grid(scene.camera)
    expected_grid = {
        "columns": columns,
        "rows": rows,
        "width": scene.camera.width,
        "height": scene.camera.height,
        "block_width": scene.camera.block_width,
        "block_height": scene.camera.block_height,
    }
    if payload.get("grid") != expected_grid:
        raise CheckpointInvalid("block grid mismatch")
    state = payload.get("state")
    if state not in ("partial", "complete"):
        raise CheckpointInvalid("checkpoint state is invalid")
    completed = payload.get("completed_blocks")
    entries = payload.get("blocks")
    if (
        not isinstance(completed, list)
        or not all(isinstance(index, int) for index in completed)
        or not isinstance(entries, list)
    ):
        raise CheckpointInvalid("completed block list is invalid")
    if completed != sorted(set(completed)):
        raise CheckpointInvalid("completed block list is duplicated or unsorted")
    if len(entries) != len(completed):
        raise CheckpointInvalid("completed blocks and payloads have different lengths")
    total_blocks = columns * rows
    if any(index < 0 or index >= total_blocks for index in completed):
        raise CheckpointInvalid("completed block index is outside the grid")
    if state == "complete" and len(completed) != total_blocks:
        raise CheckpointInvalid("complete checkpoint is missing blocks")
    entries_by_index: dict[int, Any] = {}
    for entry in entries:
        if not isinstance(entry, dict) or not isinstance(
            entry.get("block_index"), int
        ):
            raise CheckpointInvalid("block entry index is invalid")
        index = int(entry["block_index"])
        if index in entries_by_index:
            raise CheckpointInvalid("duplicate block payload")
        entries_by_index[index] = entry
    if sorted(entries_by_index) != completed:
        raise CheckpointInvalid("block payloads do not match completed list")
    blocks = {
        index: _decode_block(scene, entries_by_index[index], index)
        for index in completed
    }
    recovery_count = payload.get("recovery_count")
    discarded_count = payload.get("discarded_checkpoints")
    if not isinstance(recovery_count, int) or recovery_count < 0:
        raise CheckpointInvalid("recovery count is invalid")
    if not isinstance(discarded_count, int) or discarded_count < 0:
        raise CheckpointInvalid("discarded checkpoint count is invalid")
    return LoadedCheckpoint(
        blocks=blocks,
        recovery_count=recovery_count,
        discarded_count=discarded_count,
        state=state,
    )
