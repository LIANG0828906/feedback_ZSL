from __future__ import annotations

import shutil
import tempfile
import unittest
from pathlib import Path

from motion_workbench.constraints import load_constraints
from motion_workbench.generator import SampleSpec
from motion_workbench.session import WorkbenchSession

from .helpers import (
    read_constraint_document,
    write_constraint_document,
    write_sample,
)


class IncrementalSolveTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.bvh_path, self.constraints_path = write_sample(
            self.root,
            SampleSpec(frames=12, joints=6, conflict="none", seed=21),
        )

    def tearDown(self):
        self.temporary.cleanup()

    def _fresh_full(self, constraints_path: Path, name: str):
        fresh_dir = self.root / name
        session = WorkbenchSession(
            self.bvh_path, constraints_path, fresh_dir
        )
        return session.solve(run_id=f"fresh:{name}", chunk_size=5)

    def test_effector_target_change_recomputes_only_its_frame(self):
        session_dir = self.root / "incremental"
        session = WorkbenchSession(
            self.bvh_path, self.constraints_path, session_dir
        )
        first = session.solve(run_id="baseline", chunk_size=5)
        self.assertEqual(first.status, "complete")
        document = read_constraint_document(self.constraints_path)
        for constraint in document["constraints"]:
            if constraint["id"] == "ee:effector":
                target = constraint["targets"][5][1]
                constraint["targets"][5][1] = [
                    target[0] + 0.006,
                    target[1],
                    target[2],
                ]
        edited_path = self.root / "edited-effector.json"
        write_constraint_document(edited_path, document)
        edited = WorkbenchSession(self.bvh_path, edited_path, session_dir)
        report = edited.solve(run_id="incremental", chunk_size=5)
        self.assertEqual(report.plan.frames, ((5, 5),))
        self.assertEqual(report.computed_frames, (5,))
        self.assertEqual(len(report.cached_frames), 11)
        self.assertNotIn(5, report.cached_frames)
        full = self._fresh_full(edited_path, "fresh-effector")
        for frame in range(12):
            cached_or_new = report.state.frame_solution(frame)
            full_frame = full.state.frame_solution(frame)
            self.assertEqual(
                cached_or_new.frame_hash, full_frame.frame_hash, frame
            )
            self.assertEqual(
                cached_or_new.residuals, full_frame.residuals, frame
            )
        trace = edited.store.read_trace()
        frame_events = [
            event
            for event in trace
            if event.get("event") == "frame" and event.get("frame") == 5
        ]
        self.assertTrue(frame_events)
        self.assertEqual(frame_events[-1]["recomputed_ranges"], [[5, 5]])
        for field in (
            "residual",
            "conflict_status",
            "cache_hit",
            "content_hash",
            "frame",
        ):
            self.assertIn(field, frame_events[-1])
        self.assertFalse(frame_events[-1]["cache_hit"])
        self.assertTrue(
            any(event.get("event") == "interval_committed" for event in trace)
        )

    def test_scoped_joint_weight_change_recomputes_scope_only(self):
        document = read_constraint_document(self.constraints_path)
        for constraint in document["constraints"]:
            if constraint["id"] == "limit:joint_02":
                constraint["frames"] = [[3, 7]]
        scoped_path = self.root / "scoped.json"
        write_constraint_document(scoped_path, document)
        session_dir = self.root / "scoped-session"
        session = WorkbenchSession(self.bvh_path, scoped_path, session_dir)
        self.assertEqual(session.solve(run_id="scoped-base").status, "complete")
        for constraint in document["constraints"]:
            if constraint["id"] == "limit:joint_02":
                constraint["weight"] = 0.85
                constraint["tolerance"] = 0.08
        edited_path = self.root / "scoped-edited.json"
        write_constraint_document(edited_path, document)
        session.set_constraints(load_constraints(edited_path, session.bvh))
        report = session.solve(run_id="scoped-incremental")
        self.assertEqual(report.plan.frames, ((3, 7),))
        self.assertEqual(report.computed_frames, (3, 4, 5, 6, 7))
        full = self._fresh_full(edited_path, "fresh-scoped")
        for frame in range(12):
            self.assertEqual(
                report.state.frame_solution(frame).frame_hash,
                full.state.frame_solution(frame).frame_hash,
                frame,
            )

    def test_global_solver_change_is_explicitly_conservative(self):
        session_dir = self.root / "global-session"
        session = WorkbenchSession(
            self.bvh_path, self.constraints_path, session_dir
        )
        session.solve(run_id="base")
        document = read_constraint_document(self.constraints_path)
        document["solver"]["max_iterations"] = 40
        edited_path = self.root / "global-edited.json"
        write_constraint_document(edited_path, document)
        session.set_constraints(load_constraints(edited_path, session.bvh))
        report = session.solve(run_id="global")
        self.assertEqual(report.plan.frames, ((0, 11),))
        self.assertTrue(report.plan.conservative)
        self.assertIn(
            "global_solver_parameters_changed", report.plan.reasons
        )
        self.assertEqual(len(report.computed_frames), 12)

    def test_metadata_only_change_reuses_every_frame(self):
        session_dir = self.root / "metadata-session"
        session = WorkbenchSession(
            self.bvh_path, self.constraints_path, session_dir
        )
        session.solve(run_id="base")
        changed = session.constraints.with_constraint(
            session.constraints.by_id["ee:effector"].with_changes(
                id="ee:effector"
            )
        )
        # Give the set a new metadata name without changing any solve dependency.
        from motion_workbench.constraints import ConstraintSet

        named = ConstraintSet(
            constraints=changed.constraints,
            solve=changed.solve,
            name="renamed-metadata-only",
            schema=changed.schema,
        )
        session.set_constraints(named)
        report = session.solve(run_id="metadata")
        self.assertEqual(report.computed_frames, ())
        self.assertEqual(len(report.cached_frames), 12)
        self.assertEqual(report.status, "complete")


if __name__ == "__main__":
    unittest.main()
