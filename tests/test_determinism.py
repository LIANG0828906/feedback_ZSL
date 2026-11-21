from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from surface_viewer.samplegen import write_baseline, write_cases


def snapshot_tree(root: Path) -> dict[str, bytes]:
    return {
        str(path.relative_to(root)): path.read_bytes()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


class DeterminismTests(unittest.TestCase):
    def test_same_seed_and_operations_produce_identical_artifacts_and_traces(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            samples = root / "samples"
            write_cases(samples)
            first = root / "baseline-a"
            second = root / "baseline-b"
            write_baseline(samples, first, verify_full=False)
            write_baseline(samples, second, verify_full=False)
            first_tree = snapshot_tree(first)
            second_tree = snapshot_tree(second)
            self.assertEqual(sorted(first_tree), sorted(second_tree))
            for relative_path in first_tree:
                self.assertEqual(
                    first_tree[relative_path],
                    second_tree[relative_path],
                    relative_path,
                )
                if relative_path.endswith("trace.jsonl"):
                    self.assertNotIn(b"timestamp", first_tree[relative_path])


if __name__ == "__main__":
    unittest.main()
