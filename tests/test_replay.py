"""End-to-end tests for the deterministic replay service."""

from __future__ import annotations

import json
import os
import shutil
import tempfile
import unittest

from replay_service import inject as inject_mod
from replay_service.baseline import compare
from replay_service.buffer import OpBuffer
from replay_service.errors import (BaselineDivergedError,
                                   BaselineMissingError,
                                   BaselineVersionError, BusyError,
                                   CancelledError, HardCrash,
                                   RecoveryInvalidError,
                                   SnapshotWriteError)
from replay_service.inject import OpInjector
from replay_service.model import Operation
from replay_service.runner import AdvanceTask
from replay_service.samplegen import generate_sample, write_baseline
from replay_service.snapshot import SnapshotStore


class WorkspaceMixin(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="replay_test_")
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def make_sample(self, seed=7, players=4, steps=40, disorder=0.3,
                    name="sample"):
        out = os.path.join(self.tmp, name)
        return generate_sample(seed, players, steps, disorder, out)

    def room(self, name):
        return os.path.join(self.tmp, name)

    @staticmethod
    def read_file(path):
        with open(path, "rb") as fh:
            return fh.read()


class TestDeterminism(WorkspaceMixin):
    def test_same_seed_byte_identical_trace_and_hash(self):
        sample = self.make_sample()
        traces = []
        for name in ("room_a", "room_b"):
            task = AdvanceTask(self.room(name), sample)
            task.run()
            traces.append(self.read_file(task.trace_path))
        self.assertEqual(traces[0], traces[1])

    def test_sample_generator_deterministic(self):
        first = self.read_file(self.make_sample(name="s1"))
        second = self.read_file(self.make_sample(name="s2"))
        self.assertEqual(first, second)

    def test_varied_shapes_generate(self):
        for players, steps, disorder in ((1, 10, 0.0), (3, 30, 0.5),
                                         (8, 60, 0.9)):
            sample = self.make_sample(players=players, steps=steps,
                                      disorder=disorder,
                                      name=f"v{players}_{steps}")
            task = AdvanceTask(self.room(f"rv{players}_{steps}"), sample)
            summary = task.run()
            self.assertEqual(summary["status"], "completed")


class TestBufferRules(WorkspaceMixin):
    def test_duplicate_keeps_first_and_reports(self):
        buf = OpBuffer(late_window=2)
        events = []
        op = Operation("p0", 1, 1, "move", {"dx": 1, "dy": 0}, arrival=0)
        self.assertTrue(buf.offer(op, 1, events))
        self.assertFalse(buf.offer(op, 1, events))
        self.assertEqual(events[-1]["code"], "E_OP_DUPLICATE")
        self.assertEqual(len(buf.drain(1)), 1)

    def test_late_op_rejected(self):
        buf = OpBuffer(late_window=2)
        events = []
        op = Operation("p0", 1, 1, "move", {}, arrival=0)
        self.assertFalse(buf.offer(op, 10, events))
        self.assertEqual(events[-1]["code"], "E_OP_LATE")

    def test_stable_order_within_step(self):
        buf = OpBuffer(late_window=5)
        events = []
        ops = [Operation("p1", 1, 3, "move", {}, arrival=0),
               Operation("p0", 1, 3, "move", {}, arrival=1),
               Operation("p0", 2, 2, "move", {}, arrival=2)]
        for op in ops:
            buf.offer(op, 3, events)
        drained = buf.drain(3)
        self.assertEqual([(o.tick, o.arrival) for o in drained],
                         [(2, 2), (3, 0), (3, 1)])


class TestSnapshotAndRecovery(WorkspaceMixin):
    def test_crash_resume_matches_uninterrupted(self):
        sample = self.make_sample(steps=50)
        ref = AdvanceTask(self.room("ref"), sample)
        ref.run()
        crashed = AdvanceTask(self.room("crashed"), sample)
        with self.assertRaises(HardCrash):
            crashed.run(crash_at=23)
        resumed = AdvanceTask(self.room("crashed"), sample)
        resumed.run(resume=True)
        self.assertEqual(self.read_file(ref.trace_path),
                         self.read_file(resumed.trace_path))

    def test_truncated_snapshot_rejected(self):
        sample = self.make_sample()
        task = AdvanceTask(self.room("r"), sample)
        task.run()
        inject_mod.truncate_snapshot(task.store.path)
        store = SnapshotStore(self.room("r"))
        with self.assertRaises(RecoveryInvalidError) as ctx:
            store.load()
        self.assertEqual(ctx.exception.context["reason"], "truncated")

    def test_broken_chain_locates_first_step(self):
        sample = self.make_sample()
        task = AdvanceTask(self.room("r"), sample)
        task.run()
        with open(task.store.path, "r", encoding="utf-8") as fh:
            doc = json.load(fh)
        doc["chain"][9]["state_hash"] = "0" * 64
        with open(task.store.path, "w", encoding="utf-8") as fh:
            json.dump(doc, fh)
        with self.assertRaises(RecoveryInvalidError) as ctx:
            SnapshotStore(self.room("r")).load()
        self.assertEqual(ctx.exception.context["first_broken_step"], 10)

    def test_version_mismatch_rejected(self):
        sample = self.make_sample()
        task = AdvanceTask(self.room("r"), sample)
        task.run()
        with open(task.store.path, "r", encoding="utf-8") as fh:
            doc = json.load(fh)
        doc["version"] = 999
        with open(task.store.path, "w", encoding="utf-8") as fh:
            json.dump(doc, fh)
        with self.assertRaises(RecoveryInvalidError) as ctx:
            SnapshotStore(self.room("r")).load()
        self.assertEqual(ctx.exception.context["reason"], "version")

    def test_missing_field_rejected(self):
        sample = self.make_sample()
        task = AdvanceTask(self.room("r"), sample)
        task.run()
        with open(task.store.path, "r", encoding="utf-8") as fh:
            doc = json.load(fh)
        del doc["watermark"]
        with open(task.store.path, "w", encoding="utf-8") as fh:
            json.dump(doc, fh)
        with self.assertRaises(RecoveryInvalidError) as ctx:
            SnapshotStore(self.room("r")).load()
        self.assertEqual(ctx.exception.context["field"], "watermark")

    def test_snapshot_write_failure_keeps_previous(self):
        sample = self.make_sample()
        task = AdvanceTask(self.room("r"), sample)
        original_commit = task.store.commit
        committed = []

        def flaky(doc):
            if doc["step"] == 5:
                raise SnapshotWriteError("injected io error", step=5,
                                         error_class="OSError")
            original_commit(doc)
            committed.append(doc["step"])

        task.store.commit = flaky
        with self.assertRaises(SnapshotWriteError) as ctx:
            task.run()
        self.assertEqual(ctx.exception.context["step"], 5)
        self.assertEqual(committed, [1, 2, 3, 4])
        with open(task.store.path, "r", encoding="utf-8") as fh:
            self.assertEqual(json.load(fh)["step"], 4)

    def test_no_reapply_after_resume(self):
        sample = self.make_sample(steps=30)
        crashed = AdvanceTask(self.room("r"), sample)
        with self.assertRaises(HardCrash):
            crashed.run(crash_at=12)
        with open(crashed.store.path, "r", encoding="utf-8") as fh:
            watermark_at_crash = json.load(fh)["watermark"]
        resumed = AdvanceTask(self.room("r"), sample)
        resumed.run(resume=True)
        with open(resumed.trace_path, "r", encoding="utf-8") as fh:
            lines = [json.loads(ln) for ln in fh]
        self.assertEqual(len(lines), 30)
        self.assertEqual(lines[11]["step"], 12)
        applied_after = [ref for ln in lines[12:] for ref in ln["applied"]]
        for player, seqs in watermark_at_crash.items():
            for ref in applied_after:
                if ref["player"] == player:
                    self.assertNotIn(ref["seq"], seqs)


class TestBaseline(WorkspaceMixin):
    def _baseline(self, name="baseline.jsonl"):
        sample = self.make_sample()
        ref = AdvanceTask(self.room("ref"), sample)
        ref.run()
        path = os.path.join(self.tmp, name)
        write_baseline(ref.trace_path, path, meta={})
        return sample, path

    def test_match(self):
        sample, baseline = self._baseline()
        task = AdvanceTask(self.room("r"), sample)
        task.run()
        chain = [json.loads(ln) for ln in
                 self.read_file(task.trace_path).decode().splitlines()]
        report = compare(chain, baseline)
        self.assertEqual(report["status"], "match")

    def test_divergence_locates_first_step(self):
        sample, baseline = self._baseline()
        inject_mod.corrupt_baseline(baseline, at_line=12)
        task = AdvanceTask(self.room("r"), sample)
        task.run()
        chain = [json.loads(ln) for ln in
                 self.read_file(task.trace_path).decode().splitlines()]
        with self.assertRaises(BaselineDivergedError) as ctx:
            compare(chain, baseline)
        self.assertEqual(ctx.exception.context["step"], 11)
        self.assertEqual(ctx.exception.context["field_path"],
                         "chain[11].state_hash")
        self.assertIn("local", ctx.exception.context)
        self.assertIn("baseline", ctx.exception.context)

    def test_version_mismatch(self):
        _sample, baseline = self._baseline()
        with open(baseline, "r", encoding="utf-8") as fh:
            lines = fh.read().splitlines()
        header = json.loads(lines[0])
        header["version"] = 999
        lines[0] = json.dumps(header)
        with open(baseline, "w", encoding="utf-8") as fh:
            fh.write("\n".join(lines) + "\n")
        with self.assertRaises(BaselineVersionError):
            compare([], baseline)

    def test_short_baseline_aborts(self):
        sample, baseline = self._baseline()
        with open(baseline, "r", encoding="utf-8") as fh:
            lines = fh.read().splitlines()
        with open(baseline, "w", encoding="utf-8") as fh:
            fh.write("\n".join(lines[:5]) + "\n")
        task = AdvanceTask(self.room("r"), sample)
        task.run()
        chain = [json.loads(ln) for ln in
                 self.read_file(task.trace_path).decode().splitlines()]
        with self.assertRaises(BaselineMissingError):
            compare(chain, baseline)

    def test_missing_baseline(self):
        with self.assertRaises(BaselineMissingError):
            compare([], os.path.join(self.tmp, "nope.jsonl"))


class TestInjection(WorkspaceMixin):
    def _run_with_injection(self, kind):
        sample = self.make_sample()
        injector = OpInjector(kind, at_step=5, late_window=2)
        task = AdvanceTask(self.room(f"inj_{kind}"), sample)
        task.run(injector=injector)
        return task, injector

    def _event_codes(self, task):
        return [json.loads(ln)["code"] for ln in
                self.read_file(task.events_path).decode().splitlines()]

    def test_duplicate_op_injection(self):
        task, injector = self._run_with_injection("duplicate-op")
        self.assertEqual(injector.report["expected_code"], "E_OP_DUPLICATE")
        self.assertIn("E_OP_DUPLICATE", self._event_codes(task))

    def test_out_of_bounds_injection(self):
        task, injector = self._run_with_injection("out-of-bounds")
        self.assertEqual(injector.report["expected_code"],
                         "E_OP_OUT_OF_BOUNDS")
        self.assertIn("E_OP_OUT_OF_BOUNDS", self._event_codes(task))

    def test_late_op_injection(self):
        task, injector = self._run_with_injection("late-op")
        self.assertEqual(injector.report["expected_code"], "E_OP_LATE")
        self.assertIn("E_OP_LATE", self._event_codes(task))

    def test_truncated_snapshot_injection(self):
        sample = self.make_sample()
        task = AdvanceTask(self.room("r"), sample)
        task.run()
        report = inject_mod.truncate_snapshot(task.store.path)
        self.assertEqual(report["expected_code"], "E_RECOVERY_INVALID")
        with self.assertRaises(RecoveryInvalidError):
            AdvanceTask(self.room("r"), sample).run(resume=True)


class TestConcurrency(WorkspaceMixin):
    def test_busy_on_double_start(self):
        sample = self.make_sample()
        first = AdvanceTask(self.room("r"), sample)
        first._acquire_lock()
        try:
            with self.assertRaises(BusyError):
                AdvanceTask(self.room("r"), sample)._acquire_lock()
        finally:
            first._release_lock()

    def test_stale_lock_taken_over(self):
        sample = self.make_sample()
        room = self.room("r")
        os.makedirs(room, exist_ok=True)
        with open(os.path.join(room, "room.lock"), "w") as fh:
            fh.write("999999999")
        task = AdvanceTask(room, sample)
        task._acquire_lock()
        task._release_lock()

    def test_cooperative_cancel_keeps_last_snapshot(self):
        sample = self.make_sample(steps=30)
        task = AdvanceTask(self.room("r"), sample)
        with self.assertRaises(CancelledError) as ctx:
            task.run(cancel_requested=lambda step: step == 10)
        self.assertEqual(ctx.exception.context["last_committed_step"], 9)
        with open(task.store.path, "r", encoding="utf-8") as fh:
            self.assertEqual(json.load(fh)["step"], 9)
        self.assertFalse(os.path.exists(task.lock_path))
        resumed = AdvanceTask(self.room("r"), sample)
        summary = resumed.run(resume=True)
        self.assertEqual(summary["status"], "completed")


if __name__ == "__main__":
    unittest.main()
