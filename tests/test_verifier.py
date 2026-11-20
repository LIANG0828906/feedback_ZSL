"""基线版本闸门与差异定位测试。"""

import json
import tempfile
import unittest
from pathlib import Path

from replay.config import DEFAULT_CONFIG
from replay.errors import E_BASELINE_MISMATCH
from replay.hashing import write_json_atomic
from replay.model import TurnBudget
from replay.samplegen import build_description
from replay.scheduler import ReplayScheduler
from replay.verifier import verify


class VerifierTest(unittest.TestCase):
    def _run_once(self, out: Path):
        desc = build_description("vfy", 5, 99, 150, TurnBudget(500, 500))
        scheduler = ReplayScheduler(DEFAULT_CONFIG, out)
        try:
            scheduler.submit(desc)
            scheduler.wait_all()
        finally:
            scheduler.shutdown()
        return desc

    def test_version_mismatch_short_circuits(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            desc = self._run_once(tmp / "out")
            from replay.verifier import generate_baseline

            baseline = tmp / "b.json"
            generate_baseline(
                desc,
                DEFAULT_CONFIG.task_compute_budget,
                DEFAULT_CONFIG.task_memory_budget,
                baseline,
            )
            raw = json.loads(baseline.read_text(encoding="utf-8"))
            raw["version"] = "0.0.1"
            write_json_atomic(baseline, raw)
            report = verify("vfy", tmp / "out" / "vfy", baseline)
            self.assertEqual(report.code, E_BASELINE_MISMATCH)
            self.assertFalse(report.passed)
            self.assertEqual(report.field_path, "version")

    def test_first_mismatch_turn_and_field_path(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            desc = self._run_once(tmp / "out")
            from replay.verifier import generate_baseline

            baseline = tmp / "b.json"
            generate_baseline(
                desc,
                DEFAULT_CONFIG.task_compute_budget,
                DEFAULT_CONFIG.task_memory_budget,
                baseline,
            )
            raw = json.loads(baseline.read_text(encoding="utf-8"))
            raw["turns"][2]["state_hash"] = "f" * 64
            write_json_atomic(baseline, raw)
            report = verify("vfy", tmp / "out" / "vfy", baseline)
            self.assertFalse(report.passed)
            self.assertEqual(report.first_mismatch_turn, 3)
            self.assertEqual(report.field_path, "turns[3].state_hash")

    def test_clean_run_passes(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            desc = self._run_once(tmp / "out")
            from replay.verifier import generate_baseline

            baseline = tmp / "b.json"
            generate_baseline(
                desc,
                DEFAULT_CONFIG.task_compute_budget,
                DEFAULT_CONFIG.task_memory_budget,
                baseline,
            )
            report = verify("vfy", tmp / "out" / "vfy", baseline)
            self.assertTrue(report.passed, report.message)


if __name__ == "__main__":
    unittest.main()
