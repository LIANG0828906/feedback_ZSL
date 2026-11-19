from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from iovflow.generator import GeneratorConfig, generate


class GeneratorTests(unittest.TestCase):
    def test_stats_and_ratios(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            manifest = generate(
                GeneratorConfig(
                    out_dir=tmp,
                    rows=8000,
                    batches=24,
                    vehicles=120,
                    duplicate_ratio=0.04,
                    delayed_ratio=0.03,
                )
            )
            stats = manifest["stats"]
            self.assertEqual(stats["rows"], 8000)
            self.assertEqual(stats["batch_count"], 24)
            self.assertAlmostEqual(stats["duplicate_ratio"], 0.04, delta=0.005)
            self.assertAlmostEqual(stats["delayed_ratio"], 0.03, delta=0.005)
            self.assertGreater(stats["in_batch_inversions"], 0)
            self.assertGreater(stats["cross_batch_older_rows"], 0)
            self.assertTrue(manifest["validation"]["ok"])
            files = sorted((Path(tmp) / "events").glob("*.jsonl"))
            self.assertEqual(len(files), 24)

    def test_fixed_seed_is_reproducible(self) -> None:
        def digest(root: str) -> str:
            h = hashlib.sha256()
            for path in sorted((Path(root) / "events").glob("*.jsonl")):
                h.update(path.read_bytes())
            return h.hexdigest()

        with tempfile.TemporaryDirectory() as a, tempfile.TemporaryDirectory() as b:
            generate(GeneratorConfig(out_dir=a, rows=3000, batches=10, vehicles=40))
            generate(GeneratorConfig(out_dir=b, rows=3000, batches=10, vehicles=40))
            self.assertEqual(digest(a), digest(b))


if __name__ == "__main__":
    unittest.main()

