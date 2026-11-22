import os
import shutil
import tempfile
import unittest

from forensics.errors import ForensicsError, E_VER, E_STATE
from forensics.loader import load_sample
from forensics.params import AnalysisParams, VersionControl
from forensics.scheduler import Scheduler
from forensics.states import (
    BlockState,
    PENDING,
    RUNNING,
    COMMITTED,
    FAILED,
)
from forensics.engine import run_precise


SAMPLE_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "samples"
)


class TestStateMachine(unittest.TestCase):
    def test_legal_path(self):
        st = BlockState(0)
        self.assertEqual(st.state, PENDING)
        st.transition(RUNNING)
        st.transition(COMMITTED)
        self.assertEqual(st.state, COMMITTED)

    def test_illegal_transition_keeps_state(self):
        st = BlockState(1)
        with self.assertRaises(ForensicsError) as ctx:
            st.transition(COMMITTED)
        self.assertEqual(ctx.exception.code, E_STATE)
        self.assertEqual(st.state, PENDING)

    def test_failed_then_retry_path(self):
        st = BlockState(2)
        st.transition(RUNNING)
        st.transition(FAILED)
        st.transition(RUNNING)
        st.transition(COMMITTED)
        self.assertEqual(st.state, COMMITTED)

    def test_committed_is_terminal(self):
        st = BlockState(3)
        st.transition(RUNNING)
        st.transition(COMMITTED)
        with self.assertRaises(ForensicsError):
            st.transition(RUNNING)


class TestSchedulerAndCheckpoint(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.sample = load_sample(
            os.path.join(SAMPLE_DIR, "sample_rgb_256.png"), block_size=32
        )
        self.params = AnalysisParams()
        self.vc = VersionControl(self.params)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _run(self, workers, fault=None):
        scheduler = Scheduler(
            self.sample,
            self.vc.latest,
            self.tmp,
            max_workers=workers,
            fault_injection=fault,
        )
        return scheduler, scheduler.run()

    def test_parallel_matches_serial_block_by_block(self):
        sched_serial, summary_serial = self._run(1)
        serial_records = dict(sched_serial.results)

        tmp2 = tempfile.mkdtemp()
        try:
            sched_parallel = Scheduler(
                self.sample, self.vc.latest, tmp2, max_workers=4
            )
            summary_parallel = sched_parallel.run()
            self.assertEqual(
                summary_serial["committed_order"],
                summary_parallel["committed_order"],
            )
            for bid, record in serial_records.items():
                other = sched_parallel.results[bid]
                self.assertEqual(
                    record["content_hash"], other["content_hash"]
                )
                self.assertEqual(
                    record["fingerprint"]["digest"],
                    other["fingerprint"]["digest"],
                )
                for metric in (
                    "noise_var",
                    "edge_consistency",
                    "local_contrast",
                    "score",
                ):
                    self.assertAlmostEqual(
                        record["analysis"][metric],
                        other["analysis"][metric],
                    )
        finally:
            shutil.rmtree(tmp2, ignore_errors=True)

    def test_checkpoint_resume_block_identical(self):
        scheduler, _ = self._run(1)
        before = {bid: dict(rec) for bid, rec in scheduler.results.items()}

        # New scheduler instance simulates a process restart.
        restarted = Scheduler(self.sample, self.vc.latest, self.tmp, max_workers=2)
        restored = restarted.restore()
        self.assertEqual(restored, len(before))
        summary = restarted.run()
        self.assertEqual(summary["committed"], len(before))
        for bid, record in before.items():
            self.assertEqual(
                record["content_hash"],
                restarted.results[bid]["content_hash"],
            )

    def test_version_mismatch_invalidates_entire_snapshot(self):
        self._run(1)
        new_snapshot = self.vc.edit(noise_threshold=42.0)
        restarted = Scheduler(self.sample, new_snapshot, self.tmp)
        with self.assertRaises(ForensicsError) as ctx:
            restarted.restore()
        self.assertEqual(ctx.exception.code, E_VER)
        # No partial reuse: store must be empty after invalidation.
        self.assertEqual(restarted.store.persisted_ids(), [])

    def test_algorithm_identity_mismatch_rejected(self):
        self._run(1)
        # Tamper the persisted header to simulate an algorithm-version change.
        import json

        header_path = os.path.join(
            self.tmp, self.sample.name, "snapshot.json"
        )
        with open(header_path) as fh:
            header = json.load(fh)
        header["algorithm_version"] = "alg-9.9.9"
        with open(header_path, "w") as fh:
            json.dump(header, fh)
        restarted = Scheduler(self.sample, self.vc.latest, self.tmp)
        with self.assertRaises(ForensicsError) as ctx:
            restarted.restore()
        self.assertEqual(ctx.exception.code, E_VER)

    def test_freeze_prevents_version_mixing(self):
        scheduler, _ = self._run(1)
        new_snapshot = self.vc.edit(noise_threshold=1.0)
        with self.assertRaises(ForensicsError) as ctx:
            scheduler.require_version(new_snapshot)
        self.assertEqual(ctx.exception.code, E_VER)


class TestFaultInjection(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.sample = load_sample(
            os.path.join(SAMPLE_DIR, "sample_gray_256.png"), block_size=32
        )
        self.vc = VersionControl(AnalysisParams())

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_zero_window_fails_single_block_only(self):
        scheduler = Scheduler(
            self.sample,
            self.vc.latest,
            self.tmp,
            max_workers=2,
            fault_injection={"window_zero": True, "at_block": 0},
        )
        summary = scheduler.run()
        self.assertIn(0, summary["failed"])
        self.assertEqual(scheduler.states[0].state, FAILED)
        # Other blocks are unaffected and still committed.
        self.assertEqual(scheduler.states[1].state, COMMITTED)

    def test_retry_without_fault_succeeds(self):
        scheduler = Scheduler(
            self.sample,
            self.vc.latest,
            self.tmp,
            fault_injection={"bad_granularity": True, "at_block": 3},
        )
        summary = scheduler.run()
        self.assertIn(3, summary["failed"])
        # Fresh scheduler without injection retries the failed block.
        clean = Scheduler(self.sample, self.vc.latest, self.tmp, max_workers=1)
        clean.restore()
        clean.states[3].state = FAILED
        clean.failed[3] = scheduler.failed[3]
        retry_summary = clean.retry_failed()
        self.assertEqual(retry_summary["failed"], [])
        self.assertEqual(clean.states[3].state, COMMITTED)


class TestCancelAndPause(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.sample = load_sample(
            os.path.join(SAMPLE_DIR, "sample_rgb_384x320.png"), block_size=32
        )
        self.vc = VersionControl(AnalysisParams())

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_cancel_recovers_only_persisted_blocks(self):
        scheduler = Scheduler(
            self.sample, self.vc.latest, self.tmp, max_workers=1
        )
        scheduler.cancel()
        summary = scheduler.run()
        self.assertTrue(summary["cancelled"])
        # Restart must only rely on whatever was actually persisted.
        restarted = Scheduler(self.sample, self.vc.latest, self.tmp)
        restored = restarted.restore()
        persisted = set(restarted.store.persisted_ids())
        self.assertEqual(restored, len(persisted))

    def test_pause_blocks_then_resume_completes(self):
        scheduler = Scheduler(
            self.sample, self.vc.latest, self.tmp, max_workers=2
        )
        scheduler.pause()
        # Running while paused would hang; instead verify the event state.
        self.assertFalse(scheduler._pause_event.is_set())
        scheduler.resume()
        summary = scheduler.run()
        self.assertEqual(summary["failed"], [])
        self.assertEqual(summary["committed"], len(scheduler.states))


if __name__ == "__main__":
    unittest.main()
