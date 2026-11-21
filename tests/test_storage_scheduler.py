from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from motionreview.budget import estimate_memory, plan_budget
from motionreview.errors import BusyError, CommitFailedError, RecoveryInvalidError
from motionreview.scheduler import ClipTask, Scheduler
from motionreview.solver import PoseSolver
from motionreview.storage import SnapshotStore
from tests.helpers import (
    angle_only_constraints,
    committed_frame_hashes,
    full_constraints,
    run_config,
    sample_document,
)


class StorageSchedulerTest(unittest.TestCase):
    def test_parallel_results_match_serial_results(self) -> None:
        docs = [
            sample_document("a", frames=24, joints=5),
            sample_document("b", frames=24, joints=6),
        ]
        constraints = [full_constraints(doc) for doc in docs]
        tasks = [
            ClipTask(doc.clip_id, doc, constraint)
            for doc, constraint in zip(docs, constraints, strict=True)
        ]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with Scheduler(
                run_config(parallelism=1, checkpoint=6), root / "serial"
            ) as scheduler:
                serial = scheduler.run(tasks)
            with Scheduler(
                run_config(parallelism=2, checkpoint=6), root / "parallel"
            ) as scheduler:
                parallel = scheduler.run(tasks)
            self.assertTrue(serial.all_completed)
            self.assertTrue(parallel.all_completed)
            for task in tasks:
                serial_hashes = committed_frame_hashes(
                    root / "serial", task.clip_id
                )
                parallel_hashes = committed_frame_hashes(
                    root / "parallel", task.clip_id
                )
                self.assertEqual(serial_hashes, parallel_hashes)
                self.assertEqual(
                    len(serial_hashes), task.document.frame_count
                )

    def test_global_quota_waits_without_raising_parallelism(self) -> None:
        first = sample_document("q1", frames=64, joints=6)
        second = sample_document("q2", frames=64, joints=6)
        first_constraints = angle_only_constraints(first)
        second_constraints = angle_only_constraints(second)
        per_task = 10**12
        estimate = estimate_memory(
            joints=len(first.joints),
            frames=first.frame_count,
            channels=first.channel_count,
            iteration_frames=min(first.frame_count, 64),
            batch_frames=min(first.frame_count, 8),
            cache_depth=min(first.frame_count, 64),
        ).total_bytes
        config = run_config(
            parallelism=2,
            per_task=per_task,
            total=estimate + 1,
            checkpoint=8,
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with Scheduler(config, root) as scheduler:
                report = scheduler.run(
                    [
                        ClipTask("q1", first, first_constraints),
                        ClipTask("q2", second, second_constraints),
                    ]
                )
            self.assertTrue(all(
                item.status == "COMPLETED"
                for item in report.reports
            ))
            trace = [
                json.loads(line)
                for line in Path(report.trace_path or "")
                .read_text(encoding="utf-8")
                .splitlines()
            ]
            self.assertTrue(
                any(row["type"] == "wait_queue" for row in trace)
            )
            for clip_id in ("q1", "q2"):
                self.assertEqual(
                    len(committed_frame_hashes(root, clip_id)),
                    64,
                )

    def test_duplicate_start_returns_busy(self) -> None:
        document = sample_document("busy", frames=120, joints=6)
        constraints = angle_only_constraints(document)
        with tempfile.TemporaryDirectory() as temporary:
            with Scheduler(
                run_config(parallelism=1, checkpoint=12),
                Path(temporary),
            ) as scheduler:
                first = scheduler.start_clip(
                    ClipTask("busy", document, constraints)
                )
                with self.assertRaises(BusyError) as caught:
                    scheduler.start_clip(
                        ClipTask("busy", document, constraints)
                    )
                self.assertEqual(caught.exception.code, "E_BUSY")
                first.result()

    def test_budget_degradation_and_rejection_are_isolated(self) -> None:
        cheap = sample_document("cheap", frames=40, joints=6)
        huge = sample_document("huge", frames=1600, joints=14)
        cheap_constraints = angle_only_constraints(cheap)
        huge_constraints = angle_only_constraints(huge)
        cheap_estimate = plan_budget(
            config=run_config(per_task=10**12, total=10**12),
            joints=len(cheap.joints),
            frames=cheap.frame_count,
            channels=cheap.channel_count,
        )
        huge_estimate = plan_budget(
            config=run_config(per_task=10**12, total=10**12),
            joints=len(huge.joints),
            frames=huge.frame_count,
            channels=huge.channel_count,
        )
        self.assertGreater(cheap_estimate.estimate.total_bytes, 0)
        # A quota between full and degraded usage forces a recorded retry.
        full_usage = estimate_memory(
            joints=len(cheap.joints),
            frames=cheap.frame_count,
            channels=cheap.channel_count,
            iteration_frames=min(cheap.frame_count, 64),
            batch_frames=min(cheap.frame_count, 8),
            cache_depth=min(cheap.frame_count, 64),
        ).total_bytes
        stage_one_usage = estimate_memory(
            joints=len(cheap.joints),
            frames=cheap.frame_count,
            channels=cheap.channel_count,
            iteration_frames=min(cheap.frame_count, 32),
            batch_frames=min(cheap.frame_count, 8),
            cache_depth=min(cheap.frame_count, 64),
        ).total_bytes
        constrained = run_config(
            per_task=(full_usage + stage_one_usage) // 2,
            total=10**12,
            checkpoint=8,
        )
        degraded_plan = plan_budget(
            config=constrained,
            joints=len(cheap.joints),
            frames=cheap.frame_count,
            channels=cheap.channel_count,
        )
        self.assertGreaterEqual(degraded_plan.stage_index, 1)
        self.assertTrue(degraded_plan.attempted)

        # Select a quota that accepts cheap and rejects huge. The cheap task
        # may degrade; the huge task must not start or commit.
        pass_quota = max(
            cheap_estimate.estimate.total_bytes,
            32 * 1024,
        ) + 8
        self.assertGreater(
            huge_estimate.estimate.total_bytes, pass_quota
        )
        config = run_config(
            per_task=pass_quota,
            total=pass_quota * 2,
            checkpoint=8,
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with Scheduler(config, root) as scheduler:
                report = scheduler.run(
                    [
                        ClipTask("huge", huge, huge_constraints),
                        ClipTask("cheap", cheap, cheap_constraints),
                    ]
                )
            statuses = {
                item.clip_id: item.status for item in report.reports
            }
            self.assertEqual(statuses["huge"], "REJECTED_BUDGET")
            self.assertEqual(statuses["cheap"], "COMPLETED")
            self.assertEqual(
                committed_frame_hashes(root, "cheap"),
                committed_frame_hashes(root, "cheap"),
            )
            state = SnapshotStore(root, "huge").recover()
            self.assertFalse(state.has_commits)
            trace_path = Path(report.trace_path or "")
            self.assertTrue(trace_path.exists())
            rows = [
                json.loads(line)
                for line in trace_path.read_text(encoding="utf-8").splitlines()
            ]
            budget_rows = [
                row for row in rows if row["type"] == "budget_rejected"
            ]
            self.assertTrue(budget_rows)

    def test_commit_failure_preserves_previous_and_chain_recovers(self) -> None:
        document = sample_document("atomic", frames=24, joints=5)
        constraints = angle_only_constraints(document)
        config = run_config(parallelism=1, checkpoint=6)
        solver = PoseSolver(
            document,
            constraints,
            max_iterations=config.max_iterations,
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            store = SnapshotStore(root, "atomic")
            first = [
                solver.solve_frame(frame) for frame in range(6)
            ]
            payload = store.commit_range(
                first,
                parameter_version=solver.parameter_version,
                document_hash=document.content_hash,
            )
            state = store.recover()
            self.assertEqual(state.next_frame, 6)
            store.fail_next_commit = True
            second = [
                solver.solve_frame(frame) for frame in range(6, 12)
            ]
            with self.assertRaises(CommitFailedError) as caught:
                store.commit_range(
                    second,
                    parameter_version=solver.parameter_version,
                    document_hash=document.content_hash,
                )
            self.assertEqual(caught.exception.code, "E_COMMIT_FAILED")
            state_after = store.recover()
            self.assertEqual(state_after.next_frame, 6)
            self.assertEqual(state_after.head_hash, payload["chain_hash"])
            self.assertEqual(len(state_after.solutions), 6)

            # Corrupt the payload hash: recovery points at the first break.
            path = sorted(store.root.glob("interval_*.json"))[0]
            data = json.loads(path.read_text(encoding="utf-8"))
            data["records"][0]["pose_hash"] = "0" * 64
            path.write_text(json.dumps(data), encoding="utf-8")
            with self.assertRaises(RecoveryInvalidError) as corrupt:
                store.recover()
            self.assertEqual(corrupt.exception.code, "E_RECOVERY_INVALID")
            self.assertEqual(corrupt.exception.frame, 0)
            self.assertIn("breakpoint_file", corrupt.exception.details)

    def test_recovery_reports_first_break_in_second_interval(self) -> None:
        document = sample_document("break2", frames=18, joints=5)
        constraints = angle_only_constraints(document)
        solver = PoseSolver(document, constraints, max_iterations=30)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            store = SnapshotStore(root, "break2")
            for start in (0, 6, 12):
                solutions = [
                    solver.solve_frame(frame)
                    for frame in range(start, start + 6)
                ]
                store.commit_range(
                    solutions,
                    parameter_version=solver.parameter_version,
                    document_hash=document.content_hash,
                )
            second = sorted(store.root.glob("interval_*.json"))[1]
            payload = json.loads(second.read_text(encoding="utf-8"))
            payload["snapshot_version"] = 999
            second.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(RecoveryInvalidError) as caught:
                store.recover()
            self.assertEqual(caught.exception.frame, 6)
            self.assertEqual(
                caught.exception.details["breakpoint_file"],
                str(second),
            )

    def test_degraded_execution_matches_full_frame_hashes(self) -> None:
        document = sample_document("degrade", frames=40, joints=6)
        constraints = angle_only_constraints(document)
        full_config = run_config(
            parallelism=1,
            per_task=10**12,
            total=10**12,
            checkpoint=8,
        )
        full_usage = estimate_memory(
            joints=len(document.joints),
            frames=document.frame_count,
            channels=document.channel_count,
            iteration_frames=min(document.frame_count, 64),
            batch_frames=min(document.frame_count, 8),
            cache_depth=min(document.frame_count, 64),
        ).total_bytes
        stage_usage = estimate_memory(
            joints=len(document.joints),
            frames=document.frame_count,
            channels=document.channel_count,
            iteration_frames=min(document.frame_count, 32),
            batch_frames=min(document.frame_count, 8),
            cache_depth=min(document.frame_count, 64),
        ).total_bytes
        tight_config = run_config(
            parallelism=1,
            per_task=(full_usage + stage_usage) // 2,
            total=10**12,
            checkpoint=8,
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with Scheduler(full_config, root / "full") as scheduler:
                full = scheduler.run(
                    [ClipTask("degrade", document, constraints)]
                )
            with Scheduler(tight_config, root / "tight") as scheduler:
                tight = scheduler.run(
                    [ClipTask("degrade", document, constraints)]
                )
            self.assertEqual(full.reports[0].status, "COMPLETED")
            self.assertEqual(tight.reports[0].status, "COMPLETED")
            self.assertTrue(tight.reports[0].degraded)
            self.assertGreaterEqual(tight.reports[0].stage_index, 1)
            self.assertEqual(
                committed_frame_hashes(root / "full", "degrade"),
                committed_frame_hashes(root / "tight", "degrade"),
            )

    def test_budget_rejection_leaves_existing_commits_untouched(self) -> None:
        document = sample_document("preserve", frames=24, joints=5)
        constraints = angle_only_constraints(document)
        solver = PoseSolver(document, constraints, max_iterations=30)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            store = SnapshotStore(root, "preserve")
            first = [solver.solve_frame(frame) for frame in range(8)]
            store.commit_range(
                first,
                parameter_version=solver.parameter_version,
                document_hash=document.content_hash,
            )
            before = store.recover()
            impossible = run_config(
                parallelism=1,
                per_task=16 * 1024,
                total=16 * 1024,
            )
            with Scheduler(impossible, root) as scheduler:
                report = scheduler.run(
                    [
                        ClipTask(
                            "preserve",
                            document,
                            constraints,
                            resume=True,
                        )
                    ]
                )
            task = report.reports[0]
            self.assertEqual(task.status, "REJECTED_BUDGET")
            self.assertEqual(task.error["code"], "E_BUDGET_EXCEEDED")
            after = store.recover()
            self.assertEqual(before.next_frame, after.next_frame)
            self.assertEqual(before.head_hash, after.head_hash)

    def test_resume_parameter_mismatch_is_recovery_invalid(self) -> None:
        document = sample_document("version", frames=24, joints=5)
        constraints = angle_only_constraints(document)
        solver = PoseSolver(document, constraints, max_iterations=30)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            store = SnapshotStore(root, "version")
            first = [solver.solve_frame(frame) for frame in range(6)]
            store.commit_range(
                first,
                parameter_version=solver.parameter_version,
                document_hash=document.content_hash,
            )
            changed = constraints.with_updates("limit_1", weight=2.0)
            with Scheduler(run_config(parallelism=1, checkpoint=6), root) as scheduler:
                report = scheduler.run(
                    [
                        ClipTask(
                            "version",
                            document,
                            changed,
                            resume=True,
                        )
                    ]
                )
            task = report.reports[0]
            self.assertEqual(task.status, "RECOVERY_INVALID")
            self.assertEqual(task.error["code"], "E_RECOVERY_INVALID")
            state = store.recover()
            self.assertEqual(state.next_frame, 6)

    def test_cancel_at_checkpoint_keeps_last_commit_and_resume_matches(self) -> None:
        document = sample_document("cancel", frames=40, joints=5)
        constraints = angle_only_constraints(document)
        config = run_config(parallelism=1, checkpoint=8)

        def cancel_at_checkpoint(clip_id: str, frame: int) -> None:
            if frame >= 8:
                scheduler.cancel(clip_id)

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with Scheduler(config, root / "full") as baseline_scheduler:
                baseline_scheduler.run(
                    [ClipTask("cancel", document, constraints)]
                )
            full_hashes = committed_frame_hashes(
                root / "full", "cancel"
            )
            with Scheduler(
                config,
                root / "cancelled",
                checkpoint_hook=cancel_at_checkpoint,
            ) as scheduler:
                cancelled = scheduler.run(
                    [ClipTask("cancel", document, constraints)]
                )
            report = cancelled.reports[0]
            self.assertEqual(report.status, "CANCELLED")
            self.assertEqual(report.next_frame, 8)
            state = SnapshotStore(root / "cancelled", "cancel").recover()
            self.assertEqual(state.next_frame, 8)
            with Scheduler(config, root / "cancelled") as scheduler:
                resumed = scheduler.run(
                    [
                        ClipTask(
                            "cancel",
                            document,
                            constraints,
                            resume=True,
                        )
                    ]
                )
            self.assertEqual(resumed.reports[0].status, "COMPLETED")
            self.assertEqual(
                committed_frame_hashes(root / "cancelled", "cancel"),
                full_hashes,
            )

    def test_resume_uses_committed_frames_and_matches_full_run(self) -> None:
        document = sample_document("resume", frames=32, joints=5)
        constraints = angle_only_constraints(document)
        config = run_config(parallelism=1, checkpoint=8)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with Scheduler(config, root / "full") as scheduler:
                full = scheduler.run(
                    [ClipTask("resume", document, constraints)]
                )
            self.assertEqual(full.reports[0].status, "COMPLETED")
            full_hashes = committed_frame_hashes(
                root / "full", "resume"
            )

            partial_solver = PoseSolver(
                document,
                constraints,
                max_iterations=config.max_iterations,
            )
            partial_store = SnapshotStore(root / "partial", "resume")
            partial = [
                partial_solver.solve_frame(frame)
                for frame in range(12)
            ]
            partial_store.commit_range(
                partial,
                parameter_version=partial_solver.parameter_version,
                document_hash=document.content_hash,
            )
            with Scheduler(config, root / "partial") as scheduler:
                resumed = scheduler.run(
                    [ClipTask("resume", document, constraints, resume=True)]
                )
            report = resumed.reports[0]
            self.assertEqual(report.status, "COMPLETED")
            self.assertEqual(report.start_frame, 12)
            self.assertEqual(report.frames_reused, 12)
            self.assertEqual(report.frames_solved, 20)
            self.assertEqual(
                committed_frame_hashes(root / "partial", "resume"),
                full_hashes,
            )

    def test_conflict_stops_later_frames_but_other_clip_commits(self) -> None:
        conflict_doc = sample_document(
            "bad", frames=32, joints=6, conflict=True
        )
        good_doc = sample_document("good", frames=32, joints=6)
        conflict_constraints = full_constraints(
            conflict_doc, conflict=True
        )
        good_constraints = full_constraints(good_doc)
        conflict_frame = conflict_doc.frame_count // 2
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config = run_config(parallelism=2, checkpoint=8)
            with Scheduler(config, root) as scheduler:
                report = scheduler.run(
                    [
                        ClipTask(
                            "bad", conflict_doc, conflict_constraints
                        ),
                        ClipTask("good", good_doc, good_constraints),
                    ]
                )
            statuses = {
                item.clip_id: item.status for item in report.reports
            }
            self.assertEqual(statuses["bad"], "CONFLICT_STOPPED")
            self.assertEqual(statuses["good"], "COMPLETED")
            bad = SnapshotStore(root, "bad").recover()
            self.assertEqual(bad.next_frame, conflict_frame)
            self.assertEqual(len(bad.solutions), conflict_frame)
            diagnostics = SnapshotStore(root, "bad").list_diagnostics()
            self.assertTrue(diagnostics)
            diagnostic = json.loads(
                diagnostics[0].read_text(encoding="utf-8")
            )
            self.assertEqual(diagnostic["frame"], conflict_frame)
            self.assertIn(
                "effector_impossible", diagnostic["constraint_ids"]
            )
            good = SnapshotStore(root, "good").recover()
            self.assertEqual(good.next_frame, good_doc.frame_count)


if __name__ == "__main__":
    unittest.main()
