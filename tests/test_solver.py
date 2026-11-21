from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from a11y_rhythm.compliance import build_compliance_report
from a11y_rhythm.errors import E_DESC_CONFLICT, RhythmError
from a11y_rhythm.solver import RhythmSolver

from tests.helpers import conflict_fixture, item, load_fixture, write_fixture


class SolverTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def solve_fixture(self, fixture):
        bundle, frame_table, coverage, _ = load_fixture(fixture)
        solver = RhythmSolver(bundle, frame_table, coverage)
        return bundle, solver.solve(version=1)

    def test_description_preempts_and_chain_stabilizes(self) -> None:
        fixture = conflict_fixture(self.root)
        _, result = self.solve_fixture(fixture)
        self.assertTrue(result.stable)
        self.assertGreater(len(result.conflicts), 1)
        by_id = result.by_id()
        self.assertEqual(by_id["ds001"].status, "active")
        self.assertIn(by_id["ds001"].decision, {"initial", "shifted"})
        for sentence in result.items:
            if sentence.status == "active":
                self.assertTrue(sentence.readable)
        descriptions = [item for item in result.items if item.kind == "description"]
        speeches = [item for item in result.items if item.kind == "speech"]
        for desc in descriptions:
            for speech in speeches:
                self.assertFalse(max(desc.start, speech.start) < min(desc.end, speech.end))

    def test_lower_priority_description_is_rejected_with_evidence(self) -> None:
        fixture = conflict_fixture(self.root, desc_priority=10)
        bundle, result = self.solve_fixture(fixture)
        desc = result.by_id()["ds001"]
        self.assertEqual(desc.status, "rejected")
        self.assertFalse(desc.readable)
        self.assertTrue(desc.depends_on)
        report = build_compliance_report(bundle, result)
        self.assertFalse(report.compliant)
        self.assertTrue(any(item["type"] == "description_rejected" for item in report.violations))

    def test_unreadable_sentence_is_marked_not_silently_accepted(self) -> None:
        fixture = write_fixture(
            self.root,
            name="unreadable",
            duration_seconds=4.0,
            speech=[
                item(
                    "sp001",
                    0.2,
                    0.5,
                    "This sentence is far too long for its tiny display window and must be unreadable.",
                    max_cps=10,
                )
            ],
        )
        _, result = self.solve_fixture(fixture)
        sentence = result.items[0]
        self.assertFalse(sentence.readable)
        self.assertEqual(sentence.decision, "unreadable")
        self.assertGreater(sentence.minimum_duration, sentence.duration)

    def test_iteration_limit_returns_conflict_chain_evidence(self) -> None:
        fixture = conflict_fixture(self.root, max_iterations=0)
        bundle, frame_table, coverage, _ = load_fixture(fixture)
        solver = RhythmSolver(bundle, frame_table, coverage)
        with self.assertRaises(RhythmError) as caught:
            solver.solve(version=1)
        self.assertEqual(caught.exception.code, E_DESC_CONFLICT)
        self.assertIn("conflict_chain", caught.exception.evidence)
        self.assertGreaterEqual(len(caught.exception.evidence["conflict_chain"]), 2)
        self.assertEqual(caught.exception.evidence["max_iterations"], 0)
        self.assertIn("unresolved", caught.exception.evidence)
        self.assertIn("checkpoint", caught.exception.evidence)

    def test_solver_is_deterministic(self) -> None:
        fixture = conflict_fixture(self.root)
        bundle, frames, coverage, _ = load_fixture(fixture)
        result_a = RhythmSolver(bundle, frames, coverage).solve(version=1)
        solver_b = RhythmSolver(bundle, frames, coverage)
        result_b = solver_b.solve(version=1)
        self.assertEqual(result_a.result_hash, result_b.result_hash)
        self.assertEqual(
            [record.to_dict() for record in result_a.conflicts],
            [record.to_dict() for record in result_b.conflicts],
        )


if __name__ == "__main__":
    unittest.main()
