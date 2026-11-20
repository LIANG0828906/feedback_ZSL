"""要求 6/7/8：并行度无关确定性、分叉归因与样本生成器。"""

import os
import tempfile
import unittest

from replay_service.baseline import (
    build_baseline,
    compare_with_baseline,
    read_baseline,
)
from replay_service.engine import QuotaReplayEngine
from replay_service.errors import E_BASELINE_VERSION, ReplayError
from replay_service.generator import generate_all
from replay_service.parser import parse_input_file
from replay_service.runner import BatchItem, CancellationToken, replay_batch


class ParallelDeterminismTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.sample_dir = os.path.join(self.tmp, "samples")
        generate_all(self.sample_dir, seed=42)

    def _items(self, work_dir):
        items = []
        for name in sorted(os.listdir(self.sample_dir)):
            scenario_dir = os.path.join(self.sample_dir, name)
            if not os.path.isdir(scenario_dir):
                continue
            config, operations = parse_input_file(
                os.path.join(scenario_dir, "input.json")
            )
            items.append(
                BatchItem(
                    name=name,
                    config=config,
                    operations=operations,
                    checkpoint_dir=os.path.join(work_dir, "cp", name),
                    trajectory_path=os.path.join(work_dir, "traj", f"{name}.jsonl"),
                )
            )
        return items

    def test_parallelism_independent_byte_identical(self):
        outcomes_by_degree = {}
        for degree in (1, 2, 3, 5):
            work = os.path.join(self.tmp, f"work{degree}")
            outcomes = replay_batch(self._items(work), parallelism=degree, resume=False)
            outcomes_by_degree[degree] = [
                [r.step_hash for r in o.results] for o in outcomes
            ]
            for item in self._items(work):
                baseline = read_baseline(
                    os.path.join(self.sample_dir, item.name, "baseline.json")
                )
                outcome = next(o for o in outcomes if o.exercise_id == item.config.exercise_id)
                self.assertIsNone(
                    compare_with_baseline(baseline, item.config, outcome.results)
                )
        reference = outcomes_by_degree[1]
        for degree, hashes in outcomes_by_degree.items():
            self.assertEqual(hashes, reference, f"并行度 {degree} 出现分叉")

    def test_trajectory_files_byte_identical_across_runs(self):
        work_a = os.path.join(self.tmp, "traj_a")
        work_b = os.path.join(self.tmp, "traj_b")
        replay_batch(self._items(work_a), parallelism=1, resume=False)
        replay_batch(self._items(work_b), parallelism=4, resume=False)
        for name in os.listdir(os.path.join(work_a, "traj")):
            with open(os.path.join(work_a, "traj", name), "rb") as handle:
                bytes_a = handle.read()
            with open(os.path.join(work_b, "traj", name), "rb") as handle:
                bytes_b = handle.read()
            self.assertEqual(bytes_a, bytes_b, f"轨迹 {name} 非逐字节一致")

    def test_generator_is_byte_reproducible(self):
        again = os.path.join(self.tmp, "samples_again")
        generate_all(again, seed=42)
        for name in os.listdir(self.sample_dir):
            left = os.path.join(self.sample_dir, name)
            right = os.path.join(again, name)
            if not os.path.isdir(left):
                continue
            for filename in ("input.json", "baseline.json", "trajectory.jsonl"):
                with open(os.path.join(left, filename), "rb") as handle:
                    a = handle.read()
                with open(os.path.join(right, filename), "rb") as handle:
                    b = handle.read()
                self.assertEqual(a, b, f"{name}/{filename} 重新生成后不一致")

    def test_divergence_report_finds_first_mismatch_with_seq(self):
        items = self._items(os.path.join(self.tmp, "work_div"))
        item = items[0]
        baseline = build_baseline(item.config,
                                  QuotaReplayEngine(item.config, item.operations).run())
        # 篡改基线第 2 步某玩家累计配额，制造可归因分叉
        target = baseline["steps"][2]
        player = item.config.player_ids[0]
        target["cumulative_used"][player] += 1.0
        actual = QuotaReplayEngine(item.config, item.operations).run()
        divergence = compare_with_baseline(baseline, item.config, actual)
        self.assertIsNotNone(divergence)
        self.assertEqual(divergence.step, 2)
        self.assertEqual(divergence.player_id, player)
        self.assertEqual(divergence.field, "cumulative_used")
        self.assertTrue(divergence.op_client_seqs)

    def test_baseline_version_mismatch_refuses_conclusion(self):
        items = self._items(os.path.join(self.tmp, "work_ver"))
        item = items[0]
        baseline = build_baseline(item.config,
                                  QuotaReplayEngine(item.config, item.operations).run())
        baseline["baseline_version"] = "wrong/v9"
        with self.assertRaises(ReplayError) as ctx:
            compare_with_baseline(
                baseline, item.config,
                QuotaReplayEngine(item.config, item.operations).run(),
            )
        self.assertEqual(ctx.exception.code, E_BASELINE_VERSION)

    def test_cooperative_cancellation(self):
        items = self._items(os.path.join(self.tmp, "work_cancel"))
        token = CancellationToken()
        token.cancel()
        from replay_service.errors import E_CANCELLED

        with self.assertRaises(ReplayError) as ctx:
            replay_batch(items, parallelism=2, token=token)
        self.assertEqual(ctx.exception.code, E_CANCELLED)

    def test_pause_then_resume_produces_same_hashes(self):
        items_paused = self._items(os.path.join(self.tmp, "work_pause"))
        token = CancellationToken()
        token.pause()
        import threading

        def unlock():
            import time
            time.sleep(0.1)
            token.resume()

        threading.Thread(target=unlock, daemon=True).start()
        paused_outcomes = replay_batch(items_paused, parallelism=2, token=token)

        items_normal = self._items(os.path.join(self.tmp, "work_normal"))
        normal_outcomes = replay_batch(items_normal, parallelism=2, resume=False)
        self.assertEqual(
            [[r.step_hash for r in o.results] for o in paused_outcomes],
            [[r.step_hash for r in o.results] for o in normal_outcomes],
        )


if __name__ == "__main__":
    unittest.main()
