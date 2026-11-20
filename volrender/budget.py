"""Global render budgets and the declared degradation policy."""

from __future__ import annotations

from dataclasses import dataclass, replace
import math
import threading
from typing import Callable, Any, Mapping

from .errors import BudgetExceededError, RenderCancelledError


DEGRADATION_SEQUENCE = ("increase_step", "lower_early_term", "reduce_batch")


@dataclass(frozen=True)
class BudgetConfig:
    max_samples: int = 10**18
    max_memory_bytes: int = 10**18
    ray_batch: int = 8192
    reduced_ray_batch: int = 1024
    increase_step_factor: float = 4.0
    early_term_alpha: float = 0.98
    degraded_early_term_alpha: float = 0.70
    sequence: tuple[str, ...] = DEGRADATION_SEQUENCE

    def __post_init__(self) -> None:
        if self.max_samples < 0 or self.max_memory_bytes < 0:
            raise ValueError("budgets must be non-negative")
        if self.ray_batch <= 0 or self.reduced_ray_batch <= 0:
            raise ValueError("ray batch sizes must be positive")
        if self.increase_step_factor <= 1.0:
            raise ValueError("increase_step_factor must be > 1")
        if not 0.0 < self.early_term_alpha <= 1.0:
            raise ValueError("early_term_alpha must be in (0,1]")
        if not 0.0 < self.degraded_early_term_alpha <= 1.0:
            raise ValueError("degraded_early_term_alpha must be in (0,1]")
        allowed = set(DEGRADATION_SEQUENCE)
        if any(stage not in allowed for stage in self.sequence):
            raise ValueError(f"unknown degradation stage in {self.sequence!r}")

    def to_dict(self) -> dict[str, Any]:
        return {
            "early_term_alpha": self.early_term_alpha,
            "increase_step_factor": self.increase_step_factor,
            "max_memory_bytes": self.max_memory_bytes,
            "max_samples": self.max_samples,
            "ray_batch": self.ray_batch,
            "reduced_early_term_alpha": self.degraded_early_term_alpha,
            "reduced_ray_batch": self.reduced_ray_batch,
            "sequence": list(self.sequence),
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "BudgetConfig":
        sequence = value.get("sequence", list(DEGRADATION_SEQUENCE))
        return cls(
            max_samples=int(value.get("max_samples", 10**18)),
            max_memory_bytes=int(value.get("max_memory_bytes", 10**18)),
            ray_batch=int(value.get("ray_batch", 8192)),
            reduced_ray_batch=int(value.get("reduced_ray_batch", 1024)),
            increase_step_factor=float(value.get("increase_step_factor", 4.0)),
            early_term_alpha=float(value.get("early_term_alpha", 0.98)),
            degraded_early_term_alpha=float(
                value.get(
                    "reduced_early_term_alpha",
                    value.get("degraded_early_term_alpha", 0.70),
                )
            ),
            sequence=tuple(str(item) for item in sequence),
        )


@dataclass(frozen=True)
class RenderPlan:
    step_size: float
    early_term_alpha: float
    ray_batch: int
    stages: tuple[dict[str, Any], ...]
    estimated_samples: int
    estimated_memory_bytes: int

    @property
    def degradation_names(self) -> list[str]:
        return [str(stage["stage"]) for stage in self.stages]

    def to_dict(self) -> dict[str, Any]:
        return {
            "early_term_alpha": self.early_term_alpha,
            "estimated_memory_bytes": self.estimated_memory_bytes,
            "estimated_samples": self.estimated_samples,
            "ray_batch": self.ray_batch,
            "stage_usage": [dict(stage) for stage in self.stages],
            "step_size": self.step_size,
        }


def plan_render(
    *,
    base_step_size: float,
    budget: BudgetConfig,
    estimate_samples: Callable[[float, float], int],
    estimate_memory: Callable[[int], int],
    cancel_event: threading.Event | None = None,
    stage_checkpoint: Callable[[str, Mapping[str, Any]], None] | None = None,
) -> RenderPlan:
    """Apply the declared degradation sequence until estimates fit.

    ``estimate_samples(step, early_alpha)`` must be conservative for the
    current view.  Every transition is recorded, including the before/after
    estimates, so a changed step can never be silent.
    """

    step = float(base_step_size)
    alpha = float(budget.early_term_alpha)
    batch = int(budget.ray_batch)
    stages: list[dict[str, Any]] = []

    def estimates() -> tuple[int, int]:
        samples = max(0, int(estimate_samples(step, alpha)))
        memory = max(0, int(estimate_memory(batch)))
        return samples, memory

    def over() -> bool:
        samples, memory = estimates()
        return samples > budget.max_samples or memory > budget.max_memory_bytes

    def check_cancel(stage: str) -> None:
        if cancel_event is not None and cancel_event.is_set():
            raise RenderCancelledError(
                "CANCELLED",
                f"渲染在降级阶段 {stage} 检查点被取消",
                usage={"stage": stage},
            )

    for stage in budget.sequence:
        check_cancel(stage)
        if not over():
            break
        samples_before, memory_before = estimates()
        if samples_before > budget.max_samples and memory_before > budget.max_memory_bytes:
            reason = "sample_and_memory_budget"
        elif samples_before > budget.max_samples:
            reason = "sample_budget"
        else:
            reason = "memory_budget"
        applied = False
        if stage == "increase_step" and samples_before > budget.max_samples:
            candidate_step = step * budget.increase_step_factor
            candidate_samples = int(estimate_samples(candidate_step, alpha))
            if candidate_samples < samples_before:
                step = candidate_step
                applied = True
        elif stage == "lower_early_term" and samples_before > budget.max_samples:
            candidate_alpha = budget.degraded_early_term_alpha
            candidate_samples = int(estimate_samples(step, candidate_alpha))
            # The conservative estimator can only justify this stage when it
            # lowers the expected active sample count.
            if alpha > candidate_alpha and candidate_samples < samples_before:
                alpha = candidate_alpha
                applied = True
        elif stage == "reduce_batch" and memory_before > budget.max_memory_bytes:
            candidate_batch = budget.reduced_ray_batch
            if batch > candidate_batch and estimate_memory(candidate_batch) < memory_before:
                batch = candidate_batch
                applied = True
        if not applied:
            continue
        samples_after, memory_after = estimates()
        stages.append(
            {
                "memory_after_bytes": memory_after,
                "memory_before_bytes": memory_before,
                "reason": reason,
                "samples_after": samples_after,
                "samples_before": samples_before,
                "stage": stage,
            }
        )
        if stage_checkpoint is not None:
            stage_checkpoint(
                "degradation",
                {
                    "stage": stage,
                    "stage_index": len(stages) - 1,
                    "stage_usage": dict(stages[-1]),
                },
            )

    samples, memory = estimates()
    if samples > budget.max_samples or memory > budget.max_memory_bytes:
        usage = {
            "budget": budget.to_dict(),
            "final_estimate": {
                "memory_bytes": memory,
                "samples": samples,
            },
            "stage_usage": stages,
        }
        raise BudgetExceededError(
            "DEGRADATION_EXHAUSTED",
            (
                f"降级序列耗尽仍超预算: 采样 {samples}>{budget.max_samples} "
                f"或内存 {memory}>{budget.max_memory_bytes}"
            ),
            usage=usage,
        )
    return RenderPlan(
        step_size=step,
        early_term_alpha=alpha,
        ray_batch=batch,
        stages=tuple(stages),
        estimated_samples=samples,
        estimated_memory_bytes=memory,
    )


def budget_with_overrides(budget: BudgetConfig, **overrides: Any) -> BudgetConfig:
    return replace(budget, **overrides)


__all__ = [
    "BudgetConfig",
    "DEGRADATION_SEQUENCE",
    "RenderPlan",
    "budget_with_overrides",
    "plan_render",
]

