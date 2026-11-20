"""确定性与规则引擎测试：同输入必须逐字节一致。"""

import unittest
from pathlib import Path

from replay.config import DEFAULT_CONFIG
from replay.engine import RuleEngine
from replay.hashing import hash_json
from replay.samplegen import build_description
from replay.model import TurnBudget
from replay.verifier import generate_baseline


def _desc(name="det", seed=777, turns=10):
    return build_description(name, turns, seed, 180, TurnBudget(500, 500))


class DeterminismTest(unittest.TestCase):
    def test_engine_runs_are_identical(self):
        desc = _desc()
        engine_a = RuleEngine(
            desc,
            DEFAULT_CONFIG.task_compute_budget,
            DEFAULT_CONFIG.task_memory_budget,
        )
        engine_b = RuleEngine(
            desc,
            DEFAULT_CONFIG.task_compute_budget,
            DEFAULT_CONFIG.task_memory_budget,
        )
        snaps_a, snaps_b = [], []
        for _ in range(desc.turn_count):
            snaps_a.append(engine_a.run_turn())
            snaps_b.append(engine_b.run_turn())
        self.assertEqual(
            [s["snapshot_hash"] for s in snaps_a],
            [s["snapshot_hash"] for s in snaps_b],
        )
        self.assertEqual(
            hash_json(engine_a.events), hash_json(engine_b.events)
        )
        self.assertEqual(engine_a.verdict(), engine_b.verdict())

    def test_baseline_file_byte_stable(self):
        import tempfile

        desc = _desc("byte_stable", turns=7)
        with tempfile.TemporaryDirectory() as tmp:
            path_a = Path(tmp) / "a.json"
            path_b = Path(tmp) / "b.json"
            generate_baseline(
                desc,
                DEFAULT_CONFIG.task_compute_budget,
                DEFAULT_CONFIG.task_memory_budget,
                path_a,
            )
            generate_baseline(
                desc,
                DEFAULT_CONFIG.task_compute_budget,
                DEFAULT_CONFIG.task_memory_budget,
                path_b,
            )
            self.assertEqual(path_a.read_bytes(), path_b.read_bytes())

    def test_events_have_sequential_ids(self):
        desc = _desc(turns=4)
        engine = RuleEngine(
            desc,
            DEFAULT_CONFIG.task_compute_budget,
            DEFAULT_CONFIG.task_memory_budget,
        )
        for _ in range(desc.turn_count):
            engine.run_turn()
        seqs = [event["seq"] for event in engine.events]
        self.assertEqual(seqs, list(range(len(seqs))))


if __name__ == "__main__":
    unittest.main()
