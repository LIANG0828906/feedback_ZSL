"""Deterministic local battlefield sample generation."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .changes import (
    Change,
    SetBaseCost,
    SetDanger,
    SetObstacle,
    SetUnitPassability,
    apply_changes,
    parse_change,
)
from .costfield import CostField, compute_full_field, reconstruct_path
from .grid import WORLD_BLOCK_BIT, BattleGrid
from .hashing import canonical_json_bytes, sha256_bytes

MASK64 = (1 << 64) - 1
GOLDEN = 0x9E3779B97F4A7C15


class SplitMix64:
    def __init__(self, seed: int) -> None:
        self.state = int(seed) & MASK64

    def next_u64(self) -> int:
        self.state = (self.state + GOLDEN) & MASK64
        value = self.state
        value = ((value ^ (value >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
        value = ((value ^ (value >> 27)) * 0x94D049BB133111EB) & MASK64
        return (value ^ (value >> 31)) & MASK64

    def randbelow(self, upper: int) -> int:
        if upper <= 0:
            raise ValueError("upper must be positive")
        return self.next_u64() % int(upper)

    def choice(self, values: list[int]) -> int:
        return values[self.randbelow(len(values))]


def _mix64_array(values: np.ndarray) -> np.ndarray:
    values = np.asarray(values, dtype=np.uint64).copy()
    values ^= values >> np.uint64(30)
    values *= np.uint64(0xBF58476D1CE4E5B9)
    values ^= values >> np.uint64(27)
    values *= np.uint64(0x94D049BB133111EB)
    values ^= values >> np.uint64(31)
    return values


def _hash_grid(height: int, width: int, seed: int, salt: int) -> np.ndarray:
    count = height * width
    indices = np.arange(count, dtype=np.uint64)
    indices += np.uint64((int(seed) + int(salt) * GOLDEN) & MASK64)
    return _mix64_array(indices).reshape(height, width)


def _force_passable(mask: np.ndarray, unit: int, row: int, col: int) -> None:
    mask[row, col] |= WORLD_BLOCK_BIT
    mask[row, col] |= np.uint64(1) << np.uint64(unit)


def build_sample(seed: int, size: int) -> dict[str, Any]:
    height = width = int(size)
    unit_count = 4
    danger_penalty = 5
    if height < 32 or width < 32:
        raise ValueError("sample size must be at least 32")

    base_values = _hash_grid(height, width, seed, 1)
    obstacle_values = _hash_grid(height, width, seed, 2)
    danger_values = _hash_grid(height, width, seed, 3)
    base = (1 + (base_values % np.uint64(5))).astype(np.int32)
    mask = np.full((height, width), WORLD_BLOCK_BIT, dtype=np.uint64)
    for unit in range(unit_count):
        allowed_values = _hash_grid(height, width, seed, 10 + unit)
        allowed = (allowed_values % np.uint64(100)) < np.uint64(82)
        mask[allowed] |= np.uint64(1) << np.uint64(unit)
    blocked = (obstacle_values % np.uint64(100)) < np.uint64(3)
    mask[blocked] &= ~WORLD_BLOCK_BIT
    danger = ((danger_values % np.uint64(100)) < np.uint64(9)).astype(np.uint8)

    targets: dict[int, tuple[int, int]] = {}
    for unit in range(unit_count):
        target = (
            height // 2 + (unit - unit_count // 2),
            width - 3,
        )
        _force_passable(mask, unit, target[0], target[1])
        targets[unit] = target

    rng = SplitMix64(seed ^ 0xD1B54A32D192ED03)
    cache_queries: list[dict[str, Any]] = []
    for unit in range(unit_count):
        for row_seed in (0.18, 0.34, 0.50, 0.68):
            row = min(height - 2, max(1, int(height * row_seed)))
            source_found: tuple[int, int] | None = None
            for delta in range(height):
                candidate_row = (row + delta) % (height - 2) + 1
                candidate_col = 2 + rng.randbelow(3)
                value = int(mask[candidate_row, candidate_col])
                if (value & int(WORLD_BLOCK_BIT)) and (value & (1 << unit)):
                    source_found = (candidate_row, candidate_col)
                    break
            if source_found is None:
                source_found = (row, 2)
                _force_passable(mask, unit, source_found[0], source_found[1])
            cache_queries.append(
                {"unit": unit, "source": [source_found[0], source_found[1]]}
            )

    grid = BattleGrid(base, mask, danger, unit_count, danger_penalty)
    batches = _build_change_batches(grid, targets, cache_queries, rng)
    return {
        "format": "tactical-sample-v1",
        "generator": "splitmix-grid-v1",
        "seed": int(seed),
        "height": height,
        "width": width,
        "unit_count": unit_count,
        "danger_penalty": danger_penalty,
        "targets": {
            str(unit): [targets[unit][0], targets[unit][1]] for unit in sorted(targets)
        },
        "cache_queries": cache_queries,
        "batches": [
            {
                "batch": index + 1,
                "changes": [change.to_dict() for change in batch],
            }
            for index, batch in enumerate(batches)
        ],
    }


def _build_change_batches(
    initial_grid: BattleGrid,
    targets: dict[int, tuple[int, int]],
    cache_queries: list[dict[str, Any]],
    rng: SplitMix64,
) -> list[list[Change]]:
    grid = initial_grid
    target_cells = set(targets.values())
    batches: list[list[Change]] = []

    def pick_unique(
        count: int,
        used: set[tuple[int, int]],
    ) -> list[tuple[int, int]]:
        cells: list[tuple[int, int]] = []
        for _ in range(count):
            for _attempt in range(10_000):
                row = 1 + rng.randbelow(max(1, grid.height - 2))
                col = max(1, grid.width // 3) + rng.randbelow(
                    max(1, grid.width - max(1, grid.width // 3) - 2)
                )
                cell = (min(row, grid.height - 2), min(col, grid.width - 2))
                if cell in used or cell in target_cells:
                    continue
                used.add(cell)
                cells.append(cell)
                break
            else:
                raise RuntimeError("could not choose deterministic change cell")
        return cells

    def commit(batch_changes: list[Change]) -> None:
        nonlocal grid
        grid = apply_changes(grid, batch_changes)
        batches.append(batch_changes)

    def pick_unit_passable(unit: int, used: set[tuple[int, int]]) -> tuple[int, int]:
        candidates = np.argwhere(grid.passable_mask(unit))
        filtered = [
            (int(row), int(col))
            for row, col in candidates
            if (int(row), int(col)) not in used
            and (int(row), int(col)) not in target_cells
        ]
        if not filtered:
            raise RuntimeError("no passable cell available")
        cell = filtered[rng.randbelow(len(filtered))]
        used.add(cell)
        return cell

    def pick_dangerous(used: set[tuple[int, int]]) -> tuple[int, int]:
        candidates = np.argwhere(grid.danger == 1)
        filtered = [
            (int(row), int(col))
            for row, col in candidates
            if (int(row), int(col)) not in used
            and (int(row), int(col)) not in target_cells
        ]
        if not filtered:
            raise RuntimeError("no dangerous cell available")
        cell = filtered[rng.randbelow(len(filtered))]
        used.add(cell)
        return cell

    def pick_cached_path_cell(
        used: set[tuple[int, int]],
        query_offset: int,
    ) -> tuple[int, int]:
        ordered = cache_queries[query_offset:] + cache_queries[:query_offset]
        for query in ordered:
            unit = int(query["unit"])
            source = (int(query["source"][0]), int(query["source"][1]))
            target = targets[unit]
            weights = grid.cell_cost(unit)
            field = CostField.create(unit, target, compute_full_field(weights, target))
            path, _cost, reachable = reconstruct_path(field, weights, source)
            if not reachable:
                continue
            candidates = [
                cell for cell in path if cell not in used and cell not in target_cells
            ]
            if candidates:
                cell = candidates[len(candidates) // 2]
                used.add(cell)
                return cell
        raise RuntimeError("no cached path cell available for invalidation")

    used = set()
    cells = pick_unique(3, used)
    commit(
        [
            SetObstacle(cells[0][0], cells[0][1], True),
            SetDanger(cells[1][0], cells[1][1], True),
            SetDanger(cells[2][0], cells[2][1], True),
        ]
    )

    blocked_cells = np.argwhere((grid.pass_mask & WORLD_BLOCK_BIT) == 0)
    removable = [
        (int(row), int(col))
        for row, col in blocked_cells
        if (int(row), int(col)) not in target_cells
    ]
    removed = removable[rng.randbelow(len(removable))]
    unit_cell = pick_unit_passable(1, used | {removed})
    path_cell = pick_cached_path_cell(used | {removed, unit_cell}, 0)
    old_path_cost = int(grid.base_cost[path_cell[0], path_cell[1]])
    commit(
        [
            SetObstacle(removed[0], removed[1], False),
            SetBaseCost(
                path_cell[0],
                path_cell[1],
                8 if old_path_cost != 8 else 6,
            ),
            SetUnitPassability(unit_cell[0], unit_cell[1], 1, False),
        ]
    )

    center = (grid.height // 2, (grid.width * 3) // 4)
    diffusion = [
        center,
        (min(grid.height - 2, center[0] + 1), center[1]),
        (center[0], min(grid.width - 2, center[1] + 1)),
        (max(1, center[0] - 1), center[1]),
    ]
    diffusion_changes: list[Change] = []
    for row, col in diffusion:
        if (row, col) in target_cells:
            continue
        if (row, col) not in used:
            use_cell = (row, col)
            used.add(use_cell)
        else:
            use_cell = pick_unique(1, used)[0]
        diffusion_changes.append(SetDanger(use_cell[0], use_cell[1], True))
    if len(diffusion_changes) < 4:
        for cell in pick_unique(4 - len(diffusion_changes), used):
            diffusion_changes.append(SetDanger(cell[0], cell[1], True))
    commit(diffusion_changes)

    unit_cell = pick_unit_passable(2, used)
    cells = pick_unique(2, used | {unit_cell})
    commit(
        [
            SetObstacle(cells[0][0], cells[0][1], True),
            SetUnitPassability(unit_cell[0], unit_cell[1], 2, False),
            SetDanger(cells[1][0], cells[1][1], True),
        ]
    )

    blocked_cells = np.argwhere((grid.pass_mask & WORLD_BLOCK_BIT) == 0)
    removable = [
        (int(row), int(col))
        for row, col in blocked_cells
        if (int(row), int(col)) not in target_cells
    ]
    removed = removable[rng.randbelow(len(removable))]
    danger_cell = pick_dangerous(used | {removed})
    path_cell = pick_cached_path_cell(used | {removed, danger_cell}, 8)
    old_base = int(grid.base_cost[path_cell[0], path_cell[1]])
    commit(
        [
            SetObstacle(removed[0], removed[1], False),
            SetBaseCost(
                path_cell[0],
                path_cell[1],
                9 if old_base < 9 else 2,
            ),
            SetDanger(danger_cell[0], danger_cell[1], False),
        ]
    )

    unit0_cell = pick_unit_passable(0, used)
    unit3_cell = pick_unit_passable(3, used | {unit0_cell})
    cells = pick_unique(2, used | {unit0_cell, unit3_cell})
    commit(
        [
            SetUnitPassability(unit0_cell[0], unit0_cell[1], 0, False),
            SetUnitPassability(unit3_cell[0], unit3_cell[1], 3, False),
            SetDanger(cells[0][0], cells[0][1], True),
            SetObstacle(cells[1][0], cells[1][1], True),
        ]
    )
    return batches


def sample_from_dict(
    value: dict[str, Any],
) -> tuple[
    BattleGrid,
    dict[int, tuple[int, int]],
    list[tuple[int, tuple[int, int]]],
    list[tuple[Change, ...]],
]:
    if value.get("format") != "tactical-sample-v1":
        raise ValueError("unsupported sample format")
    seed = int(value["seed"])
    height = int(value["height"])
    width = int(value["width"])
    if height != width:
        raise ValueError("sample must be square")
    generated = build_sample(seed, height)
    if generated != value:
        raise ValueError("sample content does not match deterministic generator")

    # Recreate the exact arrays and forced passability decisions.
    base_values = _hash_grid(height, width, seed, 1)
    obstacle_values = _hash_grid(height, width, seed, 2)
    danger_values = _hash_grid(height, width, seed, 3)
    base = (1 + (base_values % np.uint64(5))).astype(np.int32)
    mask = np.full((height, width), WORLD_BLOCK_BIT, dtype=np.uint64)
    for unit in range(int(value["unit_count"])):
        allowed_values = _hash_grid(height, width, seed, 10 + unit)
        allowed = (allowed_values % np.uint64(100)) < np.uint64(82)
        mask[allowed] |= np.uint64(1) << np.uint64(unit)
    blocked = (obstacle_values % np.uint64(100)) < np.uint64(3)
    mask[blocked] &= ~WORLD_BLOCK_BIT
    danger = ((danger_values % np.uint64(100)) < np.uint64(9)).astype(np.uint8)
    targets = {
        int(unit): (int(cell[0]), int(cell[1]))
        for unit, cell in value["targets"].items()
    }
    for unit, target in targets.items():
        _force_passable(mask, unit, target[0], target[1])
    for query in value["cache_queries"]:
        unit = int(query["unit"])
        source = (int(query["source"][0]), int(query["source"][1]))
        if (int(mask[source[0], source[1]]) & int(WORLD_BLOCK_BIT)) == 0 or not (
            int(mask[source[0], source[1]]) & (1 << unit)
        ):
            _force_passable(mask, unit, source[0], source[1])
    grid = BattleGrid(
        base,
        mask,
        danger,
        int(value["unit_count"]),
        int(value["danger_penalty"]),
    )
    queries = [
        (int(query["unit"]), (int(query["source"][0]), int(query["source"][1])))
        for query in value["cache_queries"]
    ]
    batches = [
        tuple(parse_change(change) for change in batch["changes"])
        for batch in value["batches"]
    ]
    return grid, targets, queries, batches


def write_sample(path: str | Path, sample: dict[str, Any]) -> str:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    data = canonical_json_bytes(sample) + b"\n"
    temporary = target.with_name(target.name + ".tmp")
    temporary.write_bytes(data)
    temporary.replace(target)
    return sha256_bytes(data)


def read_sample(path: str | Path) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


@dataclass(frozen=True)
class GeneratedSample:
    size: int
    path: Path
    sha256: str


def generate_sample_file(
    output_dir: str | Path, seed: int, size: int
) -> GeneratedSample:
    sample = build_sample(seed, size)
    path = Path(output_dir) / f"sample_{size}.json"
    digest = write_sample(path, sample)
    return GeneratedSample(size, path, digest)
