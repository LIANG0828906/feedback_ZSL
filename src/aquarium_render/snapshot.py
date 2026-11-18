"""Atomic progress snapshots for resumable block rendering."""

from __future__ import annotations

import base64
import binascii
import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .canonical import canonical_bytes, sha256_bytes, sha256_json
from .errors import SnapshotResetRequired

SNAPSHOT_FORMAT = "aquarium-render-snapshot"
SNAPSHOT_VERSION = 1


def color_summary(image: np.ndarray) -> dict[str, Any]:
    array = image.astype(np.float64)
    return {
        "mean": [round(float(v), 8) for v in array.mean(axis=(0, 1))],
        "min": [int(v) for v in image.min(axis=(0, 1))],
        "max": [int(v) for v in image.max(axis=(0, 1))],
        "sha256": sha256_bytes(np.ascontiguousarray(image, dtype=np.uint8).tobytes()),
    }


@dataclass
class CompletedBlock:
    sequence: int
    angle_index: int
    block_id: int
    rgba: np.ndarray
    ownership: np.ndarray
    summary: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        rgba_bytes = np.ascontiguousarray(self.rgba, dtype=np.uint8).tobytes()
        ownership_bytes = np.ascontiguousarray(self.ownership, dtype="<u2").tobytes()
        return {
            "sequence": self.sequence,
            "angle_index": self.angle_index,
            "block_id": self.block_id,
            "shape": [int(self.rgba.shape[0]), int(self.rgba.shape[1])],
            "rgba_b64": base64.b64encode(rgba_bytes).decode("ascii"),
            "rgba_sha256": sha256_bytes(rgba_bytes),
            "ownership_b64": base64.b64encode(ownership_bytes).decode("ascii"),
            "ownership_sha256": sha256_bytes(ownership_bytes),
            "color_summary": self.summary,
        }

    @classmethod
    def from_dict(cls, value: Any) -> "CompletedBlock":
        if not isinstance(value, dict):
            raise ValueError("completed block is not an object")
        rgba = np.frombuffer(base64.b64decode(value["rgba_b64"], validate=True), dtype=np.uint8)
        ownership = np.frombuffer(
            base64.b64decode(value["ownership_b64"], validate=True), dtype="<u2"
        ).astype(np.uint16)
        height, width = (int(v) for v in value["shape"])
        rgba = rgba.reshape(height, width, 4).copy()
        ownership = ownership.reshape(height, width).copy()
        if sha256_bytes(rgba.tobytes()) != value["rgba_sha256"]:
            raise ValueError("RGBA block hash mismatch")
        if sha256_bytes(ownership.astype("<u2").tobytes()) != value["ownership_sha256"]:
            raise ValueError("ownership block hash mismatch")
        return cls(
            sequence=int(value["sequence"]),
            angle_index=int(value["angle_index"]),
            block_id=int(value["block_id"]),
            rgba=rgba,
            ownership=ownership,
            summary=value["color_summary"],
        )


@dataclass
class SnapshotState:
    scene_summary: dict[str, Any]
    scene_summary_sha256: str
    particle_fingerprint: str
    total_angles: int
    blocks_per_angle: int
    completed: list[CompletedBlock] = field(default_factory=list)
    layer_stats: dict[str, dict[str, float]] = field(default_factory=dict)
    complete: bool = False
    reset_reason: str | None = None
    resumed_from_blocks: int = 0

    def expected_sequence(self) -> int:
        return len(self.completed)

    def add(self, block: CompletedBlock) -> None:
        if block.sequence != len(self.completed):
            raise ValueError("snapshot blocks must be appended contiguously")
        self.completed.append(block)


def _payload(state: SnapshotState) -> dict[str, Any]:
    return {
        "format": SNAPSHOT_FORMAT,
        "version": SNAPSHOT_VERSION,
        "scene_summary": state.scene_summary,
        "scene_summary_sha256": state.scene_summary_sha256,
        "particle_fingerprint": state.particle_fingerprint,
        "total_angles": state.total_angles,
        "blocks_per_angle": state.blocks_per_angle,
        "completed": [block.to_dict() for block in state.completed],
        "layer_stats": state.layer_stats,
        "complete": state.complete,
    }


def write_snapshot(path: str | Path, state: SnapshotState) -> str:
    payload = _payload(state)
    checksum = sha256_json(payload)
    document = dict(payload)
    document["checksum"] = checksum
    data = canonical_bytes(document)
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.with_name(f".{target.name}.tmp")
    with temporary.open("wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, target)
    try:
        directory_fd = os.open(target.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        pass
    return checksum


def _validate_sequence(
    completed: list[CompletedBlock],
    total_angles: int,
    blocks_per_angle: int,
) -> None:
    limit = total_angles * blocks_per_angle
    if len(completed) > limit:
        raise ValueError("snapshot contains more blocks than the scene")
    for index, block in enumerate(completed):
        expected_angle, expected_block = divmod(index, blocks_per_angle)
        if (
            block.sequence != index
            or block.angle_index != expected_angle
            or block.block_id != expected_block
        ):
            raise ValueError("snapshot block sequence is not contiguous")
    if completed and completed[-1].angle_index >= total_angles:
        raise ValueError("snapshot angle is outside the scene")


def load_snapshot(
    path: str | Path,
    scene_summary: dict[str, Any],
    particle_fingerprint: str,
) -> SnapshotState:
    target = Path(path)
    if not target.exists():
        return SnapshotState(
            scene_summary=scene_summary,
            scene_summary_sha256=sha256_json(scene_summary),
            particle_fingerprint=particle_fingerprint,
            total_angles=len(scene_summary.get("angles_deg", [])),
            blocks_per_angle=_blocks_from_summary(scene_summary),
        )
    try:
        document = json.loads(target.read_text(encoding="utf-8"))
        if not isinstance(document, dict):
            raise ValueError("snapshot root is not an object")
        checksum = document.pop("checksum")
        if checksum != sha256_json(document):
            raise ValueError("snapshot checksum mismatch")
        if document.get("format") != SNAPSHOT_FORMAT or document.get("version") != SNAPSHOT_VERSION:
            raise ValueError("unsupported snapshot format")
        if document.get("scene_summary_sha256") != sha256_json(scene_summary):
            raise ValueError("scene summary changed")
        if canonical_bytes(document.get("scene_summary")) != canonical_bytes(scene_summary):
            raise ValueError("scene summary content changed")
        if document.get("particle_fingerprint") != particle_fingerprint:
            raise ValueError("particle seed/table fingerprint changed")
        total_angles = int(document["total_angles"])
        blocks_per_angle = int(document["blocks_per_angle"])
        if total_angles != len(scene_summary["angles_deg"]):
            raise ValueError("snapshot angle count changed")
        if blocks_per_angle != _blocks_from_summary(scene_summary):
            raise ValueError("snapshot block layout changed")
        completed = [CompletedBlock.from_dict(value) for value in document.get("completed", [])]
        _validate_sequence(completed, total_angles, blocks_per_angle)
        state = SnapshotState(
            scene_summary=scene_summary,
            scene_summary_sha256=sha256_json(scene_summary),
            particle_fingerprint=particle_fingerprint,
            total_angles=total_angles,
            blocks_per_angle=blocks_per_angle,
            completed=completed,
            layer_stats={
                str(key): {
                    "transmission": float(value["transmission"]),
                    "scattering": float(value["scattering"]),
                }
                for key, value in document.get("layer_stats", {}).items()
            },
            complete=bool(document.get("complete", False)),
            resumed_from_blocks=len(completed),
        )
        expected_complete = len(completed) == total_angles * blocks_per_angle
        if state.complete != expected_complete:
            raise ValueError("snapshot completion flag is inconsistent")
        return state
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError, binascii.Error) as exc:
        raise SnapshotResetRequired(str(exc)) from exc


def _blocks_from_summary(scene_summary: dict[str, Any]) -> int:
    render = scene_summary["render"]
    width = int(render["width"])
    height = int(render["height"])
    block_width = int(render["block_width"])
    block_height = int(render["block_height"])
    return ((width + block_width - 1) // block_width) * (
        (height + block_height - 1) // block_height
    )


def new_state(
    scene_summary: dict[str, Any],
    particle_fingerprint: str,
    reset_reason: str | None = None,
) -> SnapshotState:
    return SnapshotState(
        scene_summary=scene_summary,
        scene_summary_sha256=sha256_json(scene_summary),
        particle_fingerprint=particle_fingerprint,
        total_angles=len(scene_summary["angles_deg"]),
        blocks_per_angle=_blocks_from_summary(scene_summary),
        reset_reason=reset_reason,
    )


def state_report(state: SnapshotState) -> dict[str, Any]:
    next_sequence = len(state.completed)
    next_angle, next_block = divmod(next_sequence, state.blocks_per_angle)
    counts: dict[int, int] = {}
    for block in state.completed:
        counts[block.angle_index] = counts.get(block.angle_index, 0) + 1
    completed_angles = [
        angle for angle in sorted(counts) if counts[angle] == state.blocks_per_angle
    ]
    return {
        "completed_blocks": len(state.completed),
        "total_blocks": state.total_angles * state.blocks_per_angle,
        "completed_angles": completed_angles,
        "angles_with_committed_blocks": sorted(counts),
        "next_angle": next_angle,
        "next_block": next_block,
        "complete": state.complete,
        "resumed_from_blocks": state.resumed_from_blocks,
        "reset_reason": state.reset_reason,
    }

