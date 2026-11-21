"""Immutable screen-space bucket index."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable, Sequence

from .geometry import Rect
from .projection import UnitProjection


@dataclass(frozen=True, slots=True)
class BucketConfig:
    columns: int = 32
    rows: int = 32
    neighbor_cells_for_points: int = 1

    def __post_init__(self) -> None:
        if self.columns < 1 or self.rows < 1:
            raise ValueError("bucket grid dimensions must be positive")
        if self.neighbor_cells_for_points < 0:
            raise ValueError("point search neighbourhood cannot be negative")


@dataclass(frozen=True, slots=True)
class ScreenBucketIndex:
    columns: int
    rows: int
    width: int
    height: int
    cells: tuple[tuple[int, ...], ...]

    @property
    def cell_width(self) -> float:
        return float(self.width) / float(self.columns)

    @property
    def cell_height(self) -> float:
        return float(self.height) / float(self.rows)

    @property
    def memory_bytes(self) -> int:
        # Conservative accounting: cell tuple object, eight-byte references,
        # and per-cell tuple overhead.
        entry_count = sum(len(cell) for cell in self.cells)
        return 256 + len(self.cells) * 64 + entry_count * 8

    @classmethod
    def build(
        cls,
        projections: Sequence[UnitProjection],
        *,
        width: int,
        height: int,
        config: BucketConfig,
    ) -> "ScreenBucketIndex":
        cells: list[list[int]] = [[] for _ in range(config.columns * config.rows)]
        cell_width = float(width) / config.columns
        cell_height = float(height) / config.rows
        for projection in projections:
            if not projection.eligible_for_selection or projection.bbox is None:
                continue
            xmin, ymin, xmax, ymax = projection.bbox
            x0 = max(0, min(config.columns - 1, int(math.floor(xmin / cell_width))))
            y0 = max(0, min(config.rows - 1, int(math.floor(ymin / cell_height))))
            x1 = max(0, min(config.columns - 1, int(math.floor((xmax - 1e-9) / cell_width))))
            y1 = max(0, min(config.rows - 1, int(math.floor((ymax - 1e-9) / cell_height))))
            for row in range(y0, y1 + 1):
                row_offset = row * config.columns
                for column in range(x0, x1 + 1):
                    cells[row_offset + column].append(projection.unit_id)
        frozen = tuple(tuple(sorted(set(cell))) for cell in cells)
        return cls(
            columns=config.columns,
            rows=config.rows,
            width=width,
            height=height,
            cells=frozen,
        )

    def _cell_range(self, rect: Rect) -> tuple[int, int, int, int]:
        xmin, ymin, xmax, ymax = rect
        cell_width = self.cell_width
        cell_height = self.cell_height
        x0 = max(0, min(self.columns - 1, int(math.floor(xmin / cell_width))))
        y0 = max(0, min(self.rows - 1, int(math.floor(ymin / cell_height))))
        x1 = max(0, min(self.columns - 1, int(math.floor((xmax - 1e-9) / cell_width))))
        y1 = max(0, min(self.rows - 1, int(math.floor((ymax - 1e-9) / cell_height))))
        return x0, y0, x1, y1

    def query_rect(self, rect: Rect) -> tuple[int, ...]:
        x0, y0, x1, y1 = self._cell_range(rect)
        candidates: set[int] = set()
        for row in range(y0, y1 + 1):
            offset = row * self.columns
            for column in range(x0, x1 + 1):
                candidates.update(self.cells[offset + column])
        return tuple(sorted(candidates))

    def query_point(self, x: float, y: float, *, neighbors: int | None = None) -> tuple[int, ...]:
        neighbourhood = self.neighbor_cells_for_points if neighbors is None else neighbors
        column = max(0, min(self.columns - 1, int(math.floor(x / self.cell_width))))
        row = max(0, min(self.rows - 1, int(math.floor(y / self.cell_height))))
        candidates: set[int] = set()
        for candidate_row in range(max(0, row - neighbourhood), min(self.rows, row + neighbourhood + 1)):
            offset = candidate_row * self.columns
            for candidate_column in range(
                max(0, column - neighbourhood),
                min(self.columns, column + neighbourhood + 1),
            ):
                candidates.update(self.cells[offset + candidate_column])
        return tuple(sorted(candidates))


def build_index(
    projections: Sequence[UnitProjection],
    *,
    width: int,
    height: int,
    config: BucketConfig,
) -> ScreenBucketIndex:
    return ScreenBucketIndex.build(projections, width=width, height=height, config=config)


def candidate_ids_for_rect(index: ScreenBucketIndex, rect: Rect) -> tuple[int, ...]:
    return index.query_rect(rect)


def unique_sorted(values: Iterable[int]) -> tuple[int, ...]:
    return tuple(sorted(set(values)))
