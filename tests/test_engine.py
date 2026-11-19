from __future__ import annotations

import json
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

from iovflow.generator import GeneratorConfig, generate
from iovflow.orchestrator import run_job, status
from iovflow.verify import verify_jobs

from .helpers import assert_results_equal, independent_results, load_results, make_config


ROOT = Path(__file__).resolve().parents[1]


class EngineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._tmp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._tmp.name)
        generate(
            GeneratorConfig(
                out_dir=str(cls.root / "data"),
                rows=4000,
                batches=12,
                vehicles=80,
                trips_per_vehicle=3,
                hot_vehicle_share=0.15,
            )
        )
        (cls.root / "window_5m.sql").write_text(
            (ROOT / "sql/window_5m.sql").read_text(encoding="utf-8"), encoding="utf-8"
        )
        cls.batch = run_job(make_config(cls.root, "batch", "batch"))
        cls.stream = run_job(make_config(cls.root, "stream", "stream"))
        cls.fault = run_job(
            make_config(
                cls.root,
                "fault",
                "stream",
                inject_kill="stage=window_aggregate,batch=5,shard=0",
                inject_delay_ms=0,
            )
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls._tmp.cleanup()

    def test_batch_and_stream_final_results_are_equal(self) -> None:
        expected = load_results(self.root / "results-batch.db", "batch")
        actual = load_results(self.root / "results-stream.db", "stream")
        assert_results_equal(self, expected, actual)
        self.assertEqual(self.batch["watermark"]["final"], self.stream["watermark"]["final"])
        self.assertEqual(
            self.batch["watermark"]["history_count"],
            self.stream["watermark"]["history_count"],
        )

    def test_matches_independent_reference(self) -> None:
        expected, late = independent_results(self.root, self.root / "window_5m.sql", 300)
        actual = load_results(self.root / "results-stream.db", "stream")
        assert_results_equal(self, expected, actual)
        self.assertEqual(self.stream["results"]["late_events"], late)

    def test_injected_stage_kill_recovers_without_changing_results(self) -> None:
        self.assertGreaterEqual(self.fault["fault_recovery"]["takeover_count"], 1)
        self.assertEqual(self.fault["status"], "complete")
        expected = load_results(self.root / "results-stream.db", "stream")
        actual = load_results(self.root / "results-fault.db", "fault")
        assert_results_equal(self, expected, actual)
        conn = sqlite3.connect(self.root / "results-fault.db")
        try:
            rows = conn.execute(
                "SELECT vehicle_id, window_start_epoch, COUNT(*) c FROM window_results "
                "WHERE job_id='fault' GROUP BY vehicle_id, window_start_epoch HAVING c>1"
            ).fetchall()
        finally:
            conn.close()
        self.assertEqual(rows, [])

    def test_lateness_zero_and_large_change_closure_and_late_counts(self) -> None:
        zero = run_job(make_config(self.root, "late0", "stream", allowed_lateness=0))
        large = run_job(make_config(self.root, "late600", "stream", allowed_lateness=600))
        self.assertGreater(zero["results"]["late_events"], 0)
        self.assertEqual(large["results"]["late_events"], 0)
        self.assertGreaterEqual(zero["watermark"]["final"], large["watermark"]["final"])
        a = load_results(self.root / "results-late0.db", "late0")
        b = load_results(self.root / "results-late600.db", "late600")
        self.assertNotEqual(a, b)

    def test_job_level_kill_and_resume_is_idempotent(self) -> None:
        state_db = self.root / "state-resume.db"
        results_db = self.root / "results-resume.db"
        cmd = [
            sys.executable,
            "-m",
            "iovflow",
            "run",
            "--sql",
            str(self.root / "window_5m.sql"),
            "--input",
            str(self.root / "data"),
            "--mode",
            "stream",
            "--job-id",
            "resume",
            "--state-db",
            str(state_db),
            "--results-db",
            str(results_db),
            "--temp-dir",
            str(self.root / "tmp-resume"),
            "--report-dir",
            str(self.root / "reports-resume"),
            "--allowed-lateness",
            "300",
            "--parallelism",
            "2",
            "--max-shards",
            "4",
            "--no-progress-timeout",
            "10",
        ]
        proc = subprocess.Popen(
            cmd,
            cwd=str(ROOT),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
        )
        killed = False
        for _ in range(2000):
            time.sleep(0.005)
            if proc.poll() is not None:
                break
            try:
                conn = sqlite3.connect(state_db)
                count = conn.execute(
                    "SELECT COUNT(*) FROM job_batches WHERE job_id='resume' "
                    "AND committed_at IS NOT NULL"
                ).fetchone()[0]
                conn.close()
            except Exception:
                count = 0
            if count >= 4:
                proc.kill()
                killed = True
                break
        proc.wait()
        if proc.stderr is not None:
            proc.stderr.close()
        self.assertTrue(killed, "job finished before the external kill point")
        resumed = subprocess.run(
            cmd + ["--resume"], cwd=str(ROOT), capture_output=True, text=True
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        report = json.loads(resumed.stdout)
        self.assertEqual(report["status"], "complete")
        self.assertGreaterEqual(report["checkpoint"]["resumed_from_batch"], 0)
        expected = load_results(self.root / "results-stream.db", "stream")
        actual = load_results(results_db, "resume")
        assert_results_equal(self, expected, actual)
        # Re-entering the same completed commit point must not add rows.
        again = run_job(
            make_config(
                self.root,
                "resume",
                "stream",
                state_db=str(state_db),
                results_db=str(results_db),
                temp_dir=str(self.root / "tmp-resume"),
                report_dir=str(self.root / "reports-resume"),
                resume=True,
            )
        )
        self.assertEqual(again["status"], "complete")
        self.assertEqual(load_results(results_db, "resume"), actual)


if __name__ == "__main__":
    unittest.main()

