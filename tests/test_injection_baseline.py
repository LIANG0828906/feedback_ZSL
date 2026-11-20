import json
import tempfile
import unittest
from pathlib import Path

from replay.errors import ErrorCode, ReplayError
from replay.generator import GenerationPlan, write_generation
from replay.injector import apply_corrupt_snapshot, build_faulty_replay
from replay.pipeline import ROOM_LOCK, ReplayPipeline


class InjectionTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.paths = write_generation(
            self.root / "sample",
            GenerationPlan(room_id="room-inj", seed=5, players=3, total_ticks=10, disorder=2),
        )
        self.v3 = json.loads(Path(self.paths["v3"]).read_text())
        self.v1 = json.loads(Path(self.paths["v1"]).read_text())

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def _assert_ingress_blocked(self, faulty: dict, fault: str, record: int):
        snap_dir = self.root / f"block_{fault}"
        pipe = ReplayPipeline(snapshot_dir=snap_dir, fault=fault, fault_record=record)
        with self.assertRaises(ReplayError) as ctx:
            pipe.run(faulty)
        self.assertEqual(ctx.exception.code.value, fault_to_code(fault))
        self.assertIsNotNone(ctx.exception.injection_point)
        # 入口拦截：不得写出任何快照文件。
        self.assertFalse(snap_dir.exists() and any(snap_dir.iterdir()))

    def test_duplicate_op_blocked_at_ingress(self):
        faulty = build_faulty_replay(self.v1, "duplicate_op", record_index=1)
        self._assert_ingress_blocked(faulty, "duplicate_op", 1)

    def test_out_of_bounds_blocked_at_ingress(self):
        faulty = build_faulty_replay(self.v1, "out_of_bounds", record_index=0)
        self._assert_ingress_blocked(faulty, "out_of_bounds", 0)

    def test_late_op_blocked_at_ingress(self):
        faulty = build_faulty_replay(self.v1, "late_op", record_index=0)
        self._assert_ingress_blocked(faulty, "late_op", 0)

    def test_truncated_diff_blocked_at_ingress(self):
        faulty = build_faulty_replay(self.v3, "truncated_diff", record_index=0)
        self._assert_ingress_blocked(faulty, "truncated_diff", 0)

    def test_format_order_blocked_at_ingress(self):
        faulty = build_faulty_replay(self.v1, "format_order", record_index=0)
        self._assert_ingress_blocked(faulty, "format_order", 0)

    def test_corrupt_snapshot_recovery_code(self):
        snap_dir = self.root / "snap_corrupt"
        ReplayPipeline(snapshot_dir=snap_dir).run(self.paths["v2"])
        apply_corrupt_snapshot(snap_dir, tick=4)
        pipe = ReplayPipeline(
            snapshot_dir=snap_dir,
            fault="corrupt_snapshot",
            fault_extra={"tick": 4, "snapshot_dir": str(snap_dir)},
        )
        with self.assertRaises(ReplayError) as ctx:
            pipe.run(self.paths["v2"])
        self.assertEqual(ctx.exception.code, ErrorCode.E_RECOVERY_INVALID)

    def test_fault_without_condition_rejected(self):
        # 声明注入但数据无故障，入口按 E_FORMAT_INVALID 拒绝，不允许误报。
        pipe = ReplayPipeline(snapshot_dir=self.root / "nofault", fault="duplicate_op")
        with self.assertRaises(ReplayError) as ctx:
            pipe.run(self.v1)
        self.assertEqual(ctx.exception.code, ErrorCode.E_FORMAT_INVALID)


class BaselineTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.paths = write_generation(
            self.root / "sample",
            GenerationPlan(room_id="room-b", seed=9, players=2, total_ticks=8),
        )

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_baseline_missing(self):
        pipe = ReplayPipeline(
            snapshot_dir=self.root / "s",
            baseline_path=self.root / "nope.json",
        )
        with self.assertRaises(ReplayError) as ctx:
            pipe.run(self.paths["v1"])
        self.assertEqual(ctx.exception.code, ErrorCode.E_BASELINE_MISSING)

    def test_baseline_version_mismatch(self):
        baseline = json.loads(Path(self.paths["baseline"]).read_text())
        baseline["state_model_version"] = 99
        wrong = self.root / "wrong.json"
        wrong.write_text(json.dumps(baseline), encoding="utf-8")
        pipe = ReplayPipeline(snapshot_dir=self.root / "s", baseline_path=wrong)
        with self.assertRaises(ReplayError) as ctx:
            pipe.run(self.paths["v1"])
        self.assertEqual(ctx.exception.code, ErrorCode.E_BASELINE_VERSION)

    def test_fork_diagnosis_locates_first_tick_and_path(self):
        baseline = json.loads(Path(self.paths["baseline"]).read_text())
        # 在 tick=4 的基线状态制造资源漂移，首个分叉应定位到 tick 4。
        entry = next(item for item in baseline["entries"] if item["tick"] == 4)
        entry["state"]["resources"]["ammo"] += 7
        entry["state_hash"] = "a" * 64
        drifted = self.root / "drift.json"
        drifted.write_text(json.dumps(baseline), encoding="utf-8")
        pipe = ReplayPipeline(snapshot_dir=self.root / "s", baseline_path=drifted)
        result = pipe.run(self.paths["v1"])
        fork = result.fork
        self.assertIsNotNone(fork)
        self.assertEqual(fork.tick, 4)
        self.assertIn("resources.ammo", fork.field_path)
        self.assertTrue(fork.applied_ops)
        self.assertIsInstance(fork.water_level, int)


class BusyLockTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.paths = write_generation(
            self.root / "sample",
            GenerationPlan(room_id="room-lock", seed=1, players=2, total_ticks=5),
        )

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_second_start_returns_busy_then_releases(self):
        ROOM_LOCK.acquire("room-lock")
        pipe = ReplayPipeline(snapshot_dir=self.root / "s")
        with self.assertRaises(ReplayError) as ctx:
            pipe.run(self.paths["v1"])
        self.assertEqual(ctx.exception.code, ErrorCode.E_BUSY)
        ROOM_LOCK.release("room-lock")
        # 释放后同房间可再次启动。
        result = pipe.run(self.paths["v1"])
        self.assertEqual(len(result.hash_chain), 5)

    def test_cancel_releases_room_lock(self):
        pipe = ReplayPipeline(snapshot_dir=self.root / "s2")
        pipe.request_cancel()
        with self.assertRaises(ReplayError):
            pipe.run(self.paths["v1"])
        # 取消路径经过 finally 释放锁，后续可重新获取。
        ROOM_LOCK.acquire("room-lock-cancel")
        ROOM_LOCK.release("room-lock-cancel")


def fault_to_code(fault: str) -> str:
    mapping = {
        "duplicate_op": ErrorCode.E_OP_DUPLICATE.value,
        "out_of_bounds": ErrorCode.E_OP_OUT_OF_BOUNDS.value,
        "late_op": ErrorCode.E_OP_LATE.value,
        "truncated_diff": ErrorCode.E_DIFF_TRUNCATED.value,
        "format_order": ErrorCode.E_FORMAT_ORDER.value,
    }
    return mapping[fault]


if __name__ == "__main__":
    unittest.main()
