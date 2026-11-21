from __future__ import annotations

import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from a11y_rhythm.compliance import build_compliance_report
from a11y_rhythm.errors import E_RECOVERY_INVALID, RhythmError
from a11y_rhythm.store import DeliverySession, VersionStore

from tests.helpers import conflict_fixture, load_fixture


class SimulatedKill(BaseException):
    pass


class PersistenceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.fixture = conflict_fixture(self.root)
        self.bundle, self.frames, self.coverage, _ = load_fixture(self.fixture)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_atomic_snapshot_hash_chain_tracks_and_report(self) -> None:
        store_path = self.root / "atomic.session.json"
        session = DeliverySession.open(self.bundle, self.frames, self.coverage, store_path)
        snapshot = session.initialize()
        self.assertEqual(snapshot.version, 1)
        self.assertEqual(snapshot.history[0]["version"], 1)
        self.assertTrue(snapshot.chain_hash)
        self.assertTrue(session.store.speech_track_path.exists())
        self.assertTrue(session.store.description_track_path.exists())
        self.assertTrue(session.store.compliance_path.exists())
        self.assertTrue(session.store.trace_path.exists())
        self.assertEqual(list(session.store.path.parent.glob("*.tmp")), [])
        reloaded = VersionStore(store_path).load()
        self.assertIsNotNone(reloaded)
        self.assertEqual(reloaded.chain_hash, snapshot.chain_hash)
        report = reloaded.payload["compliance"]
        expected = build_compliance_report(self.bundle, reloaded.result)
        self.assertEqual(report["report_hash"], expected.report_hash)
        self.assertIn("rules", report)
        self.assertIn("descriptions", report)
        self.assertIn("violations", report)
        trace_lines = session.store.trace_path.read_text(encoding="utf-8").splitlines()
        self.assertTrue(trace_lines)
        first = json.loads(trace_lines[0])
        for key in ("version", "sentence_id", "affected_count", "conflict_chain_length", "content_hash"):
            self.assertIn(key, first)

    def test_version_mismatch_is_rejected(self) -> None:
        session = DeliverySession.open(
            self.bundle, self.frames, self.coverage, self.root / "mismatch.session.json"
        )
        session.initialize()
        with self.assertRaises(RhythmError) as caught:
            session.update_item("ds001", {"text": "Changed description."}, expected_version=99)
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)

    def test_broken_hash_chain_and_missing_fields_are_rejected(self) -> None:
        store_path = self.root / "broken.session.json"
        session = DeliverySession.open(self.bundle, self.frames, self.coverage, store_path)
        session.initialize()
        original = json.loads(store_path.read_text(encoding="utf-8"))
        broken = copy.deepcopy(original)
        broken["payload"]["result"]["result_hash"] = "0" * 64
        store_path.write_text(json.dumps(broken), encoding="utf-8")
        with self.assertRaises(RhythmError) as caught:
            VersionStore(store_path).load()
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)

        missing = copy.deepcopy(original)
        del missing["history"]
        store_path.write_text(json.dumps(missing), encoding="utf-8")
        with self.assertRaises(RhythmError) as caught_missing:
            VersionStore(store_path).load()
        self.assertEqual(caught_missing.exception.code, E_RECOVERY_INVALID)

    def test_forced_termination_recovers_to_exact_clean_result(self) -> None:
        interrupted_store = self.root / "interrupted.session.json"
        interrupted = DeliverySession.open(
            self.bundle, self.frames, self.coverage, interrupted_store
        )
        interrupted.initialize()
        patch = {
            "text": "A longer recovered description resumes the same unfinished conflict chain."
        }
        checks = {"count": 0}

        def stop(checkpoint):
            checks["count"] += 1
            if checks["count"] == 2:
                raise SimulatedKill()

        with self.assertRaises(SimulatedKill):
            interrupted.update_item("ds001", patch, progress=stop)
        self.assertTrue(interrupted.store.journal_path.exists())

        reopened = DeliverySession.open(
            self.bundle, self.frames, self.coverage, interrupted_store
        )
        status = reopened.recover()
        self.assertTrue(status.recovered)
        self.assertGreaterEqual(status.resumed_iteration, 2)
        self.assertEqual(reopened.snapshot.version, 2)
        self.assertEqual([entry["version"] for entry in reopened.snapshot.history], [1, 2])
        self.assertFalse(reopened.store.journal_path.exists())

        clean = DeliverySession.open(
            self.bundle, self.frames, self.coverage, self.root / "clean.session.json"
        )
        clean.initialize()
        clean_snapshot = clean.update_item("ds001", patch)
        self.assertEqual(reopened.snapshot.chain_hash, clean_snapshot.chain_hash)
        self.assertEqual(
            reopened.snapshot.result.result_hash,
            clean_snapshot.result.result_hash,
        )

    def test_real_process_exit_recovers_from_journal(self) -> None:
        store_path = self.root / "process-exit.session.json"
        baseline = DeliverySession.open(
            self.bundle, self.frames, self.coverage, store_path
        )
        baseline.initialize()
        source_path = str(self.fixture["source_path"])
        script = """
import os
import sys
from a11y_rhythm.store import DeliverySession
from a11y_rhythm.timebase import load_source_bundle

bundle, wav_info, frame_table = load_source_bundle(sys.argv[1])
coverage = min(wav_info.sample_count, frame_table.duration_samples)
session = DeliverySession.open(bundle, frame_table, coverage, sys.argv[2])

def progress(_checkpoint):
    os._exit(77)

session.update_item(
    "ds001",
    {"text": "A process exits after its first durable checkpoint."},
    progress=progress,
)
os._exit(78)
"""
        completed = subprocess.run(
            [sys.executable, "-c", script, source_path, str(store_path)],
            cwd=Path(__file__).resolve().parents[1],
            check=False,
        )
        self.assertEqual(completed.returncode, 77)
        self.assertTrue(baseline.store.journal_path.exists())
        reopened = DeliverySession.open(
            self.bundle, self.frames, self.coverage, store_path
        )
        status = reopened.recover()
        self.assertTrue(status.recovered)
        self.assertEqual(reopened.snapshot.version, 2)
        self.assertFalse(reopened.store.journal_path.exists())

    def test_corrupt_journal_is_rejected(self) -> None:
        session = DeliverySession.open(
            self.bundle, self.frames, self.coverage, self.root / "journal.session.json"
        )
        session.initialize()

        def stop(_checkpoint):
            raise SimulatedKill()

        with self.assertRaises(SimulatedKill):
            session.update_item("ds001", {"text": "A different description with readable length."}, progress=stop)
        journal = json.loads(session.store.journal_path.read_text(encoding="utf-8"))
        journal["checkpoint"]["iteration"] = 999
        session.store.journal_path.write_text(json.dumps(journal), encoding="utf-8")
        with self.assertRaises(RhythmError) as caught:
            session.recover()
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)


if __name__ == "__main__":
    unittest.main()
