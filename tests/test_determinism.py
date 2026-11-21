from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from pcwb.demo import run_demo
from pcwb.samplegen import DEFAULT_SPECS, generate_samples


def file_tree(root: Path) -> dict[str, bytes]:
    return {
        str(path.relative_to(root)): path.read_bytes()
        for path in sorted(root.rglob("*"))
        if path.is_file() and path.name != ".lock"
    }


class DeterminismTests(unittest.TestCase):
    def test_sample_generator_is_byte_reproducible(self) -> None:
        with (
            tempfile.TemporaryDirectory() as first,
            tempfile.TemporaryDirectory() as second,
        ):
            generate_samples(first, seed=12345, specs=DEFAULT_SPECS[:1])
            generate_samples(second, seed=12345, specs=DEFAULT_SPECS[:1])
            first_files = file_tree(Path(first))
            second_files = file_tree(Path(second))
            self.assertEqual(first_files, second_files)

    def test_scripted_demo_trace_and_clouds_are_byte_reproducible(self) -> None:
        with (
            tempfile.TemporaryDirectory() as first,
            tempfile.TemporaryDirectory() as second,
        ):
            first_root = Path(first) / "demo"
            second_root = Path(second) / "demo"
            run_demo(first_root, seed=4242, progress_threshold_ms=10.0)
            run_demo(second_root, seed=4242, progress_threshold_ms=10.0)

            self.assertEqual(
                (first_root / "trace.jsonl").read_bytes(),
                (second_root / "trace.jsonl").read_bytes(),
            )
            self.assertEqual(
                (first_root / "summary.json").read_bytes(),
                (second_root / "summary.json").read_bytes(),
            )
            first_files = file_tree(first_root)
            second_files = file_tree(second_root)
            for relative_name in first_files:
                if relative_name.startswith(("repo/objects/", "samples/")):
                    self.assertIn(relative_name, second_files)
                    self.assertEqual(
                        first_files[relative_name], second_files[relative_name]
                    )


if __name__ == "__main__":
    unittest.main()
