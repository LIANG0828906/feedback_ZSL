from __future__ import annotations

import shutil
import tempfile
import unittest
from pathlib import Path

from tests.helpers import PROJECT_ROOT  # noqa: F401

from fpstore.audio import generate_single_sample
from fpstore.config import FingerprintConfig
from fpstore.errors import E_ALIGN_MISMATCH
from fpstore.library import Library, TrackSpec
from fpstore.migration import MigrationContext
from fpstore.records import pcm_compression_metadata
from fpstore.retrieval import align_offsets, compare_query_sequences


class AlignmentConsistencyTests(unittest.TestCase):
    def test_millisecond_quantization_aligns_to_coarser_basis(self) -> None:
        aligned = align_offsets(
            {"kind": "ms", "bits": 0},
            101,
            {"kind": "ms", "bits": 3},
            12,
        )
        self.assertTrue(aligned.ok)
        self.assertEqual(aligned.mode, "ms_coarsened:3")
        self.assertEqual(aligned.offset_ms, 96 - 96)  # 101ms -> 96ms; 12*8 -> 96ms

    def test_incompatible_frame_durations_are_excluded(self) -> None:
        aligned = align_offsets(
            {"kind": "frame", "sample_rate": 16000, "hop_length": 256},
            8,
            {"kind": "frame", "sample_rate": 8000, "hop_length": 256},
            8,
        )
        self.assertFalse(aligned.ok)
        self.assertEqual(aligned.detail["code"], E_ALIGN_MISMATCH)
        self.assertEqual(aligned.detail["reason"], "frame durations differ")

    def test_consistency_report_has_row_level_hash_and_offset_detail(self) -> None:
        before = {
            "library_version_hash": "sha256:before",
            "candidates": [
                {
                    "sample": "a",
                    "fingerprint_version": 1,
                    "match_start_ms": 100,
                    "match_end_ms": 900,
                    "offset_ms": 100.0,
                    "confidence": 0.9,
                    "match_hashes": ["h1", "h2"],
                }
            ],
        }
        after = {
            "library_version_hash": "sha256:after",
            "candidates": [
                {
                    "sample": "a",
                    "fingerprint_version": 3,
                    "match_start_ms": 150,
                    "match_end_ms": 950,
                    "offset_ms": 150.0,
                    "confidence": 0.9,
                    "match_hashes": ["h2", "h3"],
                }
            ],
        }
        report = compare_query_sequences([before], [after], tolerance_ms=20, query_names=["q"])
        self.assertFalse(report["ok"])
        self.assertEqual(report["differences"][0]["status"], "time_drift")
        difference = report["differences"][0]
        self.assertEqual(difference["sample"], "a")
        self.assertEqual(difference["before"]["library_version_hash"], "sha256:before")
        self.assertEqual(difference["after"]["library_version_hash"], "sha256:after")
        self.assertEqual(difference["offset_delta_ms"], 50.0)
        self.assertEqual(difference["hash_intersection"], ["h2"])

        after["candidates"][0]["match_hashes"] = ["h3"]
        hash_report = compare_query_sequences([before], [after], tolerance_ms=100, query_names=["q"])
        self.assertEqual(hash_report["differences"][0]["status"], "hash_mismatch")
        self.assertEqual(hash_report["differences"][0]["sample"], "a")

    def test_v1_migration_retrieval_consistency_is_row_level(self) -> None:
        temp = Path(tempfile.mkdtemp(prefix="fpstore-consistency-"))
        try:
            wav = generate_single_sample(temp / "a.wav", seed=41, sample_rate=8000, duration_s=4.0)
            library = Library(temp / "library")
            v1 = FingerprintConfig(version=1, time_quant_bits=None)
            library.import_tracks([TrackSpec("a", wav, v1)])
            before_publish = library.publish()
            before = library.search(wav, config=v1, top_k=2)
            library.migrate(MigrationContext("mono", pcm_compression_metadata(16)))
            after = library.search(wav, config=v1, top_k=2)
            report = compare_query_sequences([before], [after], tolerance_ms=20, query_names=["a"])
            self.assertTrue(report["ok"], report["differences"])
            self.assertEqual(report["rows"][0]["sample"], "a")
            self.assertEqual(report["rows"][0]["before"]["library_version_hash"], before_publish["published_version_hash"])
            self.assertNotEqual(report["rows"][0]["after"]["library_version_hash"], before_publish["published_version_hash"])
        finally:
            shutil.rmtree(temp, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
