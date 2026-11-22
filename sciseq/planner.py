"""Deterministic budget-aware algorithm selection."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import asdict, dataclass
from typing import Any

from .config import BudgetConfig, PipelineConfig
from .errors import SciSeqError


@dataclass(frozen=True)
class ModeDecision:
    mode: str
    downsample_factor: int
    preview_denoise_quota: int
    estimated_full_seconds: float
    estimated_selected_seconds: float
    estimated_memory_mb: float
    time_budget_s: float
    memory_budget_mb: float
    max_workers: int
    frames_per_worker: int
    reason: str
    budget_basis: dict[str, Any]
    decisions: tuple[dict[str, Any], ...]

    def to_dict(self) -> dict[str, Any]:
        payload = asdict(self)
        payload["decisions"] = list(self.decisions)
        return payload

    def decision_hash(self) -> str:
        encoded = json.dumps(self.to_dict(), sort_keys=True, separators=(",", ":"), allow_nan=False)
        return hashlib.sha256(encoded.encode("utf-8")).hexdigest()[:24]


def _cost_per_pixel(upsample_factor: int) -> tuple[float, float]:
    registration_seconds = 0.165e-6 * (1.0 + upsample_factor / 80.0)
    denoise_seconds = 0.150e-6
    feature_seconds = 0.075e-6
    return registration_seconds + feature_seconds, denoise_seconds


def _decision_sequence(
    frame_count: int,
    mode: str,
    downsample_factor: int,
    denoise_quota: int,
) -> tuple[dict[str, Any], ...]:
    return tuple(
        {
            "frame": index,
            "mode": mode,
            "downsample_factor": downsample_factor,
            "denoise_planned": mode == "full" or index < denoise_quota,
        }
        for index in range(frame_count)
    )


def plan_mode(
    frame_count: int,
    shape: tuple[int, int],
    config: PipelineConfig,
) -> ModeDecision:
    """Choose full or preview mode from workload and declared budgets only."""

    config.validate(shape, frame_count)
    budget: BudgetConfig = config.budget
    height, width = shape
    pixels = height * width
    workers = max(1, min(int(budget.max_workers), frame_count))
    reg_per_pixel, denoise_per_pixel = _cost_per_pixel(config.registration.upsample_factor)
    full_seconds = (
        frame_count * pixels * (reg_per_pixel + denoise_per_pixel)
        + 4.0
        + 0.004 * frame_count
    )
    full_memory = (
        frame_count * pixels * 4.0 / (1024.0 * 1024.0)
        + workers * pixels * 4.0 * 14.0 / (1024.0 * 1024.0)
        + 48.0
    )
    basis: dict[str, Any] = {
        "frame_count": int(frame_count),
        "frame_shape": [int(height), int(width)],
        "frame_pixels": int(pixels),
        "upsample_factor": int(config.registration.upsample_factor),
        "cost_model": {
            "registration_and_features_seconds_per_pixel": reg_per_pixel,
            "denoise_seconds_per_pixel": denoise_per_pixel,
            "working_buffers_per_worker": 14,
            "format": "deterministic analytic estimate; wall clock is not consulted",
        },
    }

    if full_memory <= budget.memory_budget_mb and full_seconds <= budget.time_budget_s:
        basis.update(
            {
                "estimated_full_memory_mb": full_memory,
                "estimated_full_seconds": full_seconds,
                "memory_within_budget": True,
                "time_within_budget": True,
            }
        )
        return ModeDecision(
            mode="full",
            downsample_factor=1,
            preview_denoise_quota=frame_count,
            estimated_full_seconds=full_seconds,
            estimated_selected_seconds=full_seconds,
            estimated_memory_mb=full_memory,
            time_budget_s=budget.time_budget_s,
            memory_budget_mb=budget.memory_budget_mb,
            max_workers=workers,
            frames_per_worker=int(budget.frames_per_worker),
            reason="full precision and full-frame denoise fit both declared budgets",
            budget_basis=basis,
            decisions=_decision_sequence(frame_count, "full", 1, frame_count),
        )

    reasons: list[str] = []
    if full_memory > budget.memory_budget_mb:
        reasons.append("full-mode memory estimate exceeds memory budget")
    if full_seconds > budget.time_budget_s:
        reasons.append("full-mode time estimate exceeds time budget")
    candidates = (2, 3, 4, 5, 6, 8, 10, 12, 16, 24, 32)
    selected: ModeDecision | None = None
    for factor in candidates:
        down_height = height // factor
        down_width = width // factor
        if down_height < 16 or down_width < 16:
            continue
        down_pixels = down_height * down_width
        quota = max(1, int(math.ceil(frame_count * budget.preview_denoise_fraction)))
        preview_seconds = (
            frame_count * down_pixels * reg_per_pixel
            + quota * down_pixels * denoise_per_pixel
            + 4.0
            + 0.004 * frame_count
        )
        preview_memory = (
            frame_count * down_pixels * 4.0 / (1024.0 * 1024.0)
            + workers * down_pixels * 4.0 * 14.0 / (1024.0 * 1024.0)
            + 48.0
        )
        if preview_memory <= budget.memory_budget_mb and preview_seconds <= budget.time_budget_s:
            candidate_basis = dict(basis)
            candidate_basis.update(
                {
                    "estimated_full_memory_mb": full_memory,
                    "estimated_full_seconds": full_seconds,
                    "estimated_preview_memory_mb": preview_memory,
                    "estimated_preview_seconds": preview_seconds,
                    "downsample_pixels": down_pixels,
                    "preview_denoise_quota": quota,
                    "memory_within_budget": True,
                    "time_within_budget": True,
                }
            )
            selected = ModeDecision(
                mode="preview",
                downsample_factor=factor,
                preview_denoise_quota=quota,
                estimated_full_seconds=full_seconds,
                estimated_selected_seconds=preview_seconds,
                estimated_memory_mb=preview_memory,
                time_budget_s=budget.time_budget_s,
                memory_budget_mb=budget.memory_budget_mb,
                max_workers=workers,
                frames_per_worker=int(budget.frames_per_worker),
                reason=(
                    "; ".join(reasons)
                    + f"; selected downsample x{factor} and top-noise denoise quota {quota}"
                ),
                budget_basis=candidate_basis,
                decisions=_decision_sequence(frame_count, "preview", factor, quota),
            )
            break
    if selected is None:
        raise SciSeqError(
            "E_BUDGET",
            "no supported preview configuration fits the declared budgets",
            field="budget",
            details={
                "time_budget_s": budget.time_budget_s,
                "memory_budget_mb": budget.memory_budget_mb,
                "minimum_downsample": 32,
            },
        )
    return selected

