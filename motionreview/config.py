"""Frozen scheduler configuration loaded from JSON."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class DegradationStage:
    name: str
    iteration_frames: int | None = None
    batch_frames: int | None = None
    cache_depth: int | None = None


@dataclass(frozen=True)
class RunConfig:
    """All scheduler limits are immutable once a run starts.

    Sizes use bytes (or explicitly named JSON keys). ``total_memory_bytes`` is
    the sum of reservations across concurrently running clips. A task may wait
    for a global reservation but is never launched over its own quota.
    """

    max_parallelism: int
    per_task_memory_bytes: int
    total_memory_bytes: int
    degradation_sequence: tuple[DegradationStage, ...] = field(
        default_factory=lambda: (
            DegradationStage(
                name="shrink_iteration_buffer", iteration_frames=64
            ),
            DegradationStage(name="batch_frame_ranges", batch_frames=32),
            DegradationStage(
                name="reduce_cache_depth", cache_depth=1
            ),
        )
    )
    max_iterations: int = 80
    default_cache_depth: int = 64
    checkpoint_frames: int = 64
    schema_version: int = 1

    def __post_init__(self) -> None:
        if self.max_parallelism < 1:
            raise ValueError("max_parallelism must be >= 1")
        if self.per_task_memory_bytes < 1:
            raise ValueError("per_task_memory_bytes must be >= 1")
        if self.total_memory_bytes < 1:
            raise ValueError("total_memory_bytes must be >= 1")
        if self.max_iterations < 1:
            raise ValueError("max_iterations must be >= 1")
        if self.checkpoint_frames < 1:
            raise ValueError("checkpoint_frames must be >= 1")
        for stage in self.degradation_sequence:
            if stage.name not in {
                "shrink_iteration_buffer",
                "batch_frame_ranges",
                "reduce_cache_depth",
            }:
                raise ValueError(f"unknown degradation stage: {stage.name}")

    @property
    def fingerprint(self) -> str:
        import hashlib

        payload = {
            "max_parallelism": self.max_parallelism,
            "per_task_memory_bytes": self.per_task_memory_bytes,
            "total_memory_bytes": self.total_memory_bytes,
            "max_iterations": self.max_iterations,
            "default_cache_depth": self.default_cache_depth,
            "checkpoint_frames": self.checkpoint_frames,
            "degradation_sequence": [
                {
                    "name": stage.name,
                    "iteration_frames": stage.iteration_frames,
                    "batch_frames": stage.batch_frames,
                    "cache_depth": stage.cache_depth,
                }
                for stage in self.degradation_sequence
            ],
        }
        raw = json.dumps(
            payload, sort_keys=True, separators=(",", ":")
        ).encode("utf-8")
        return hashlib.sha256(raw).hexdigest()

def _stage_from_dict(item: dict[str, Any] | str) -> DegradationStage:
    if isinstance(item, str):
        return DegradationStage(name=item)
    return DegradationStage(
        name=str(item["name"]),
        iteration_frames=(
            int(item["iteration_frames"])
            if item.get("iteration_frames") is not None
            else None
        ),
        batch_frames=(
            int(item["batch_frames"])
            if item.get("batch_frames") is not None
            else None
        ),
        cache_depth=(
            int(item["cache_depth"])
            if item.get("cache_depth") is not None
            else None
        ),
    )


def load_run_config(path: str | Path) -> RunConfig:
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError("config root must be an object")
    stages_raw = data.get("degradation_sequence")
    if stages_raw is None:
        stages = RunConfig(
            max_parallelism=1,
            per_task_memory_bytes=1,
            total_memory_bytes=1,
        ).degradation_sequence
    else:
        stages = tuple(_stage_from_dict(item) for item in stages_raw)
    max_parallelism = int(
        data.get("max_parallelism", data.get("parallelism"))
    )
    per_task = int(
        data.get(
            "per_task_memory_bytes",
            data.get("memory_per_task_bytes"),
        )
    )
    total = int(
        data.get(
            "total_memory_bytes",
            data.get("memory_total_bytes"),
        )
    )
    return RunConfig(
        max_parallelism=max_parallelism,
        per_task_memory_bytes=per_task,
        total_memory_bytes=total,
        degradation_sequence=stages,
        max_iterations=int(data.get("max_iterations", 80)),
        default_cache_depth=int(data.get("default_cache_depth", 64)),
        checkpoint_frames=int(data.get("checkpoint_frames", 64)),
        schema_version=int(data.get("schema_version", 1)),
    )
