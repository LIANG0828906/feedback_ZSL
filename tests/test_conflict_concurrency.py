from __future__ import annotations

import json
import tempfile
import threading
import time
import unittest
from pathlib import Path

from motion_workbench.errors import E_BUSY, WorkbenchError
from motion_workbench.generator import SampleSpec
from motion_workbench.session import WorkbenchSession
from motion_workbench.store import SolveLock

from .helpers import (
    read_constraint_document,
    write_constraint_document,
    write_sample,
)


class ConflictTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)

    def tearDown(self):
        self.temporary.cleanup()

    def test_conflict_report_and_barrier_prevent_bad_commits(self):
        bvh_path, constraints_path = write_sample(
            self.root,
            SampleSpec(frames=8, joints=6, conflict="none", seed=41),
        )
        document = read_constraint_document(constraints_path)
        for constraint in document["constraints"]:
            if constraint["id"] == "ee:effector":
                constraint["targets"][3][1] = [40.0, 40.0, 40.0]
        edited_path = self.root / "conflict.json"
        write_constraint_document(edited_path, document)
        session = WorkbenchSession(
            bvh_path, edited_path, self.root / "conflict-session"
        )
        report = session.solve(run_id="conflict", chunk_size=4)
        self.assertEqual(report.status, "conflict")
        self.assertEqual(report.state.committed_ranges, ((0, 2),))
        self.assertTrue(all(frame < 3 for frame in report.state.frame_map))
        diagnostic = report.conflict["diagnostic"]
        self.assertEqual(diagnostic["frame"], 3)
        self.assertTrue(diagnostic["constraints"])
        self.assertIn("constraint_id", diagnostic["worst"])
        self.assertIn("evidence", diagnostic["worst"])
        report_path = (
            self.root
            / "conflict-session"
            / report.conflict["report"]["report_file"]
        )
        report_payload = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertEqual(report_payload["frame"], 3)
        self.assertIn("report_hash", report_payload)
        self.assertIn("worst", report_payload)
        self.assertEqual(report_payload["constraints"], diagnostic["constraints"])
        manifest = json.loads(
            (self.root / "conflict-session" / "manifest.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertEqual(manifest["barriers"][0]["frame"], 3)

    def test_unreachable_effector_conflicts_on_first_frame(self):
        bvh_path, constraints_path = write_sample(
            self.root,
            SampleSpec(frames=5, joints=5, conflict="ee_unreachable", seed=42),
        )
        session = WorkbenchSession(
            bvh_path, constraints_path, self.root / "unreachable"
        )
        report = session.solve(run_id="bad")
        self.assertEqual(report.status, "conflict")
        self.assertEqual(report.state.frame_map, {})
        self.assertEqual(report.conflict["diagnostic"]["frame"], 0)
        self.assertEqual(
            report.conflict["diagnostic"]["max_iterations"],
            session.constraints.solve.max_iterations,
        )


class ConcurrencyTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)

    def tearDown(self):
        self.temporary.cleanup()

    def test_lock_returns_busy_and_recovers_after_release(self):
        lock_path = self.root / "lock-root"
        first = SolveLock(lock_path).acquire()
        try:
            with self.assertRaises(WorkbenchError) as context:
                SolveLock(lock_path).acquire()
            self.assertEqual(context.exception.code, E_BUSY)
        finally:
            first.release()
        second = SolveLock(lock_path).acquire()
        second.release()

    def test_repeated_solve_task_is_busy(self):
        bvh_path, constraints_path = write_sample(
            self.root,
            SampleSpec(frames=16, joints=7, conflict="none", seed=43),
        )
        session_dir = self.root / "busy-session"
        session = WorkbenchSession(bvh_path, constraints_path, session_dir)
        cancel = threading.Event()
        pause = threading.Event()
        pause.set()
        started = threading.Event()
        errors: list[Exception] = []

        def progress(event):
            if event.get("event") == "impact_plan":
                started.set()

        def run_first():
            try:
                session.solve(
                    run_id="paused",
                    cancel_event=cancel,
                    pause_event=pause,
                    progress_callback=progress,
                )
            except Exception as exc:
                errors.append(exc)

        worker = threading.Thread(target=run_first)
        worker.start()
        self.assertTrue(started.wait(5))
        second = WorkbenchSession(
            bvh_path, constraints_path, self.root / "busy-session-other"
        )
        with self.assertRaises(WorkbenchError) as context:
            second.solve(run_id="busy")
        self.assertEqual(context.exception.code, E_BUSY)
        cancel.set()
        pause.clear()
        worker.join(timeout=20)
        self.assertFalse(worker.is_alive())
        self.assertEqual(errors, [])

    def test_cancellation_keeps_last_committed_interval(self):
        bvh_path, constraints_path = write_sample(
            self.root,
            SampleSpec(frames=12, joints=6, conflict="none", seed=44),
        )
        session = WorkbenchSession(
            bvh_path, constraints_path, self.root / "cancel-session"
        )
        cancel = threading.Event()

        def progress(event):
            if (
                event.get("event") == "progress"
                and event.get("last_committed_frame") == 2
            ):
                cancel.set()

        report = session.solve(
            run_id="cancel",
            cancel_event=cancel,
            chunk_size=3,
            progress_callback=progress,
        )
        self.assertEqual(report.status, "cancelled")
        self.assertEqual(report.state.committed_ranges, ((0, 2),))
        self.assertEqual(report.state.next_frame, 3)
        state = session.store.load_committed(
            session.bvh, constraints=session.constraints
        )
        self.assertEqual(state.chain_head, report.state.chain_head)


if __name__ == "__main__":
    unittest.main()
