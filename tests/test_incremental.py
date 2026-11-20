from __future__ import annotations

import unittest

import numpy as np

from tactical.cache import path_index_dependencies
from tactical.changes import (
    SetBaseCost,
    SetDanger,
    SetObstacle,
    SetUnitPassability,
    apply_changes,
)
from tactical.costfield import (
    CostField,
    compute_full_field,
    incremental_update,
    reconstruct_path,
)
from tactical.grid import INF, WORLD_BLOCK_BIT
from tactical.hashing import path_hash
from tactical.samples import build_sample, sample_from_dict
from tactical.service import TacticalService

from .helpers import random_grid


class IncrementalTests(unittest.TestCase):
    def test_random_weight_changes_match_full_recompute(self) -> None:
        rng = np.random.default_rng(48271)
        for trial in range(80):
            grid = random_grid(rng, height=9, width=10, units=3)
            unit = trial % 3
            old_weights = grid.cell_cost(unit)
            passable = np.argwhere(old_weights != INF)
            target = tuple(
                int(value) for value in passable[rng.integers(len(passable))]
            )
            old_field = CostField.create(
                unit, target, compute_full_field(old_weights, target)
            )
            changes = []
            for _ in range(10):
                row = int(rng.integers(grid.height))
                col = int(rng.integers(grid.width))
                kind = int(rng.integers(4))
                if kind == 0:
                    changes.append(SetBaseCost(row, col, int(rng.integers(1, 8))))
                elif kind == 1:
                    changes.append(SetDanger(row, col, not bool(grid.danger[row, col])))
                elif kind == 2:
                    if (row, col) != target:
                        changes.append(
                            SetObstacle(
                                row,
                                col,
                                not bool(
                                    int(grid.pass_mask[row, col]) & int(WORLD_BLOCK_BIT)
                                ),
                            )
                        )
                else:
                    changes.append(
                        SetUnitPassability(
                            row,
                            col,
                            unit,
                            not grid.is_passable(unit, row, col),
                        )
                    )
            new_grid = apply_changes(grid, changes)
            if not new_grid.is_passable(unit, target[0], target[1]):
                continue
            new_weights = new_grid.cell_cost(unit)
            new_field, actual_changed, weight_changed = incremental_update(
                old_field, old_weights, new_weights
            )
            expected = compute_full_field(new_weights, target)
            np.testing.assert_array_equal(new_field.distance, expected)
            np.testing.assert_array_equal(
                actual_changed | weight_changed,
                (old_field.distance != expected) | (old_weights != new_weights),
            )

    def test_detached_cycle_is_cleared(self) -> None:
        base = np.ones((3, 5), dtype=np.int32)
        mask = np.full((3, 5), WORLD_BLOCK_BIT, dtype=np.uint64)
        mask |= np.uint64(1)
        danger = np.zeros((3, 5), dtype=np.uint8)
        from tactical.grid import BattleGrid

        grid = BattleGrid(base, mask, danger, 1, 5)
        weights = grid.cell_cost(0)
        target = (0, 4)
        old_field = CostField.create(0, target, compute_full_field(weights, target))
        blocked = apply_changes(
            grid,
            [
                SetObstacle(0, 2, True),
                SetObstacle(1, 2, True),
                SetObstacle(2, 2, True),
            ],
        )
        new_weights = blocked.cell_cost(0)
        new_field, changed, weight_changed = incremental_update(
            old_field, weights, new_weights
        )
        expected = compute_full_field(new_weights, target)
        np.testing.assert_array_equal(new_field.distance, expected)
        self.assertEqual(int(new_field.distance[0, 0]), int(INF))
        self.assertTrue(bool(changed[0, 0]))
        self.assertTrue(bool(weight_changed[0, 2]))

    def test_service_batch_closure_and_cache_dependencies(self) -> None:
        sample = build_sample(20261121, 64)
        grid, targets, queries, batches = sample_from_dict(sample)
        service = TacticalService(grid, targets, cache_queries=queries)
        try:
            old_snapshot = service.snapshot
            stats = service.commit_batch(batches[1])
            self.assertTrue(stats.closure_equals_merged)
            self.assertGreaterEqual(stats.cache_invalidated, 1)
            self.assertGreaterEqual(stats.cache_hits, 1)
            self.assertEqual(
                stats.cache_invalidated + stats.cache_hits,
                len(old_snapshot.path_cache),
            )
            self.assertEqual(stats.cache_recomputed, stats.cache_invalidated)
            for entry in service.snapshot.path_cache.values():
                self.assertEqual(entry.version, service.version)
                path = entry.path
                if path:
                    width = service.snapshot.grid.width
                    expected_dependencies = path_index_dependencies(
                        path, service.snapshot.grid.height, width
                    )
                    self.assertEqual(entry.dependencies, expected_dependencies)
                weights = service.snapshot.grid.cell_cost(entry.unit_id)
                fresh_path, fresh_cost, fresh_reachable = reconstruct_path(
                    service.snapshot.fields[entry.unit_id],
                    weights,
                    entry.source,
                )
                self.assertEqual(entry.path, fresh_path)
                self.assertEqual(entry.path_hash, path_hash(fresh_path))
                self.assertEqual(entry.cost, fresh_cost)
                self.assertEqual(entry.reachable, fresh_reachable)
                cached = service.get_path(entry.unit_id, entry.source)
                self.assertTrue(cached.cache_hit)
                self.assertEqual(cached.path, fresh_path)
        finally:
            service.close()

    def test_random_multi_cell_batches_use_closure_impact(self) -> None:
        rng = np.random.default_rng(2026)
        height = width = 20
        units = 3
        base = rng.integers(1, 6, size=(height, width), dtype=np.int32)
        mask = np.full((height, width), WORLD_BLOCK_BIT, dtype=np.uint64)
        for unit in range(units):
            allowed = (rng.random((height, width)) < 0.85).astype(np.uint64)
            mask |= allowed * (np.uint64(1) << np.uint64(unit))
        mask[rng.random((height, width)) < 0.05] &= ~WORLD_BLOCK_BIT
        danger = (rng.random((height, width)) < 0.1).astype(np.uint8)
        targets = {unit: (18, 18) for unit in range(units)}
        for unit, cell in targets.items():
            mask[cell] |= WORLD_BLOCK_BIT | (np.uint64(1) << np.uint64(unit))
        from tactical.grid import BattleGrid

        service = TacticalService(
            BattleGrid(base, mask, danger, units, 5),
            targets,
            cache_queries=[
                (unit, (row, 1)) for unit in range(units) for row in (2, 8, 14)
            ],
        )
        try:
            saw_closure_extension = False
            for _ in range(80):
                changes = []
                used: set[tuple[int, int]] = set()
                for _ in range(4):
                    for _attempt in range(100):
                        row = int(rng.integers(height))
                        col = int(rng.integers(width))
                        if (row, col) not in used and (
                            row,
                            col,
                        ) not in targets.values():
                            break
                    used.add((row, col))
                    kind = int(rng.integers(4))
                    current = service.snapshot.grid
                    if kind == 0:
                        changes.append(SetBaseCost(row, col, int(rng.integers(1, 9))))
                    elif kind == 1:
                        changes.append(
                            SetDanger(row, col, not bool(current.danger[row, col]))
                        )
                    elif kind == 2:
                        changes.append(
                            SetObstacle(
                                row,
                                col,
                                not bool(
                                    int(current.pass_mask[row, col])
                                    & int(WORLD_BLOCK_BIT)
                                ),
                            )
                        )
                    else:
                        unit = int(rng.integers(units))
                        changes.append(
                            SetUnitPassability(
                                row,
                                col,
                                unit,
                                not current.is_passable(unit, row, col),
                            )
                        )
                stats = service.commit_batch(changes)
                self.assertTrue(stats.closure_equals_merged)
                self.assertTrue(stats.field_replay_equals_merged)
                self.assertGreaterEqual(
                    stats.merged_impact_size, stats.raw_merged_impact_size
                )
                snapshot = service.snapshot
                for entry in snapshot.path_cache.values():
                    weights = snapshot.grid.cell_cost(entry.unit_id)
                    fresh_path, fresh_cost, fresh_reachable = reconstruct_path(
                        snapshot.fields[entry.unit_id], weights, entry.source
                    )
                    self.assertEqual(entry.path, fresh_path)
                    self.assertEqual(entry.cost, fresh_cost)
                    self.assertEqual(entry.reachable, fresh_reachable)
                saw_closure_extension = (
                    saw_closure_extension or stats.closure_extends_raw
                )
            self.assertTrue(saw_closure_extension)
        finally:
            service.close()


if __name__ == "__main__":
    unittest.main()
