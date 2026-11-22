"""Explainable mode selection for variable scene and optimization scales."""

from __future__ import annotations

import math
from dataclasses import asdict, dataclass
from typing import Any

from .errors import fail


@dataclass(frozen=True)
class ScaleConfig:
    mode: str = "auto"
    memory_budget_mb: float = 64.0
    max_exact_pixels: int = 262_144
    max_exact_iterations: int = 200
    downsample_iteration_threshold: int = 60
    tile_size: int = 16
    max_parallel_blocks: int = 2
    requested_downsample_factor: int = 0
    shadow_samples: int = 4

    def validate(self) -> None:
        if self.mode not in {"auto", "exact", "downsample", "tiled"}:
            fail(
                "E_PARA",
                "mode must be auto, exact, downsample or tiled",
                "mode",
                value=self.mode,
            )
        value = float(self.memory_budget_mb)
        if not math.isfinite(value) or value <= 0.0:
            fail(
                "E_PARA",
                "memory_budget_mb must be a positive finite number",
                "memory_budget_mb",
                value=value,
            )
        for field_name in ("max_exact_pixels", "max_exact_iterations"):
            field_value = getattr(self, field_name)
            if isinstance(field_value, bool) or not isinstance(field_value, int) or field_value <= 0:
                fail(
                    "E_PARA",
                    f"{field_name} must be a positive integer",
                    field_name,
                )
        if (
            isinstance(self.downsample_iteration_threshold, bool)
            or not isinstance(self.downsample_iteration_threshold, int)
            or self.downsample_iteration_threshold <= 0
        ):
            fail(
                "E_PARA",
                "downsample_iteration_threshold must be a positive integer",
                "downsample_iteration_threshold",
            )
        if isinstance(self.tile_size, bool) or not isinstance(self.tile_size, int):
            fail("E_PARA", "tile_size must be an integer", "tile_size")
        if not (4 <= self.tile_size <= 128):
            fail("E_PARA", "tile_size must be in [4, 128]", "tile_size")
        if (
            isinstance(self.max_parallel_blocks, bool)
            or not isinstance(self.max_parallel_blocks, int)
            or self.max_parallel_blocks <= 0
        ):
            fail(
                "E_PARA",
                "max_parallel_blocks must be a positive integer",
                "max_parallel_blocks",
            )
        if self.requested_downsample_factor not in {0, 1, 2, 4, 8, 16}:
            fail(
                "E_PARA",
                "requested_downsample_factor must be one of 0, 1, 2, 4, 8, 16",
                "requested_downsample_factor",
            )
        if isinstance(self.shadow_samples, bool) or not isinstance(
            self.shadow_samples, int
        ):
            fail("E_PARA", "shadow_samples must be an integer", "shadow_samples")
        if not (1 <= self.shadow_samples <= 64):
            fail(
                "E_PARA",
                "shadow_samples must be in [1, 64]",
                "shadow_samples",
            )


@dataclass(frozen=True)
class StrategyDecision:
    mode: str
    downsample_factor: int
    tile_size: int
    max_parallel_blocks: int
    buffer_limit_elements: int
    exact_pixel_limit: int
    total_pixels: int
    object_count: int
    iterations: int
    memory_budget_mb: float
    estimated_full_working_elements: int
    estimated_tile_working_elements: int
    explanation: str

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


def _elements_budget(memory_budget_mb: float) -> int:
    return max(1, int(memory_budget_mb * 1024.0 * 1024.0 / 8.0))


def _working_per_pixel(object_count: int, shadow_samples: int) -> int:
    # Rays, nearest-hit candidates, normals, albedo and soft-shadow samples.
    return max(8, object_count * 4 + shadow_samples * 3 + 8)


def estimated_element_cost(
    *,
    object_count: int,
    shadow_samples: int,
    pixels: int,
) -> int:
    if pixels <= 0:
        fail("E_PARA", "pixels must be positive", "pixels")
    return pixels * _working_per_pixel(object_count, shadow_samples)


def choose_strategy(
    *,
    object_count: int,
    width: int,
    height: int,
    image_count: int,
    iterations: int,
    config: ScaleConfig,
) -> StrategyDecision:
    config.validate()
    if isinstance(object_count, bool) or not isinstance(object_count, int) or object_count <= 0:
        fail("E_PARA", "object_count must be a positive integer", "object_count")
    if isinstance(image_count, bool) or not isinstance(image_count, int) or image_count <= 0:
        fail("E_PARA", "image_count must be a positive integer", "image_count")
    if isinstance(iterations, bool) or not isinstance(iterations, int) or iterations <= 0:
        fail("E_PARA", "iterations must be a positive integer", "iterations")
    if isinstance(width, bool) or not isinstance(width, int) or width <= 0:
        fail("E_PARA", "width must be a positive integer", "width")
    if isinstance(height, bool) or not isinstance(height, int) or height <= 0:
        fail("E_PARA", "height must be a positive integer", "height")

    total_pixels = width * height * image_count
    per_pixel = _working_per_pixel(object_count, config.shadow_samples)
    full_working = total_pixels * per_pixel
    budget = _elements_budget(config.memory_budget_mb)
    tile_pixels = min(config.tile_size * config.tile_size, width * height)
    tile_working = (
        tile_pixels * per_pixel * min(config.max_parallel_blocks, 8)
    )

    mode = config.mode
    factor = 1
    reasons: list[str] = []
    if mode == "auto":
        exact_memory_fits = full_working <= int(budget * 0.65)
        exact_pixels_fit = total_pixels <= config.max_exact_pixels
        iteration_friendly = iterations <= config.max_exact_iterations
        tiled_memory_fits = tile_working <= budget
        if exact_memory_fits and exact_pixels_fit and iteration_friendly:
            mode = "exact"
            reasons.append(
                "full-image working set fits 65% of memory budget and pixel/iteration limits"
            )
        elif (
            iterations <= config.downsample_iteration_threshold
            and tiled_memory_fits
        ):
            mode = "tiled"
            reasons.append(
                "full-image set is too large, but a fixed tile working set fits the budget"
            )
        elif tiled_memory_fits:
            mode = "downsample"
            reasons.append(
                "long optimization budget makes repeated tiled boundary overhead unattractive"
            )
        else:
            mode = "tiled"
            reasons.append(
                "memory is too tight for downsampling to preserve enough detail; tiles are reduced"
            )
    else:
        reasons.append(f"mode explicitly requested as {mode}")

    if mode == "downsample":
        factor = config.requested_downsample_factor
        if factor <= 1:
            target_working = int(budget * 0.55)
            factor = 2
            while factor < 16:
                reduced_pixels = math.ceil(width / factor) * math.ceil(height / factor) * image_count
                reduced_working = reduced_pixels * per_pixel
                if (
                    reduced_pixels <= config.max_exact_pixels
                    and reduced_working <= target_working
                ):
                    break
                factor *= 2
            reasons.append(
                f"smallest power-of-two factor keeping reduced working set within budget is {factor}x"
            )
        else:
            reasons.append(f"downsample factor fixed by configuration at {factor}x")

        reduced_pixels = (
            math.ceil(width / factor)
            * math.ceil(height / factor)
            * image_count
        )
        reduced_working = reduced_pixels * per_pixel
        if reduced_working > budget:
            if config.mode == "auto":
                mode = "tiled"
                factor = 1
                reasons.append(
                    "downsampling through 16x could not meet the memory budget; "
                    "falling back to bounded tiles"
                )
            else:
                fail(
                    "E_PARA",
                    "requested downsample factor does not fit the memory budget",
                    "memory_budget_mb",
                    required_elements=reduced_working,
                    budget_elements=budget,
                    downsample_factor=factor,
                )

    effective_total_pixels = (
        math.ceil(width / factor) * math.ceil(height / factor) * image_count
        if mode == "downsample"
        else total_pixels
    )
    effective_working = effective_total_pixels * per_pixel
    if mode == "exact":
        if full_working > budget:
            fail(
                "E_PARA",
                "exact mode exceeds the memory budget",
                "memory_budget_mb",
                required_elements=full_working,
                budget_elements=budget,
            )
        buffer_limit = max(full_working, 1)
    elif mode == "downsample":
        if effective_working > budget:
            fail(
                "E_PARA",
                "downsample mode exceeds the memory budget",
                "memory_budget_mb",
                required_elements=effective_working,
                budget_elements=budget,
            )
        buffer_limit = max(int(effective_working), 1)
    else:
        tile_size = config.tile_size
        min_tile_pixels = 1
        while (
            tile_size > 4
            and min(tile_size * tile_size, width * height)
            * per_pixel
            * min(config.max_parallel_blocks, 8)
            > budget
        ):
            tile_size = max(4, tile_size // 2)
        if tile_size < config.tile_size:
            reasons.append(
                f"tile size reduced from {config.tile_size} to {tile_size} to satisfy buffer budget"
            )
        tile_pixels = min(tile_size * tile_size, width * height)
        tile_working = tile_pixels * per_pixel * min(config.max_parallel_blocks, 8)
        if tile_working > budget:
            fail(
                "E_PARA",
                "memory budget cannot hold even the minimum tiled working set",
                "memory_budget_mb",
                required_elements=tile_working,
                budget_elements=budget,
            )
        buffer_limit = max(min(tile_working, budget), 1)
        config = ScaleConfig(
            mode=config.mode,
            memory_budget_mb=config.memory_budget_mb,
            max_exact_pixels=config.max_exact_pixels,
            max_exact_iterations=config.max_exact_iterations,
            downsample_iteration_threshold=config.downsample_iteration_threshold,
            tile_size=tile_size,
            max_parallel_blocks=config.max_parallel_blocks,
            requested_downsample_factor=config.requested_downsample_factor,
            shadow_samples=config.shadow_samples,
        )

    explanation = "; ".join(reasons)
    return StrategyDecision(
        mode=mode,
        downsample_factor=factor if mode == "downsample" else 1,
        tile_size=config.tile_size if mode == "tiled" else 0,
        max_parallel_blocks=(
            config.max_parallel_blocks if mode == "tiled" else 1
        ),
        buffer_limit_elements=buffer_limit,
        exact_pixel_limit=config.max_exact_pixels,
        total_pixels=total_pixels,
        object_count=object_count,
        iterations=iterations,
        memory_budget_mb=config.memory_budget_mb,
        estimated_full_working_elements=full_working,
        estimated_tile_working_elements=tile_working,
        explanation=explanation,
    )


def mode_difference_report(
    exact_image: Any,
    reconstructed_image: Any,
) -> dict[str, float]:
    import torch

    difference = (exact_image - reconstructed_image).abs()
    return {
        "mae": float(difference.mean().item()),
        "rmse": float(torch.sqrt(torch.mean(difference.square())).item()),
        "max_abs": float(difference.max().item()),
    }

