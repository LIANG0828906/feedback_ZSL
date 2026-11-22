import json
import os
import unittest

from afpipe.errors import SchemaError
from afpipe.hashing import atomic_write_bytes, canonical_bytes
from afpipe.verify import (
    verify_against_baseline,
    verify_against_ground_truth,
)

from . import CONFIG, INDEX_DIR, MANIFEST_PATH, fresh_dir


class VerifySchemaTest(unittest.TestCase):
    def test_ground_truth_verification_passes(self):
        report = verify_against_ground_truth(
            MANIFEST_PATH, INDEX_DIR, CONFIG)
        self.assertTrue(report.ok, report.to_dict())
        self.assertEqual(report.checked_queries, 6)

    def test_baseline_round_trip(self):
        from afpipe.verify import run_queries_from_persistence

        results = run_queries_from_persistence(
            MANIFEST_PATH, INDEX_DIR, CONFIG)
        baseline_path = os.path.join(fresh_dir("baseline"), "baseline.json")
        atomic_write_bytes(
            baseline_path,
            canonical_bytes([r.to_dict() for r in results]),
        )
        report = verify_against_baseline(
            MANIFEST_PATH, INDEX_DIR, baseline_path, CONFIG)
        self.assertTrue(report.ok)

    def test_baseline_drift_reports_first_mismatch(self):
        from afpipe.verify import run_queries_from_persistence

        results = run_queries_from_persistence(
            MANIFEST_PATH, INDEX_DIR, CONFIG)
        drifted = [r.to_dict() for r in results]
        drifted[0]["matches"][0]["confidence"] = 0.0001
        path = os.path.join(fresh_dir("drift"), "baseline.json")
        atomic_write_bytes(path, canonical_bytes(drifted))
        report = verify_against_baseline(
            MANIFEST_PATH, INDEX_DIR, path, CONFIG)
        self.assertFalse(report.ok)
        first = report.issues[0]
        self.assertEqual(first.query_id, drifted[0]["query_id"])
        self.assertEqual(first.field, "confidence")
        self.assertEqual(first.match_source,
                         drifted[0]["matches"][0]["file_id"])

    def test_corrupted_manifest_returns_E_SCHEMA(self):
        path = os.path.join(fresh_dir("schema"), "broken.json")
        atomic_write_bytes(path, b'{"groups": []}')
        with self.assertRaises(SchemaError) as ctx:
            verify_against_ground_truth(path, INDEX_DIR, CONFIG)
        self.assertEqual(ctx.exception.code, "E_SCHEMA")

    def test_invalid_json_returns_E_SCHEMA(self):
        path = os.path.join(fresh_dir("schema2"), "broken.json")
        atomic_write_bytes(path, b"{not json")
        with self.assertRaises(SchemaError) as ctx:
            verify_against_ground_truth(path, INDEX_DIR, CONFIG)
        self.assertEqual(ctx.exception.code, "E_SCHEMA")


if __name__ == "__main__":
    unittest.main()
