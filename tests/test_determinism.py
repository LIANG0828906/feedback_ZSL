from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from tactical.experiment import generate_and_run


class DeterminismTests(unittest.TestCase):
    def test_same_seed_produces_identical_trace_and_baseline(self) -> None:
        with (
            tempfile.TemporaryDirectory() as first_directory,
            tempfile.TemporaryDirectory() as second_directory,
        ):
            first, first_baseline = generate_and_run(first_directory, [64], 20261121)
            second, second_baseline = generate_and_run(second_directory, [64], 20261121)
            first_trace = first[0].trace_path.read_bytes()
            second_trace = second[0].trace_path.read_bytes()
            self.assertEqual(first_trace, second_trace)
            self.assertEqual(first_baseline, second_baseline)
            first_sample = (Path(first_directory) / "sample_64.json").read_bytes()
            second_sample = (Path(second_directory) / "sample_64.json").read_bytes()
            self.assertEqual(first_sample, second_sample)
            self.assertEqual(first[0].final_version_hash, second[0].final_version_hash)
            self.assertEqual(
                first[0].baseline["trace_sha256"],
                second[0].baseline["trace_sha256"],
            )


if __name__ == "__main__":
    unittest.main()
