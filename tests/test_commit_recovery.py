from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from motion_workbench.errors import (
    E_COMMIT_FAILED,
    E_RECOVERY_INVALID,
    WorkbenchError,
)
from motion_workbench.generator import SampleSpec
from motion_workbench.session import WorkbenchSession, resume_session

from .helpers import write_sample


class CommitRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.bvh_path, self.constraints_path = write_sample(
            self.root,
            SampleSpec(frames=12, joints=6, conflict="none", seed=31),
        )

    def tearDown(self):
        self.temporary.cleanup()

    def test_commit_failure_preserves_previous_manifest(self):
        session_dir = self.root / "fault"

        def fault(stage, path, payload):
            if (
                stage == "manifest"
                and payload
                and payload.get("commits")
            ):
                raise RuntimeError("injected manifest failure")

        session = WorkbenchSession(
            self.bvh_path,
            self.constraints_path,
            session_dir,
            fault_hook=fault,
        )
        with self.assertRaises(WorkbenchError) as context:
            session.solve(chunk_size=3)
        self.assertEqual(context.exception.code, E_COMMIT_FAILED)
        manifest = json.loads(
            (session_dir / "manifest.json").read_text(encoding="utf-8")
        )
        self.assertEqual(manifest["commits"], [])
        self.assertEqual(manifest["chain_head"], "GENESIS")
        state = session.store.load_committed(
            session.bvh, constraints=session.constraints
        )
        self.assertEqual(state.frame_map, {})
        session.store.fault_hook = None
        recovered = session.solve(chunk_size=3)
        self.assertEqual(recovered.status, "complete")

    def test_failed_rewrite_does_not_alter_existing_interval(self):
        session_dir = self.root / "existing-interval"
        base = WorkbenchSession(
            self.bvh_path, self.constraints_path, session_dir
        )
        base.solve(chunk_size=4)
        manifest_before = (session_dir / "manifest.json").read_bytes()
        state_before = base.store.load_committed(
            base.bvh, constraints=base.constraints
        )
        old_interval_path = session_dir / state_before.frame_map[6].file
        old_interval_bytes = old_interval_path.read_bytes()
        old_commit_count = len(state_before.manifest["commits"])

        document = json.loads(
            self.constraints_path.read_text(encoding="utf-8")
        )
        for constraint in document["constraints"]:
            if constraint["id"] == "ee:effector":
                target = constraint["targets"][6][1]
                constraint["targets"][6][1] = [
                    target[0] + 0.004,
                    target[1],
                    target[2],
                ]
        edited_path = self.root / "edited-during-fault.json"
        edited_path.write_text(
            json.dumps(document, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )

        def fault(stage, path, payload):
            if (
                stage == "manifest"
                and payload
                and len(payload.get("commits", [])) > old_commit_count
            ):
                raise RuntimeError("injected rewrite failure")

        changed = WorkbenchSession(
            self.bvh_path,
            edited_path,
            session_dir,
            fault_hook=fault,
        )
        with self.assertRaises(WorkbenchError) as context:
            changed.solve(chunk_size=4)
        self.assertEqual(context.exception.code, E_COMMIT_FAILED)
        manifest_after = json.loads(
            (session_dir / "manifest.json").read_text(encoding="utf-8")
        )
        before = json.loads(manifest_before)
        self.assertEqual(
            manifest_after["commits"], before["commits"]
        )
        self.assertEqual(
            manifest_after["chain_head"], before["chain_head"]
        )
        self.assertEqual(old_interval_path.read_bytes(), old_interval_bytes)
        state_after = base.store.load_committed(
            base.bvh, constraints=base.constraints
        )
        self.assertEqual(state_after.chain_head, state_before.chain_head)
        self.assertEqual(state_after.committed_ranges, ((0, 11),))

    def test_tampered_interval_is_rejected_at_first_broken_frame(self):
        session_dir = self.root / "tampered-interval"
        session = WorkbenchSession(
            self.bvh_path, self.constraints_path, session_dir
        )
        session.solve(chunk_size=4)
        state = session.store.load_committed(
            session.bvh, constraints=session.constraints
        )
        interval = state.frame_map[0]
        path = session_dir / interval.file
        payload = json.loads(path.read_text(encoding="utf-8"))
        payload["frames"][0]["pose"]["scales"][0] += 0.000001
        path.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(WorkbenchError) as context:
            session.store.load_committed(
                session.bvh, constraints=session.constraints
            )
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(context.exception.details["first_break_frame"], 0)

    def test_hash_chain_break_reports_interval_start(self):
        session_dir = self.root / "tampered-chain"
        session = WorkbenchSession(
            self.bvh_path, self.constraints_path, session_dir
        )
        session.solve(chunk_size=3)
        manifest_path = session_dir / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertGreaterEqual(len(manifest["commits"]), 2)
        broken_start = manifest["commits"][1]["start"]
        manifest["commits"][1]["previous_hash"] = "broken"
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
        with self.assertRaises(WorkbenchError) as context:
            session.store.load_committed(
                session.bvh, constraints=session.constraints
            )
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(
            context.exception.details["first_break_frame"], broken_start
        )

    def test_snapshot_version_mismatch_is_recovery_invalid(self):
        session_dir = self.root / "snapshot-mismatch"
        session = WorkbenchSession(
            self.bvh_path, self.constraints_path, session_dir
        )
        session.solve(chunk_size=4)
        with self.assertRaises(WorkbenchError) as context:
            session.store.load_committed(
                session.bvh,
                constraints=session.constraints,
                expected_table_version=999,
            )
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(context.exception.details["first_break_frame"], 0)
        with self.assertRaises(WorkbenchError) as context:
            session.store.load_committed(
                session.bvh,
                constraints=session.constraints,
                expected_parameters_version="incorrect",
            )
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)

    def test_parameter_snapshot_tamper_is_recovery_invalid(self):
        session_dir = self.root / "tampered-parameters"
        session = WorkbenchSession(
            self.bvh_path, self.constraints_path, session_dir
        )
        session.solve(chunk_size=4)
        manifest = json.loads(
            (session_dir / "manifest.json").read_text(encoding="utf-8")
        )
        parameter_path = session_dir / manifest["parameters_file"]
        payload = json.loads(parameter_path.read_text(encoding="utf-8"))
        payload["constraints"]["name"] = "tampered"
        parameter_path.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(WorkbenchError) as context:
            session.store.load_committed(
                session.bvh, constraints=session.constraints
            )
        self.assertEqual(context.exception.code, E_RECOVERY_INVALID)

    def test_hard_kill_and_disk_only_resume(self):
        session_dir = self.root / "hard-kill"
        command = [
            sys.executable,
            "-m",
            "motion_workbench.cli",
            "solve",
            "--bvh",
            str(self.bvh_path),
            "--constraints",
            str(self.constraints_path),
            "--session",
            str(session_dir),
            "--chunk-size",
            "3",
            "--crash-after-commits",
            "2",
        ]
        process = subprocess.run(
            command,
            cwd=str(Path(__file__).resolve().parents[1]),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=60,
        )
        self.assertEqual(process.returncode, 137, process.stderr)
        resumed = resume_session(session_dir)
        pre = resumed.store.load_committed(
            resumed.bvh, constraints=resumed.constraints
        )
        self.assertEqual(pre.next_frame, 6)
        self.assertEqual(pre.committed_ranges, ((0, 5),))
        report = resumed.solve(run_id="resume-after-kill", chunk_size=4)
        self.assertEqual(report.computed_frames, tuple(range(6, 12)))
        self.assertEqual(report.cached_frames, tuple(range(0, 6)))
        self.assertEqual(report.status, "complete")

        fresh_dir = self.root / "fresh-after-kill"
        fresh = WorkbenchSession(
            self.bvh_path, self.constraints_path, fresh_dir
        )
        full = fresh.solve(run_id="fresh")
        for frame in range(12):
            self.assertEqual(
                report.state.frame_solution(frame).frame_hash,
                full.state.frame_solution(frame).frame_hash,
                frame,
            )
        verified = resumed.store.load_committed(
            resumed.bvh, constraints=resumed.constraints
        )
        self.assertEqual(verified.chain_head, report.state.chain_head)
        self.assertEqual(verified.missing_ranges, ())


if __name__ == "__main__":
    unittest.main()
