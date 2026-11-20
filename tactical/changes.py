"""Validated local terrain changes and deterministic batch application."""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass
from typing import Any

import numpy as np

from .grid import WORLD_BLOCK_BIT, BattleGrid


class ChangeError(ValueError):
    pass


@dataclass(frozen=True)
class SetBaseCost:
    row: int
    col: int
    cost: int
    op: str = "base_cost"

    def apply(self, base: np.ndarray, mask: np.ndarray, danger: np.ndarray) -> None:
        base[self.row, self.col] = np.int32(self.cost)

    def to_dict(self) -> dict[str, Any]:
        return {"op": self.op, "row": self.row, "col": self.col, "cost": self.cost}


@dataclass(frozen=True)
class SetDanger:
    row: int
    col: int
    active: bool
    op: str = "danger"

    def apply(self, base: np.ndarray, mask: np.ndarray, danger: np.ndarray) -> None:
        danger[self.row, self.col] = np.uint8(1 if self.active else 0)

    def to_dict(self) -> dict[str, Any]:
        return {
            "op": self.op,
            "row": self.row,
            "col": self.col,
            "active": bool(self.active),
        }


@dataclass(frozen=True)
class SetObstacle:
    row: int
    col: int
    blocked: bool
    op: str = "obstacle"

    def apply(self, base: np.ndarray, mask: np.ndarray, danger: np.ndarray) -> None:
        if self.blocked:
            mask[self.row, self.col] &= ~WORLD_BLOCK_BIT
        else:
            mask[self.row, self.col] |= WORLD_BLOCK_BIT

    def to_dict(self) -> dict[str, Any]:
        return {
            "op": self.op,
            "row": self.row,
            "col": self.col,
            "blocked": bool(self.blocked),
        }


@dataclass(frozen=True)
class SetUnitPassability:
    row: int
    col: int
    unit_id: int
    allowed: bool
    op: str = "unit_pass"

    def apply(self, base: np.ndarray, mask: np.ndarray, danger: np.ndarray) -> None:
        bit = np.uint64(1) << np.uint64(self.unit_id)
        if self.allowed:
            mask[self.row, self.col] |= bit
        else:
            mask[self.row, self.col] &= ~bit

    def to_dict(self) -> dict[str, Any]:
        return {
            "op": self.op,
            "row": self.row,
            "col": self.col,
            "unit": self.unit_id,
            "allowed": bool(self.allowed),
        }


Change = SetBaseCost | SetDanger | SetObstacle | SetUnitPassability


def parse_change(raw: dict[str, Any]) -> Change:
    try:
        op = raw["op"]
        if op == "base_cost":
            return SetBaseCost(int(raw["row"]), int(raw["col"]), int(raw["cost"]))
        if op == "danger":
            return SetDanger(int(raw["row"]), int(raw["col"]), bool(raw["active"]))
        if op == "obstacle":
            return SetObstacle(int(raw["row"]), int(raw["col"]), bool(raw["blocked"]))
        if op == "unit_pass":
            return SetUnitPassability(
                int(raw["row"]),
                int(raw["col"]),
                int(raw["unit"]),
                bool(raw["allowed"]),
            )
    except (KeyError, TypeError, ValueError) as exc:
        raise ChangeError(f"invalid change payload: {raw!r}") from exc
    raise ChangeError(f"unknown change op: {op!r}")


def validate_changes(grid: BattleGrid, changes: Iterable[Change]) -> tuple[Change, ...]:
    ordered = tuple(changes)
    for change in ordered:
        row, col = int(change.row), int(change.col)
        if not (0 <= row < grid.height and 0 <= col < grid.width):
            raise ChangeError(f"change cell out of range: ({row}, {col})")
        if isinstance(change, SetBaseCost) and not 1 <= change.cost <= 1_000_000:
            raise ChangeError("base cost must be in [1, 1000000]")
        if isinstance(change, SetUnitPassability) and not (
            0 <= change.unit_id < grid.unit_count
        ):
            raise ChangeError(f"unit id out of range: {change.unit_id}")
    return ordered


def apply_changes(grid: BattleGrid, changes: Iterable[Change]) -> BattleGrid:
    ordered = validate_changes(grid, changes)
    base, mask, danger = grid.clone_arrays()
    for change in ordered:
        change.apply(base, mask, danger)
    return BattleGrid(base, mask, danger, grid.unit_count, grid.danger_penalty)


def changed_cell_indices(changes: Iterable[Change], width: int) -> list[int]:
    return sorted({int(c.row) * width + int(c.col) for c in changes})
