"""验收：相同输入、配置与调度顺序下，两次完整运行输出逐字节一致。"""

import filecmp
import tempfile
import unittest
from pathlib import Path

from replay.config import DEFAULT_CONFIG
from replay.model import MatchDescription
from replay.scheduler import ReplayScheduler
from replay.verifier import verify


def _run_tree(root: Path) -> None:
    manifest = root.parent
    scheduler = ReplayScheduler(
        DEFAULT_CONFIG, root, turn_pause_seconds=0.0
    )
    sample_dir = Path("samples/matches")
    manifest_data = __import__("replay.hashing", fromlist=["read_json"]).read_json(
        sample_dir / "manifest.json"
    )
    try:
        for entry in manifest_data["matches"]:
            if entry.get("expect_error"):
                continue
            scheduler.submit(MatchDescription.load(entry["sample"]))
        scheduler.wait_all()
    finally:
        scheduler.shutdown()


class ByteIdenticalTest(unittest.TestCase):
    def test_two_runs_produce_identical_tree(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            run_a = tmp / "a"
            run_b = tmp / "b"
            _run_tree(run_a)
            _run_tree(run_b)
            files_a = sorted(
                str(path.relative_to(run_a))
                for path in run_a.rglob("*")
                if path.is_file()
            )
            files_b = sorted(
                str(path.relative_to(run_b))
                for path in run_b.rglob("*")
                if path.is_file()
            )
            self.assertEqual(files_a, files_b)
            diffs = []
            for rel in files_a:
                if not filecmp.cmp(run_a / rel, run_b / rel, shallow=False):
                    diffs.append(rel)
            self.assertEqual(diffs, [], f"存在非逐字节一致文件: {diffs}")

            # 两次运行的全部任务都应通过基线比对。
            for match_dir in sorted(p for p in run_a.iterdir() if p.is_dir()):
                baseline = (
                    Path("samples/baselines")
                    / f"{match_dir.name}.baseline.json"
                )
                report = verify(match_dir.name, match_dir, baseline)
                self.assertTrue(report.passed, report.message)


if __name__ == "__main__":
    unittest.main()
