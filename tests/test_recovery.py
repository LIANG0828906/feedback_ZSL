"""快照原子提交、哈希链恢复与 E_RECOVERY_INVALID 场景。"""

import json
import tempfile
import unittest
from pathlib import Path

from replay.config import DEFAULT_CONFIG, FrozenConfig
from replay.errors import E_BUDGET_EXCEEDED, E_RECOVERY_INVALID
from replay.hashing import read_json
from replay.model import TurnBudget
from replay.samplegen import build_description
from replay.scheduler import ReplayScheduler
from replay.snapshot import SnapshotStore


def desc(name="recover", turns=10, seed=42):
    return build_description(name, turns, seed, 160, TurnBudget(500, 500))


class RecoveryTest(unittest.TestCase):
    def test_resume_after_budget_failure_matches_full_run(self):
        tight = FrozenConfig("1.0.0", 1, 260, 260, 10)
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "out"
            # 第一次：小预算导致中途失败，保留已提交快照。
            scheduler = ReplayScheduler(tight, out)
            try:
                scheduler.submit(desc())
                scheduler.wait_all()
                failed = scheduler.get_task("recover").snapshot_status()
                self.assertEqual(failed["error_code"], E_BUDGET_EXCEEDED)
                committed = failed["current_turn"]
                self.assertLess(committed, 10)
            finally:
                scheduler.shutdown()

            # 第二次：恢复模式 + 充足预算，从下一未执行回合继续。
            scheduler2 = ReplayScheduler(DEFAULT_CONFIG, out)
            try:
                code, _ = scheduler2.submit(desc(), recover=True)
                self.assertEqual(code, "OK")
                scheduler2.wait_all()
                done = scheduler2.get_task("recover").snapshot_status()
                self.assertEqual(done["status"], "COMPLETED")
                self.assertEqual(done["current_turn"], 10)
            finally:
                scheduler2.shutdown()

            # 与一次性充足预算跑完的结果逐字节比较。
            full_out = Path(tmp) / "full"
            scheduler3 = ReplayScheduler(DEFAULT_CONFIG, full_out)
            try:
                scheduler3.submit(desc())
                scheduler3.wait_all()
            finally:
                scheduler3.shutdown()
            for turn in range(1, 11):
                name = SnapshotStore.snapshot_filename(turn)
                a = (out / "recover" / "snapshots" / name).read_bytes()
                b = (full_out / "recover" / "snapshots" / name).read_bytes()
                self.assertEqual(a, b, f"回合 {turn} 快照不一致")
            self.assertEqual(
                (out / "recover" / "result.json").read_bytes(),
                (full_out / "recover" / "result.json").read_bytes(),
            )
            self.assertEqual(
                read_json(out / "recover" / "result.json")["event_stream_hash"],
                read_json(full_out / "recover" / "result.json")[
                    "event_stream_hash"
                ],
            )

    def test_tampered_snapshot_chain_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "out"
            scheduler = ReplayScheduler(DEFAULT_CONFIG, out)
            try:
                scheduler.submit(desc(turns=4))
                scheduler.wait_all()
            finally:
                scheduler.shutdown()
            store = SnapshotStore(out / "recover")
            # 篡改第 2 个快照的状态内容但不更新哈希。
            path = store.path_for(2)
            raw = json.loads(path.read_text(encoding="utf-8"))
            raw["state"]["players"][0]["hp"] += 1
            path.write_text(json.dumps(raw), encoding="utf-8")
            with self.assertRaises(Exception) as ctx:
                store.recovery_point()
            self.assertEqual(ctx.exception.code, E_RECOVERY_INVALID)

            scheduler2 = ReplayScheduler(DEFAULT_CONFIG, out)
            try:
                code, task = scheduler2.submit(desc(turns=4), recover=True)
                scheduler2.wait_all()
                self.assertEqual(task.status, "FAILED")
                self.assertEqual(task.error_code, E_RECOVERY_INVALID)
            finally:
                scheduler2.shutdown()

    def test_recover_without_snapshot_is_invalid(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "out"
            scheduler = ReplayScheduler(DEFAULT_CONFIG, out)
            try:
                _, task = scheduler.submit(desc(turns=3), recover=True)
                scheduler.wait_all()
                self.assertEqual(task.error_code, E_RECOVERY_INVALID)
            finally:
                scheduler.shutdown()

    def test_no_tmp_files_left_after_commits(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "out"
            scheduler = ReplayScheduler(DEFAULT_CONFIG, out)
            try:
                scheduler.submit(desc(turns=3))
                scheduler.wait_all()
            finally:
                scheduler.shutdown()
            leftovers = list((out / "recover").rglob("*.tmp"))
            self.assertEqual(leftovers, [])


if __name__ == "__main__":
    unittest.main()
