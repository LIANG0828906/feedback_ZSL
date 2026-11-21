from __future__ import annotations

import unittest

from motionreview.solver import PoseSolver
from tests.helpers import full_constraints, sample_document


class SolverTest(unittest.TestCase):
    def test_result_is_independent_of_solver_instance(self) -> None:
        document = sample_document("determinism", frames=18, joints=6)
        constraints = full_constraints(document)
        first = PoseSolver(document, constraints, max_iterations=40)
        second = PoseSolver(document, constraints, max_iterations=40)
        for frame in range(document.frame_count):
            left = first.solve_frame(frame)
            right = second.solve_frame(frame)
            self.assertEqual(left.pose_hash, right.pose_hash)
            self.assertEqual(left.content_hash, right.content_hash)
            self.assertEqual(
                [(r["constraint_id"], r["residual"]) for r in left.residuals],
                [(r["constraint_id"], r["residual"]) for r in right.residuals],
            )

    def test_weight_change_only_resolves_affected_range(self) -> None:
        document = sample_document("cache", frames=18, joints=6)
        constraints = full_constraints(document)
        solver = PoseSolver(
            document, constraints, max_iterations=40, cache_depth=18
        )
        baseline = {
            frame: solver.solve_frame(frame)
            for frame in range(document.frame_count)
        }
        midpoint = document.frame_count // 2
        changed = constraints.with_updates(
            "effector_mid", weight=3.5
        )
        affected = solver.update_constraints(changed)
        self.assertEqual(affected, (midpoint, midpoint + 1))
        batch = solver.solve_interval(0, document.frame_count)
        self.assertEqual(batch.frames_solved, (midpoint,))
        self.assertEqual(
            set(batch.frames_reused),
            set(range(document.frame_count)) - {midpoint},
        )
        for solution in batch.solutions:
            if solution.frame != midpoint:
                self.assertEqual(
                    solution.pose_hash, baseline[solution.frame].pose_hash
                )
                self.assertEqual(
                    solution.content_hash,
                    baseline[solution.frame].content_hash,
                )
        self.assertNotEqual(
            baseline[midpoint].content_hash,
            solver.solve_frame(midpoint).content_hash,
        )

    def test_conflict_report_contains_frame_and_constraint(self) -> None:
        document = sample_document(
            "conflict", frames=16, joints=6
        )
        constraints = full_constraints(document, conflict=True)
        solver = PoseSolver(document, constraints, max_iterations=12)
        frame = document.frame_count // 2
        solution = solver.solve_frame(frame)
        self.assertEqual(solution.status, "CONFLICT")
        self.assertIsNotNone(solution.conflict)
        assert solution.conflict is not None
        self.assertEqual(solution.conflict["frame"], frame)
        self.assertIn(
            "effector_impossible",
            solution.conflict["constraint_ids"],
        )
        self.assertIn("joint", solution.conflict["worst_residual"])
        self.assertGreater(
            solution.conflict["worst_residual"]["residual"], 1.0
        )

    def test_constraint_reduction_order_is_fixed(self) -> None:
        document = sample_document("order", frames=8, joints=5)
        constraints = full_constraints(document)
        solver = PoseSolver(document, constraints, max_iterations=30)
        for frame in range(document.frame_count):
            solution = solver.solve_frame(frame)
            indices = [
                record["constraint_index"]
                for record in solution.residuals
            ]
            self.assertEqual(indices, sorted(indices))
            total = 0.0
            for record in solution.residuals:
                total += record["weighted_residual"]
            self.assertEqual(total, solution.total_residual)


if __name__ == "__main__":
    unittest.main()
