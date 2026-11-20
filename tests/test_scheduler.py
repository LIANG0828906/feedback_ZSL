"""调度器：并行上限、稳定排序、E_BUSY、取消、暂停与状态查询。"""

import tempfile
import time
import unittest
from pathlib import Path

from replay.config import DEFAULT_CONFIG, FrozenConfig
from replay.errors import E_BUSY, E_BUDGET_EXCEEDED, E_CANCELLED, OK
from replay.model import TurnBudget
from replay.samplegen import build_description
from replay.scheduler import ReplayScheduler


def desc(name, turns=5, seed=1):
    return build_description(name, turns, seed, 150, TurnBudget(500, 500))


def make_scheduler(cfg, path):
    # 测试中关闭可调度停顿，避免整体耗时。
    return ReplayScheduler(cfg, path, turn_pause_seconds=0.0)


class SchedulerTest(unittest.TestCase):
    def test_concurrency_limit_and_stable_order(self):
        cfg = FrozenConfig("1.0.0", 2, 10_000, 10_000, 10)
        with tempfile.TemporaryDirectory() as tmp:
            scheduler = make_scheduler(cfg, Path(tmp) / "out")
            try:
                names = [f"m{i}" for i in range(5)]
                for index, name in enumerate(names):
                    code, _ = scheduler.submit(desc(name, turns=3, seed=index))
                    self.assertEqual(code, OK)
                scheduler.wait_all()
                statuses = scheduler.statuses()
                self.assertEqual([s["match_id"] for s in statuses], names)
                self.assertTrue(
                    all(s["status"] == "COMPLETED" for s in statuses)
                )
            finally:
                scheduler.shutdown()

    def test_duplicate_submit_returns_busy(self):
        with tempfile.TemporaryDirectory() as tmp:
            scheduler = make_scheduler(DEFAULT_CONFIG, Path(tmp) / "out")
            try:
                code, task = scheduler.submit(desc("dup", turns=8))
                self.assertEqual(code, OK)
                # 立即再次提交同一未结束任务。
                task.request_pause()
                code2, same = scheduler.submit(desc("dup", turns=8))
                self.assertEqual(code2, E_BUSY)
                self.assertIs(same, task)
                task.resume()
                scheduler.wait_all()
            finally:
                scheduler.shutdown()

    def test_cancel_releases_slot_and_keeps_snapshots(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "out"
            # 该用例需要真实的回合边界停顿，以便暂停/取消稳定生效。
            scheduler = ReplayScheduler(DEFAULT_CONFIG, out, turn_pause_seconds=0.02)
            try:
                _, task = scheduler.submit(desc("cancel_me", turns=12))
                # 等待至少一个快照原子提交，保证取消后磁盘保留已提交回合。
                deadline = time.time() + 5
                while task.current_turn < 1 and time.time() < deadline:
                    time.sleep(0.01)
                self.assertLess(time.time(), deadline)
                task.request_pause()
                paused = False
                while time.time() < deadline:
                    if task.status == "PAUSED":
                        paused = True
                        break
                    time.sleep(0.01)
                self.assertTrue(paused, "任务未进入 PAUSED 状态")
                task.request_cancel()
                scheduler.wait_all()
                status = task.snapshot_status()
                self.assertEqual(status["status"], "CANCELLED")
                self.assertEqual(status["error_code"], E_CANCELLED)
                snapshot_dir = out / "cancel_me" / "snapshots"
                remaining = list(snapshot_dir.glob("snapshot_*.json"))
                self.assertGreaterEqual(len(remaining), 1)
                # 槽位已释放：新任务可立即获得并发资源并完成。
                code, _ = scheduler.submit(desc("after_cancel", turns=2))
                self.assertEqual(code, OK)
                scheduler.wait_all()
                self.assertEqual(
                    scheduler.get_task("after_cancel").status, "COMPLETED"
                )
            finally:
                scheduler.shutdown()

    def test_budget_exceeded_code(self):
        tight = FrozenConfig("1.0.0", 1, 200, 200, 10)
        with tempfile.TemporaryDirectory() as tmp:
            scheduler = make_scheduler(tight, Path(tmp) / "out")
            try:
                scheduler.submit(desc("broke", turns=10))
                scheduler.wait_all()
                status = scheduler.get_task("broke").snapshot_status()
                self.assertEqual(status["status"], "FAILED")
                self.assertEqual(status["error_code"], E_BUDGET_EXCEEDED)
            finally:
                scheduler.shutdown()

    def test_transition_query(self):
        with tempfile.TemporaryDirectory() as tmp:
            scheduler = make_scheduler(DEFAULT_CONFIG, Path(tmp) / "out")
            try:
                scheduler.submit(desc("trans", turns=2))
                scheduler.wait_all()
                info = scheduler.transitions("trans")
                self.assertEqual(info["current"], "COMPLETED")
                self.assertTrue(info["terminal"])
                self.assertEqual(info["allowed_next"], [])
            finally:
                scheduler.shutdown()

    def test_queue_when_pool_exhausted(self):
        cfg = FrozenConfig("1.0.0", 1, 10_000, 10_000, 10)
        with tempfile.TemporaryDirectory() as tmp:
            scheduler = ReplayScheduler(
                cfg, Path(tmp) / "out", turn_pause_seconds=0.02
            )
            try:
                first = desc("q0", turns=6)
                code, task0 = scheduler.submit(first)
                self.assertEqual(code, OK)
                task0.request_pause()
                deadline = time.time() + 5
                while task0.status != "PAUSED" and time.time() < deadline:
                    time.sleep(0.01)
                # 池已满（唯一槽位被 q0 持有），后续任务必须稳定排队。
                code, _ = scheduler.submit(desc("q1", turns=3))
                self.assertEqual(code, OK)
                code, _ = scheduler.submit(desc("q2", turns=3))
                self.assertEqual(code, OK)
                statuses = {s["match_id"]: s["status"] for s in scheduler.statuses()}
                self.assertEqual(statuses["q0"], "PAUSED")
                self.assertEqual(statuses["q1"], "WAITING")
                self.assertEqual(statuses["q2"], "WAITING")
                task0.resume()
                scheduler.wait_all()
                self.assertEqual(
                    [s["match_id"] for s in scheduler.statuses()],
                    ["q0", "q1", "q2"],
                )
            finally:
                scheduler.shutdown()


if __name__ == "__main__":
    unittest.main()
