"""Declared memory accounting and degradation planning."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .config import DegradationStage, RunConfig
from .errors import BudgetExceededError


@dataclass(frozen=True)
class MemoryEstimate:
    joints: int
    frames: int
    channels: int
    iteration_frames: int
    batch_frames: int
    cache_depth: int
    total_bytes: int
    breakdown: dict[str, int] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "joints": self.joints,
            "frames": self.frames,
            "channels": self.channels,
            "iteration_frames": self.iteration_frames,
            "batch_frames": self.batch_frames,
            "cache_depth": self.cache_depth,
            "total_bytes": self.total_bytes,
            "breakdown": dict(self.breakdown),
        }


@dataclass(frozen=True)
class BudgetPlan:
    stage_name: str
    stage_index: int
    iteration_frames: int
    batch_frames: int
    cache_depth: int
    estimate: MemoryEstimate
    attempted: tuple[dict[str, Any], ...]

    @property
    def fits(self) -> bool:
        return True

    def to_dict(self) -> dict[str, Any]:
        return {
            "stage_name": self.stage_name,
            "stage_index": self.stage_index,
            "iteration_frames": self.iteration_frames,
            "batch_frames": self.batch_frames,
            "cache_depth": self.cache_depth,
            "usage_bytes": self.estimate.total_bytes,
            "usage": self.estimate.to_dict(),
            "attempted": list(self.attempted),
        }


def estimate_memory(
    *,
    joints: int,
    frames: int,
    channels: int,
    iteration_frames: int,
    batch_frames: int,
    cache_depth: int,
) -> MemoryEstimate:
    """Deterministic conservative estimate in bytes.

    Pose cache entries include 4x4 float64 world transforms plus the source
    channels. Iteration workspace holds three copies (current, plus, minus).
    Batch bookkeeping adds one residual record per frame/constraint.
    """

    if any(
        value < 0
        for value in (
            joints,
            frames,
            channels,
            iteration_frames,
            batch_frames,
            cache_depth,
        )
    ):
        raise ValueError("memory estimate inputs must be non-negative")
    transform_bytes = joints * 16 * 8
    channel_bytes = channels * 8
    fixed = 64 * 1024 + joints * 1024
    motion = frames * channel_bytes
    retained = min(frames, cache_depth)
    cache = retained * (transform_bytes + channel_bytes) * 2
    iteration = iteration_frames * (transform_bytes + channel_bytes) * 3
    batch = min(frames, batch_frames) * (64 + channels * 8)
    breakdown = {
        "fixed": fixed,
        "motion": motion,
        "cache": cache,
        "iteration_workspace": iteration,
        "batch_workspace": batch,
    }
    total = sum(breakdown.values())
    return MemoryEstimate(
        joints=joints,
        frames=frames,
        channels=channels,
        iteration_frames=iteration_frames,
        batch_frames=batch_frames,
        cache_depth=cache_depth,
        total_bytes=total,
        breakdown=breakdown,
    )


def _apply_stage_to_parameters(
    stage: DegradationStage,
    *,
    iteration_frames: int,
    batch_frames: int,
    cache_depth: int,
    frames: int,
) -> tuple[int, int, int]:
    if stage.name == "shrink_iteration_buffer":
        requested = stage.iteration_frames or max(1, iteration_frames // 2)
        iteration_frames = max(1, min(iteration_frames, requested))
    elif stage.name == "batch_frame_ranges":
        requested = stage.batch_frames or max(1, batch_frames // 4)
        batch_frames = max(1, min(frames, requested))
    elif stage.name == "reduce_cache_depth":
        requested = stage.cache_depth or 1
        cache_depth = max(1, min(cache_depth, requested))
    return iteration_frames, batch_frames, cache_depth


def plan_budget(
    *,
    config: RunConfig,
    joints: int,
    frames: int,
    channels: int,
    clip_id: str | None = None,
) -> BudgetPlan:
    """Try declared degradation stages in order and never silently enlarge."""

    initial_iteration = min(frames, config.default_cache_depth)
    initial_batch = min(frames, config.checkpoint_frames)
    initial_cache = min(frames, config.default_cache_depth)
    attempts: list[dict[str, Any]] = []

    candidates: list[
        tuple[str, int, int, int, int]
    ] = [("full", 0, initial_iteration, initial_batch, initial_cache)]
    iteration_frames = initial_iteration
    batch_frames = initial_batch
    cache_depth = initial_cache
    for stage_index, stage in enumerate(config.degradation_sequence, 1):
        iteration_frames, batch_frames, cache_depth = (
            _apply_stage_to_parameters(
                stage,
                iteration_frames=iteration_frames,
                batch_frames=batch_frames,
                cache_depth=cache_depth,
                frames=frames,
            )
        )
        candidates.append(
            (
                stage.name,
                stage_index,
                iteration_frames,
                batch_frames,
                cache_depth,
            )
        )

    for stage_name, stage_index, iteration, batch, depth in candidates:
        estimate = estimate_memory(
            joints=joints,
            frames=frames,
            channels=channels,
            iteration_frames=iteration,
            batch_frames=batch,
            cache_depth=depth,
        )
        tried = {
            "stage_name": stage_name,
            "stage_index": stage_index,
            "iteration_frames": iteration,
            "batch_frames": batch,
            "cache_depth": depth,
            "usage_bytes": estimate.total_bytes,
            "fits": estimate.total_bytes <= config.per_task_memory_bytes,
        }
        attempts.append(tried)
        if estimate.total_bytes <= config.per_task_memory_bytes:
            return BudgetPlan(
                stage_name=stage_name,
                stage_index=stage_index,
                iteration_frames=iteration,
                batch_frames=batch,
                cache_depth=depth,
                estimate=estimate,
                attempted=tuple(attempts),
            )
    raise BudgetExceededError(
        "task exceeds per-task memory quota after all degradation stages",
        clip_id=clip_id,
        details={
            "per_task_memory_bytes": config.per_task_memory_bytes,
            "attempted": attempts,
        },
    )
