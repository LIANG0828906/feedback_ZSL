"""要求 4/5：原子检查点、写失败保护与续算完整性。"""

import json
import os
import tempfile
import unittest

from replay_service.checkpoint import CheckpointStore
from replay_service.engine import QuotaReplayEngine
from replay_service.errors import (
    E_CHECKPOINT_WRITE,
    E_RESUME_INVALID,
    ReplayError,
)
from tests.test_engine_settlement import make_config, op


class CheckpointResumeTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.config = make_config("reject", steps=6)
        self.operations = [
            op(f"o{i}", "AB"[i % 2], i // 2, i % 6, 3.0) for i in range(10)
        ]

    def test_write_failure_keeps_previous_checkpoint(self):
        store = CheckpointStore(self.tmp)
        engine = QuotaReplayEngine(self.config, self.operations)
        engine.step()
        store.write(self.config, engine.snapshot())
        with open(store.path, encoding="utf-8") as handle:
            good_payload = handle.read()

        engine.step()
        with self.assertRaises(ReplayError) as ctx:
            store.write(self.config, engine.snapshot(), fail_at="before_replace")
        self.assertEqual(ctx.exception.code, E_CHECKPOINT_WRITE)
        self.assertEqual(ctx.exception.context["step"], 2)
        with open(store.path, encoding="utf-8") as handle:
            self.assertEqual(handle.read(), good_payload)
        leftovers = [
            name
            for name in os.listdir(self.tmp)
            if name.startswith(".checkpoint.")
        ]
        self.assertEqual(leftovers, [])

    def test_resume_does_not_double_count(self):
        store = CheckpointStore(self.tmp)
        engine = QuotaReplayEngine(self.config, self.operations)
        for _ in range(3):
            engine.step()
        store.write(self.config, engine.snapshot())
        snapshot_used = dict(engine.cumulative_used)
        snapshot_hash = engine.prev_hash

        resumed = store.resume_engine(self.config, self.operations)
        self.assertEqual(resumed.step_index, 3)
        resumed.run()

        fresh = QuotaReplayEngine(self.config, self.operations)
        fresh.run()
        self.assertEqual(
            resumed.cumulative_used, fresh.cumulative_used
        )
        self.assertEqual(resumed.prev_hash, fresh.prev_hash)
        self.assertEqual(resumed.step_hashes() if hasattr(resumed, "step_hashes") else
                         [r.step_hash for r in resumed.results],
                         [r.step_hash for r in fresh.results])
        self.assertNotEqual(resumed.cumulative_used, {})
        self.assertEqual(snapshot_used["A"], resumed.step_records[2]["cumulative_used"]["A"])
        self.assertEqual(snapshot_hash, resumed.step_records[2]["step_hash"])

    def test_resume_via_runner_is_byte_identical(self):
        from replay_service.runner import replay_exercise

        traj = os.path.join(self.tmp, "traj.jsonl")
        cp_dir = os.path.join(self.tmp, "cp")
        full = replay_exercise(
            self.config, self.operations,
            checkpoint_dir=cp_dir, trajectory_path=traj, resume=False,
        )
        # 模拟中途终止：只跑 2 步后再续算
        partial_dir = os.path.join(self.tmp, "cp_partial")
        store = CheckpointStore(partial_dir)
        engine = QuotaReplayEngine(self.config, self.operations)
        engine.step()
        engine.step()
        store.write(self.config, engine.snapshot())
        resumed = replay_exercise(
            self.config, self.operations,
            checkpoint_dir=partial_dir,
            trajectory_path=os.path.join(self.tmp, "traj_resumed.jsonl"),
            resume=True,
        )
        self.assertEqual(
            [r.step_hash for r in full.results],
            [r.step_hash for r in resumed.results],
        )

    def test_broken_hash_chain_locates_first_bad_step(self):
        store = CheckpointStore(self.tmp)
        engine = QuotaReplayEngine(self.config, self.operations)
        engine.run()
        store.write(self.config, engine.snapshot())

        with open(store.path, encoding="utf-8") as handle:
            payload = json.load(handle)
        # 篡改第 3 步（下标 2）的累计配额但保留原哈希
        payload["step_records"][2]["cumulative_used"]["A"] = 999999.0
        with open(store.path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle)

        with self.assertRaises(ReplayError) as ctx:
            store.resume_engine(self.config, self.operations)
        self.assertEqual(ctx.exception.code, E_RESUME_INVALID)
        self.assertEqual(ctx.exception.context["broken_step"], 2)

    def test_broken_link_locates_first_broken_step(self):
        store = CheckpointStore(self.tmp)
        engine = QuotaReplayEngine(self.config, self.operations)
        engine.run()
        store.write(self.config, engine.snapshot())

        with open(store.path, encoding="utf-8") as handle:
            payload = json.load(handle)
        payload["step_records"][4]["prev_hash"] = "f" * 64
        with open(store.path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle)

        with self.assertRaises(ReplayError) as ctx:
            store.resume_engine(self.config, self.operations)
        self.assertEqual(ctx.exception.code, E_RESUME_INVALID)
        self.assertEqual(ctx.exception.context["broken_step"], 4)

    def test_version_mismatch_aborts_before_any_write(self):
        store = CheckpointStore(self.tmp)
        engine = QuotaReplayEngine(self.config, self.operations)
        engine.step()
        store.write(self.config, engine.snapshot())
        with open(store.path, encoding="utf-8") as handle:
            payload = json.load(handle)
        payload["version"] = "quota-replay-cp/v0"
        with open(store.path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle)

        before = open(store.path, encoding="utf-8").read()
        with self.assertRaises(ReplayError) as ctx:
            store.resume_engine(self.config, self.operations)
        self.assertEqual(ctx.exception.code, E_RESUME_INVALID)
        self.assertEqual(open(store.path, encoding="utf-8").read(), before)

    def test_missing_field_aborts(self):
        store = CheckpointStore(self.tmp)
        engine = QuotaReplayEngine(self.config, self.operations)
        engine.step()
        store.write(self.config, engine.snapshot())
        with open(store.path, encoding="utf-8") as handle:
            payload = json.load(handle)
        del payload["cumulative_used"]
        with open(store.path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle)
        with self.assertRaises(ReplayError) as ctx:
            store.resume_engine(self.config, self.operations)
        self.assertEqual(ctx.exception.code, E_RESUME_INVALID)

    def test_checkpoint_contains_watermark_and_chain_prefix(self):
        store = CheckpointStore(os.path.join(self.tmp, "cp_fields"))
        engine = QuotaReplayEngine(self.config, self.operations)
        engine.step()
        engine.step()
        store.write(self.config, engine.snapshot())
        with open(store.path, encoding="utf-8") as handle:
            payload = json.load(handle)
        self.assertEqual(payload["step_index"], 2)
        self.assertEqual(payload["buffer_watermark"], engine.buffer_watermark)
        self.assertEqual(payload["prev_hash"], engine.prev_hash)
        self.assertEqual(payload["step_records"][0]["prev_hash"], "0" * 64)
        self.assertEqual(
            payload["step_records"][1]["prev_hash"],
            payload["step_records"][0]["step_hash"],
        )
        self.assertIn("cumulative_penalty", payload)
        self.assertIn("penalties", payload["step_records"][1])


if __name__ == "__main__":
    unittest.main()
