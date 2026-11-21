from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from a11y_rhythm.solver import RhythmSolver

from tests.helpers import incremental_fixture, load_fixture


class IncrementalTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.fixture = incremental_fixture(Path(self.temp.name))
        self.bundle, self.frames, self.coverage, _ = load_fixture(self.fixture)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_only_changed_conflict_chain_is_recomputed(self) -> None:
        initial_solver = RhythmSolver(self.bundle, self.frames, self.coverage)
        initial = initial_solver.solve(version=1)
        changed = []
        for source in self.bundle.items:
            if source.item_id == "ds001":
                from dataclasses import replace

                changed.append(
                    replace(
                        source,
                        text="A much longer red signal lamp blinks twice across the rail yard.",
                    )
                )
            else:
                changed.append(source)
        edited_bundle = self.bundle.__class__(
            material_id=self.bundle.material_id,
            sample_rate=self.bundle.sample_rate,
            frame_table_path=self.bundle.frame_table_path,
            wav_path=self.bundle.wav_path,
            items=tuple(changed),
            params_version=self.bundle.params_version,
            max_iterations=self.bundle.max_iterations,
        )
        solver = RhythmSolver(edited_bundle, self.frames, self.coverage)
        updated = solver.solve(
            version=2,
            changed_id="ds001",
            base_results=initial.by_id(),
        )
        self.assertLess(len(updated.affected_ids), len(updated.items))
        self.assertIn("ds001", updated.affected_ids)
        self.assertIn("sp001", updated.reused_ids)
        base_by_id = initial.by_id()
        for item_id in updated.reused_ids:
            self.assertEqual(
                base_by_id[item_id].result_hash,
                updated.by_id()[item_id].result_hash,
            )
        self.assertTrue(any("conservatively reused" in reason for reason in updated.conservative_reasons.values()))

    def test_missing_baseline_marks_conservative_full_solve(self) -> None:
        solver = RhythmSolver(self.bundle, self.frames, self.coverage)
        result = solver.solve(version=1, changed_id="ds001", base_results=None)
        self.assertEqual(len(result.affected_ids), len(result.items))
        self.assertIn("*", result.conservative_reasons)


if __name__ == "__main__":
    unittest.main()
