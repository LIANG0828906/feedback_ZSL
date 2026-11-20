from __future__ import annotations

import unittest

import numpy as np

from tactical.costfield import (
    CostField,
    compute_full_field,
    reconstruct_path,
)
from tactical.grid import INF


class CostFieldTests(unittest.TestCase):
    def test_reverse_field_matches_bellman_fixed_point(self) -> None:
        weights = np.array(
            [
                [1, 2, 1, 4, 3],
                [2, INF, 3, 1, 2],
                [1, 3, 1, 2, 1],
                [4, 1, 2, INF, 2],
            ],
            dtype=np.int64,
        )
        target = (2, 4)
        field = compute_full_field(weights, target)
        reference = np.full(weights.shape, INF, dtype=np.int64)
        reference[target] = 0
        changed = True
        while changed:
            changed = False
            for row in range(weights.shape[0]):
                for col in range(weights.shape[1]):
                    if (row, col) == target or weights[row, col] == INF:
                        continue
                    best = int(INF)
                    for delta_row, delta_col in ((-1, 0), (0, -1), (0, 1), (1, 0)):
                        next_row, next_col = row + delta_row, col + delta_col
                        if (
                            0 <= next_row < weights.shape[0]
                            and 0 <= next_col < weights.shape[1]
                        ):
                            best = min(best, int(reference[next_row, next_col]))
                    candidate = (
                        int(INF) if best == int(INF) else best + int(weights[row, col])
                    )
                    if candidate < int(reference[row, col]):
                        reference[row, col] = np.int64(candidate)
                        changed = True
        np.testing.assert_array_equal(field, reference)

    def test_field_hash_and_path_are_deterministic(self) -> None:
        weights = np.array(
            [
                [1, 1, 1, 1],
                [1, 8, 8, 1],
                [1, 1, 1, 1],
            ],
            dtype=np.int64,
        )
        target = (1, 3)
        first = CostField.create(0, target, compute_full_field(weights, target))
        second = CostField.create(0, target, compute_full_field(weights, target))
        self.assertEqual(first.field_hash, second.field_hash)
        path_a, cost_a, reachable_a = reconstruct_path(first, weights, (0, 0))
        path_b, cost_b, reachable_b = reconstruct_path(second, weights, (0, 0))
        self.assertTrue(reachable_a and reachable_b)
        self.assertEqual(path_a, path_b)
        self.assertEqual(cost_a, cost_b)
        self.assertEqual(path_a[0], (0, 0))
        self.assertEqual(path_a[-1], target)

    def test_blocked_target_is_rejected(self) -> None:
        weights = np.array([[1, 1], [INF, 1]], dtype=np.int64)
        with self.assertRaises(ValueError):
            compute_full_field(weights, (1, 0))


if __name__ == "__main__":
    unittest.main()
