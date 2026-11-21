"""Memory quota accounting and declared degradation sequence (requirement 4).

Per-task footprint is modelled from:
  - spectral buffer:        n_wavelength_samples * 8 bytes (float64)
  - CMF integration buffer: n_wavelength_samples * 3 * 8 bytes
  - sphere sampling buffer: sphere_samples * internal_batch * 8 bytes
  - preview buffer:         preview_resolution^2 * 16 bytes (float32 RGBA)
Degradation stages are applied strictly in the declared sequence and every
stage application is recorded. If the sequence is exhausted and the task
still exceeds the per-task quota, E_BUDGET_EXCEEDED is raised and the
previously published batch is left untouched.
"""
from __future__ import annotations

from dataclasses import dataclass, replace

from .config import BakeConfig
from .errors import BudgetExceeded

FLOAT64 = 8
PREVIEW_BYTES_PER_PIXEL = 16  # float32 RGBA


@dataclass(frozen=True)
class TaskShape:
    n_wavelength: int
    sphere_samples: int
    internal_batch: int
    preview_resolution: int


def estimate_task_bytes(shape: TaskShape) -> int:
    spectral = shape.n_wavelength * FLOAT64
    cmf = shape.n_wavelength * 3 * FLOAT64
    sphere = shape.sphere_samples * shape.internal_batch * FLOAT64
    preview = shape.preview_resolution * shape.preview_resolution * PREVIEW_BYTES_PER_PIXEL
    return spectral + cmf + sphere + preview


def base_shape(config: BakeConfig) -> TaskShape:
    return TaskShape(
        n_wavelength=config.grid.sample_count,
        sphere_samples=config.sphere_samples,
        internal_batch=config.internal_batch,
        preview_resolution=config.preview_resolution,
    )


def apply_stage(stage: str, shape: TaskShape) -> TaskShape:
    if stage == "reduce_sphere_samples":
        return replace(shape, sphere_samples=max(16, shape.sphere_samples // 2))
    if stage == "shrink_internal_batch":
        return replace(shape, internal_batch=max(1, shape.internal_batch // 2))
    if stage == "reduce_preview_resolution":
        return replace(shape, preview_resolution=max(8, shape.preview_resolution // 2))
    raise ValueError(f"unknown degradation stage: {stage}")


def fit_within_budget(config: BakeConfig, shape: TaskShape | None = None) -> tuple[TaskShape, list[dict]]:
    """Return (shape, degradation_log). Raises BudgetExceeded when exhausted."""
    shape = shape or base_shape(config)
    log: list[dict] = []
    usage = estimate_task_bytes(shape)
    if usage <= config.per_task_memory_bytes:
        return shape, log
    for stage in config.degradation_sequence:
        candidate = apply_stage(stage, shape)
        if candidate == shape:
            continue  # stage at its floor, try next declared stage
        shape = candidate
        usage = estimate_task_bytes(shape)
        log.append(
            {
                "stage": stage,
                "shape": {
                    "sphere_samples": shape.sphere_samples,
                    "internal_batch": shape.internal_batch,
                    "preview_resolution": shape.preview_resolution,
                },
                "usage_bytes": usage,
            }
        )
        if usage <= config.per_task_memory_bytes:
            return shape, log
    raise BudgetExceeded(
        "task exceeds per-task quota after full degradation sequence",
        usage_bytes=usage,
        quota_bytes=config.per_task_memory_bytes,
        stages_applied=[entry["stage"] for entry in log],
    )
