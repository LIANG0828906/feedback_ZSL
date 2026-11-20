from __future__ import annotations

import numpy as np

from tactical.grid import WORLD_BLOCK_BIT, BattleGrid


def open_grid(
    height: int = 16,
    width: int = 16,
    units: int = 3,
    danger_penalty: int = 5,
) -> BattleGrid:
    base = np.ones((height, width), dtype=np.int32)
    mask = np.full((height, width), WORLD_BLOCK_BIT, dtype=np.uint64)
    for unit in range(units):
        mask |= np.uint64(1) << np.uint64(unit)
    danger = np.zeros((height, width), dtype=np.uint8)
    return BattleGrid(base, mask, danger, units, danger_penalty)


def random_grid(
    rng: np.random.Generator,
    height: int = 10,
    width: int = 11,
    units: int = 3,
) -> BattleGrid:
    base = rng.integers(1, 7, size=(height, width), dtype=np.int32)
    mask = np.full((height, width), WORLD_BLOCK_BIT, dtype=np.uint64)
    for unit in range(units):
        allowed = (rng.random((height, width)) < 0.82).astype(np.uint64)
        mask |= allowed * (np.uint64(1) << np.uint64(unit))
    mask[rng.random((height, width)) < 0.07] &= ~WORLD_BLOCK_BIT
    danger = (rng.random((height, width)) < 0.12).astype(np.uint8)
    return BattleGrid(base, mask, danger, units, 4)
