from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from mediaforge.jsonutil import sha256_bytes
from mediaforge.samplegen import GROUP_SPECS, generate_samples
from mediaforge.truth import compute_truth_fingerprint

from .support import tree_hashes


class SampleGenerationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="mf-samples-")
        cls.root = Path(cls.temp.name)
        cls.first = cls.root / "first"
        cls.second = cls.root / "second"
        cls.index_first = generate_samples(cls.first, seed=20261123)
        cls.index_second = generate_samples(cls.second, seed=20261123)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_generated_tree_is_byte_reproducible(self) -> None:
        self.assertEqual(tree_hashes(self.first), tree_hashes(self.second))
        self.assertEqual(self.index_first["index_fingerprint"], self.index_second["index_fingerprint"])

    def test_group_and_metadata_counts(self) -> None:
        self.assertEqual(len(GROUP_SPECS), 3)
        self.assertEqual(self.index_first["group_count"], 3)
        self.assertEqual(self.index_first["metadata_versions"], ["v1", "v2", "v3"])
        for spec in GROUP_SPECS:
            group = self.first / spec.sample_id
            self.assertTrue((group / "raw" / "video_main.json").exists())
            self.assertTrue((group / "raw" / "audio_main.json").exists())
            for version in (1, 2, 3):
                self.assertTrue((group / "metadata" / f"meta_v{version}.json").exists())

    def test_truth_fingerprints_match_index(self) -> None:
        import json

        index_by_id = {item["sample_id"]: item for item in self.index_first["groups"]}
        for spec in GROUP_SPECS:
            truth = json.loads((self.first / spec.sample_id / "truth.json").read_text(encoding="utf-8"))
            self.assertEqual(compute_truth_fingerprint(truth), truth["fingerprint"])
            self.assertEqual(index_by_id[spec.sample_id]["truth_fingerprint"], truth["fingerprint"])

    def test_known_truth_golden_hashes(self) -> None:
        expected = {
            "g01_neon_city": "6fc7231cbb77156a3997cf805f5b7955b3d370c3570da68db5621b94c1766756",
            "g02_clockwork": "1a0abb38faf516f1b21062d5cb3ca6d4e7d9c4d29b0d83caf6b683c40de03d95",
            "g03_tidal": "e14834853d068ebeea0d44bd69e0f5528f9b5c92d3f5dc98e64d1cc74b1d85e1",
        }
        measured = {item["sample_id"]: item["truth_fingerprint"] for item in self.index_first["groups"]}
        self.assertEqual(measured, expected)

    def test_generated_anomaly_parameters_exist(self) -> None:
        for name in ("conflict", "order", "missing_track", "corrupt_segment"):
            self.assertTrue((self.first / "injections" / f"{name}.json").exists())
        self.assertTrue((self.first / "anomalies" / "corrupt_audio_fragment.json").exists())


if __name__ == "__main__":
    unittest.main()
