"""Explainable and deterministic data-scale strategy selection."""

from __future__ import annotations

import math

from .config import Budget
from .errors import BusinessError
from .models import ModeDecision

_FINE_SECONDS_PER_PIXEL = 7.8e-6
_COARSE_SECONDS_PER_PIXEL = 8.2e-6
_FULL_PIXEL_BYTES = 48.0
_COARSE_PIXEL_BYTES = 54.0
_FULL_LABEL_BYTES = 7.0
_MB = 1024.0 * 1024.0


def _estimate(pixels: int, region_count: int, factor: int) -> tuple[float, float]:
    overhead = 1.0 + 0.025 * max(0, region_count - 1)
    if factor == 1:
        seconds = pixels * _FINE_SECONDS_PER_PIXEL * overhead
        memory = pixels * _FULL_PIXEL_BYTES / _MB
        return seconds, memory
    coarse_pixels = math.ceil(pixels / (factor * factor))
    seconds = (
        coarse_pixels * _COARSE_SECONDS_PER_PIXEL * overhead
        + pixels * 1.6e-6
    )
    memory = (
        pixels * _FULL_LABEL_BYTES + coarse_pixels * _COARSE_PIXEL_BYTES
    ) / _MB
    return seconds, memory


def choose_mode(
    width: int, height: int, region_count: int, budget: Budget
) -> ModeDecision:
    budget.validate()
    if width <= 0 or height <= 0:
        raise BusinessError(
            "E_PARA",
            "图像尺寸必须为正数",
            field="image.shape",
            details={"width": width, "height": height},
        )
    pixels = int(width) * int(height)
    fine_seconds, fine_mb = _estimate(pixels, region_count, 1)
    considered: list[dict[str, float | int | bool]] = [
        {
            "factor": 1,
            "mode": "fine",
            "estimated_seconds": round(fine_seconds, 4),
            "estimated_mb": round(fine_mb, 3),
            "fits_time": fine_seconds <= budget.time_seconds,
            "fits_memory": fine_mb <= budget.memory_mb,
        }
    ]
    if fine_seconds <= budget.time_seconds and fine_mb <= budget.memory_mb:
        return ModeDecision(
            mode="fine",
            downsample_factor=1,
            block_size=budget.tile_size,
            pixel_count=pixels,
            region_count=region_count,
            estimated_fine_seconds=round(fine_seconds, 4),
            estimated_fine_mb=round(fine_mb, 3),
            estimated_seconds=round(fine_seconds, 4),
            estimated_mb=round(fine_mb, 3),
            reason=(
                f"像素数 {pixels}、区域数 {region_count}；预测耗时 {fine_seconds:.2f}s "
                f"和内存 {fine_mb:.1f}MB 均在预算内，选用逐像素精细生长"
            ),
            degradation_reason=None,
            impact_scope=[],
            considered=considered,
            budget={
                "time_seconds": budget.time_seconds,
                "memory_mb": budget.memory_mb,
                "max_workers": budget.max_workers,
                "tile_size": budget.tile_size,
                "block_buffer_mb": budget.block_buffer_mb,
            },
        )

    chosen_factor = None
    chosen_seconds = 0.0
    chosen_mb = 0.0
    for factor in (2, 4, 8, 16):
        seconds, memory = _estimate(pixels, region_count, factor)
        fits_time = seconds <= budget.time_seconds
        fits_memory = memory <= budget.memory_mb
        considered.append(
            {
                "factor": factor,
                "mode": "downsampled",
                "estimated_seconds": round(seconds, 4),
                "estimated_mb": round(memory, 3),
                "fits_time": fits_time,
                "fits_memory": fits_memory,
            }
        )
        if fits_time and fits_memory:
            chosen_factor = factor
            chosen_seconds = seconds
            chosen_mb = memory
            break

    if chosen_factor is None:
        raise BusinessError(
            "E_BUDGET",
            "预算过紧，最大 16 倍降采样仍无法满足时间或内存约束",
            field="budget",
            details={
                "budget": {
                    "time_seconds": budget.time_seconds,
                    "memory_mb": budget.memory_mb,
                },
                "considered": considered,
            },
        )

    reasons = []
    if fine_seconds > budget.time_seconds:
        reasons.append(
            f"精细模式预测耗时 {fine_seconds:.2f}s 超过 {budget.time_seconds:.2f}s"
        )
    if fine_mb > budget.memory_mb:
        reasons.append(
            f"精细模式预测内存 {fine_mb:.1f}MB 超过 {budget.memory_mb:.1f}MB"
        )
    reason = (
        "；".join(reasons)
        + f"；选择 {chosen_factor} 倍降采样，预测耗时 {chosen_seconds:.2f}s、"
        f"内存 {chosen_mb:.1f}MB"
    )
    return ModeDecision(
        mode="downsampled",
        downsample_factor=chosen_factor,
        block_size=budget.tile_size,
        pixel_count=pixels,
        region_count=region_count,
        estimated_fine_seconds=round(fine_seconds, 4),
        estimated_fine_mb=round(fine_mb, 3),
        estimated_seconds=round(chosen_seconds, 4),
        estimated_mb=round(chosen_mb, 3),
        reason=reason,
        degradation_reason="；".join(reasons),
        impact_scope=[
            f"区域生长在 1/{chosen_factor} 分辨率进行",
            "掩码确定性最近邻上采样回原尺寸",
            "几何统计与轮廓始终基于上采样后的最终全分辨率掩码",
            "分块提交与检查点粒度不变",
        ],
        considered=considered,
        budget={
            "time_seconds": budget.time_seconds,
            "memory_mb": budget.memory_mb,
            "max_workers": budget.max_workers,
            "tile_size": budget.tile_size,
            "block_buffer_mb": budget.block_buffer_mb,
        },
    )


def force_mode_decision(
    decision: ModeDecision,
    *,
    mode: str,
    factor: int,
    label: str = "experiment",
) -> ModeDecision:
    """Create a reproducible forced-mode decision for A/B experiments."""

    if mode not in {"fine", "downsampled"}:
        raise BusinessError(
            "E_PARA",
            "强制模式必须为 fine 或 downsampled",
            field="mode",
            details={"actual": mode},
        )
    if mode == "fine":
        factor = 1
    if factor not in {1, 2, 4, 8, 16} or (mode == "downsampled" and factor == 1):
        raise BusinessError(
            "E_PARA",
            "强制降采样倍率必须为 2/4/8/16",
            field="factor",
            details={"actual": factor},
        )
    return ModeDecision(
        mode=mode,
        downsample_factor=factor,
        block_size=decision.block_size,
        pixel_count=decision.pixel_count,
        region_count=decision.region_count,
        estimated_fine_seconds=decision.estimated_fine_seconds,
        estimated_fine_mb=decision.estimated_fine_mb,
        estimated_seconds=decision.estimated_seconds,
        estimated_mb=decision.estimated_mb,
        reason=f"{label}: 强制选用 {mode} 路径，倍率 {factor}",
        degradation_reason=(
            decision.degradation_reason if mode == "downsampled" else None
        ),
        impact_scope=decision.impact_scope if mode == "downsampled" else [],
        considered=decision.considered,
        budget=decision.budget,
    )
