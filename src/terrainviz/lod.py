from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .camera import Camera
from .errors import E_INVALID_ARGUMENT, TerrainError
from .terrain import Block, Terrain


@dataclass(frozen=True)
class LodConfig:
    screen_error_threshold: float
    max_level: int = 4

    def __post_init__(self) -> None:
        if not math.isfinite(self.screen_error_threshold) or self.screen_error_threshold <= 0.0:
            raise TerrainError(
                E_INVALID_ARGUMENT,
                "screen error threshold must be finite and positive",
                screen_error_threshold=self.screen_error_threshold,
            )
        if self.max_level < 0:
            raise TerrainError(
                E_INVALID_ARGUMENT, "max LOD level must be non-negative", max_level=self.max_level
            )


@dataclass(frozen=True)
class LodSelection:
    block_id: str
    chunk_row: int
    chunk_col: int
    level: int
    stride: int
    simplification_error: float
    quantization_error: float
    relief: float
    error_upper_bound: float
    distance: float
    projected_error_pixels: float
    budget_usage: float
    fallback: bool
    seam_adjusted: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "block_id": self.block_id,
            "chunk": [self.chunk_row, self.chunk_col],
            "level": self.level,
            "stride": self.stride,
            "simplification_error": round(self.simplification_error, 9),
            "quantization_error": round(self.quantization_error, 9),
            "relief": round(self.relief, 9),
            "error_upper_bound": round(self.error_upper_bound, 9),
            "distance": round(self.distance, 9),
            "projected_error_pixels": round(self.projected_error_pixels, 9),
            "budget_usage": round(self.budget_usage, 9),
            "fallback": self.fallback,
            "seam_adjusted": self.seam_adjusted,
        }


@dataclass
class LodDecision:
    config: LodConfig
    camera: Camera
    selections: dict[str, LodSelection]

    @property
    def level_table(self) -> list[dict[str, Any]]:
        return [self.selections[key].to_dict() for key in sorted(self.selections)]

    def levels_by_id(self) -> dict[str, int]:
        return {key: value.level for key, value in sorted(self.selections.items())}

    def error_summary(self) -> dict[str, Any]:
        values = [self.selections[key] for key in sorted(self.selections)]
        if not values:
            return {
                "block_count": 0,
                "max_error_upper_bound": 0.0,
                "max_projected_error_pixels": 0.0,
                "max_budget_usage": 0.0,
                "fallback_blocks": 0,
                "mean_level": 0.0,
            }
        return {
            "block_count": len(values),
            "max_error_upper_bound": round(
                max(value.error_upper_bound for value in values), 9
            ),
            "max_projected_error_pixels": round(
                max(value.projected_error_pixels for value in values), 9
            ),
            "max_budget_usage": round(max(value.budget_usage for value in values), 9),
            "fallback_blocks": sum(1 for value in values if value.fallback),
            "adjusted_for_seams": sum(1 for value in values if value.seam_adjusted),
            "mean_level": round(
                sum(value.level for value in values) / len(values), 9
            ),
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "screen_error_threshold": self.config.screen_error_threshold,
            "max_level": self.config.max_level,
            "camera_view_matrix": self.camera.view_matrix().round(12).tolist(),
            "levels": self.level_table,
            "summary": self.error_summary(),
        }


def stride_for_level(level: int) -> int:
    return 1 << int(level)


def max_level_for_block(block: Block, configured_max: int = 4) -> int:
    dimension = max(1, min(block.quad_rows, block.quad_cols))
    hard_limit = int(math.floor(math.log2(dimension))) if dimension >= 1 else 0
    return max(0, min(configured_max, hard_limit))


def _coarse_indices(start: int, end: int, stride: int) -> np.ndarray:
    indices = np.arange(start, end + 1, stride, dtype=np.int64)
    if indices.size == 0 or indices[-1] != end:
        indices = np.append(indices, end)
    return indices


def coarse_indices_for_block(block: Block, stride: int) -> tuple[np.ndarray, np.ndarray]:
    rows = _coarse_indices(block.row0, block.row1, stride)
    cols = _coarse_indices(block.col0, block.col1, stride)
    return rows, cols


def simplification_max_error(
    heights: np.ndarray,
    block: Block,
    stride: int,
) -> float:
    if stride <= 1:
        return 0.0
    patch = heights[block.row0 : block.row1 + 1, block.col0 : block.col1 + 1].astype(
        np.float64, copy=False
    )
    local_rows = np.arange(block.rows, dtype=np.int64)
    local_cols = np.arange(block.cols, dtype=np.int64)
    coarse_rows = _coarse_indices(0, block.rows - 1, stride)
    coarse_cols = _coarse_indices(0, block.cols - 1, stride)
    coarse = patch[np.ix_(coarse_rows, coarse_cols)]
    along_cols = np.vstack(
        [
            np.interp(local_cols, coarse_cols, coarse[row_index])
            for row_index in range(coarse.shape[0])
        ]
    )
    reconstructed = np.empty_like(patch)
    for col_index in range(block.cols):
        reconstructed[:, col_index] = np.interp(
            local_rows, coarse_rows, along_cols[:, col_index]
        )
    return float(np.max(np.abs(reconstructed - patch)))


def _block_distance(camera: Camera, terrain: Terrain, block: Block) -> float:
    patch = terrain.block_heights(block)
    center = np.array(
        [
            block.center_col * terrain.spacing,
            (float(np.min(patch)) + float(np.max(patch))) * 0.5,
            block.center_row * terrain.spacing,
        ],
        dtype=np.float64,
    )
    half_diagonal = 0.5 * math.sqrt(
        (block.quad_cols * terrain.spacing) ** 2
        + (block.quad_rows * terrain.spacing) ** 2
        + (float(np.max(patch)) - float(np.min(patch))) ** 2
    )
    return max(camera.near, camera.point_distance(center) - half_diagonal)


def selection_for_level(
    terrain: Terrain,
    camera: Camera,
    block: Block,
    level: int,
    config: LodConfig,
    *,
    fallback: bool = False,
    seam_adjusted: bool = False,
) -> LodSelection:
    stride = stride_for_level(level)
    simplification_error = simplification_max_error(
        terrain.heightfield.heights, block, stride
    )
    quantization_error = terrain.heightfield.quantization_step
    patch = terrain.block_heights(block)
    relief = float(np.max(patch)) - float(np.min(patch))
    bound = simplification_error + quantization_error
    distance = _block_distance(camera, terrain, block)
    projected = camera.projected_vertical_error_pixels(bound, distance)
    return LodSelection(
        block_id=block.id,
        chunk_row=block.chunk_row,
        chunk_col=block.chunk_col,
        level=level,
        stride=stride,
        simplification_error=simplification_error,
        quantization_error=quantization_error,
        relief=relief,
        error_upper_bound=bound,
        distance=distance,
        projected_error_pixels=projected,
        budget_usage=projected / config.screen_error_threshold,
        fallback=fallback,
        seam_adjusted=seam_adjusted,
    )


def choose_lod(
    terrain: Terrain,
    camera: Camera,
    screen_error_threshold: float,
    max_level: int = 4,
) -> LodDecision:
    config = LodConfig(
        screen_error_threshold=float(screen_error_threshold), max_level=int(max_level)
    )
    selections: dict[str, LodSelection] = {}
    for block in sorted(terrain.blocks, key=lambda item: item.id):
        candidates = [
            selection_for_level(terrain, camera, block, level, config)
            for level in range(max_level_for_block(block, config.max_level) + 1)
        ]
        within_budget = [
            candidate
            for candidate in candidates
            if candidate.projected_error_pixels <= config.screen_error_threshold + 1e-12
        ]
        if within_budget:
            chosen = max(within_budget, key=lambda item: item.level)
        else:
            chosen = selection_for_level(
                terrain, camera, block, 0, config, fallback=True
            )
        selections[block.id] = chosen
    return LodDecision(config=config, camera=camera, selections=selections)

