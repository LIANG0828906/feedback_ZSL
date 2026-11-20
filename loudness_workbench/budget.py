"""Conceptual per-block memory accounting and the declared degrade sequence.

The accounting is deliberately explicit: block samples, retained filter
history, oversampling scratch, and the internal ring buffer all have separate
terms.  A degrade stage changes only scratch/buffer strategy, never the
measurement formula or the published values.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, replace
from typing import Any

from .config import SchedulerConfig
from .errors import ErrorCode, MeterError, error


FLOAT_BYTES = 8
FIXED_OVERHEAD_BYTES = 8 * 1024
FILTER_STATE_BYTES_PER_CHANNEL = 64


@dataclass(frozen=True)
class BudgetPlan:
    stage_index: int = 0
    degrade_stage: str = "none"
    oversample_batch_samples: int = 4096
    internal_buffer_samples: int = 4096

    def estimate_memory_bytes(
        self,
        *,
        block_samples: int,
        channels: int,
        retained_samples: int,
        oversample_factor: int,
    ) -> int:
        input_bytes = block_samples * channels * FLOAT_BYTES
        filtered_bytes = block_samples * channels * FLOAT_BYTES
        history_bytes = max(0, retained_samples) * channels * FLOAT_BYTES
        internal_bytes = self.internal_buffer_samples * channels * FLOAT_BYTES
        # Larger batches reduce fixed per-batch staging overhead while the
        # actual block always remains resident.
        batches = max(
            1,
            math.ceil(block_samples / max(1, self.oversample_batch_samples)),
        )
        oversample_bytes = (
            block_samples * channels * FLOAT_BYTES
            + batches * 512
            + 65 * FLOAT_BYTES
        )
        state_bytes = channels * FILTER_STATE_BYTES_PER_CHANNEL
        return int(
            FIXED_OVERHEAD_BYTES
            + input_bytes
            + filtered_bytes
            + history_bytes
            + internal_bytes
            + oversample_bytes
            + state_bytes
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "stage_index": self.stage_index,
            "degrade_stage": self.degrade_stage,
            "oversample_batch_samples": self.oversample_batch_samples,
            "internal_buffer_samples": self.internal_buffer_samples,
        }


@dataclass(frozen=True)
class DegradeDecision:
    before: BudgetPlan
    after: BudgetPlan
    before_bytes: int
    after_bytes: int
    stage: str
    accepted: bool


def initial_plan(config: SchedulerConfig) -> BudgetPlan:
    return BudgetPlan(
        stage_index=0,
        degrade_stage="none",
        oversample_batch_samples=config.oversample_batch_samples,
        internal_buffer_samples=config.internal_buffer_samples,
    )


def next_degrade_plan(
    plan: BudgetPlan,
    config: SchedulerConfig,
    *,
    block_samples: int,
    channels: int,
    retained_samples: int,
    oversample_factor: int,
) -> DegradeDecision | None:
    if plan.stage_index >= len(config.degrade_sequence):
        return None
    stage = config.degrade_sequence[plan.stage_index]
    if stage == "increase_oversample_batch":
        after = replace(
            plan,
            stage_index=plan.stage_index + 1,
            degrade_stage=stage,
            oversample_batch_samples=max(
                plan.oversample_batch_samples,
                min(block_samples, plan.oversample_batch_samples * 2),
            ),
        )
    elif stage == "shrink_internal_buffer":
        after = replace(
            plan,
            stage_index=plan.stage_index + 1,
            degrade_stage=stage,
            internal_buffer_samples=max(1, plan.internal_buffer_samples // 2),
        )
    else:
        raise error(
            ErrorCode.E_CONFIG_INVALID,
            "unsupported degrade stage",
            {"stage": stage},
        )
    before_bytes = plan.estimate_memory_bytes(
        block_samples=block_samples,
        channels=channels,
        retained_samples=retained_samples,
        oversample_factor=oversample_factor,
    )
    after_bytes = after.estimate_memory_bytes(
        block_samples=block_samples,
        channels=channels,
        retained_samples=retained_samples,
        oversample_factor=oversample_factor,
    )
    return DegradeDecision(
        before=plan,
        after=after,
        before_bytes=before_bytes,
        after_bytes=after_bytes,
        stage=stage,
        accepted=True,
    )


def fit_plan(
    config: SchedulerConfig,
    *,
    block_samples: int,
    channels: int,
    retained_samples: int,
    oversample_factor: int,
    budget_bytes: int,
) -> tuple[BudgetPlan, list[dict[str, Any]]]:
    """Fit one task into ``budget_bytes`` using the declared sequence.

    Every attempted stage is returned for the trajectory.  If no stage fits,
    the caller receives the last plan together with its over-budget estimate
    and can reject the block with ``E_BUDGET_EXCEEDED``.
    """
    plan = initial_plan(config)
    events: list[dict[str, Any]] = []
    estimated = plan.estimate_memory_bytes(
        block_samples=block_samples,
        channels=channels,
        retained_samples=retained_samples,
        oversample_factor=oversample_factor,
    )
    while estimated > budget_bytes:
        decision = next_degrade_plan(
            plan,
            config,
            block_samples=block_samples,
            channels=channels,
            retained_samples=retained_samples,
            oversample_factor=oversample_factor,
        )
        if decision is None:
            break
        plan = decision.after
        estimated = decision.after_bytes
        events.append(
            {
                "stage": decision.stage,
                "stage_index": plan.stage_index,
                "before_bytes": decision.before_bytes,
                "after_bytes": decision.after_bytes,
                "budget_bytes": budget_bytes,
                "fits": estimated <= budget_bytes,
            }
        )
        if estimated <= budget_bytes:
            break
    return plan, events


def enforce_or_raise(
    config: SchedulerConfig,
    *,
    asset_id: str,
    block_index: int,
    block_samples: int,
    channels: int,
    retained_samples: int,
    oversample_factor: int,
) -> tuple[BudgetPlan, list[dict[str, Any]], int]:
    plan, events = fit_plan(
        config,
        block_samples=block_samples,
        channels=channels,
        retained_samples=retained_samples,
        oversample_factor=oversample_factor,
        budget_bytes=config.per_task_memory_budget_bytes,
    )
    estimated = plan.estimate_memory_bytes(
        block_samples=block_samples,
        channels=channels,
        retained_samples=retained_samples,
        oversample_factor=oversample_factor,
    )
    if estimated > config.per_task_memory_budget_bytes:
        raise error(
            ErrorCode.E_BUDGET_EXCEEDED,
            "block exceeds the per-task memory budget after all degrade stages",
            {
                "asset_id": asset_id,
                "block_index": block_index,
                "estimated_bytes": estimated,
                "budget_bytes": config.per_task_memory_budget_bytes,
                "degrade_stage": plan.degrade_stage,
                "attempts": events,
            },
        )
    return plan, events, estimated
