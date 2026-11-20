"""Immutable battlefield grid and per-unit traversal costs."""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass

import numpy as np

from .hashing import canonical_json_bytes, sha256_bytes

WORLD_BLOCK_BIT = np.uint64(1 << 63)
INF = np.int64(np.iinfo(np.int64).max)
MAX_UNITS = 63


def _readonly(array: np.ndarray) -> np.ndarray:
    array.flags.writeable = False
    return array


@dataclass(frozen=True)
class BattleGrid:
    """A grid with base cost, per-unit pass mask, and danger marker per cell."""

    base_cost: np.ndarray
    pass_mask: np.ndarray
    danger: np.ndarray
    unit_count: int
    danger_penalty: int = 5

    def __post_init__(self) -> None:
        base = np.asarray(self.base_cost, dtype=np.int32)
        mask = np.asarray(self.pass_mask, dtype=np.uint64)
        danger = np.asarray(self.danger, dtype=np.uint8)
        if base.ndim != 2 or base.shape[0] < 1 or base.shape[1] < 1:
            raise ValueError("grid must be a non-empty 2-D array")
        if base.shape != mask.shape or base.shape != danger.shape:
            raise ValueError("grid arrays must have identical shapes")
        if not 1 <= int(self.unit_count) <= MAX_UNITS:
            raise ValueError(f"unit_count must be in [1, {MAX_UNITS}]")
        if np.any(base < 1):
            raise ValueError("base movement costs must be positive")
        if np.any((danger != 0) & (danger != 1)):
            raise ValueError("danger markers must be 0 or 1")
        if int(self.danger_penalty) < 0:
            raise ValueError("danger_penalty must be non-negative")

        object.__setattr__(self, "base_cost", _readonly(base.copy()))
        object.__setattr__(self, "pass_mask", _readonly(mask.copy()))
        object.__setattr__(self, "danger", _readonly(danger.copy()))
        object.__setattr__(self, "unit_count", int(self.unit_count))
        object.__setattr__(self, "danger_penalty", int(self.danger_penalty))

    @property
    def height(self) -> int:
        return int(self.base_cost.shape[0])

    @property
    def width(self) -> int:
        return int(self.base_cost.shape[1])

    @property
    def shape(self) -> tuple[int, int]:
        return (self.height, self.width)

    def unit_bit(self, unit_id: int) -> np.uint64:
        if not 0 <= int(unit_id) < self.unit_count:
            raise ValueError(f"unit_id out of range: {unit_id}")
        return np.uint64(1) << np.uint64(int(unit_id))

    def passable_mask(self, unit_id: int) -> np.ndarray:
        bit = self.unit_bit(unit_id)
        allowed = (self.pass_mask & bit) != 0
        unblocked = (self.pass_mask & WORLD_BLOCK_BIT) != 0
        return allowed & unblocked

    def cell_cost(self, unit_id: int) -> np.ndarray:
        """Return immutable int64 traversal costs; blocked cells are INF."""

        costs = self.base_cost.astype(np.int64, copy=True) + self.danger.astype(
            np.int64
        ) * np.int64(self.danger_penalty)
        costs[~self.passable_mask(unit_id)] = INF
        return _readonly(costs)

    def is_passable(self, unit_id: int, row: int, col: int) -> bool:
        self._check_cell(row, col)
        bit = self.unit_bit(unit_id)
        value = int(self.pass_mask[row, col])
        return bool(value & int(WORLD_BLOCK_BIT)) and bool(value & int(bit))

    def _check_cell(self, row: int, col: int) -> None:
        if not (0 <= int(row) < self.height and 0 <= int(col) < self.width):
            raise ValueError(f"cell out of range: ({row}, {col})")

    def content_hash(self) -> str:
        header = canonical_json_bytes(
            {
                "format": "battle-grid-v1",
                "shape": [self.height, self.width],
                "unit_count": self.unit_count,
                "danger_penalty": self.danger_penalty,
            }
        )
        digest = sha256_bytes(header)
        digest = sha256_bytes(
            digest.encode("ascii") + self.base_cost.astype("<i4", copy=False).tobytes()
        )
        digest = sha256_bytes(
            digest.encode("ascii") + self.pass_mask.astype("<u8", copy=False).tobytes()
        )
        digest = sha256_bytes(
            digest.encode("ascii") + self.danger.astype("u1", copy=False).tobytes()
        )
        return digest

    def array_nbytes(self) -> int:
        return int(self.base_cost.nbytes + self.pass_mask.nbytes + self.danger.nbytes)

    def clone_arrays(self) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        return (
            self.base_cost.copy(),
            self.pass_mask.copy(),
            self.danger.copy(),
        )

    @classmethod
    def from_arrays(
        cls,
        base_cost: np.ndarray,
        pass_mask: np.ndarray,
        danger: np.ndarray,
        unit_count: int,
        danger_penalty: int,
    ) -> BattleGrid:
        return cls(base_cost, pass_mask, danger, unit_count, danger_penalty)

    def all_unit_ids(self) -> Iterable[int]:
        return range(self.unit_count)
